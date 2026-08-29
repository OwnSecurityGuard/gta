package plugin

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"

	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// ---------- 进程内隧道帧管道（复用 SDK tunnel_test.go 的思路） ----------

// tunnelHubPipe 提供一对互联的 PluginRegistry_ConnectServer（交给 TunnelHub）
// 与 pluginEnd（测试扮演插件侧）假实现。
type tunnelHubPipe struct {
	ctx    context.Context
	hub2p  chan *pb.TunnelFrame // hub(服务端) → 插件
	p2hub  chan *pb.TunnelFrame // 插件 → hub
	closed chan struct{}
	once   sync.Once
}

func newTunnelHubPipe(ctx context.Context) *tunnelHubPipe {
	return &tunnelHubPipe{
		ctx:    ctx,
		hub2p:  make(chan *pb.TunnelFrame, 16),
		p2hub:  make(chan *pb.TunnelFrame, 16),
		closed: make(chan struct{}),
	}
}

func (p *tunnelHubPipe) close() {
	p.once.Do(func() { close(p.closed) })
}

// hubEnd 实现 pb.PluginRegistry_ConnectServer，交给 TunnelHub.Connect。
type hubEnd struct{ p *tunnelHubPipe }

func (h hubEnd) Recv() (*pb.TunnelFrame, error) {
	select {
	case f := <-h.p.p2hub:
		return f, nil
	case <-h.p.closed:
		return nil, io.EOF
	case <-h.p.ctx.Done():
		return nil, h.p.ctx.Err()
	}
}

func (h hubEnd) Send(f *pb.TunnelFrame) error {
	select {
	case h.p.hub2p <- f:
		return nil
	case <-h.p.closed:
		return errors.New("pipe closed")
	case <-h.p.ctx.Done():
		return h.p.ctx.Err()
	}
}

func (h hubEnd) Context() context.Context     { return h.p.ctx }
func (h hubEnd) SendMsg(interface{}) error    { return errors.New("not supported") }
func (h hubEnd) RecvMsg(interface{}) error    { return errors.New("not supported") }
func (h hubEnd) SendHeader(metadata.MD) error { return nil }
func (h hubEnd) SetHeader(metadata.MD) error  { return nil }
func (h hubEnd) SetTrailer(metadata.MD)       {}

var _ pb.PluginRegistry_ConnectServer = hubEnd{}

// pluginEnd 测试扮演插件侧：读 hub2p 帧、写 p2hub 帧。
type pluginEnd struct{ p *tunnelHubPipe }

// recvRequestFrame 带超时读一帧（期待 request/half_close/reset）。
func (e pluginEnd) recvFrame(t *testing.T) *pb.TunnelFrame {
	t.Helper()
	select {
	case f := <-e.p.hub2p:
		return f
	case <-e.p.closed:
		t.Fatal("pipe closed while waiting for frame")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tunnel frame")
	}
	return nil
}

func (e pluginEnd) sendFrame(f *pb.TunnelFrame) {
	select {
	case e.p.p2hub <- f:
	case <-e.p.closed:
	case <-time.After(5 * time.Second):
		panic("timeout sending frame to hub")
	}
}

// respond 解 request 帧，回一条 result + done + end（正常结束）。
func (e pluginEnd) respond(t *testing.T, streamID uint32, eventType string) {
	f := e.recvFrame(t)
	if f.GetStreamId() != streamID {
		t.Fatalf("expected stream_id %d, got %d", streamID, f.GetStreamId())
	}
	data, ok := f.GetPayload().(*pb.TunnelFrame_Request)
	if !ok {
		t.Fatalf("expected request frame, got %T", f.GetPayload())
	}
	req := &pb.DecodeRequest{}
	if err := proto.Unmarshal(data.Request, req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	for _, resp := range []*pb.DecodeResponseV2{
		{InputId: req.InputId, EventType: eventType},
		{InputId: req.InputId, Done: true},
	} {
		b, err := proto.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		e.sendFrame(&pb.TunnelFrame{
			StreamId: streamID,
			Payload:  &pb.TunnelFrame_Response{Response: b},
		})
	}
	e.sendFrame(&pb.TunnelFrame{StreamId: streamID, Payload: &pb.TunnelFrame_End{End: &pb.StreamEnd{}}})
}

// ---------- 测试 ----------

// TestTunnelRoundTrip 验证 DecodeV2 经隧道往返（result + done + end）。
func TestTunnelRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelOwnerResolver(func(context.Context) string { return "inst-1" }),
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Connect(hubEnd{p}) }()

	var client pb.DecoderClient
	select {
	case client = <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("connect hook not called")
	}

	pe := pluginEnd{p}
	stream, err := client.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	req := &pb.DecodeRequest{SessionId: "s1", Payload: []byte("hello"), InputId: "in-1"}
	go func() {
		if err := stream.Send(req); err != nil {
			t.Errorf("send: %v", err)
		}
	}()
	pe.respond(t, 1, "http.request")

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv result: %v", err)
	}
	if resp.EventType != "http.request" || resp.InputId != "in-1" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	resp, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv done: %v", err)
	}
	if !resp.Done {
		t.Fatalf("expected done=true, got %+v", resp)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF after end frame, got %v", err)
	}
}

// TestTunnelReset 验证取消 DecodeV2 上下文会向插件发 reset 帧。
func TestTunnelReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	go func() { _ = hub.Connect(hubEnd{p}) }()
	client := <-connected

	callCtx, callCancel := context.WithCancel(ctx)
	stream, err := client.DecodeV2(callCtx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	callCancel()

	pe := pluginEnd{p}
	f := pe.recvFrame(t)
	if _, ok := f.GetPayload().(*pb.TunnelFrame_Reset_); !ok {
		t.Fatalf("expected reset frame, got %T", f.GetPayload())
	}
	if _, err := stream.Recv(); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestTunnelEndWithError 验证 end 帧带错误时 Recv 返回该错误。
func TestTunnelEndWithError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	go func() { _ = hub.Connect(hubEnd{p}) }()
	client := <-connected

	stream, err := client.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	go func() {
		_ = stream.Send(&pb.DecodeRequest{InputId: "in-err"})
	}()

	pe := pluginEnd{p}
	f := pe.recvFrame(t)
	pe.sendFrame(&pb.TunnelFrame{
		StreamId: f.GetStreamId(),
		Payload:  &pb.TunnelFrame_End{End: &pb.StreamEnd{Error: "decode exploded"}},
	})
	if _, err := stream.Recv(); err == nil || err.Error() != "tunnel: plugin stream error: decode exploded" {
		t.Fatalf("expected plugin error, got %v", err)
	}
}

// TestTunnelHalfClose 验证 CloseSend 发出 half_close 帧。
func TestTunnelHalfClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	go func() { _ = hub.Connect(hubEnd{p}) }()
	client := <-connected

	stream, err := client.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	pe := pluginEnd{p}
	f := pe.recvFrame(t)
	if f.GetPayload().(*pb.TunnelFrame_HalfClose) == nil {
		t.Fatalf("expected half_close frame, got %T", f.GetPayload())
	}
}

// TestTunnelDisconnectHook 验证流断开触发 onDisconnect。
func TestTunnelDisconnectHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	var offline atomic.Value
	offlineCh := make(chan struct{}, 1)
	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelOwnerResolver(func(context.Context) string { return "inst-42" }),
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
		WithTunnelDisconnectHook(func(owner string) {
			offline.Store(owner)
			offlineCh <- struct{}{}
		}),
	)
	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Connect(hubEnd{p}) }()
	<-connected

	p.close() // 模拟插件断开
	select {
	case <-offlineCh:
		if owner, _ := offline.Load().(string); owner != "inst-42" {
			t.Fatalf("expected owner inst-42, got %v", owner)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect hook not called")
	}
	select {
	case err := <-hubErr:
		if err != nil {
			t.Fatalf("Connect should return nil on clean EOF, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return")
	}
}

// ---------- 真实 gRPC 传输的端到端集成 ----------

// fakeTunnelRegistry 把 TunnelHub.Connect 暴露成完整 PluginRegistry 服务，
// 用 bufconn 跑真实 gRPC 传输，验证 Connect 处理器与生成代码的适配。
type fakeTunnelRegistry struct {
	pb.UnimplementedPluginRegistryServer
	hub *TunnelHub
}

func (f *fakeTunnelRegistry) Connect(stream pb.PluginRegistry_ConnectServer) error {
	return f.hub.Connect(stream)
}

// TestTunnelConnectOverRealGRPC 用 bufconn 走一遍完整链路：
// tunnelClient（Dispatcher 侧）→ gRPC → 插件侧应答器 → gRPC → Recv。
func TestTunnelConnectOverRealGRPC(t *testing.T) {
	connected := make(chan pb.DecoderClient, 1)
	hub2 := NewTunnelHub(
		WithTunnelOwnerResolver(func(context.Context) string { return "inst-e2e" }),
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	srv := grpc.NewServer()
	pb.RegisterPluginRegistryServer(srv, &fakeTunnelRegistry{hub: hub2})

	lis := bufconn.Listen(64 * 1024)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer cc.Close()

	client := pb.NewPluginRegistryClient(cc)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// 插件侧应答器：模拟 SDK runTunnel 的帧语义（首帧开流、result+done+end）。
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				return
			}
			reqFrame, ok := frame.GetPayload().(*pb.TunnelFrame_Request)
			if !ok {
				continue
			}
			req := &pb.DecodeRequest{}
			if err := proto.Unmarshal(reqFrame.Request, req); err != nil {
				continue
			}
			sid := frame.GetStreamId()
			_ = stream.Send(&pb.TunnelFrame{StreamId: sid, Payload: &pb.TunnelFrame_Response{
				Response: mustMarshal(t, &pb.DecodeResponseV2{InputId: req.InputId, EventType: "e2e.event"}),
			}})
			_ = stream.Send(&pb.TunnelFrame{StreamId: sid, Payload: &pb.TunnelFrame_Response{
				Response: mustMarshal(t, &pb.DecodeResponseV2{InputId: req.InputId, Done: true}),
			}})
			_ = stream.Send(&pb.TunnelFrame{StreamId: sid, Payload: &pb.TunnelFrame_End{End: &pb.StreamEnd{}}})
		}
	}()

	var decoderClient pb.DecoderClient
	select {
	case decoderClient = <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("connect hook not called")
	}

	s, err := decoderClient.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	if err := s.Send(&pb.DecodeRequest{InputId: "e2e-1", Payload: []byte("payload")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := s.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.EventType != "e2e.event" {
		t.Fatalf("unexpected event type: %s", resp.EventType)
	}
	if resp, err = s.Recv(); err != nil || !resp.Done {
		t.Fatalf("expected done response, got %v, %v", resp, err)
	}
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTunnelDeathWithBlockedRecv 隧道断开时，阻塞中的 Recv 必须立即报错返回，
// 不能永久挂起（Dispatcher 可能用 context.Background() 调用）。
func TestTunnelDeathWithBlockedRecv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Connect(hubEnd{p}) }()
	client := <-connected

	stream, err := client.DecodeV2(ctx) // 背景为 Background 的调用方视角
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	recvErr := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvErr <- err
	}()
	// 等待 Recv 真正阻塞后再断开隧道
	time.Sleep(50 * time.Millisecond)
	p.close()

	select {
	case err := <-recvErr:
		if !errors.Is(err, ErrTunnelSessionClosed) {
			t.Fatalf("expected ErrTunnelSessionClosed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Recv did not return after tunnel death")
	}
	select {
	case <-hubErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return")
	}
	// 会话关闭后再 DecodeV2 必须立刻失败，而不是注册一个永远无人应答的流
	if _, err := client.DecodeV2(ctx); !errors.Is(err, ErrTunnelSessionClosed) {
		t.Fatalf("expected ErrTunnelSessionClosed from DecodeV2 after close, got %v", err)
	}
}

// TestTunnelQueueOverflowAbortsAndResets 响应队列溢出（排队超过 enqueueWait）
// 时放弃该逻辑流：本地 Recv 报错，并向插件发 reset 帧。
func TestTunnelQueueOverflowAbortsAndResets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
		WithTunnelQueueParams(1, 100*time.Millisecond),
	)
	go func() { _ = hub.Connect(hubEnd{p}) }()
	client := <-connected

	stream, err := client.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	if err := stream.Send(&pb.DecodeRequest{InputId: "ovf-1"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	pe := pluginEnd{p}
	f := pe.recvFrame(t) // request 帧
	sid := f.GetStreamId()

	// 不消费，连发 2 条响应：第 1 条填满队列（cap=1），第 2 条排队超时触发 abort
	for _, resp := range []*pb.DecodeResponseV2{
		{InputId: "ovf-1", EventType: "a"},
		{InputId: "ovf-1", EventType: "b"},
	} {
		pe.sendFrame(&pb.TunnelFrame{StreamId: sid, Payload: &pb.TunnelFrame_Response{Response: mustMarshal(t, resp)}})
	}

	// abort 后应向插件发 reset 帧
	f = pe.recvFrame(t)
	if _, ok := f.GetPayload().(*pb.TunnelFrame_Reset_); !ok {
		t.Fatalf("expected reset frame after queue overflow, got %T", f.GetPayload())
	}
	// 第 1 条已入队的响应仍可被消费，随后才报溢出中止错误
	if resp, err := stream.Recv(); err != nil || resp.EventType != "a" {
		t.Fatalf("expected queued response before abort, got %v, %v", resp, err)
	}
	if _, err := stream.Recv(); err == nil || !strings.Contains(err.Error(), "response queue overflow") {
		t.Fatalf("expected queue overflow error, got %v", err)
	}
}

// TestSendAfterCloseSendRejected CloseSend 之后的 Send 应被本地拒绝。
func TestSendAfterCloseSendRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)

	connected := make(chan pb.DecoderClient, 1)
	hub := NewTunnelHub(
		WithTunnelConnectHook(func(_ string, c pb.DecoderClient) { connected <- c }),
	)
	go func() { _ = hub.Connect(hubEnd{p}) }()
	client := <-connected

	stream, err := client.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	err = stream.Send(&pb.DecodeRequest{InputId: "late"})
	if err == nil || !strings.Contains(err.Error(), "send after CloseSend") {
		t.Fatalf("expected send-after-CloseSend error, got %v", err)
	}
}
