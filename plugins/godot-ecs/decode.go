// Package main 是 godot-ecs 解码插件：解析 Godot "op" 客户端与 ecs-server 之间
// 的世界同步流量。
//
// # 协议真源
//
//	op/net_client.gd                        （客户端收发）
//	ecs-server/internal/transport/hub.go    （收包 / 帧格式 / 上行消息）
//	ecs-server/internal/ecsx/game.go        （下行消息 welcome / pong / state）
//
// 传输层：TCP，监听 :9250。
// 帧格式：每帧 = [4 字节大端长度 N][N 字节 UTF-8 JSON]，判别字段为 "t"。
//
//	上行 (client -> server)：pos{x,y,z,yaw,mv} 20Hz、ping{ts}
//	下行 (server -> client)：welcome{id,hz,min_x,max_x,min_z,max_z,npcs}、
//	                        pong{ts,tick}、state{tick,ents[]} 30Hz
//
// 帧可以跨 TCP 段，因此必须先 framing.ExtractL7 剥头、再经 Reassembler 重组，
// 不能把 DecodeRequest.payload 当 L7 直接解（payload 是完整链路层帧）。
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

const (
	// serverPort 是 ecs-server 的监听端口，用于判定方向。
	serverPort = 9250
	// maxFrame 与服务端 transport.frameMax 保持一致（16MB），超过即视为失步。
	maxFrame = 16 << 20
	// rawMax 是兜底事件里保留的原始 JSON 最大长度。
	rawMax = 4096
)

// 消息判别值（"t" 字段）。
const (
	msgPos     = "pos"
	msgPing    = "ping"
	msgWelcome = "welcome"
	msgPong    = "pong"
	msgState   = "state"
)

// 方向常量，写入 _meta.direction（宿主解析进 Context.Direction）。
const (
	dirC2S    = "client_to_server"
	dirS2C    = "server_to_client"
	dirUnknwn = "unknown"
)

// ra 是跨包 / 跨会话的 TCP 重组器，按 FlowKey 每个方向一份缓冲。
var ra = framing.NewReassembler()

// Event 是一条解码结果。
type Event struct {
	EventType        string
	SchemaID         string
	Payload          map[string]any // 业务字段（payload 根对象）
	Meta             map[string]any // 保留 _meta 对象
	CorrelationKey   string
	CausationInputID string
}

// Decode 把一个捕获帧解成零到多条事件。它永不 panic、畸形输入也不返回 error：
// 能解出多少就返回多少，剩下的字节等下一个段（契约：malformed-input-safe、
// one-input-may-carry-many-messages）。
func Decode(req *pb.DecodeRequest) (events []*Event, err error) {
	events = []*Event{} // 即使一条都解不出也返回非 nil
	defer func() {
		if r := recover(); r != nil {
			events = []*Event{}
		}
	}()

	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		// 非 IP 流量、截断帧，或纯 ACK / 握手 / FIN —— 正常现象，不是错误。
		return events, nil
	}

	dir := directionOf(seg)
	corr := seg.Flow.Canonical()

	s := ra.Push(seg)
	for {
		raw := s.Bytes()
		if len(raw) < 4 {
			break // 帧头都没收全，等下一个段
		}
		n := int(binary.BigEndian.Uint32(raw[:4]))
		if n <= 0 || n > maxFrame {
			// 长度非法说明流已失步（抓包漏段或 mid-stream attach）：
			// 丢弃该方向的重排缓冲，等下一次连接/新数据重新对齐。
			ra.Forget(seg.Flow)
			break
		}
		if len(raw) < 4+n {
			break // 负载没收全，等下一个段
		}
		body := raw[4 : 4+n]

		ev := decodeMessage(body, dir)
		ev.CorrelationKey = corr
		events = append(events, ev)

		s.Consume(4 + n) // n>0，必然推进，不会死循环
	}
	return events, nil
}

// directionOf 按端口判定方向：目的端口是 9250 即上行，源端口是 9250 即下行。
// 端口不可用时（代理类输入没有传输头）返回空串，交由 decodeMessage 按消息类型推断。
func directionOf(seg framing.Segment) string {
	switch {
	case seg.Flow.Dst.Port() == serverPort:
		return dirC2S
	case seg.Flow.Src.Port() == serverPort:
		return dirS2C
	default:
		return ""
	}
}

// decodeMessage 解析单帧 JSON 并产出一条事件。JSON 损坏时产出 unknown 兜底事件，
// 保证字节流仍然向前推进。
func decodeMessage(body []byte, dir string) *Event {
	obj, ok := parseJSON(body)
	if !ok {
		return newEvent("unknown", dir, msgTypeOf(nil), map[string]any{
			"msg_type": "",
			"raw":      truncate(string(body)),
		}, false)
	}

	t := msgTypeOf(obj)
	switch t {
	case msgWelcome:
		return newEvent("welcome", dir, t, decodeWelcome(obj), true)
	case msgState:
		return newEvent("state", dir, t, decodeState(obj), true)
	case msgPong:
		return newEvent("pong", dir, t, decodePong(obj), true)
	case msgPos:
		return newEvent("pos", dir, t, decodePos(obj), false)
	case msgPing:
		return newEvent("ping", dir, t, decodePing(obj), false)
	default:
		return newEvent("unknown", dir, t, map[string]any{
			"msg_type": t,
			"raw":      truncate(string(body)),
		}, false)
	}
}

// newEvent 组装事件。name 同时用作 event_type 与 schema id 前缀；push 表示这是
// 服务端主动下推的消息（welcome / state / pong）。
//
// dir 为空时（代理类输入无端口信息）按消息类型推断方向：上行只有 pos/ping，
// 下行只有 welcome/pong/state。
func newEvent(name, dir, msgType string, payload map[string]any, push bool) *Event {
	finalDir := dir
	if finalDir == "" {
		finalDir = inferDirection(msgType)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return &Event{
		EventType: "godot_ecs." + name,
		SchemaID:  "godot_ecs." + name + ".v1",
		Payload:   payload,
		Meta: map[string]any{
			"direction": finalDir,
			"msg_name":  msgType,
			"is_push":   push,
		},
	}
}

// inferDirection 在拿不到端口信息时的兜底方向推断。
func inferDirection(msgType string) string {
	switch msgType {
	case msgPos, msgPing:
		return dirC2S
	case msgWelcome, msgPong, msgState:
		return dirS2C
	default:
		return dirUnknwn
	}
}

// decodeWelcome 解 welcome{id,hz,min_x,max_x,min_z,max_z,npcs}。
func decodeWelcome(o map[string]any) map[string]any {
	out := map[string]any{
		"id": intOf(o["id"]),
		"hz": intOf(o["hz"]),
	}
	if v, ok := o["npcs"]; ok {
		out["npcs"] = intOf(v)
	}
	for _, k := range []string{"min_x", "max_x", "min_z", "max_z"} {
		if v, ok := o[k]; ok {
			out[k] = num(floatOf(v))
		}
	}
	return out
}

// decodePos 解 pos{x,y,z,yaw,mv}。
func decodePos(o map[string]any) map[string]any {
	out := map[string]any{
		"x":   num(floatOf(o["x"])),
		"y":   num(floatOf(o["y"])),
		"z":   num(floatOf(o["z"])),
		"yaw": num(floatOf(o["yaw"])),
		"mv":  boolOf(o["mv"]),
	}
	if v, ok := o["ts"]; ok {
		out["ts"] = intOf(v)
	}
	return out
}

// decodePing 解 ping{ts}。
func decodePing(o map[string]any) map[string]any {
	return map[string]any{"ts": intOf(o["ts"])}
}

// decodePong 解 pong{ts,tick}。
func decodePong(o map[string]any) map[string]any {
	return map[string]any{
		"ts":   intOf(o["ts"]),
		"tick": intOf(o["tick"]),
	}
}

// decodeState 解 state{tick,ents[]}，ent_count 为派生字段方便直接聚合。
func decodeState(o map[string]any) map[string]any {
	out := map[string]any{"tick": intOf(o["tick"])}
	ents := []any{}
	if arr, ok := o["ents"].([]any); ok {
		for _, it := range arr {
			e, ok := it.(map[string]any)
			if !ok {
				continue
			}
			ent := map[string]any{"id": intOf(e["id"])}
			for _, k := range []string{"x", "y", "z", "yaw", "ph", "dist"} {
				if v, ok := e[k]; ok {
					ent[k] = num(floatOf(v))
				}
			}
			if v, ok := e["mv"]; ok {
				ent["mv"] = boolOf(v)
			}
			for _, k := range []string{"near", "nearest"} {
				if v, ok := e[k]; ok {
					ent[k] = intOf(v)
				}
			}
			ents = append(ents, ent)
		}
	}
	out["ent_count"] = int64(len(ents))
	if len(ents) > 0 {
		out["ents"] = ents
	}
	return out
}

// parseJSON 把一帧 JSON 解成 map[string]any，并把 json.Number 归一化为
// int64 / uint64 / float64。非 object 或解析失败时返回 ok=false。
func parseJSON(b []byte) (map[string]any, bool) {
	if len(b) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	m, ok := normalize(raw).(map[string]any)
	return m, ok
}

// msgTypeOf 取判别字段 "t"。
func msgTypeOf(o map[string]any) string {
	if s, ok := o["t"].(string); ok {
		return s
	}
	return ""
}

// ---- 标量取值：字段缺失或类型不符都返回零值，绝不 panic ----

func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
	}
	return 0
}

func floatOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case int:
		return float64(x)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
	}
	return 0
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// normalize 递归把 json.Number 转成具体数值类型，避免 msgpack 编码时出现
// stringly-typed 数字。
func normalize(v any) any {
	switch x := v.(type) {
	case json.Number:
		return jsonNumber(x)
	case []any:
		for i, e := range x {
			x[i] = normalize(e)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = normalize(e)
		}
		return x
	default:
		return v
	}
}

func jsonNumber(n json.Number) any {
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		if f, err := n.Float64(); err == nil {
			return f
		}
		return s
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		return u
	}
	return s
}

func truncate(s string) string {
	if len(s) > rawMax {
		return s[:rawMax]
	}
	return s
}
