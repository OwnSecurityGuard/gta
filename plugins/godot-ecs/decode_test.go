package main

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"google.golang.org/grpc/metadata"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// ---------------------------------------------------------------------------
// fake stream：grpc 1.71 的 BidiStreamingServer 是泛型接口，需实现全部方法。
// ---------------------------------------------------------------------------

type fakeStream struct {
	in   *pb.DecodeRequest
	sent []*pb.DecodeResponseV2
}

func (f *fakeStream) Send(r *pb.DecodeResponseV2) error { f.sent = append(f.sent, r); return nil }
func (f *fakeStream) Recv() (*pb.DecodeRequest, error)  { return f.in, nil }
func (f *fakeStream) SetHeader(metadata.MD) error       { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error      { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)            {}
func (f *fakeStream) Context() context.Context          { return context.Background() }
func (f *fakeStream) SendMsg(any) error                 { return nil }
func (f *fakeStream) RecvMsg(any) error                 { return nil }

// ---------------------------------------------------------------------------
// 构造输入
// ---------------------------------------------------------------------------

// frame 把一段 JSON 封装成线上帧：[4B 大端长度 N][N 字节 JSON]。
func frame(t *testing.T, s string) []byte {
	t.Helper()
	b := []byte(s)
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// tcpFrame 构造一个以太网 + IPv4 + TCP 帧，使 framing.ExtractL7 能读出端口，
// 从而验证方向判定。link_type 用 1（Ethernet）。
func tcpFrame(t *testing.T, srcPort, dstPort uint16, seq uint32, payload []byte) []byte {
	t.Helper()
	eth := layers.Ethernet{
		SrcMAC:       net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01},
		DstMAC:       net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x02},
		EthernetType: layers.EthernetTypeIPv4,
	}
	ip := layers.IPv4{
		Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.IP{127, 0, 0, 1}, DstIP: net.IP{127, 0, 0, 1},
	}
	tcp := layers.TCP{
		SrcPort: layers.TCPPort(srcPort), DstPort: layers.TCPPort(dstPort),
		Seq: seq, PSH: true, ACK: true,
	}
	_ = tcp.SetNetworkLayerForChecksum(&ip)

	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		&eth, &ip, &tcp, gopacket.Payload(payload)); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return buf.Bytes()
}

// run 跑一次 decodePacket，返回事件响应（不含 done 终止帧）。
func run(t *testing.T, req *pb.DecodeRequest) []*pb.DecodeResponseV2 {
	t.Helper()
	ra.Reset() // 每个用例独立，避免流状态串味
	st := &fakeStream{in: req}
	if err := decodePacket(req, st); err != nil {
		t.Fatalf("decodePacket: %v", err)
	}
	var out []*pb.DecodeResponseV2
	var dones int
	for _, r := range st.sent {
		if r.GetDone() {
			dones++
			continue
		}
		out = append(out, r)
	}
	if dones != 1 {
		t.Fatalf("expected exactly one done=true terminator, got %d", dones)
	}
	return out
}

// payloadOf 把 msgpack 载荷解回 map。
func payloadOf(t *testing.T, r *pb.DecodeResponseV2) map[string]event.Value {
	t.Helper()
	v, err := event.UnmarshalValueMsgpack(r.GetPayloadMsgpack())
	if err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	m, ok := v.AsObject()
	if !ok {
		t.Fatalf("payload root is %s, want object", v.Kind)
	}
	return m
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestDecodeUplink 验证上行 pos / ping 的解码、方向与标量类型。
func TestDecodeUplink(t *testing.T) {
	body := frame(t, `{"t":"pos","x":1.5,"y":0,"z":-2,"yaw":0.75,"mv":true}`) // y 是整值浮点
	got := run(t, &pb.DecodeRequest{
		InputId: "in-1", LinkType: int32(event.LinkTypeEthernet),
		Payload: tcpFrame(t, 54321, serverPort, 1, body),
	})

	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].GetEventType() != "godot_ecs.pos" || got[0].GetSchemaId() != "godot_ecs.pos.v1" {
		t.Fatalf("unexpected type/schema: %s / %s", got[0].GetEventType(), got[0].GetSchemaId())
	}
	if got[0].GetInputId() != "in-1" {
		t.Fatalf("input_id not echoed: %q", got[0].GetInputId())
	}

	p := payloadOf(t, got[0])
	if d, _ := metaStr(p, "direction"); d != "client_to_server" {
		t.Errorf("direction = %q, want client_to_server", d)
	}
	if x, ok := p["x"].AsFloat(); !ok || x != 1.5 {
		t.Errorf("x = %v (kind %s), want float 1.5", p["x"], p["x"].Kind)
	}
	// 整值浮点必须保持 Float，否则宿主 schema 校验会判 kind 不匹配。
	if p["y"].Kind != event.Float {
		t.Errorf("y kind = %s, want float (integral float64 must not collapse to int)", p["y"].Kind)
	}
	if z, ok := p["z"].AsFloat(); !ok || z != -2 {
		t.Errorf("z = %v, want float -2", p["z"])
	}
	if mv, ok := p["mv"].AsBool(); !ok || !mv {
		t.Errorf("mv = %v, want bool true", p["mv"])
	}
}

// TestDecodeDownlink 验证下行 welcome / state / pong 的解码与方向。
func TestDecodeDownlink(t *testing.T) {
	body := frame(t, `{"t":"welcome","id":3,"hz":30,"npcs":4,"min_x":-28,"max_x":28,"min_z":-28,"max_z":28}`)
	got := run(t, &pb.DecodeRequest{
		InputId: "in-2", LinkType: int32(event.LinkTypeEthernet),
		Payload: tcpFrame(t, serverPort, 54321, 1, body),
	})
	if len(got) != 1 || got[0].GetSchemaId() != "godot_ecs.welcome.v1" {
		t.Fatalf("unexpected events: %+v", got)
	}
	p := payloadOf(t, got[0])
	if d, _ := metaStr(p, "direction"); d != "server_to_client" {
		t.Errorf("direction = %q, want server_to_client", d)
	}
	if id, ok := p["id"].AsInt(); !ok || id != 3 {
		t.Errorf("id = %v, want 3", p["id"])
	}
	if hz, ok := p["hz"].AsInt(); !ok || hz != 30 {
		t.Errorf("hz = %v, want 30", p["hz"])
	}
}

// TestDecodeState 验证世界快照：ents 数组、派生 ent_count、嵌套浮点保型。
func TestDecodeState(t *testing.T) {
	raw := `{"t":"state","tick":900,"ents":[` +
		`{"id":3,"x":0,"y":0,"z":1.25,"yaw":0,"mv":false,"ph":0.5,"near":1,"nearest":10000,"dist":3.75},` +
		`{"id":10000,"x":-5.5,"y":0,"z":7,"yaw":1.5707963,"mv":true,"ph":0.125,"near":1,"nearest":3,"dist":3.75}]}`
	got := run(t, &pb.DecodeRequest{
		InputId: "in-3", LinkType: int32(event.LinkTypeEthernet),
		Payload: tcpFrame(t, serverPort, 54321, 1, frame(t, raw)),
	})
	if len(got) != 1 || got[0].GetSchemaId() != "godot_ecs.state.v1" {
		t.Fatalf("unexpected events: %+v", got)
	}
	p := payloadOf(t, got[0])
	if tick, ok := p["tick"].AsInt(); !ok || tick != 900 {
		t.Errorf("tick = %v, want 900", p["tick"])
	}
	if n, ok := p["ent_count"].AsInt(); !ok || n != 2 {
		t.Errorf("ent_count = %v, want 2", p["ent_count"])
	}
	arr, ok := p["ents"].AsArray()
	if !ok || len(arr) != 2 {
		t.Fatalf("ents = %v, want 2 elements", p["ents"])
	}
	first, _ := arr[0].AsObject()
	if id, ok := first["id"].AsInt(); !ok || id != 3 {
		t.Errorf("ents[0].id = %v, want 3", first["id"])
	}
	// 整值浮点（x=0/yaw=0）必须保持 Float
	if first["x"].Kind != event.Float || first["yaw"].Kind != event.Float {
		t.Errorf("ents[0] x/yaw kind = %s/%s, want float/float", first["x"].Kind, first["yaw"].Kind)
	}
	second, _ := arr[1].AsObject()
	if z, ok := second["z"].AsFloat(); !ok || z != 7 {
		t.Errorf("ents[1].z = %v, want float 7", second["z"])
	}
	if nearest, ok := second["nearest"].AsInt(); !ok || nearest != 3 {
		t.Errorf("ents[1].nearest = %v, want 3", second["nearest"])
	}
}

// TestDecodeSplitAcrossSegments 验证跨 TCP 段的一帧能被重组后解出（TCP 重组必需）。
func TestDecodeSplitAcrossSegments(t *testing.T) {
	full := frame(t, `{"t":"pong","ts":1700000000123,"tick":42}`)

	st := &fakeStream{}
	ra.Reset()
	half := len(full) / 2

	// 第一段：只到帧的中间（seq=1）
	if err := decodePacket(&pb.DecodeRequest{
		InputId: "a", LinkType: int32(event.LinkTypeEthernet),
		Payload: tcpFrame(t, serverPort, 54321, 1, full[:half]),
	}, st); err != nil {
		t.Fatalf("seg1: %v", err)
	}
	if events(st.sent) != 0 {
		t.Fatalf("segment 1 must not yield an event (frame incomplete), got %d", events(st.sent))
	}

	// 第二段：剩余字节，seq 紧接第一段
	if err := decodePacket(&pb.DecodeRequest{
		InputId: "b", LinkType: int32(event.LinkTypeEthernet),
		Payload: tcpFrame(t, serverPort, 54321, 1+uint32(half), full[half:]),
	}, st); err != nil {
		t.Fatalf("seg2: %v", err)
	}
	if events(st.sent) != 1 {
		t.Fatalf("segment 2 must complete the frame and yield 1 event, got %d", events(st.sent))
	}
	for _, r := range st.sent {
		if r.GetDone() || r.GetSchemaId() != "godot_ecs.pong.v1" {
			continue
		}
		p := payloadOf(t, r)
		if ts, ok := p["ts"].AsInt(); !ok || ts != 1700000000123 {
			t.Errorf("ts = %v, want 1700000000123", p["ts"])
		}
		if tick, ok := p["tick"].AsInt(); !ok || tick != 42 {
			t.Errorf("tick = %v, want 42", p["tick"])
		}
	}
}

// TestDecodeMultipleFramesPerPacket 验证一个 TCP 段里携带多帧（one-input-many-messages）。
func TestDecodeMultipleFramesPerPacket(t *testing.T) {
	var payload []byte
	payload = append(payload, frame(t, `{"t":"ping","ts":7}`)...)
	payload = append(payload, frame(t, `{"t":"pos","x":0,"y":0,"z":0,"yaw":0,"mv":false}`)...)

	got := run(t, &pb.DecodeRequest{
		InputId: "in-4", LinkType: int32(event.LinkTypeEthernet),
		Payload: tcpFrame(t, 54321, serverPort, 1, payload),
	})
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	if got[0].GetSchemaId() != "godot_ecs.ping.v1" || got[1].GetSchemaId() != "godot_ecs.pos.v1" {
		t.Errorf("unexpected order/types: %s, %s", got[0].GetSchemaId(), got[1].GetSchemaId())
	}
}

// TestDecodeProxyPayloadFallsBackToTypeInference 验证代理类输入（已是纯 L7、无端口）
// 仍能按消息类型推断方向。
func TestDecodeProxyPayloadFallsBackToTypeInference(t *testing.T) {
	got := run(t, &pb.DecodeRequest{
		InputId: "in-5", LinkType: int32(event.LinkTypeProxyPayload),
		Payload: frame(t, `{"t":"state","tick":1,"ents":[]}`),
	})
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	p := payloadOf(t, got[0])
	if d, _ := metaStr(p, "direction"); d != "server_to_client" {
		t.Errorf("direction = %q, want server_to_client (inferred from t=state)", d)
	}
}

// TestDecodeMalformedIsSafe 验证畸形输入不 panic、不返回 error，只产出兜底事件。
func TestDecodeMalformedIsSafe(t *testing.T) {
	req := &pb.DecodeRequest{
		InputId: "in-6", LinkType: int32(event.LinkTypeProxyPayload),
		Payload: frame(t, `{not json`),
	}
	got := run(t, req)
	if len(got) != 1 || got[0].GetSchemaId() != "godot_ecs.unknown.v1" {
		t.Fatalf("want 1 unknown event, got %+v", got)
	}
	if err := got[0].GetError(); err != "" {
		t.Errorf("unexpected error field: %s", err)
	}

	// 空载荷 / 纯 ACK 不产出事件，但必须回 done
	ra.Reset()
	st := &fakeStream{in: &pb.DecodeRequest{InputId: "in-7", LinkType: int32(event.LinkTypeProxyPayload)}}
	if err := decodePacket(st.in, st); err != nil {
		t.Fatalf("empty payload: %v", err)
	}
	if len(st.sent) != 1 || !st.sent[0].GetDone() {
		t.Fatalf("empty payload must yield exactly one done frame, got %+v", st.sent)
	}
}

// TestDecodedEventsConformToManifest 把上面各条真实报文解出的事件拿到契约校验器上跑一遍：
// schema 必须在 manifest 中声明，且 payload 必须符合声明（类型/必填/未知字段）。
func TestDecodedEventsConformToManifest(t *testing.T) {
	data, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatalf("read plugin.yaml: %v", err)
	}
	m, err := sdk.ParseManifest(data)
	if err != nil {
		t.Fatalf("parse plugin.yaml: %v", err)
	}
	if err := contract.CheckManifest(data); err != nil {
		t.Fatalf("manifest violates contract: %v", err)
	}

	checker := contract.NewPluginChecker()
	if r := checker.Check(m); r.HasErrors() {
		t.Fatalf("manifest schema layer has errors: %v", r)
	}

	cases := []struct {
		name string
		json string
	}{
		{"welcome", `{"t":"welcome","id":1,"hz":30,"npcs":4,"min_x":-28,"max_x":28,"min_z":-28,"max_z":28}`},
		{"pos", `{"t":"pos","x":1.5,"y":0,"z":-2,"yaw":0.75,"mv":true}`},
		{"ping", `{"t":"ping","ts":1700000000123}`},
		{"pong", `{"t":"pong","ts":1700000000123,"tick":42}`},
		{"state", `{"t":"state","tick":900,"ents":[{"id":3,"x":0,"y":0,"z":1.25,"yaw":0,"mv":false,"ph":0.5,"near":1,"nearest":10000,"dist":3.75},{"id":10000,"x":-5.5,"y":0,"z":7,"yaw":1.5707963,"mv":true,"ph":0.125,"near":1,"nearest":3,"dist":3.75}]}`},
		{"unknown", `{"t":"future_msg","foo":1}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &pb.DecodeRequest{
				InputId: "c-" + tc.name, LinkType: int32(event.LinkTypeProxyPayload),
				Payload: frame(t, tc.json),
			}
			got := run(t, req)
			if len(got) != 1 {
				t.Fatalf("want 1 event, got %d", len(got))
			}
			v, err := event.UnmarshalValueMsgpack(got[0].GetPayloadMsgpack())
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			d := &event.Draft{
				Type:      event.EventType(got[0].GetEventType()),
				SchemaRef: got[0].GetSchemaId(),
				Value:     v,
			}
			if r := checker.CheckEvent(m, d); r.HasErrors() {
				t.Fatalf("event does not conform to declared schema: %v", r)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func metaStr(p map[string]event.Value, key string) (string, bool) {
	meta, ok := p["_meta"].AsObject()
	if !ok {
		return "", false
	}
	v, ok := meta[key]
	if !ok {
		return "", false
	}
	return v.AsString()
}

func events(rs []*pb.DecodeResponseV2) int {
	n := 0
	for _, r := range rs {
		if !r.GetDone() {
			n++
		}
	}
	return n
}
