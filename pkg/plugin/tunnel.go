package plugin

// 隧道服务端：把一条插件拨出的 Connect 双向流（TunnelFrame 帧）
// 适配成 pb.DecoderClient，供 pkg/decode.Dispatcher 透明使用。
//
// 帧协议与 SDK（gta-plugin-sdk/tunnel.go 插件侧）逐字节对齐：
//
//	服务端侧                                     插件侧
//	Dispatcher ──DecodeV2()──▶ tunnelClient      tunnelServer ──▶ decodeFuncV2
//	                     │                            ▲
//	                     └── stream_id 复用 ── TunnelFrame ──┘
//
// 流的开启是隐式的：某 stream_id 的第一个 request 帧即开流；
// half_close = 请求侧 EOF；插件回 end（error 空 = 正常结束）或 reset 取消。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// 隧道默认参数（背压：有界队列 + 超时）。
const (
	defaultTunnelRespQueueSize = 16
	defaultTunnelEnqueueWait   = 5 * time.Second
)

// TunnelHub 管理所有插件拨出的 Connect 隧道流。
// 每条 Connect 流对应一个 tunnelSession，session 暴露的 tunnelClient
// 实现 pb.DecoderClient，可直接交给 NewDispatcher。
type TunnelHub struct {
	// onDisconnect 在任一隧道断开时被调用（owner 为建流时解析出的属主标识，
	// 通常是 instance_id；T4 用它触发 PluginEventOffline）。不可为空时才回调。
	onDisconnect func(owner string)
	// onConnect 在 Connect 隧道建立时被调用（owner 为属主标识，client 为
	// 该隧道的 DecoderClient；T4 用它把隧道插件挂到注册表）。
	onConnect func(owner string, client pb.DecoderClient)
	// ownerResolver 在 Connect 建流时从流上下文解析属主（T4/T5 接入认证后注入）。
	ownerResolver func(ctx context.Context) string
	respQueueSize int
	enqueueWait   time.Duration

	mu       sync.Mutex
	sessions map[uint64]*tunnelSession
	nextID   atomic.Uint64
}

// TunnelHubOption 配置 TunnelHub。
type TunnelHubOption func(*TunnelHub)

// WithTunnelDisconnectHook 设置隧道断开回调（owner 为建流时解析的属主标识）。
// 回调在隧道会话的收尾路径上被调用，必须非阻塞。
func WithTunnelDisconnectHook(fn func(owner string)) TunnelHubOption {
	return func(h *TunnelHub) {
		if fn != nil {
			h.onDisconnect = fn
		}
	}
}

// WithTunnelConnectHook 设置隧道建立回调：建流成功即通知 owner 与可用的
// DecoderClient（回调应尽快返回，重活交给调用方）。先于任何解码调用发生。
func WithTunnelConnectHook(fn func(owner string, client pb.DecoderClient)) TunnelHubOption {
	return func(h *TunnelHub) {
		if fn != nil {
			h.onConnect = fn
		}
	}
}

// WithTunnelOwnerResolver 设置建流时的属主解析函数（从流的 auth 上下文取 owner）。
func WithTunnelOwnerResolver(fn func(ctx context.Context) string) TunnelHubOption {
	return func(h *TunnelHub) {
		if fn != nil {
			h.ownerResolver = fn
		}
	}
}

// WithTunnelQueueParams 调整每逻辑流响应队列的容量与入队等待超时（背压控制）。
func WithTunnelQueueParams(queueSize int, enqueueWait time.Duration) TunnelHubOption {
	return func(h *TunnelHub) {
		if queueSize > 0 {
			h.respQueueSize = queueSize
		}
		if enqueueWait > 0 {
			h.enqueueWait = enqueueWait
		}
	}
}

// NewTunnelHub 创建隧道管理器。
func NewTunnelHub(opts ...TunnelHubOption) *TunnelHub {
	h := &TunnelHub{
		respQueueSize: defaultTunnelRespQueueSize,
		enqueueWait:   defaultTunnelEnqueueWait,
		sessions:      map[uint64]*tunnelSession{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Connect 实现 PluginRegistry 的 Connect 双向流（服务端）。
// 收包循环把插件的 response/end 帧路由到对应逻辑流；流断开时
// 关闭所有未决逻辑流并触发断开回调。
// 该方法由 T4 注册到宿主的 PluginRegistry gRPC 服务上。
func (h *TunnelHub) Connect(stream pb.PluginRegistry_ConnectServer) error {
	owner := ""
	if h.ownerResolver != nil {
		owner = h.ownerResolver(stream.Context())
	}
	sess := &tunnelSession{
		hub:           h,
		stream:        stream,
		owner:         owner,
		streams:       map[uint32]*pendingStream{},
		closed:        make(chan struct{}),
		respQueueSize: h.respQueueSize,
	}
	if h.onConnect != nil {
		h.onConnect(owner, sess.client())
	}
	id := h.nextID.Add(1)
	h.mu.Lock()
	h.sessions[id] = sess
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.sessions, id)
		h.mu.Unlock()
		sess.close(errors.New("tunnel: connect stream closed"))
		if h.onDisconnect != nil {
			h.onDisconnect(sess.owner)
		}
	}()

	for {
		frame, err := stream.Recv()
		if err != nil {
			// 正常 EOF（插件侧 CloseSend/流结束）与错误都走统一收尾
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := sess.dispatch(frame); err != nil {
			slog.Warn("tunnel: dispatch frame failed", "owner", sess.owner, "stream_id", frame.GetStreamId(), "error", err)
		}
	}
}

// dispatch 把一帧路由到对应逻辑流。
func (s *tunnelSession) dispatch(frame *pb.TunnelFrame) error {
	switch p := frame.Payload.(type) {
	case *pb.TunnelFrame_Response:
		return s.deliverResponse(frame.GetStreamId(), p.Response)
	case *pb.TunnelFrame_End:
		return s.finishStream(frame.GetStreamId(), p.End)
	default:
		// request/half_close/reset 是服务端→插件方向的帧，插件不应发回。
		slog.Warn("tunnel: ignoring unexpected frame from plugin", "owner", s.owner, "stream_id", frame.GetStreamId())
		return nil
	}
}

// deliverResponse 把响应字节解包并投递到逻辑流的有界队列。
// 队列满时等待 enqueueWait，仍投不进去则放弃该逻辑流（reset 插件侧）。
func (s *tunnelSession) deliverResponse(streamID uint32, data []byte) error {
	resp := &pb.DecodeResponseV2{}
	if err := proto.Unmarshal(data, resp); err != nil {
		return s.abortStream(streamID, fmt.Errorf("tunnel: bad response frame: %w", err))
	}
	s.mu.Lock()
	ps, ok := s.streams[streamID]
	s.mu.Unlock()
	if !ok {
		slog.Warn("tunnel: response for unknown stream", "owner", s.owner, "stream_id", streamID)
		return nil
	}
	timer := time.NewTimer(s.hub.enqueueWait)
	defer timer.Stop()
	select {
	case ps.respCh <- resp:
		return nil
	case <-ps.done:
		return nil // 逻辑流已被取消/结束，丢弃
	case <-s.closed:
		return errors.New("tunnel: session closed")
	case <-timer.C:
		return s.abortStream(streamID, errors.New("tunnel: response queue overflow"))
	}
}

// finishStream 处理 end 帧：error 空 = 正常 EOF，非空 = 流以错误结束。
func (s *tunnelSession) finishStream(streamID uint32, end *pb.StreamEnd) error {
	var endErr error
	if end != nil && end.Error != "" {
		endErr = fmt.Errorf("tunnel: plugin stream error: %s", end.Error)
	}
	s.mu.Lock()
	ps, ok := s.streams[streamID]
	if ok {
		delete(s.streams, streamID)
	}
	s.mu.Unlock()
	if ok {
		ps.finish(endErr)
	}
	return nil
}

// abortStream 因协议错误/队列溢出放弃逻辑流：本地报错并通知插件侧 reset。
func (s *tunnelSession) abortStream(streamID uint32, cause error) error {
	s.mu.Lock()
	ps, ok := s.streams[streamID]
	if ok {
		delete(s.streams, streamID)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	ps.finish(cause)
	return s.sendFrame(&pb.TunnelFrame{
		StreamId: streamID,
		Payload:  &pb.TunnelFrame_Reset_{Reset_: &pb.StreamReset{Reason: cause.Error()}},
	})
}

// close 关闭会话：所有未决逻辑流以 err 结束。
func (s *tunnelSession) close(err error) {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.mu.Lock()
		pending := make([]*pendingStream, 0, len(s.streams))
		for id, ps := range s.streams {
			pending = append(pending, ps)
			delete(s.streams, id)
		}
		s.mu.Unlock()
		for _, ps := range pending {
			ps.finish(err)
		}
	})
}

// tunnelSession 一条 Connect 流的多路分解状态。
type tunnelSession struct {
	hub       *TunnelHub
	stream    pb.PluginRegistry_ConnectServer
	owner     string
	sendMu    sync.Mutex // 保护 stream.Send 的并发访问
	mu        sync.Mutex
	streams   map[uint32]*pendingStream
	closed    chan struct{}
	closeOnce sync.Once

	respQueueSize int
	nextStreamID  atomic.Uint32
}

// sendFrame 并发安全地向插件方向发送一帧。
func (s *tunnelSession) sendFrame(frame *pb.TunnelFrame) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.stream.Context().Err() != nil {
		return s.stream.Context().Err()
	}
	return s.stream.Send(frame)
}

// client 返回实现 pb.DecoderClient 的对象，供 Dispatcher 使用。
func (s *tunnelSession) client() pb.DecoderClient { return &tunnelClient{sess: s} }

// pendingStream 一个逻辑 DecodeV2 流在服务端侧的未决状态。
type pendingStream struct {
	respCh    chan *pb.DecodeResponseV2
	done      chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	endErr error
	ctx    context.Context    // DecodeV2 调用方的 ctx
	cancel context.CancelFunc // reset 时取消调用方 ctx
}

// finish 结束逻辑流（err 为 nil 表示正常 EOF）。
func (ps *pendingStream) finish(err error) {
	ps.closeOnce.Do(func() {
		ps.mu.Lock()
		if err != nil {
			ps.endErr = err
		}
		ps.mu.Unlock()
		close(ps.done)
		ps.cancel()
	})
}

func (ps *pendingStream) endError() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.endErr
}

// tunnelClient 实现 pb.DecoderClient：每次 DecodeV2 调用
// 在隧道上分配一个新 stream_id，返回一个逻辑流的客户端视图。
type tunnelClient struct {
	sess *tunnelSession
}

var _ pb.DecoderClient = (*tunnelClient)(nil)

// DecodeV2 分配 stream_id 并返回逻辑流客户端（grpc.BidiStreamingClient）。
func (c *tunnelClient) DecodeV2(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[pb.DecodeRequest, pb.DecodeResponseV2], error) {
	sess := c.sess
	if sess.stream.Context().Err() != nil {
		return nil, fmt.Errorf("tunnel: connect stream closed: %w", sess.stream.Context().Err())
	}
	select {
	case <-sess.closed:
		return nil, errors.New("tunnel: session closed")
	default:
	}

	streamCtx, cancel := context.WithCancel(ctx)
	ps := &pendingStream{
		respCh: make(chan *pb.DecodeResponseV2, sess.respQueueSize),
		done:   make(chan struct{}),
		ctx:    streamCtx,
		cancel: cancel,
	}
	streamID := sess.nextStreamID.Add(1)

	sess.mu.Lock()
	sess.streams[streamID] = ps
	sess.mu.Unlock()

	// 调用方取消（超时/请求放弃）→ 向插件发 reset 并结束本地逻辑流。
	go func() {
		select {
		case <-streamCtx.Done():
			_ = sess.sendFrame(&pb.TunnelFrame{
				StreamId: streamID,
				Payload:  &pb.TunnelFrame_Reset_{Reset_: &pb.StreamReset{Reason: streamCtx.Err().Error()}},
			})
			ps.finish(streamCtx.Err())
			sess.mu.Lock()
			if cur, ok := sess.streams[streamID]; ok && cur == ps {
				delete(sess.streams, streamID)
			}
			sess.mu.Unlock()
		case <-ps.done:
		}
	}()

	return &tunnelStreamClient{sess: sess, ps: ps, streamID: streamID}, nil
}

// tunnelStreamClient 一个逻辑 DecodeV2 流的客户端视图。
type tunnelStreamClient struct {
	sess     *tunnelSession
	ps       *pendingStream
	streamID uint32
}

var _ grpc.BidiStreamingClient[pb.DecodeRequest, pb.DecodeResponseV2] = (*tunnelStreamClient)(nil)

// Send 把 DecodeRequest 编成 request 帧发给插件（首帧隐式开流）。
func (t *tunnelStreamClient) Send(req *pb.DecodeRequest) error {
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("tunnel: marshal request: %w", err)
	}
	select {
	case <-t.ps.done:
		if e := t.ps.endError(); e != nil {
			return e
		}
		return io.EOF
	default:
	}
	return t.sess.sendFrame(&pb.TunnelFrame{
		StreamId: t.streamID,
		Payload:  &pb.TunnelFrame_Request{Request: data},
	})
}

// Recv 读取插件的下一条 DecodeResponseV2；正常结束返回 io.EOF。
// 注意：done/cancel 分支要先排空可能已入队的响应，避免丢失合法结果；
// 流已结束时优先按 done 语义收尾（endErr/EOF），其次才是 ctx 取消错误。
func (t *tunnelStreamClient) Recv() (*pb.DecodeResponseV2, error) {
	if resp := t.pollResp(); resp != nil {
		return resp, nil
	}
	// 流已结束（end/disconnect/reset 后 close(done)）：直接走收尾语义，
	// 避免与 ctx.Done 同时就绪时被随机选中而误报 context.Canceled。
	select {
	case <-t.ps.done:
		return t.recvAfterDone()
	default:
	}
	select {
	case resp := <-t.ps.respCh:
		return resp, nil
	case <-t.ps.done:
		return t.recvAfterDone()
	case <-t.ps.ctx.Done():
		if resp := t.pollResp(); resp != nil {
			return resp, nil
		}
		// finish() 先 close(done) 再 cancel，若此处 done 已关闭则按流结束语义收尾，
		// 避免与 ctx.Done 同时就绪时被随机选中而误报 context.Canceled。
		select {
		case <-t.ps.done:
			return t.recvAfterDone()
		default:
		}
		return nil, t.ps.ctx.Err()
	}
}

// recvAfterDone 在流结束后收尾：先排空残余响应，再报 endErr 或 EOF。
func (t *tunnelStreamClient) recvAfterDone() (*pb.DecodeResponseV2, error) {
	if resp := t.pollResp(); resp != nil {
		return resp, nil
	}
	if e := t.ps.endError(); e != nil {
		return nil, e
	}
	return nil, io.EOF
}

// pollResp 非阻塞排空可能已入队但未被消费的响应。
func (t *tunnelStreamClient) pollResp() *pb.DecodeResponseV2 {
	select {
	case resp := <-t.ps.respCh:
		return resp
	default:
		return nil
	}
}

// CloseSend 关闭请求侧（对应 half_close 帧）。
func (t *tunnelStreamClient) CloseSend() error {
	return t.sess.sendFrame(&pb.TunnelFrame{
		StreamId: t.streamID,
		Payload:  &pb.TunnelFrame_HalfClose{HalfClose: true},
	})
}

// Context 返回 DecodeV2 调用方的派生上下文（reset 后取消）。
func (t *tunnelStreamClient) Context() context.Context { return t.ps.ctx }

func (t *tunnelStreamClient) Header() (metadata.MD, error) { return metadata.MD{}, nil }
func (t *tunnelStreamClient) Trailer() metadata.MD         { return metadata.MD{} }

// SendMsg / RecvMsg 满足 grpc.ClientStream 契约。
func (t *tunnelStreamClient) SendMsg(m interface{}) error {
	if req, ok := m.(*pb.DecodeRequest); ok {
		return t.Send(req)
	}
	return fmt.Errorf("tunnel: SendMsg unsupported type %T", m)
}

func (t *tunnelStreamClient) RecvMsg(m interface{}) error {
	resp, err := t.Recv()
	if err != nil {
		return err
	}
	if dst, ok := m.(*pb.DecodeResponseV2); ok {
		proto.Reset(dst)
		proto.Merge(dst, resp)
		return nil
	}
	return fmt.Errorf("tunnel: RecvMsg unsupported type %T", m)
}
