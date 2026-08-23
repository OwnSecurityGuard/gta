package mobile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"gta/pkg/capture"
	"gta/pkg/capture/internal/base"
	"gta/pkg/capture/mobile/proto"
	"gta/pkg/event"
)

func init() {
	capture.Register("mobile", capture.FactoryFunc{
		ValidateFunc: validateConfig,
		NewFunc:      newSource,
	})
}

func newSource(cfg any) (capture.Source, error) {
	c := cfg.(MobileConfig)
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	s := &mobileSource{
		cfg:   c,
		out:   make(chan event.Packet, 256),
		reasm: NewReassembler(c),
		conns: make(map[string]*connState),
	}
	s.StatTracker.Init()
	return s, nil
}

// connState 保存单个连接（conn_id）的元数据与打开时间。
// 一个连接的两个方向（request/response）是独立的字节流，由 Reassembler 分开缓冲。
type connState struct {
	id      string
	open    *proto.ConnOpen
	created time.Time
}

// mobileSource 是移动代理抓包源：
//
//	gta-singbox-agent ── gRPC Push(stream AgentEvent) ──▶ 本 Source
//
// Source 内部做：
//  1. 按 conn_id 维护连接元数据；
//  2. Reassembler 把 TCP 字节流重组为应用层帧（raw 或 length_prefix）；
//  3. 每帧构造一个 event.Packet（LinkType=ProxyPayload，Metadata 携带五元组），
//     经 EnrichFromMetadata 等价逻辑回填 Src/Dst/Protocol。
type mobileSource struct {
	cfg MobileConfig

	proto.UnimplementedMobileCaptureServer

	out   chan event.Packet
	reasm *Reassembler

	connsMu sync.Mutex
	conns   map[string]*connState

	lis        net.Listener
	grpcServer *grpc.Server

	connsOpened atomic.Uint64 // 已打开的连接数（统计用，不依赖 StatTracker）
	bytesRecv   atomic.Uint64 // 已接收字节数（跨流累计）

	base.Lifecycle
	base.StatTracker
}

// Start 启动 gRPC server 并开始接收 agent 推送。
func (s *mobileSource) Start(ctx context.Context) error {
	return s.Lifecycle.Start(ctx, s.setup, s.run)
}

func (s *mobileSource) setup() error {
	network := s.cfg.listenNetwork()
	addr := s.cfg.listenPath()
	lis, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", network, addr, err)
	}
	s.lis = lis
	s.grpcServer = grpc.NewServer()
	proto.RegisterMobileCaptureServer(s.grpcServer, s)
	slog.Info("mobile capture source listening",
		"network", network, "addr", addr,
		"frame_style", s.cfg.FrameStyle, "prefix_len", s.cfg.PrefixLen)
	return nil
}

func (s *mobileSource) run(ctx context.Context) {
	defer close(s.out)
	go func() {
		<-ctx.Done()
		s.grpcServer.Stop()
	}()
	if err := s.grpcServer.Serve(s.lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		s.SetErr(err)
		slog.Error("mobile capture server error", "error", err)
	}
	slog.Info("mobile capture source closed")
}

func (s *mobileSource) Packets() <-chan event.Packet { return s.out }
func (s *mobileSource) Err() error                   { return s.Lifecycle.Err() }
func (s *mobileSource) Close() error                 { return s.Lifecycle.Close() }
func (s *mobileSource) Stats() capture.Stats         { return s.StatTracker.Stats() }

// Addr 返回实际监听地址（ListenAddr 为 ":0" 时在 Start 后有效），供 agent 确定连接目标。
func (s *mobileSource) Addr() net.Addr {
	if s.lis == nil {
		return nil
	}
	return s.lis.Addr()
}

// Push 实现 proto.MobileCaptureServer：接收 agent 的客户端流。
// 一条流内可包含多条连接的事件（靠 conn_id 区分），流结束返回汇总统计。
// 流中途断开（agent 重连）不影响 server 继续接受新流。
func (s *mobileSource) Push(stream proto.MobileCapture_PushServer) error {
	for {
		evt, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(s.pushResult())
		}
		if err != nil {
			return err
		}
		s.handleEvent(evt)
	}
}

func (s *mobileSource) handleEvent(evt *proto.AgentEvent) {
	ts := time.Now()
	if evt.GetTimestampUnix() > 0 {
		ts = time.Unix(evt.GetTimestampUnix(), 0)
	}
	connID := evt.GetConnId()
	switch e := evt.GetEvent().(type) {
	case *proto.AgentEvent_Open:
		s.handleOpen(connID, ts, e.Open)
	case *proto.AgentEvent_Data:
		s.handleData(connID, e.Data.GetDirection(), ts, e.Data.GetPayload())
	case *proto.AgentEvent_Close:
		s.handleClose(connID, ts, e.Close.GetReason())
	default:
		s.countError("unknown agent event for conn %s", connID)
	}
}

func (s *mobileSource) handleOpen(connID string, ts time.Time, open *proto.ConnOpen) {
	if connID == "" {
		s.countError("conn_open without conn_id")
		return
	}
	s.connsMu.Lock()
	if prev, ok := s.conns[connID]; ok {
		prev.open = open // 更新元数据（agent 重连时地址可能变化）
	} else {
		s.conns[connID] = &connState{id: connID, open: open, created: ts}
		s.connsOpened.Add(1)
	}
	s.connsMu.Unlock()
}

func (s *mobileSource) handleData(connID, direction string, ts time.Time, payload []byte) {
	if connID == "" {
		s.countError("conn_data without conn_id")
		return
	}
	s.connsMu.Lock()
	conn := s.conns[connID]
	s.connsMu.Unlock()
	if conn == nil {
		// open 未到就收到数据（agent 违约）：丢弃并计数。
		s.countError("conn_data before conn_open, conn=%s", connID)
		return
	}
	// 方向归一化：request=client→server，其余视为 response=server→client。
	if direction == "" {
		direction = "request"
	}
	s.bytesRecv.Add(uint64(len(payload)))
	frames, dropped := s.reasm.Write(connID, direction, payload)
	if dropped > 0 {
		s.StatTracker.AddErrors(uint64(dropped))
		slog.Debug("mobile reassembler dropped bytes", "conn", connID, "bytes", dropped)
	}
	for _, f := range frames {
		s.emit(conn, direction, f, ts)
	}
}

func (s *mobileSource) handleClose(connID string, ts time.Time, reason string) {
	s.connsMu.Lock()
	conn := s.conns[connID]
	delete(s.conns, connID)
	s.connsMu.Unlock()
	if conn == nil {
		s.countError("conn_close for unknown conn %s", connID)
		return
	}
	// flush 两个方向残余缓冲：长度前缀模式下不完整的尾帧被丢弃。
	for _, dir := range []string{"request", "response"} {
		frames, dropped := s.reasm.Flush(connID, dir)
		if dropped > 0 {
			s.StatTracker.AddErrors(uint64(dropped))
		}
		for _, f := range frames {
			s.emit(conn, dir, f, ts)
		}
	}
	s.reasm.Drop(connID)
}

// emit 把一个应用帧包装为 event.Packet 并发往 out channel。
func (s *mobileSource) emit(conn *connState, direction string, frame []byte, ts time.Time) {
	// Reassembler 返回的是内部缓冲切片，后续 Write 可能复用，必须复制。
	payload := make([]byte, len(frame))
	copy(payload, frame)
	pkt := buildPacket(conn.open, conn.id, direction, payload, ts)
	s.StatTracker.AddIn(len(payload))
	select {
	case s.out <- pkt:
	default:
		// 背压：消费者跟不上时记录阻塞并等待，同时可被 Close 中断。
		s.StatTracker.AddBlocked()
		select {
		case s.out <- pkt:
		case <-s.Context().Done():
		}
	}
}

func (s *mobileSource) countError(msg string, args ...any) {
	s.StatTracker.AddErrors(1)
	slog.Debug("mobile source dropped event", "msg", fmt.Sprintf(msg, args...))
}

// pushResult 汇总本次流的统计（SendAndClose 返回值）。
func (s *mobileSource) pushResult() *proto.PushResult {
	st := s.StatTracker.Stats()
	return &proto.PushResult{
		Connections: s.connsOpened.Load(),
		Packets:     st.PacketsIn,
		Bytes:       s.bytesRecv.Load(),
		Errors:      st.Errors,
	}
}

// buildPacket 根据连接元数据与方向构造 event.Packet。
//
// 移动代理数据没有 IP/TCP 头，不伪造头，而是：
//   - LinkType = LinkTypeProxyPayload（自定义值，明确表达"只有应用层 payload"）；
//   - Src/Dst 由 ConnOpen 的 client_addr/server_addr 按方向回填；
//   - Protocol = network（"tcp"），使现有解码路径（含按协议 hint 路由插件）无需改动即可工作。
func buildPacket(open *proto.ConnOpen, connID, direction string, payload []byte, ts time.Time) event.Packet {
	network := strings.ToLower(open.GetNetwork())
	if network == "" {
		network = "tcp"
	}
	client, _ := netip.ParseAddrPort(open.GetClientAddr())
	server, _ := netip.ParseAddrPort(open.GetServerAddr())
	var src, dst netip.AddrPort
	if direction == "request" {
		src, dst = client, server
	} else {
		src, dst = server, client
	}
	return event.Packet{
		Timestamp: ts,
		Raw:       payload,
		LinkType:  event.LinkTypeProxyPayload,
		Src:       src,
		Dst:       dst,
		Protocol:  network,
		Metadata: map[string]any{
			capture.MetaSource:      "mobile",
			capture.MetaClientAddr:  open.GetClientAddr(),
			capture.MetaServerAddr:  open.GetServerAddr(),
			capture.MetaAppPackage:  open.GetApp(),
			capture.MetaDevice:      open.GetDevice(),
			capture.MetaProcessName: open.GetProcessName(),
			"network":               network,
			"conn_id":               connID,
			"direction":             direction,
		},
	}
}
