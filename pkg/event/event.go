package event

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/gopacket/layers"
)

// LinkType 是链路层类型，使用 int 以支持标准 DLT 和自定义扩展值。
type LinkType int

// 标准 DLT 值，与 gopacket/layers.LinkType 保持一致。
const (
	LinkTypeNull      LinkType = 0
	LinkTypeEthernet  LinkType = 1
	LinkTypeLoop      LinkType = 108
	LinkTypeLinuxSLL  LinkType = 113
	LinkTypeRaw       LinkType = 101
	LinkTypeIPv4      LinkType = 228
	LinkTypeIPv6      LinkType = 229
	LinkTypeIEEE80211 LinkType = 105
)

// 自定义 LinkType，从 1000 开始分配，避免与标准 DLT 冲突。
const (
	// LinkTypeRawIP 表示没有链路层封装的原始 IP 包（如 tun/Android VPN service 输出）。
	LinkTypeRawIP LinkType = 1000 + iota

	// LinkTypeProxyPayload 表示代理层拿到的应用层 payload，没有 IP/TCP 头。
	LinkTypeProxyPayload

	// LinkTypeTLSPlaintext 表示 TLS 解密后的明文数据。
	LinkTypeTLSPlaintext
)

// LinkTypeFromLayers 将 gopacket/layers.LinkType 转换为 event.LinkType。
func LinkTypeFromLayers(l layers.LinkType) LinkType {
	return LinkType(l)
}

// TCPFlags 描述 TCP 控制位。仅 Protocol=="tcp" 时有效。
// 用于判断连接关闭方（哪一侧先发 FIN/RST）等 TCP 层事件。
type TCPFlags struct {
	FIN bool
	SYN bool
	RST bool
	ACK bool
	PSH bool
	URG bool
}

// String 返回标志位字符串表示，如 "FIN", "RST", "FIN|ACK"。
// 无标志位时返回 ""。
func (f TCPFlags) String() string {
	var parts []string
	if f.FIN {
		parts = append(parts, "FIN")
	}
	if f.SYN {
		parts = append(parts, "SYN")
	}
	if f.RST {
		parts = append(parts, "RST")
	}
	if f.ACK {
		parts = append(parts, "ACK")
	}
	if f.PSH {
		parts = append(parts, "PSH")
	}
	if f.URG {
		parts = append(parts, "URG")
	}
	return strings.Join(parts, "|")
}

// HasCloseFlags 返回是否包含连接关闭相关标志位（FIN 或 RST）。
func (f TCPFlags) HasCloseFlags() bool {
	return f.FIN || f.RST
}

// Packet 是抓取层输出，也是整个系统统一的数据载体。
type Packet struct {
	ID        string    // 原始包在 raw_packets 表中的唯一标识；为空时由存储层生成
	Timestamp time.Time
	Raw       []byte
	LinkType  LinkType
	Src       netip.AddrPort
	Dst       netip.AddrPort
	Protocol  string
	Metadata  map[string]any
	TCPFlags  TCPFlags // 仅 TCP 协议有效
}

// StateChange 描述一个通用状态变更投影。
type StateChange struct {
	SubjectType string `msgpack:"subject_type"`
	SubjectID   string `msgpack:"subject_id"`
	Op          string `msgpack:"op"`
	Path        string `msgpack:"path"`
	Before      Value  `msgpack:"before,omitempty"`
	After       Value  `msgpack:"after,omitempty"`
	Version     int64  `msgpack:"version,omitempty"`
	Metadata    Value  `msgpack:"metadata,omitempty"`
}

// Validate 校验 StateChange 的最小必填字段与 op 取值。
// 返回的 error 描述具体缺失/非法项，供调用方决定是否丢弃或记录。
func (sc StateChange) Validate() error {
	if strings.TrimSpace(sc.SubjectType) == "" {
		return fmt.Errorf("subject_type is required")
	}
	if strings.TrimSpace(sc.SubjectID) == "" {
		return fmt.Errorf("subject_id is required")
	}
	if strings.TrimSpace(sc.Op) == "" {
		return fmt.Errorf("op is required")
	}
	if strings.TrimSpace(sc.Path) == "" {
		return fmt.Errorf("path is required")
	}
	switch sc.Op {
	case "set", "delete", "merge":
		// ok
	default:
		return fmt.Errorf("op %q is not supported (allowed: set|delete|merge)", sc.Op)
	}
	return nil
}

// TimestampedEvent 是带时间戳的事件抽象，供 Aggregator 等跨版本组件使用。
type TimestampedEvent interface {
	GetTimestamp() time.Time
}

// Metric 是聚合结果。
type Metric struct {
	Name   string
	Window time.Time
	Group  map[string]string
	Value  float64
}

// Aggregator 是跨包聚合器接口。
// Add 接收 TimestampedEvent，使聚合逻辑无需在不同事件版本之间转换。
type Aggregator interface {
	Add(event TimestampedEvent, ctx map[string]any) error
	Flush() []Metric      // 输出已关闭窗口的指标
	FinalFlush() []Metric // 输出已关闭窗口 + 当前窗口，用于 shutdown
	Window() time.Duration
}

// EventContext 描述事件的网络上下文，跨协议通用。不放业务数据。
type EventContext struct {
	FlowID         string `msgpack:"flow_id,omitempty"`
	RawPacketID    string `msgpack:"raw_packet_id,omitempty"`
	MessageOrdinal int    `msgpack:"message_ordinal,omitempty"`
	Direction      string `msgpack:"direction,omitempty"`
}

// MarshalMsgpack 将 EventContext 编码为 MsgPack 字节。
func (c EventContext) MarshalMsgpack() ([]byte, error) {
	v := ValueFromAny(map[string]any{
		"flow_id":         c.FlowID,
		"raw_packet_id":   c.RawPacketID,
		"message_ordinal": c.MessageOrdinal,
		"direction":       c.Direction,
	})
	return v.MarshalMsgpack()
}

// UnmarshalContextMsgpack 从 MsgPack 字节解码 EventContext。
func UnmarshalContextMsgpack(data []byte) (EventContext, error) {
	v, err := UnmarshalValueMsgpack(data)
	if err != nil {
		return EventContext{}, err
	}
	ctx := EventContext{}
	if obj, ok := v.AsObject(); ok {
		if f, ok := obj["flow_id"]; ok {
			if s, ok := f.AsString(); ok {
				ctx.FlowID = s
			}
		}
		if f, ok := obj["raw_packet_id"]; ok {
			if s, ok := f.AsString(); ok {
				ctx.RawPacketID = s
			}
		}
		if f, ok := obj["message_ordinal"]; ok {
			if n, ok := f.AsInt(); ok {
				ctx.MessageOrdinal = int(n)
			}
		}
		if f, ok := obj["direction"]; ok {
			if s, ok := f.AsString(); ok {
				ctx.Direction = s
			}
		}
	}
	return ctx, nil
}

// Event 是事件模型，遵循 Event Sourcing 原则：
// Event = Identity + Relation + Context + Payload
//
// 核心原则：
// 1. 不可变性（Immutable）：创建后不可修改
// 2. 追加写入（Append-only）：只能新增，不能更新或删除
// 3. 事实表示：代表已发生的事件，而非命令或请求
type Event struct {
	// Identity 描述事件的身份信息
	Identity Identity

	// Relation 描述事件的因果关系
	Relation Relation

	// Context 描述事件的网络上下文
	Context EventContext

	// Payload 包含事件的实际数据
	Payload Payload
}

// NewEvent 创建新的 Event
// 自动生成 UUIDv7 作为 ID，使用当前时间作为 Timestamp
func NewEvent(sessionID string, eventType EventType, schemaID string, source SourceID, payload Value, ctx EventContext) *Event {
	return &Event{
		Identity: NewIdentity(sessionID, eventType, schemaID, source),
		Relation: Relation{},
		Context:  ctx,
		Payload: Payload{
			SchemaID: schemaID,
			Value:    payload,
		},
	}
}

// NewEventWithTime 创建新的 Event，使用指定的时间戳
func NewEventWithTime(sessionID string, eventType EventType, schemaID string, source SourceID, payload Value, ts time.Time, ctx EventContext) *Event {
	return &Event{
		Identity: NewIdentityWithTime(sessionID, eventType, schemaID, source, ts),
		Relation: Relation{},
		Context:  ctx,
		Payload: Payload{
			SchemaID: schemaID,
			Value:    payload,
		},
	}
}

// NewEventWithRelation 创建带有因果关系的 Event
func NewEventWithRelation(sessionID string, eventType EventType, schemaID string, source SourceID, payload Value, relation Relation, ctx EventContext) *Event {
	return &Event{
		Identity: NewIdentity(sessionID, eventType, schemaID, source),
		Relation: relation,
		Context:  ctx,
		Payload: Payload{
			SchemaID: schemaID,
			Value:    payload,
		},
	}
}

// Validate 验证 Event 的必填字段
func (e *Event) Validate() error {
	if err := e.Identity.Validate(); err != nil {
		return err
	}
	return nil
}

// GetID 返回事件 ID
func (e *Event) GetID() EventID {
	return e.Identity.ID
}

// GetType 返回事件类型
func (e *Event) GetType() EventType {
	return e.Identity.Type
}

// GetSessionID 返回会话 ID
func (e *Event) GetSessionID() string {
	return e.Identity.SessionID
}

// GetTimestamp 返回事件时间戳
func (e *Event) GetTimestamp() time.Time {
	return e.Identity.Timestamp
}

// Get 从 Payload 中获取指定键的值
func (e *Event) Get(key string) (Value, bool) {
	return e.Payload.Get(key)
}

// WithRelation 设置因果关系，返回新的 Event（不可变性）
func (e *Event) WithRelation(relation Relation) *Event {
	return &Event{
		Identity: e.Identity,
		Relation: relation,
		Context:  e.Context,
		Payload:  e.Payload,
	}
}

// WithCausation 设置直接原因事件 ID，返回新的 Event
func (e *Event) WithCausation(causationID EventID) *Event {
	return e.WithRelation(e.Relation.WithCausation(causationID))
}

// WithCorrelation 设置关联的业务流程 ID，返回新的 Event
func (e *Event) WithCorrelation(correlationID string) *Event {
	return e.WithRelation(e.Relation.WithCorrelation(correlationID))
}

// WithOrigin 设置原始来源事件 ID，返回新的 Event
func (e *Event) WithOrigin(originID EventID) *Event {
	return e.WithRelation(e.Relation.WithOrigin(originID))
}

// ExtractStateChanges 从 Payload 的 _state_changes 字段提取状态变更。
func (e *Event) ExtractStateChanges() []StateChange {
	if e == nil {
		return nil
	}
	return ExtractStateChanges(e.Payload.Value)
}

// MetaValue 返回 _meta 中指定键的值，不存在时返回 false。
// _meta 用于存放 direction、flow_id、msg_name 等由系统附加的结构化字段。
func (e *Event) MetaValue(key string) (Value, bool) {
	if e == nil {
		return Value{}, false
	}
	obj, ok := e.Payload.Value.AsObject()
	if !ok {
		return Value{}, false
	}
	meta, ok := obj["_meta"]
	if !ok {
		return Value{}, false
	}
	metaObj, ok := meta.AsObject()
	if !ok {
		return Value{}, false
	}
	v, ok := metaObj[key]
	return v, ok
}

