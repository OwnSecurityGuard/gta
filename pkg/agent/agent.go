// Package agent 实现 gta-singbox-agent：移动端流量入口（sing-box 侧）到 GTA 的桥接。
//
// 职责边界：
//   - 不修改 sing-box、不 fork：sing-box 提供 TUN/混合代理，本包只做"流量中继 + 连接元数据推送"；
//   - 连接级数据通过 gRPC 客户端流推送给 GTA mobile capture source，GTA 侧按数据块原样透传
//     （应用层分帧/重组是协议语义，由绑定到会话的解码插件按连接自行完成）；
//   - 本包不关心解码、不关心存储，保持 Source 边界。
//
// 数据流：
//
//	Game ──▶ Relay(本包 TCP 中继) ──▶ 上游 server
//	              │
//	              └── gRPC Push ──▶ GTA mobile source ──▶ event.Packet（数据块直通）
package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"gta/pkg/capture/mobile/proto"
)

// PushClient 管理到 GTA mobile source 的 gRPC 客户端流。
// 所有连接共用一个 Push 流（靠 conn_id 区分）；Send 线程安全。
// 连接是惰性且尽力而为的：GTA 未启动 / 未运行代理抓包会话时不阻塞、不退出，
// 后台 keepalive 会在 GTA 上线后自动重连并恢复推送，使本 Agent 可常驻等待代理连接。
type PushClient struct {
	addr   string
	logger *slog.Logger

	mu        sync.Mutex
	conn      *grpc.ClientConn
	stream    proto.MobileCapture_PushClient
	connected bool

	stopCh chan struct{}
	once   sync.Once
}

// NewPushClient 创建 PushClient 并启动后台连接维护。
// GTA 暂不可达时不视为错误：返回的客户端可用，推送会在 GTA 上线后自动恢复。
func NewPushClient(addr string, logger *slog.Logger) (*PushClient, error) {
	if logger == nil {
		logger = slog.Default()
	}
	p := &PushClient{addr: addr, logger: logger, stopCh: make(chan struct{})}
	go p.keepalive()
	p.mu.Lock()
	p.dialLocked() // 首次尽力连接；失败仅记录，不致命
	p.mu.Unlock()
	return p, nil
}

// keepalive 周期性确保连接存在：GTA 启动/重启后自动重连，无需依赖流量触发。
func (p *PushClient) keepalive() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.mu.Lock()
			if !p.connected {
				if err := p.dialLocked(); err != nil {
					p.logger.Debug("push client reconnect failed (will retry)", "error", err)
				} else {
					p.logger.Info("push client connected to GTA mobile source", "addr", p.addr)
				}
			}
			p.mu.Unlock()
		}
	}
}

// dialLocked 建立 gRPC 连接与 Push 流（调用方须持有 mu）。
// grpc.NewClient 是惰性的：底层连接由后续 Send 触发，此处仅保证 stream 对象可用。
func (p *PushClient) dialLocked() error {
	if p.connected {
		return nil
	}
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial gta %s: %w", p.addr, err)
	}
	stream, err := proto.NewMobileCaptureClient(conn).Push(context.Background())
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("open push stream: %w", err)
	}
	p.conn = conn
	p.stream = stream
	p.connected = true
	return nil
}

func (p *PushClient) resetLocked() {
	if p.stream != nil {
		_ = p.stream.CloseSend()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.stream = nil
	p.conn = nil
	p.connected = false
}

// Send 推送一个事件。未连接时尝试惰性建立；发送失败时重置连接（由下一次 Send / keepalive 重连）。
// 调用方（Relay）按"抓包尽力而为"处理返回值：失败不中断数据中继。
func (p *PushClient) Send(evt *proto.AgentEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.connected {
		if err := p.dialLocked(); err != nil {
			return err
		}
	}
	if err := p.stream.Send(evt); err != nil {
		p.logger.Debug("push send failed, resetting connection", "error", err)
		p.resetLocked()
		return err
	}
	return nil
}

// Close 停止后台连接维护并关闭连接。
func (p *PushClient) Close() error {
	p.once.Do(func() { close(p.stopCh) })
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetLocked()
	return nil
}

// RelayConfig 是代理/中继配置。
type RelayConfig struct {
	ListenAddr string // 本机监听地址（手机代理软件连这里，如同 sing-box 的入站端口）
	TargetAddr string // 上游服务器地址；为空时进入纯代理模式，仅接受 HTTP CONNECT 动态指定目标
	App        string // 应用/包名
	Device     string // 设备标识
	Process    string // 进程名
	// FilterHosts 连接筛选：仅抓取目标主机（CONNECT 中的 host）在此列表内的连接。
	// 支持 IP 或域名，不区分大小写；为空表示不按主机筛选。
	FilterHosts []string
	// FilterPorts 连接筛选：仅抓取目标端口在此列表内的连接。为空表示不按端口筛选。
	FilterPorts []int
}

// connectionFilter 是编译后的连接筛选条件（空 hosts+ports 表示不过滤）。
type connectionFilter struct {
	hosts map[string]struct{} // 目标主机（小写）
	ports map[int]struct{}    // 目标端口
}

// empty 返回筛选是否未启用（全部连接都抓取）。
func (f connectionFilter) empty() bool {
	return len(f.hosts) == 0 && len(f.ports) == 0
}

// allow 判断给定目标 host:port 是否应抓包上报。
// 主机与端口列表同时非空时须同时命中（交集），可精确收窄抓包范围。
func (f connectionFilter) allow(target string) bool {
	if f.empty() {
		return true
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	if len(f.hosts) > 0 {
		if _, ok := f.hosts[strings.ToLower(host)]; !ok {
			return false
		}
	}
	if len(f.ports) > 0 {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return false
		}
		if _, ok := f.ports[port]; !ok {
			return false
		}
	}
	return true
}

// Relay 是一个常驻的代理/透明 TCP 中继：
//   - 接受本地连接，转发到上游（固定 TargetAddr 或按 HTTP CONNECT 动态解析）；
//     中继与抓包开关无关——不抓包时手机流量照常出入，只是不上报；
//   - 上报目标由 CaptureGate 动态决定（idle 时不上报，capturing 时推给当前会话）；
//   - 连接建立/关闭时推送 ConnOpen/ConnClose；
//   - 不匹配当前 capture 筛选条件的连接照常中继，但不上报。
type Relay struct {
	cfg    RelayConfig
	gate   *CaptureGate
	logger *slog.Logger

	seq atomic.Uint64 // conn_id 序列
	// agentID 是本 Relay 实例的短标识，作为所有 conn_id 的前缀。
	// conn_id 原本是纯自增序号，从 1 重新开始——同一 GTA 上跑多个 agent 进程时
	// （多设备租约、agent 重启）序号必然重复，日志与落库的 conn 无法区分来源。
	agentID string

	lis    net.Listener
	active sync.Map // 活跃客户端连接（用于 shutdown 时强制关闭）
}

// NewRelay 创建 TCP 中继。gate 决定数据上报到哪里（nil 表示永不抓包，纯中转）。
func NewRelay(cfg RelayConfig, gate *CaptureGate, logger *slog.Logger) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	if gate == nil {
		gate = NewCaptureGate(cfg, logger)
	}
	r := &Relay{cfg: cfg, gate: gate, logger: logger}
	r.agentID = newAgentID()
	return r
}

// Gate 返回本 Relay 绑定的抓包闸门（控制面与 Relay 共用同一实例）。
func (r *Relay) Gate() *CaptureGate { return r.gate }

// newAgentID 生成一个进程级短随机标识（4 字节 hex）。
// 生成失败不影响功能（退化为空前缀，仍由 GTA 侧的流隔离兜底）。
func newAgentID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// compileFilter 将 RelayConfig 中的筛选列表编译为可直接匹配的集合。
func compileFilter(cfg RelayConfig) connectionFilter {
	f := connectionFilter{}
	for _, h := range cfg.FilterHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if f.hosts == nil {
			f.hosts = make(map[string]struct{})
		}
		f.hosts[h] = struct{}{}
	}
	for _, p := range cfg.FilterPorts {
		if p < 1 || p > 65535 {
			continue
		}
		if f.ports == nil {
			f.ports = make(map[int]struct{})
		}
		f.ports[p] = struct{}{}
	}
	return f
}

// Addr 返回实际监听地址（cfg.ListenAddr 为 ":0" 时在 Serve 启动后有效）。
func (r *Relay) Addr() net.Addr {
	if r.lis == nil {
		return nil
	}
	return r.lis.Addr()
}

// Serve 监听并中继连接，直到 ctx 取消。
func (r *Relay) Serve(ctx context.Context) error {
	lis, err := net.Listen("tcp", r.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", r.cfg.ListenAddr, err)
	}
	r.lis = lis
	defer lis.Close()

	// ctx 取消时关闭所有活跃连接并关闭监听器，让阻塞的 Accept/relay goroutine 尽快退出。
	go func() {
		<-ctx.Done()
		r.active.Range(func(k, _ any) bool {
			_ = k.(net.Conn).Close()
			return true
		})
		_ = lis.Close()
	}()

	r.logger.Info("relay listening", "addr", lis.Addr().String(), "target", r.cfg.TargetAddr)

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := lis.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.handleConn(ctx, conn)
		}()
	}
}

// proxyProbeTimeout 纯代理模式下等待客户端 CONNECT 请求的超时。
const proxyProbeTimeout = 5 * time.Second

// handleConn 处理一条代理/中继连接：推送 open，双向转发，推送 close。
// 上游目标由 resolveTarget 决定：固定 TargetAddr（透明中继）或按 HTTP CONNECT 动态解析（纯代理模式）。
// 不匹配筛选条件的连接照常中继，但不上报 open/data/close 给 GTA。
func (r *Relay) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()
	seq := r.seq.Add(1)
	clientAddr := clientConn.RemoteAddr().String()

	r.active.Store(clientConn, struct{}{})
	defer r.active.Delete(clientConn)

	// 目标解析 / 上游拨号失败时不产生任何上报：连接都还没建立，
	// source 侧没有对应的 open，发 close 只会打成未知连接的错误计数。
	target, upstream, err := r.resolveTarget(clientConn)
	if err != nil {
		r.logger.Warn("resolve upstream target failed", "conn", seq, "error", err)
		return
	}
	serverConn, err := net.Dial("tcp", target)
	if err != nil {
		r.logger.Warn("dial upstream failed", "conn", seq, "target", target, "error", err)
		return
	}
	defer serverConn.Close()

	conn := &relayConn{
		id:     fmt.Sprintf("%s-%d", r.agentID, seq),
		target: target,
		open: &proto.ConnOpen{
			ClientAddr:  clientAddr,
			ServerAddr:  target,
			Network:     "tcp",
			App:         r.cfg.App,
			Device:      r.cfg.Device,
			ProcessName: r.cfg.Process,
		},
		logger: r.logger,
	}

	r.gate.activeConns.Add(1)
	r.gate.totalConns.Add(1)
	defer r.gate.activeConns.Add(-1)

	// 抓包已开启时立即声明连接（零字节连接也能在会话里看见）；
	// 未开启时留给 forward 在首次有数据时补发——该连接可能早于 capture 就已建立。
	conn.begin(r.gate.Current())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.forward(upstream, serverConn, conn, "request")
	}()
	go func() {
		defer wg.Done()
		r.forward(serverConn, clientConn, conn, "response")
	}()
	wg.Wait()
	conn.end(r.gate.Current())
}

// resolveTarget 确定连接的上游目标，返回请求方向的读取源：
//   - 透明中继（TargetAddr 非空）：不探测、零延迟直转，读取源为原始连接；
//   - 纯代理模式（TargetAddr 为空）：等待 HTTP CONNECT，解析动态目标并回 200，
//     读取源为带缓冲的 reader（CONNECT 请求头已消费，隧道数据从中继续读取）。
func (r *Relay) resolveTarget(clientConn net.Conn) (string, io.Reader, error) {
	if r.cfg.TargetAddr != "" {
		return r.cfg.TargetAddr, clientConn, nil
	}

	br := bufio.NewReader(clientConn)
	_ = clientConn.SetReadDeadline(time.Now().Add(proxyProbeTimeout))
	line, rerr := br.ReadString('\n')
	_ = clientConn.SetReadDeadline(time.Time{})
	if rerr != nil {
		if isTimeout(rerr) {
			return "", nil, fmt.Errorf("no CONNECT request within %s", proxyProbeTimeout)
		}
		return "", nil, rerr
	}

	target, ok := parseConnectTarget(line)
	if !ok {
		return "", nil, fmt.Errorf("expected CONNECT request, got %q", strings.TrimSpace(line))
	}
	// 消费剩余请求头直到空行（不回传上游，也不作为应用数据捕获）。
	for {
		hdr, herr := br.ReadString('\n')
		if herr != nil || hdr == "\r\n" || hdr == "\n" {
			break
		}
	}
	if _, werr := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); werr != nil {
		return "", nil, fmt.Errorf("write CONNECT 200: %w", werr)
	}
	return target, br, nil
}

// isTimeout 判断读错误是否为超时（net.Error.Timeout）。
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// parseConnectTarget 解析 HTTP CONNECT 首行，返回 host:port。
// 形如：CONNECT api.xxx.com:443 HTTP/1.1
func parseConnectTarget(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", false
	}
	if strings.ToUpper(fields[0]) != "CONNECT" {
		return "", false
	}
	target := fields[1]
	if _, _, err := net.SplitHostPort(target); err != nil {
		return "", false
	}
	return target, true
}

// forward 单向转发：读 src 写 dst；若当前处于 capturing 且连接命中筛选，
// 同时把字节推给当前会话的 mobile source。
//
// 抓包开关是**每块数据实时判定**的，而不是连接建立时定死的：
//   - capture 中途开启 → 运行中已建立的连接从下一块数据起进入新会话；
//   - capture 中途关闭 → 立即停止上报，连接继续转发（手机侧无感）。
//
// 不抓包时不做 payload 复制（直接把读缓冲写出去），这是「零上报成本」的
// 关键：idle 状态下除一次 socket 写外没有任何额外分配。
// 推送失败不中断中继：GTA 未运行时抓包暂时丢失，代理连接仍在转发。
func (r *Relay) forward(src io.Reader, dst net.Conn, conn *relayConn, direction string) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			r.gate.relayBytes.Add(uint64(n))
			r.gate.lastDataUnix.Store(time.Now().UnixMilli())

			sink, connID := conn.begin(r.gate.Current())
			if sink == nil {
				// 未抓包 / 未命中筛选：原样透传，不复制、不上报。
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			} else {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if _, werr := dst.Write(chunk); werr != nil {
					return
				}
				r.gate.capturedBytes.Add(uint64(n))
				if perr := sink.Send(&proto.AgentEvent{
					ConnId:        connID,
					TimestampUnix: time.Now().Unix(),
					Event:         &proto.AgentEvent_Data{Data: &proto.ConnData{Direction: direction, Payload: chunk}},
				}); perr != nil {
					r.logger.Debug("push failed, capture best-effort", "conn", connID, "direction", direction, "error", perr)
				}
			}
		}
		if err != nil {
			_ = dst.Close()
			return
		}
	}
}

// relayConn 是一条中继连接的上报状态。
//
// epoch 隔离是新旧会话不串的关键：手机上的游戏长连接往往在抓包开始前就已建立，
// 且会跨越多次 start/stop。conn_id 带 epoch 前缀后，同一条物理连接在不同
// capture 中上报为不同的 conn_id；再配合每个 capture 独占的 gRPC 流，
// 旧会话绝不可能收到新会话的数据（反之亦然）。
type relayConn struct {
	id     string // agent 内唯一连接标识（不含 epoch）
	target string // 上游目标 host:port，供筛选判定
	open   *proto.ConnOpen
	logger *slog.Logger

	mu     sync.Mutex
	epoch  uint64 // 最近一次 ConnOpen 所属的 capture epoch（0=从未）
	opened bool   // 当前 epoch 是否已发送过 ConnOpen
}

// connID 返回该连接在给定 epoch 下的上报标识。
func (c *relayConn) connID(epoch uint64) string {
	return fmt.Sprintf("%s-e%d", c.id, epoch)
}

// begin 在当前 capture 下确保已声明本连接，返回应使用的 sink 与 conn_id。
// 返回 nil 表示本次数据不上报（未抓包，或该连接未命中当前 capture 的筛选）。
//
// 幂等：同一 epoch 内只发一次 ConnOpen。
func (c *relayConn) begin(st *captureState) (*PushClient, string) {
	if st == nil || !st.filter.allow(c.target) {
		return nil, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != st.epoch {
		// 进入新 capture：上一个 epoch 的 open 已随旧流作废，需在新会话重新声明。
		c.epoch = st.epoch
		c.opened = false
		c.logger.Debug("connection entering new capture epoch", "conn", c.id, "epoch", st.epoch)
	}
	if !c.opened {
		c.opened = true
		_ = st.sink.Send(&proto.AgentEvent{
			ConnId:        c.connID(st.epoch),
			TimestampUnix: time.Now().Unix(),
			Event:         &proto.AgentEvent_Open{Open: c.open},
		})
	}
	return st.sink, c.connID(st.epoch)
}

// end 连接结束时发送 ConnClose（仅当当前 epoch 确实 open 过）。
// st 为 nil（已停止抓包）时不发：旧流已关闭，close 无处可送。
func (c *relayConn) end(st *captureState) {
	if st == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != st.epoch || !c.opened {
		return
	}
	c.opened = false
	_ = st.sink.Send(&proto.AgentEvent{
		ConnId:        c.connID(c.epoch),
		TimestampUnix: time.Now().Unix(),
		Event:         &proto.AgentEvent_Close{Close: &proto.ConnClose{Reason: "closed"}},
	})
}
