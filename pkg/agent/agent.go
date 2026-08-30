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

// Relay 是一个代理/透明 TCP 中继：
//   - 接受本地连接，转发到上游（固定 TargetAddr 或按 HTTP CONNECT 动态解析）；
//   - 两个方向的字节流原样推送给 GTA（direction=request/response），抓包尽力而为；
//   - 连接建立/关闭时推送 ConnOpen/ConnClose；
//   - 不匹配筛选条件的连接照常中继，但不上报给 GTA。
type Relay struct {
	cfg    RelayConfig
	client *PushClient
	logger *slog.Logger
	filter connectionFilter

	seq atomic.Uint64 // conn_id 序列

	lis    net.Listener
	active sync.Map // 活跃客户端连接（用于 shutdown 时强制关闭）
}

// NewRelay 创建 TCP 中继。
func NewRelay(cfg RelayConfig, client *PushClient, logger *slog.Logger) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay{cfg: cfg, client: client, logger: logger, filter: compileFilter(cfg)}
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
	connID := fmt.Sprintf("%d", r.seq.Add(1))
	clientAddr := clientConn.RemoteAddr().String()

	r.active.Store(clientConn, struct{}{})
	defer r.active.Delete(clientConn)

	target, upstream, err := r.resolveTarget(clientConn)
	if err != nil {
		r.logger.Warn("resolve upstream target failed", "conn", connID, "error", err)
		r.pushClose(connID, "no_target")
		return
	}

	// 连接筛选：命中才上报，否则只中继（避免无关连接淹没抓包结果）。
	capture := r.filter.allow(target)
	if !capture {
		r.logger.Debug("connection filtered out (relay only, not captured)", "conn", connID, "target", target)
	}

	if capture {
		r.push(&proto.AgentEvent{
			ConnId:        connID,
			TimestampUnix: time.Now().Unix(),
			Event: &proto.AgentEvent_Open{Open: &proto.ConnOpen{
				ClientAddr:  clientAddr,
				ServerAddr:  target,
				Network:     "tcp",
				App:         r.cfg.App,
				Device:      r.cfg.Device,
				ProcessName: r.cfg.Process,
			}},
		})
	}

	serverConn, err := net.Dial("tcp", target)
	if err != nil {
		r.logger.Warn("dial upstream failed", "conn", connID, "target", target, "error", err)
		if capture {
			r.pushClose(connID, "dial_failed")
		}
		return
	}
	defer serverConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.forward(upstream, serverConn, connID, "request", capture)
	}()
	go func() {
		defer wg.Done()
		r.forward(serverConn, clientConn, connID, "response", capture)
	}()
	wg.Wait()
	if capture {
		r.pushClose(connID, "closed")
	}
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

// forward 单向转发：读 src 写 dst，同时把读到的字节推送给 GTA（尽力而为）。
// capture 为 false 时只转发不上报（该连接未命中筛选）。
// 任一端出错即关闭对端，让对向 goroutine 尽快退出。
// 推送失败不中断中继：GTA 未运行时抓包暂时丢失，代理连接仍在转发。
func (r *Relay) forward(src io.Reader, dst net.Conn, connID, direction string, capture bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if _, werr := dst.Write(chunk); werr != nil {
				return
			}
			if !capture {
				continue
			}
			if perr := r.push(&proto.AgentEvent{
				ConnId:        connID,
				TimestampUnix: time.Now().Unix(),
				Event:         &proto.AgentEvent_Data{Data: &proto.ConnData{Direction: direction, Payload: chunk}},
			}); perr != nil {
				r.logger.Debug("push failed, capture best-effort", "conn", connID, "direction", direction, "error", perr)
			}
		}
		if err != nil {
			_ = dst.Close()
			return
		}
	}
}

func (r *Relay) push(evt *proto.AgentEvent) error {
	if err := r.client.Send(evt); err != nil {
		r.logger.Debug("push to GTA failed (capture best-effort)", "error", err)
		return err
	}
	return nil
}

func (r *Relay) pushClose(connID, reason string) {
	_ = r.push(&proto.AgentEvent{
		ConnId:        connID,
		TimestampUnix: time.Now().Unix(),
		Event:         &proto.AgentEvent_Close{Close: &proto.ConnClose{Reason: reason}},
	})
}
