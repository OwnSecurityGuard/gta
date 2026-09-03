package decode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/capture"
	"gta/pkg/event"
	"gta/pkg/schema"
)

// ErrDispatcherClosed 在流已关闭后提交请求时返回。
var ErrDispatcherClosed = errors.New("decode dispatcher closed")

// pendingInput 缓冲一个 input_id 的请求上下文与中间结果。
type pendingInput struct {
	req     *pb.DecodeRequest
	connID  string
	source  string
	results []*pb.DecodeResponseV2
	future  *Future
}

// Future 是一次异步解码的完成句柄，由 Dispatcher.Submit 返回。
// Wait 阻塞直到解码完成/出错/ctx 取消；结果事件保持与提交顺序无关，
// 调用方按自身需要的顺序 Wait 即可实现保序消费。
type Future struct {
	done   chan struct{}
	events []*event.Event
	err    error
}

// resolve 由 recvLoop 调用，恰好一次。
func (f *Future) resolve(events []*event.Event, err error) {
	f.events, f.err = events, err
	close(f.done)
}

// Wait 阻塞直到解码完成、出错或 ctx 取消。
func (f *Future) Wait(ctx context.Context) ([]*event.Event, error) {
	select {
	case <-f.done:
		return f.events, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
//
// 流水线模式：Submit 提交请求立即返回 Future，recvLoop goroutine 负责
// 接收响应并按 input_id 归还结果，因此支持多个在途请求（multi-in-flight），
// 单包解码 RTT 不再阻塞后续提交。gRPC stream 的 Send/Recv 分别由
// Submit 调用方（mu 保护）与 recvLoop 独占，符合 gRPC 并发约束。
type Dispatcher struct {
	client    pb.DecoderClient
	streamV2  pb.Decoder_DecodeV2Client // V2 流（MsgPack）
	sessionID string                    // 所屬 capture session ID，寫入 Event Identity
	mu        sync.Mutex                // 保护 pending map、closed 与 Send 串行化
	closed    bool
	logger    *slog.Logger // 带 session_id 等上下文的 logger

	// streamDone 在 recvLoop 退出时关闭（无论正常关闭还是流错误），
	// 用于外部检测流是否仍然健康。
	streamDone chan struct{}

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
		streamDone:   make(chan struct{}),
		pending:      make(map[string]*pendingInput),
		causationIdx: newCausationIndex(1024),
		schemaReg:    schemaReg,
	}
	for _, opt := range opts {
		opt(d)
	}
	go d.recvLoop()
	return d, nil
}

// IsHealthy 返回解码器底层流是否仍然活跃。
// 当 recvLoop 因流错误或 CloseSend 退出后返回 false，
// 此时调用方应关闭当前 Dispatcher 并重建。
func (d *Dispatcher) IsHealthy() bool {
	select {
	case <-d.streamDone:
		return false
	default:
		return true
	}
}

// DecodeV2 发送单个 packet payload 并返回 Event 列表（0..N 个事件）。
// 同步便捷封装：等价于 Submit + Future.Wait。需要流水线并发的调用方应直接用 Submit。
func (d *Dispatcher) DecodeV2(ctx context.Context, pkt event.Packet) ([]*event.Event, error) {
	f, err := d.Submit(pkt)
	if err != nil {
		return nil, err
	}
	return f.Wait(ctx)
}

// Submit 异步提交一个包进行解码，立即返回 Future。
// Send 与 pending 注册在 mu 内完成；Recv 由独立的 recvLoop goroutine 处理，
// 因此多个 Submit 可同时在途（in-flight），互不阻塞。
func (d *Dispatcher) Submit(pkt event.Packet) (*Future, error) {
	inputID := string(event.NewEventID())
	// 优先使用 Packet 自带的稳定 ID 作为 raw_packet_id；未分配时回退到 inputID。
	packetID := pkt.ID
	if packetID == "" {
		packetID = inputID
	}
	flowID := fmt.Sprintf("%d", FlowIDFromEndpoints(pkt.Src.String(), pkt.Dst.String(), pkt.Protocol))
	direction := inferDirection(pkt.Src.Port(), pkt.Dst.Port(), d.serverPort)
	// 代理抓包上下文：移动代理在 Packet.Metadata 携带 conn_id 与 source，
	// 透传到每个解码事件的 EventContext，供 Connections 页面与 Capture Context 使用。
	connID, _ := pkt.Metadata["conn_id"].(string)
	source, _ := pkt.Metadata[capture.MetaSource].(string)

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

	f := &Future{done: make(chan struct{})}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, ErrDispatcherClosed
	}
	if err := d.streamV2.Send(req); err != nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("send decode v2 request: %w", err)
	}
	d.pending[inputID] = &pendingInput{req: req, connID: connID, source: source, future: f}
	d.mu.Unlock()
	return f, nil
}

// recvLoop 独占 streamV2 的 Recv 端：接收响应、按 input_id 聚合，
// Done 时把结果事件 resolve 给对应 Future。流错误时唤醒所有等待者。
// 退出时关闭 streamDone，通知外部流已断开。
func (d *Dispatcher) recvLoop() {
	defer close(d.streamDone)
	for {
		resp, err := d.streamV2.Recv()
		if err != nil {
			d.failAllPending(fmt.Errorf("receive decode v2 response: %w", err))
			return
		}

		inputID := resp.InputId
		if inputID == "" {
			d.logger.Warn("decode response without input_id, dropped")
			continue
		}

		d.mu.Lock()
		p, ok := d.pending[inputID]
		if !ok {
			d.mu.Unlock()
			d.logger.Warn("unexpected input_id in response", "input_id", inputID)
			continue
		}
		if resp.Done {
			delete(d.pending, inputID)
		} else {
			p.results = append(p.results, resp)
		}
		d.mu.Unlock()

		if resp.Done {
			p.future.resolve(d.convertResultsToEvents(p.req, p.results, p.connID, p.source), nil)
		}
	}
}

// failAllPending 把底层流错误传播给所有在途 Future 并清空 pending。
func (d *Dispatcher) failAllPending(err error) {
	d.mu.Lock()
	pending := d.pending
	d.pending = make(map[string]*pendingInput)
	d.mu.Unlock()
	for _, p := range pending {
		p.future.resolve(nil, err)
	}
}

// convertResultsToEvents 将解码结果批量转换为 Event，处理 schema 校验和因果关系。
func (d *Dispatcher) convertResultsToEvents(req *pb.DecodeRequest, results []*pb.DecodeResponseV2, connID, source string) []*event.Event {
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
			ConnID:         connID,
			Source:         source,
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

// Close 关闭底层 gRPC 流，并把所有在途 Future 立即失败（ErrDispatcherClosed），
// 防止调用方在流销毁后无限等待。
func (d *Dispatcher) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	pending := d.pending
	d.pending = make(map[string]*pendingInput)
	err := d.streamV2.CloseSend()
	d.mu.Unlock()

	for _, p := range pending {
		p.future.resolve(nil, ErrDispatcherClosed)
	}
	return err
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
