package decode

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gta/pkg/event"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/schema"
)

// pendingInput 缓冲一个 input_id 的中间结果。
type pendingInput struct {
	results []*pb.DecodeResponseV2
}

// causationIndex 维护 input_id → EventID 的最近 N 项映射。
type causationIndex struct {
	mu      sync.Mutex
	entries map[string]event.EventID
	order   []string
	limit   int
}

// newCausationIndex 创建指定大小的因果索引。
func newCausationIndex(limit int) *causationIndex {
	return &causationIndex{
		entries: make(map[string]event.EventID),
		limit:   limit,
	}
}

// Put 记录 input_id 对应的主事件 ID（已存在则不覆盖）。
func (c *causationIndex) Put(inputID string, id event.EventID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[inputID]; ok {
		return
	}
	c.entries[inputID] = id
	c.order = append(c.order, inputID)
	for len(c.order) > c.limit {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, old)
	}
}

// Get 查询 input_id 对应的主事件 ID。
func (c *causationIndex) Get(inputID string) (event.EventID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.entries[inputID]
	return id, ok
}

// Dispatcher 把 Packet 发给插件并产出 Event。
// 内部维护一条持久的 gRPC DecodeV2 双向流。
type Dispatcher struct {
	client    pb.DecoderClient
	streamV2  pb.Decoder_DecodeV2Client // V2 流（MsgPack）
	sessionID string                    // 所屬 capture session ID，寫入 Event Identity
	mu        sync.Mutex
	logger    *slog.Logger // 带 session_id 等上下文的 logger

	pending      map[string]*pendingInput
	causationIdx *causationIndex
	schemaReg    *schema.Registry

	serverPort int // 服务端端口提示，用于方向推断（如游戏服 8989）
}

// DispatcherOption 配置 Dispatcher 的可选参数。
type DispatcherOption func(*Dispatcher)

// WithServerPort 设置服务端端口提示，使方向推断能把该端口识别为服务端。
func WithServerPort(port int) DispatcherOption {
	return func(d *Dispatcher) {
		d.serverPort = port
	}
}

// NewDispatcher 创建解码分发器。logger 用于记录 _fields 校验警告等业务降级信息。
// sessionID 會被寫入產出事件的 Identity.SessionID，應使用真實的 capture session ID。
// 错误通过 return 传递，由调用方记录日志（避免重复记录）。
func NewDispatcher(client pb.DecoderClient, sessionID string, logger *slog.Logger, schemaReg *schema.Registry, opts ...DispatcherOption) (*Dispatcher, error) {
	if schemaReg == nil {
		schemaReg = schema.NewRegistry()
	}
	if logger == nil {
		logger = slog.Default()
	}

	streamV2, err := client.DecodeV2(context.Background())
	if err != nil {
		return nil, fmt.Errorf("create decode v2 stream: %w", err)
	}

	d := &Dispatcher{
		client:       client,
		streamV2:     streamV2,
		sessionID:    sessionID,
		logger:       logger,
		pending:      make(map[string]*pendingInput),
		causationIdx: newCausationIndex(1024),
		schemaReg:    schemaReg,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// DecodeV2 发送单个 packet payload 并返回 Event 列表（0..N 个事件）。
func (d *Dispatcher) DecodeV2(ctx context.Context, pkt event.Packet) ([]*event.Event, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.decodeV2Locked(ctx, pkt)
}

// decodeV2Locked 使用 V2 协议（MsgPack）解码，支持 0..N 结果，调用方需持有锁。
func (d *Dispatcher) decodeV2Locked(ctx context.Context, pkt event.Packet) ([]*event.Event, error) {
	inputID := string(event.NewEventID())
	// 优先使用 Packet 自带的稳定 ID 作为 raw_packet_id；未分配时回退到 inputID。
	packetID := pkt.ID
	if packetID == "" {
		packetID = inputID
	}
	flowID := fmt.Sprintf("%d", FlowIDFromEndpoints(pkt.Src.String(), pkt.Dst.String(), pkt.Protocol))
	direction := inferDirection(pkt.Src.Port(), pkt.Dst.Port(), d.serverPort)

	req := &pb.DecodeRequest{
		SessionId:    d.sessionID,
		ProtocolHint: pkt.Protocol,
		Payload:      pkt.Raw,
		LinkType:     int32(pkt.LinkType),
		InputId:      inputID,
		PacketId:     packetID,
		FlowId:       flowID,
		Src:          pkt.Src.String(),
		Dst:          pkt.Dst.String(),
		Direction:    direction,
		TimestampNs:  pkt.Timestamp.UnixNano(),
	}

	if err := d.streamV2.Send(req); err != nil {
		return nil, fmt.Errorf("send decode v2 request: %w", err)
	}

	d.pending[inputID] = &pendingInput{}

	for {
		resp, err := d.streamV2.Recv()
		if err != nil {
			delete(d.pending, inputID)
			return nil, fmt.Errorf("receive decode v2 response: %w", err)
		}

		if resp.InputId != "" && resp.InputId != inputID {
			d.logger.Warn("unexpected input_id in response", "expected", inputID, "got", resp.InputId)
			continue
		}

		if resp.Done {
			p := d.pending[inputID]
			delete(d.pending, inputID)
			return d.convertResultsToEvents(req, p.results), nil
		}

		d.pending[inputID].results = append(d.pending[inputID].results, resp)
	}
}

// convertResultsToEvents 将解码结果批量转换为 Event，处理 schema 校验和因果关系。
func (d *Dispatcher) convertResultsToEvents(req *pb.DecodeRequest, results []*pb.DecodeResponseV2) []*event.Event {
	var events []*event.Event
	for i, r := range results {
		if r.Error != "" {
			d.logger.Warn("decode v2 result error", "input_id", req.InputId, "error", r.Error)
			continue
		}

		payloadValue, err := event.UnmarshalValueMsgpack(r.PayloadMsgpack)
		if err != nil {
			d.logger.Warn("unmarshal msgpack payload", "error", err)
			continue
		}

		schemaID := r.SchemaId
		if schemaID != "" {
			if _, ok := d.schemaReg.Lookup(schemaID); !ok {
				d.logger.Warn("schema not registered, falling back to unknown.v1", "schema_id", schemaID)
				schemaID = "unknown.v1"
			}
		}

		ctx := event.EventContext{
			FlowID:         req.FlowId,
			RawPacketID:    req.PacketId,
			MessageOrdinal: i,
			Direction:      req.Direction,
		}
		if dirOverride, ok := extractDirectionOverride(payloadValue); ok {
			ctx.Direction = dirOverride
		}

		ev := event.NewEventWithTime(
			d.sessionID,
			event.EventType(r.EventType),
			schemaID,
			event.SourceID(req.ProtocolHint),
			payloadValue,
			time.Unix(0, req.TimestampNs),
			ctx,
		)

		if r.CorrelationKey != "" {
			ev = ev.WithCorrelation(r.CorrelationKey)
		}
		if r.CausationInputId != "" {
			if causeID, ok := d.causationIdx.Get(r.CausationInputId); ok {
				ev = ev.WithCausation(causeID)
			}
		}

		// 注册第一条非 error 事件作为该 input_id 的主事件
		if i == 0 {
			d.causationIdx.Put(req.InputId, ev.GetID())
		}
		events = append(events, ev)
	}
	return events
}

// Close 关闭底层 gRPC 流。
func (d *Dispatcher) Close() error {
	if d.streamV2 != nil {
		return d.streamV2.CloseSend()
	}
	return nil
}

// inferDirection 根据 src/dst 端口推断通信方向。
// serverPort 为已知服务端端口提示（如游戏服 8989），命中时优先据此判断方向。
func inferDirection(srcPort, dstPort uint16, serverPort int) string {
	if serverPort > 0 {
		sp := uint16(serverPort)
		if dstPort == sp && srcPort != sp {
			return "client_to_server"
		}
		if srcPort == sp && dstPort != sp {
			return "server_to_client"
		}
	}
	if dstPort < 1024 && srcPort >= 1024 {
		return "client_to_server"
	}
	if srcPort < 1024 && dstPort >= 1024 {
		return "server_to_client"
	}
	return "unknown"
}

// extractDirectionOverride 从 payload _meta.direction 中提取方向覆盖值。
func extractDirectionOverride(v event.Value) (string, bool) {
	obj, ok := v.AsObject()
	if !ok {
		return "", false
	}
	meta, ok := obj["_meta"]
	if !ok {
		return "", false
	}
	metaObj, ok := meta.AsObject()
	if !ok {
		return "", false
	}
	if d, ok := metaObj["direction"]; ok {
		if s, ok := d.AsString(); ok {
			return s, true
		}
	}
	return "", false
}
