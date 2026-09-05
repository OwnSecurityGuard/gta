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

	"gametrace/pkg/capture"
	"gametrace/pkg/capture/internal/base"
	"gametrace/pkg/capture/mobile/proto"
	"gametrace/pkg/event"
)

func init() {
	capture.Register("mobile", capture.FactoryFunc{
		ValidateFunc: validateConfig,
		NewFunc:      newSource,
	})
}

func newSource(cfg any) (capture.Source, error) {
	c := cfg.(MobileConfig)
	if err := c.validate(); err != nil {
		return nil, err
	}
	s := &mobileSource{
		cfg:     c,
		out:     make(chan event.Packet, 256),
		conns:   make(map[string]*connState),
		streams: make(map[string]map[string]struct{}),
		// Activity 可能为 nil（未注入时仅维护内部计数器）。
		activity: c.Activity,
	}
	s.StatTracker.Init()
	return s, nil
}

// connState 保存单个连接的元数据与打开时间。
// id 是流内唯一的连接键（含流前缀，全局唯一，用于下游按连接聚合）；
// rawID 是 agent 上报的原始 conn_id（仅用于展示与追溯）。
type connState struct {
	id      string
	rawID   string
	open    *proto.ConnOpen
	created time.Time
}

// mobileSource 是移动代理抓包源：
//
//	gt-singbox-agent ── gRPC Push(stream AgentEvent) ──▶ 本 Source
//
// Source 内部做：
//  1. 按 conn_id 维护连接元数据；
//  2. 每个数据块原样封装为一个 event.Packet（不做应用层分帧，
//     协议帧边界的判定由解码插件按连接自行处理）；
//  3. 每帧构造（LinkType=ProxyPayload，Metadata 携带五元组），
//     经 EnrichFromMetadata 等价逻辑回填 Src/Dst/Protocol。
type mobileSource struct {
	cfg MobileConfig

	proto.UnimplementedMobileCaptureServer

	out chan event.Packet

	connsMu sync.Mutex
	conns   map[string]*connState
	// streams 记录每条 gRPC Push 流当前打开的连接 id 集合，用于流断开时一次性清理。
	//
	// conns 的 key 由「流 id + agent 原始 conn_id」组成（见 connKey）：agent 的
	// conn_id 是各自进程内从 1 开始的自增序号，直接拿它当 key 会让多条流（多
	// agent / agent 重连）的 1、2、3…互相覆盖——后到的 open 顶掉先到的元数据，
	// 两条不同连接的数据被当成同一连接下发，即串流。
	streams    map[string]map[string]struct{}
	nextStream atomic.Uint64

	// activity 为可选注入的运行时活动追踪器（见 MobileConfig.Activity）。
	activity *Activity

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
		"network", network, "addr", addr)
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
//
// 每条流在入口分配一个流 id，流内所有 conn_id 都按 connKey 加上该前缀后才进入
// 共享的 conns 表：agent 侧的 conn_id 只保证「本进程内唯一」，跨流不做这个
// 隔离就会互相覆盖。流退出时该流的全部连接被一次性清理（等价于 agent 侧发来
// 了一批 close，但 agent 异常退出时不会有 close 事件）。
func (s *mobileSource) Push(stream proto.MobileCapture_PushServer) error {
	sk := fmt.Sprintf("s%d", s.nextStream.Add(1))
	defer s.dropStream(sk)
	for {
		evt, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(s.pushResult())
		}
		if err != nil {
			return err
		}
		s.handleEvent(sk, evt)
	}
}

// connKey 把 agent 的 conn_id 限定在某条流内，得到全局唯一的连接键。
func connKey(streamKey, connID string) string { return streamKey + "/" + connID }

// dropStream 清理某条流遗留的全部连接（agent 断流时不会有 close 事件）。
func (s *mobileSource) dropStream(streamKey string) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	ids := s.streams[streamKey]
	delete(s.streams, streamKey)
	for id := range ids {
		if _, ok := s.conns[connKey(streamKey, id)]; !ok {
			continue
		}
		delete(s.conns, connKey(streamKey, id))
		if s.activity != nil {
			s.activity.activeConns.Add(-1)
		}
	}
}

func (s *mobileSource) handleEvent(streamKey string, evt *proto.AgentEvent) {
	ts := time.Now()
	if evt.GetTimestampUnix() > 0 {
		ts = time.Unix(evt.GetTimestampUnix(), 0)
	}
	connID := evt.GetConnId()
	switch e := evt.GetEvent().(type) {
	case *proto.AgentEvent_Open:
		s.handleOpen(streamKey, connID, ts, e.Open)
	case *proto.AgentEvent_Data:
		s.handleData(streamKey, connID, e.Data.GetDirection(), ts, e.Data.GetPayload())
	case *proto.AgentEvent_Close:
		s.handleClose(streamKey, connID)
	default:
		s.countError("unknown agent event for conn %s", connID)
	}
}

func (s *mobileSource) handleOpen(streamKey, connID string, ts time.Time, open *proto.ConnOpen) {
	if connID == "" {
		s.countError("conn_open without conn_id")
		return
	}
	key := connKey(streamKey, connID)
	s.connsMu.Lock()
	if prev, ok := s.conns[key]; ok {
		prev.open = open // 同一流内重复 open：更新元数据（agent 重连时地址可能变化）
	} else {
		s.conns[key] = &connState{id: key, rawID: connID, open: open, created: ts}
		if s.streams[streamKey] == nil {
			s.streams[streamKey] = make(map[string]struct{})
		}
		s.streams[streamKey][connID] = struct{}{}
		s.connsOpened.Add(1)
		if s.activity != nil {
			s.activity.activeConns.Add(1)
			s.activity.totalConns.Add(1)
		}
	}
	s.connsMu.Unlock()
}

func (s *mobileSource) handleData(streamKey, connID, direction string, ts time.Time, payload []byte) {
	if connID == "" {
		s.countError("conn_data without conn_id")
		return
	}
	s.connsMu.Lock()
	conn := s.conns[connKey(streamKey, connID)]
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
	if s.activity != nil {
		s.activity.lastDataUnix.Store(ts.UnixMilli())
		s.activity.totalBytes.Add(uint64(len(payload)))
	}
	// 数据块原样转发为一个 packet：不做应用层分帧，粘包/半包与帧边界
	// 的判定由解码插件按连接自行处理（见 MobileConfig 的分帧职责说明）。
	s.emit(conn, direction, payload, ts)
}

func (s *mobileSource) handleClose(streamKey, connID string) {
	key := connKey(streamKey, connID)
	s.connsMu.Lock()
	conn := s.conns[key]
	delete(s.conns, key)
	if ids := s.streams[streamKey]; ids != nil {
		delete(ids, connID)
	}
	s.connsMu.Unlock()
	if conn == nil {
		s.countError("conn_close for unknown conn %s", connID)
		return
	}
	if s.activity != nil {
		s.activity.activeConns.Add(-1)
	}
}

// emit 把一段应用层数据块包装为 event.Packet 并发往 out channel。
func (s *mobileSource) emit(conn *connState, direction string, frame []byte, ts time.Time) {
	// payload 来自 gRPC 反序列化，后续 Recv 可能复用底层字节，必须复制。
	payload := make([]byte, len(frame))
	copy(payload, frame)
	pkt := buildPacket(conn, direction, payload, ts)
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
func buildPacket(conn *connState, direction string, payload []byte, ts time.Time) event.Packet {
	open := conn.open
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
			// conn_id 是流内唯一的键（含流前缀），下游按它聚合连接；
			// conn_id_raw 是 agent 上报的原始 id，供 UI 展示与人工比对。
			"conn_id":     conn.id,
			"conn_id_raw": conn.rawID,
			"direction":   direction,
		},
	}
}
