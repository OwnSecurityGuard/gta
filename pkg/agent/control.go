// Package agent 的控制面：让 gta-singbox-agent 从「一次性抓包进程」变成
// 「常驻的手机出口」，抓包与否由外部（gta-pipeline）通过本地 HTTP 控制接口切换。
//
// 为什么需要它：
//
//	旧模型：1 租约 = 1 抓包会话 = 1 agent 进程 = 1 组端口。
//	       会话一停就杀 agent、收端口 → 手机代理端口每次都变 → 二维码失效、
//	       VPN 必须重连、用户每次抓包都要重新扫码配一遍。
//
//	新模型：agent 进程与抓包会话解耦。手机连的 CONNECT 端口在租约生命周期内
//	       恒定不变；抓包开关只切换「数据要不要上报给某个 mobile source」，
//	       不影响手机侧连接。
//
// 由此得到的四个性质：
//  1. 同一二维码 / 同一代理端口永久有效（端口只在租约创建时分配一次）；
//  2. VPN 不需要重连（进程不重启、监听不中断，start/stop 只改内存状态）；
//  3. 不抓包时零上报成本（idle 下连 payload 都不复制，直接透传给 socket）；
//  4. 新旧会话完全隔离（每次 start 递增 epoch，conn_id 带 epoch 前缀，
//     且每个 capture 独占一条 gRPC 流）。
//
// 控制接口是本地回环 HTTP（无鉴权）：信任边界是「能登录本机并访问回环端口的
// 进程」，与 gta-pipeline 的 -control-addr（默认全接口 :9888）相比更严格。
// 若将来需要跨机器控制，应在此加 token，而不是放宽监听地址。
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// captureState 是一次抓包的目标快照（创建后不再修改，供 Relay 无锁读取）。
type captureState struct {
	epoch      uint64           // 单调递增：每次 start +1，用于新旧会话隔离
	captureID  string           // 目标会话标识（pipeline 侧的 session_id）
	serverAddr string           // 目标 mobile source gRPC 地址
	sink       *PushClient      // 本次 capture 独占的推送连接（独立 gRPC 流）
	filter     connectionFilter // 本次 capture 的连接筛选（空=全部抓）
	startedAt  time.Time
}

// CaptureGate 维护 agent 当前的抓包状态机：idle ⇄ capturing。
//
// 所有 Relay goroutine 通过 Current() 无锁读当前状态；切换（start/stop）在
// mu 下串行执行。切换瞬间可能出现「连接已按旧 epoch open、新 epoch 已生效」，
// 由 relayConn 的 epoch 比对吸收（见 relayConn.begin）。
type CaptureGate struct {
	logger        *slog.Logger
	defaultFilter connectionFilter

	mu    sync.Mutex
	epoch uint64
	cur   atomic.Pointer[captureState]

	// 统计：反映「手机是否真的在走这个出口」，与抓包开关无关。
	// 有了它才能区分「代理没配好（relay_bytes 为 0）」与
	//「代理通了但没在抓包（relay_bytes 增长、captured_bytes 不动）」。
	activeConns   atomic.Int64
	totalConns    atomic.Uint64
	relayBytes    atomic.Uint64
	capturedBytes atomic.Uint64
	lastDataUnix  atomic.Int64 // unix 毫秒

	startedAt time.Time
}

// NewCaptureGate 创建一个 idle 状态的抓包闸门。
// cfg 内的 FilterHosts/FilterPorts 作为默认筛选：capture start 未指定筛选时使用。
func NewCaptureGate(cfg RelayConfig, logger *slog.Logger) *CaptureGate {
	if logger == nil {
		logger = slog.Default()
	}
	return &CaptureGate{
		logger:        logger,
		defaultFilter: compileFilter(cfg),
		startedAt:     time.Now(),
	}
}

// Current 返回当前抓包状态快照；idle 时返回 nil。
func (g *CaptureGate) Current() *captureState { return g.cur.Load() }

// State 返回 "capturing" 或 "idle"。
func (g *CaptureGate) State() string {
	if g.Current() == nil {
		return "idle"
	}
	return "capturing"
}

// Start 切换到 capturing：把数据上报给 serverAddr 指向的 mobile source。
//
// 语义：
//   - 每次调用递增 epoch，旧 capture 的 gRPC 流立即关闭（旧会话不再收到任何数据）；
//   - 幂等：同 capture_id + 同 server_addr 重复调用不改变状态；
//   - 手机侧已建立的连接不受影响（继续中继），连接在新 epoch 首次有数据时
//     由 relayConn 补发 ConnOpen，从而干净地进入新会话。
//
// hosts/ports 为空时沿用 NewCaptureGate 的默认筛选。
func (g *CaptureGate) Start(captureID, serverAddr string, hosts []string, ports []int) error {
	if strings.TrimSpace(serverAddr) == "" {
		return errors.New("capture start: server_addr is required")
	}
	if strings.TrimSpace(captureID) == "" {
		captureID = fmt.Sprintf("cap-%d", time.Now().UnixMilli())
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if cur := g.cur.Load(); cur != nil && cur.captureID == captureID && cur.serverAddr == serverAddr {
		return nil // 幂等重放
	}

	filter := g.defaultFilter
	if len(hosts) > 0 || len(ports) > 0 {
		filter = compileFilter(RelayConfig{FilterHosts: hosts, FilterPorts: ports})
	}

	sink, err := NewPushClient(serverAddr, g.logger)
	if err != nil {
		return fmt.Errorf("capture start: %w", err)
	}

	g.epoch++
	prev := g.cur.Swap(&captureState{
		epoch:      g.epoch,
		captureID:  captureID,
		serverAddr: serverAddr,
		sink:       sink,
		filter:     filter,
		startedAt:  time.Now(),
	})
	if prev != nil {
		// 旧流必须显式关闭：否则旧会话会继续收到切走之后的数据（串会话）。
		_ = prev.sink.Close()
	}
	g.logger.Info("capture started", "capture_id", captureID, "server", serverAddr,
		"epoch", g.epoch, "filter_hosts", len(hosts), "filter_ports", len(ports))
	return nil
}

// Stop 切回 idle：停止一切上报（open/data/close 都不再发送），但中继继续。
// 幂等。
func (g *CaptureGate) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	prev := g.cur.Swap(nil)
	if prev == nil {
		return nil
	}
	_ = prev.sink.Close()
	g.logger.Info("capture stopped", "capture_id", prev.captureID, "epoch", prev.epoch,
		"captured_bytes", g.capturedBytes.Load())
	return nil
}

// Close 释放闸门持有的连接（进程退出路径）。
func (g *CaptureGate) Close() error { return g.Stop() }

// Status 是 /v1/status 的响应体，也是 pipeline 侧探活的结构。
// 字段都是 agent 视角的客观事实，不含推断。
type Status struct {
	State          string `json:"state"` // "idle" | "capturing"
	CaptureID      string `json:"capture_id"`
	ServerAddr     string `json:"server_addr"`
	ListenAddr     string `json:"listen_addr"`
	ControlAddr    string `json:"control_addr"`
	Epoch          uint64 `json:"epoch"`
	ActiveConns    int64  `json:"active_conns"`
	TotalConns     uint64 `json:"total_conns"`
	RelayBytes     uint64 `json:"relay_bytes"`
	CapturedBytes  uint64 `json:"captured_bytes"`
	LastDataUnixMs int64  `json:"last_data_unix_ms"`
	UptimeSec      int64  `json:"uptime_sec"`
}

// Snapshot 组装当前状态快照（listenAddr/controlAddr 由 ControlServer 回填）。
func (g *CaptureGate) Snapshot(listenAddr, controlAddr string) Status {
	st := Status{
		State:          g.State(),
		ListenAddr:     listenAddr,
		ControlAddr:    controlAddr,
		ActiveConns:    g.activeConns.Load(),
		TotalConns:     g.totalConns.Load(),
		RelayBytes:     g.relayBytes.Load(),
		CapturedBytes:  g.capturedBytes.Load(),
		LastDataUnixMs: g.lastDataUnix.Load(),
		UptimeSec:      int64(time.Since(g.startedAt).Seconds()),
	}
	if cur := g.Current(); cur != nil {
		st.CaptureID = cur.captureID
		st.ServerAddr = cur.serverAddr
		st.Epoch = cur.epoch
	}
	return st
}

// ---- 控制接口 ----

// ControlServer 是 agent 的本地控制接口（HTTP，回环）。
//
//	GET  /v1/status          当前状态
//	POST /v1/capture/start   开始抓包（body: StartRequest）
//	POST /v1/capture/stop    停止抓包
type ControlServer struct {
	gate  *CaptureGate
	relay *Relay
	addr  string
	log   *slog.Logger
	srv   *http.Server
}

// StartRequest 是 POST /v1/capture/start 的请求体。
type StartRequest struct {
	CaptureID    string   `json:"capture_id"`
	ServerAddr   string   `json:"server_addr"`
	IncludeHosts []string `json:"include_hosts"`
	IncludePorts []int    `json:"include_ports"`
}

// NewControlServer 创建控制服务器（尚不监听）。addr 形如 "127.0.0.1:19500"。
func NewControlServer(addr string, gate *CaptureGate, relay *Relay, logger *slog.Logger) *ControlServer {
	if logger == nil {
		logger = slog.Default()
	}
	cs := &ControlServer{gate: gate, relay: relay, addr: addr, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", cs.handleStatus)
	mux.HandleFunc("/v1/capture/start", cs.handleStart)
	mux.HandleFunc("/v1/capture/stop", cs.handleStop)
	cs.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return cs
}

// Serve 监听并处理控制请求，直到 ctx 取消。
func (cs *ControlServer) Serve(ctx context.Context) error {
	lis, err := net.Listen("tcp", cs.addr)
	if err != nil {
		return fmt.Errorf("listen control %s: %w", cs.addr, err)
	}
	cs.addr = lis.Addr().String()
	cs.log.Info("agent control listening", "addr", cs.addr)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = cs.srv.Shutdown(shutdownCtx)
	}()
	if err := cs.srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Addr 返回实际监听地址（端口为 0 时在 Serve 之后有效）。
func (cs *ControlServer) Addr() string { return cs.addr }

func (cs *ControlServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": cs.status()})
}

func (cs *ControlServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := cs.gate.Start(req.CaptureID, req.ServerAddr, req.IncludeHosts, req.IncludePorts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cs.log.Info("capture start requested", "capture_id", req.CaptureID, "server", req.ServerAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": cs.status()})
}

func (cs *ControlServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := cs.gate.Stop(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cs.log.Info("capture stop requested")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": cs.status()})
}

func (cs *ControlServer) status() Status {
	var lisAddr string
	if cs.relay != nil {
		if a := cs.relay.Addr(); a != nil {
			lisAddr = a.String()
		}
	}
	return cs.gate.Snapshot(lisAddr, cs.addr)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- 控制接口客户端（pipeline 侧使用）----

// ControlClient 调用 agent 的本地控制接口。
type ControlClient struct {
	baseURL string
	cli     *http.Client
}

// NewControlClient 创建一个 agent 控制客户端。controlAddr 形如 "127.0.0.1:19500"。
func NewControlClient(controlAddr string) *ControlClient {
	addr := controlAddr
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &ControlClient{
		baseURL: strings.TrimRight(addr, "/"),
		cli:     &http.Client{Timeout: 5 * time.Second},
	}
}

// StartCapture 让 agent 开始抓包并把数据推给 serverAddr。
func (c *ControlClient) StartCapture(ctx context.Context, req StartRequest) error {
	return c.post(ctx, "/v1/capture/start", req)
}

// StopCapture 让 agent 停止抓包（中继继续）。
func (c *ControlClient) StopCapture(ctx context.Context) error {
	return c.post(ctx, "/v1/capture/stop", nil)
}

// Status 查询 agent 当前状态。
func (c *ControlClient) Status(ctx context.Context) (Status, error) {
	var out struct {
		OK     bool   `json:"ok"`
		Status Status `json:"status"`
	}
	if err := c.get(ctx, "/v1/status", &out); err != nil {
		return Status{}, err
	}
	return out.Status, nil
}

func (c *ControlClient) post(ctx context.Context, path string, body any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cli.Do(req)
	if err != nil {
		return fmt.Errorf("agent control %s: %w", path, err)
	}
	defer resp.Body.Close()
	return decodeErr(resp)
}

func (c *ControlClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		return fmt.Errorf("agent control %s: %w", path, err)
	}
	defer resp.Body.Close()
	if err := decodeErr(resp); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// decodeErr 把非 2xx 响应转成 error（携带服务端 message/error 字段）。
func decodeErr(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	msg := body.Error
	if msg == "" {
		msg = body.Message
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("agent control %s: %s", resp.Request.URL.Path, msg)
}
