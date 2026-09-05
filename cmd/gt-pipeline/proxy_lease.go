// proxy_lease.go — 常驻代理出口 + 可切换抓包会话（按用户/设备隔离）。
//
// 架构：
//
//	手机 ── HTTP CONNECT（端口恒定）──▶ gt-singbox-agent（常驻进程）
//	                                        │ 本地控制接口（195xx）
//	                                        ▼
//	                              pipeline 切 start/stop
//	                                        │ gRPC
//	                                        ▼
//	                              mobile Source（每次抓包一个新会话/端口）
//
// 租约（出口）与抓包会话的生命周期彻底解耦：
//   - 租约 = 常驻 agent 进程 + 固定的手机 CONNECT 端口 + 固定的控制端口。
//     创建后端口不再变化，同一二维码在整个租约内长期有效，VPN 无需重连；
//   - 抓包会话 = 一次 StartLeaseCapture/StopLeaseCapture，每次都是全新的
//     session_id / 独立 SQLite / 独立 mobile source，新旧会话完全隔离；
//   - 不抓包时 agent 处于 idle：手机流量照常中继，但零上报、零落盘。
//
// 端口分配：agent CONNECT 12100-12199，mobile source gRPC 19100-19199，
// agent 控制接口 19500-19599。agent 端口支持 sticky —— 同一 (owner, device)
// 重新创建租约时复用上次端口，使此前扫过的二维码继续有效。
//
// 锁序规约：leaseMu 先于 s.mu（tasks）；StartSession/StopSession 一律在
// leaseMu 之外调用（StopSession 经 finalizeTask 反向获取 leaseMu，持锁调用必死锁）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gametrace/pkg/agent"
	"gametrace/pkg/auth"
	"gametrace/pkg/capture"
	"gametrace/pkg/capture/mobile"
	"gametrace/pkg/internalipc"
	"gametrace/pkg/internalipc/capturecontrol"
)

// 租约端口段：agent HTTP CONNECT 监听 12100-12199（0.0.0.0，手机可达）；
// mobile Source gRPC 监听 19100-19199（127.0.0.1，本机回环，agent → source）；
// agent 控制接口 19500-19599（127.0.0.1，pipeline → agent）。
const (
	proxyLeaseAgentPortBase = 12100
	proxyLeaseAgentPortMax  = 12199
	proxyLeaseGRPCPortBase  = 19100
	proxyLeaseGRPCPortMax   = 19199
	proxyLeaseCtrlPortBase  = 19500
	proxyLeaseCtrlPortMax   = 19599

	maxLeasesPerOwner  = 5 // 每用户（owner）并发租约上限
	mobileReadyTimeout = 3 * time.Second
	agentStartupGrace  = 800 * time.Millisecond
	// controlReadyTimeout 是等待 agent 控制接口可拨通的上限。控制接口是
	// pipeline 切换抓包的唯一通道，租约创建必须等它真正就绪才能返回。
	controlReadyTimeout = 5 * time.Second
)

// proxyAgentSpawner 拉起一个租约专属 gt-singbox-agent（测试可注入 fake）。
//
// agent 以 idle 启动（不传 --server）：抓包目标完全由控制接口下发，
// 因此这里不需要 serverAddr / 筛选参数。
type proxyAgentSpawner func(workDir, bin, ctrlAddr, listenAddr string) (*agentProcess, error)

// agentProcess 管理租约专属的 gt-singbox-agent 子进程生命周期：
// 创建租约时拉起，租约回收/进程退出时终止（stop 幂等）。
type agentProcess struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// running 返回子进程是否存活（未启动/已退出返回 false）。
func (ap *agentProcess) running() bool {
	if ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.cmd != nil && ap.cmd.Process != nil && ap.cmd.ProcessState == nil
}

// pid 返回子进程 PID（未启动返回 0）。
func (ap *agentProcess) pid() int {
	if ap == nil {
		return 0
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.cmd != nil && ap.cmd.Process != nil {
		return ap.cmd.Process.Pid
	}
	return 0
}

// stop 终止子进程（幂等）。
func (ap *agentProcess) stop() {
	if ap == nil {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.cmd != nil && ap.cmd.Process != nil {
		_ = ap.cmd.Process.Kill()
	}
}

// spawnSingboxAgentLease 以常驻 idle 模式拉起租约专属 gt-singbox-agent。
// ctrlAddr 是 agent 的本地控制接口监听地址（pipeline 经它切 start/stop）；
// listenAddr 是 agent 对手机暴露的 HTTP CONNECT 监听地址。
// 二进制缺失或启动失败返回 error（租约模式必须有 agent，不再静默降级）。
func spawnSingboxAgentLease(workDir, bin, ctrlAddr, listenAddr string) (*agentProcess, error) {
	if strings.TrimSpace(bin) == "" {
		exe := "gt-singbox-agent"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		bin = filepath.Join(workDir, "bin", exe)
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("gt-singbox-agent binary not found at %s (install via `make build-agent`): %w", bin, err)
	}
	cmd := exec.Command(bin, "--listen", listenAddr, "--control", ctrlAddr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn gt-singbox-agent: %w", err)
	}
	ap := &agentProcess{cmd: cmd, done: make(chan struct{})}
	slog.Info("singbox agent spawned for proxy lease",
		"bin", bin, "listen", listenAddr, "control", ctrlAddr, "pid", cmd.Process.Pid)
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Warn("singbox agent exited", "pid", cmd.Process.Pid, "error", err)
		}
		close(ap.done)
	}()
	return ap, nil
}

// portRange 是一段端口的轮转分配器（自带锁，可独立于 leaseMu 使用）。
type portRange struct {
	base   int
	max    int
	mu     sync.Mutex
	used   map[int]struct{}
	cursor int // 轮转游标：上一次分配位置的下一个
}

func newPortRange(base, max int) *portRange {
	return &portRange{base: base, max: max, used: make(map[int]struct{})}
}

// allocate 从段内轮转取一个空闲端口并标记占用；段耗尽时报错。
func (pr *portRange) allocate() (int, error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	n := pr.max - pr.base + 1
	for i := 0; i < n; i++ {
		port := pr.base + (pr.cursor+i)%n
		if _, ok := pr.used[port]; !ok {
			pr.used[port] = struct{}{}
			pr.cursor = (pr.cursor + i + 1) % n
			return port, nil
		}
	}
	return 0, fmt.Errorf("port range %d-%d exhausted", pr.base, pr.max)
}

// reserve 占用指定端口（已占用或越界返回 false）。
// 用于 sticky 端口复用：同一设备重新创建租约时拿回上次那个端口，
// 让手机此前扫过的二维码继续有效。
func (pr *portRange) reserve(port int) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if port < pr.base || port > pr.max {
		return false
	}
	if _, ok := pr.used[port]; ok {
		return false
	}
	pr.used[port] = struct{}{}
	return true
}

// release 归还端口（幂等，未占用时无副作用）。
func (pr *portRange) release(port int) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	delete(pr.used, port)
}

// proxyLease 是一个常驻代理出口的全部状态。
// 与单次抓包会话解耦：sessionID 为空表示当前 idle（未抓包）。
type proxyLease struct {
	leaseID      string // 租约 id（稳定，跨多次抓包会话不变）
	owner        string
	projectID    string
	plugin       string
	pluginOwners []string // 允许解析解码插件的额外 owner 集合（项目成员共用项目插件）
	includeHosts []string
	includePorts []int
	device       string

	agentPort int // agent HTTP CONNECT 监听端口（12100-12199，租约内恒定）
	ctrlPort  int // agent 本地控制接口端口（19500-19599）
	grpcPort  int // 当前抓包会话的 mobile Source gRPC 端口（0 = 未抓包）
	sticky    bool

	// 以下为「当前抓包会话」状态；sessionID == "" 表示 idle。
	sessionID     string
	activity      *mobile.Activity
	lastCaptureAt time.Time
	captureCount  int // 本租约累计开始过多少次抓包

	agent     *agentProcess
	ctrl      *agent.ControlClient // 控制接口客户端（地址固定，随租约创建）
	createdAt time.Time
}

// stickyKey 是 sticky 端口复用的键：同一属主的同一台设备复用同一端口。
func stickyKey(owner, device string) string { return owner + "|" + device }

// probeFreePortFn 是预探测端口可 bind 的钩子（缓解 TOCTOU：分配与实际监听之间的窗口）。
// 默认实现在 127.0.0.1 上短暂 Listen 后立即关闭。测试可替换为 fake，
// 避免测试进程与开发机常驻服务（如正在跑的 gt-singbox-agent）争夺 GameTrace 专属端口。
var probeFreePortFn = defaultProbeFreePort

func defaultProbeFreePort(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return ln.Close()
}

// newLeaseID 生成租约 ID（时间戳 + 随机数，避免同毫秒内碰撞）。
// 与会话 ID 不同前缀，便于日志/UI 区分「租约」与「抓包会话」两种实体。
func newLeaseID() string {
	return fmt.Sprintf("lease_%s", nowSessionID())
}

// CreateProxyLease 创建一个常驻代理出口：分配 agent 端口 + 控制端口 →
// 拉起常驻 agent（idle）→ 等控制接口就绪 → 写入租约表。
// 默认创建后立刻开始抓包（保持既有前端行为）；NoAutoStart 时只建出口。
func (s *pipelineService) CreateProxyLease(ctx context.Context, req capturecontrol.CreateProxyLeaseRequest) (capturecontrol.ProxyLease, error) {
	owner := auth.OwnerFrom(ctx)

	// 每用户上限预检（插入时在 leaseMu 下复核，防并发超限）。
	s.leaseMu.Lock()
	over := s.countLeasesLocked(owner) >= maxLeasesPerOwner
	s.leaseMu.Unlock()
	if over {
		return capturecontrol.ProxyLease{}, fmt.Errorf("owner %q already has %d active proxy leases (limit %d); release one first",
			owner, maxLeasesPerOwner, maxLeasesPerOwner)
	}

	includePorts := toIntList(req.IncludePorts)

	// 1. leaseMu 下分配端口：agent 端口优先复用 sticky 记忆（二维码长期有效）。
	s.leaseMu.Lock()
	agentPort, sticky := s.allocateAgentPortLocked(owner, req.Device, req.NoSticky)
	ctrlPort, err := s.ctrlPorts.allocate()
	if err != nil {
		s.agentPorts.release(agentPort)
		s.leaseMu.Unlock()
		return capturecontrol.ProxyLease{}, fmt.Errorf("allocate agent control port: %w", err)
	}
	s.leaseMu.Unlock()

	releasePorts := func() {
		s.agentPorts.release(agentPort)
		s.ctrlPorts.release(ctrlPort)
	}

	// 2. 预探测端口可 bind。
	if err := probeFreePortFn(agentPort); err != nil {
		releasePorts()
		return capturecontrol.ProxyLease{}, fmt.Errorf("agent port %d not bindable: %w", agentPort, err)
	}
	if err := probeFreePortFn(ctrlPort); err != nil {
		releasePorts()
		return capturecontrol.ProxyLease{}, fmt.Errorf("agent control port %d not bindable: %w", ctrlPort, err)
	}

	// 3. 拉起常驻 agent（idle：不抓包，等控制指令）。
	listenAddr := fmt.Sprintf("0.0.0.0:%d", agentPort)
	ctrlAddr := fmt.Sprintf("127.0.0.1:%d", ctrlPort)
	ap, err := s.agentSpawner(s.workDir, s.agentBin, ctrlAddr, listenAddr)
	if err != nil {
		// 二进制缺失/启动失败：换端口无意义，直接报错。
		releasePorts()
		return capturecontrol.ProxyLease{}, err
	}
	time.Sleep(agentStartupGrace)
	if !ap.running() {
		ap.stop()
		releasePorts()
		return capturecontrol.ProxyLease{}, fmt.Errorf("gt-singbox-agent exited immediately (listen %s, control %s)", listenAddr, ctrlAddr)
	}

	// 4. 等控制接口真正可拨通——它是后续切换抓包的唯一通道。
	if err := waitAgentControlReady(ctrlAddr, controlReadyTimeout); err != nil {
		ap.stop()
		releasePorts()
		return capturecontrol.ProxyLease{}, err
	}

	lease := &proxyLease{
		leaseID:      newLeaseID(),
		owner:        owner,
		projectID:    req.ProjectID,
		plugin:       req.Plugin,
		pluginOwners: req.PluginOwners,
		includeHosts: req.IncludeHosts,
		includePorts: includePorts,
		device:       req.Device,
		agentPort:    agentPort,
		ctrlPort:     ctrlPort,
		sticky:       sticky,
		agent:        ap,
		ctrl:         agent.NewControlClient(ctrlAddr),
		createdAt:    time.Now(),
	}

	// 5. leaseMu 下复核每用户上限并写入租约表。
	s.leaseMu.Lock()
	if s.countLeasesLocked(owner) >= maxLeasesPerOwner {
		s.leaseMu.Unlock()
		ap.stop()
		releasePorts()
		return capturecontrol.ProxyLease{}, fmt.Errorf("owner %q already has %d active proxy leases (limit %d); release one first",
			owner, maxLeasesPerOwner, maxLeasesPerOwner)
	}
	s.leases[lease.leaseID] = lease
	// 端口记忆：同一 (owner, device) 释放后再建拿到同一个端口（粘不粘都记）。
	// 这是 sticky 的核心数据——后续 allocateAgentPortLocked 据此复用。
	if req.Device != "" {
		s.stickyPorts[stickyKey(owner, req.Device)] = agentPort
	}
	s.leaseMu.Unlock()

	s.logger.Info("proxy lease created (stable egress)",
		"lease_id", lease.leaseID, "owner", owner, "device", req.Device,
		"agent_listen", listenAddr, "control", ctrlAddr, "sticky", sticky, "plugin", req.Plugin)

	if req.NoAutoStart {
		return s.buildLeaseView(lease), nil
	}
	res, err := s.StartLeaseCapture(ctx, capturecontrol.StartLeaseCaptureRequest{LeaseID: lease.leaseID})
	if err != nil {
		// 出口已建好但自动抓包失败：保留出口并如实上报，让用户能看到端口、
		// 自己决定重试还是释放（此时释放端口等于让刚扫的二维码作废）。
		s.logger.Error("auto start capture failed after lease created",
			"lease_id", lease.leaseID, "error", err)
		return s.buildLeaseView(lease), fmt.Errorf("lease %s created but capture failed to start: %w", lease.leaseID, err)
	}
	return res.Lease, nil
}

// allocateAgentPortLocked 分配 agent CONNECT 端口（需持 leaseMu）。
// 非 NoSticky 时优先拿回该 (owner, device) 上次用过的端口，使二维码长期有效。
func (s *pipelineService) allocateAgentPortLocked(owner, device string, noSticky bool) (int, bool) {
	if !noSticky {
		if p, ok := s.stickyPorts[stickyKey(owner, device)]; ok && s.agentPorts.reserve(p) {
			return p, true
		}
	}
	p, err := s.agentPorts.allocate()
	if err != nil {
		return 0, false
	}
	return p, false
}

// waitAgentControlReady 轮询拨号 agent 控制接口直至 /v1/status 可应答。
func waitAgentControlReady(ctrlAddr string, timeout time.Duration) error {
	cli := agent.NewControlClient(ctrlAddr)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := cli.Status(context.Background()); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("agent control %s not ready within %s: %v", ctrlAddr, timeout, lastErr)
}

// StartLeaseCapture 在常驻租约上开一次全新的抓包会话：
// 分配 mobile gRPC 端口 → 启动独立 mobile 会话 → 就绪验证 → 通过控制接口
// 让 agent 把数据推到该会话。代理出口与手机连接全程不受影响。
func (s *pipelineService) StartLeaseCapture(ctx context.Context, req capturecontrol.StartLeaseCaptureRequest) (capturecontrol.StartLeaseCaptureResult, error) {
	lease, err := s.getLeaseForOwner(ctx, req.LeaseID)
	if err != nil {
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error()}, err
	}

	// 抓包参数：显式给出则本次覆盖，否则沿用租约配置。
	plugin := req.Plugin
	if plugin == "" {
		plugin = lease.plugin
	}
	// 插件解析 owner 候选：本次请求显式给出优先，否则沿用租约创建时的集合。
	pluginOwners := req.PluginOwners
	if len(pluginOwners) == 0 {
		pluginOwners = lease.pluginOwners
	}
	hosts := req.IncludeHosts
	if len(hosts) == 0 {
		hosts = lease.includeHosts
	}
	ports := toIntList(req.IncludePorts)
	if len(ports) == 0 {
		ports = lease.includePorts
	}

	s.leaseMu.Lock()
	if lease.sessionID != "" {
		s.leaseMu.Unlock()
		err := fmt.Errorf("lease %s is already capturing (session %s); stop it first", lease.leaseID, lease.sessionID)
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error()}, err
	}
	if !lease.agent.running() {
		s.leaseMu.Unlock()
		err := fmt.Errorf("lease %s agent is not running; release the lease and create a new one", lease.leaseID)
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error()}, err
	}
	grpcPort, perr := s.grpcPorts.allocate()
	if perr != nil {
		s.leaseMu.Unlock()
		return capturecontrol.StartLeaseCaptureResult{Message: perr.Error()}, perr
	}
	s.leaseMu.Unlock()

	rollback := func() { s.grpcPorts.release(grpcPort) }

	if err := probeFreePortFn(grpcPort); err != nil {
		rollback()
		err = fmt.Errorf("mobile gRPC port %d not bindable: %w", grpcPort, err)
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error()}, err
	}

	// 会话是临时的：每次抓包一个独立的 mobile source 与 SQLite，
	// 这是「新旧会话完全隔离」的基础（数据不可能跨会话串流）。
	activity := mobile.NewActivity()
	res, err := s.StartSession(ctx, capturecontrol.StartSessionRequest{
		Plugin:       plugin,
		ProjectID:    lease.projectID,
		PluginOwners: req.PluginOwners,
		Mobile: &capturecontrol.MobileConfig{
			ListenAddr: fmt.Sprintf("127.0.0.1:%d", grpcPort),
			Activity:   activity,
		},
	})
	if err != nil {
		rollback()
		err = fmt.Errorf("start mobile session: %w", err)
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error(), Lease: s.buildLeaseView(lease)}, err
	}
	sessionID := res.SessionID

	if err := s.waitMobileReady(sessionID, grpcPort, mobileReadyTimeout); err != nil {
		s.stopSessionQuiet(sessionID)
		rollback()
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error(), Lease: s.buildLeaseView(lease)}, err
	}

	// 让常驻 agent 切换到本次会话：它不会重启，手机连接不受影响。
	ctrlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.ctrl.StartCapture(ctrlCtx, agent.StartRequest{
		CaptureID:    sessionID,
		ServerAddr:   fmt.Sprintf("127.0.0.1:%d", grpcPort),
		IncludeHosts: hosts,
		IncludePorts: ports,
	}); err != nil {
		s.stopSessionQuiet(sessionID)
		rollback()
		err = fmt.Errorf("tell agent to start capture: %w", err)
		return capturecontrol.StartLeaseCaptureResult{Message: err.Error(), Lease: s.buildLeaseView(lease)}, err
	}

	s.leaseMu.Lock()
	lease.sessionID = sessionID
	lease.activity = activity
	lease.grpcPort = grpcPort
	lease.captureCount++
	lease.lastCaptureAt = time.Now()
	s.leaseMu.Unlock()

	s.logger.Info("lease capture started",
		"lease_id", lease.leaseID, "session_id", sessionID, "owner", lease.owner,
		"device", lease.device, "agent_listen", lease.agentPort, "mobile_grpc", grpcPort,
		"plugin", plugin, "capture_count", lease.captureCount)

	return capturecontrol.StartLeaseCaptureResult{
		OK:        true,
		Message:   "capture started",
		SessionID: sessionID,
		Lease:     s.buildLeaseView(lease),
	}, nil
}

// StopLeaseCapture 停止租约当前的抓包会话并回到 idle。
// 先让 agent 停止上报（此后零落盘、零上报），再停会话；出口与手机连接保留，
// 之后可以再次 StartLeaseCapture 开新会话。
func (s *pipelineService) StopLeaseCapture(ctx context.Context, leaseID string) (capturecontrol.StopLeaseCaptureResult, error) {
	lease, err := s.getLeaseForOwner(ctx, leaseID)
	if err != nil {
		return capturecontrol.StopLeaseCaptureResult{Message: err.Error()}, err
	}

	s.leaseMu.Lock()
	sessionID := lease.sessionID
	s.leaseMu.Unlock()
	if sessionID == "" {
		err := fmt.Errorf("lease %s is not capturing", leaseID)
		return capturecontrol.StopLeaseCaptureResult{Message: err.Error()}, err
	}

	// 先掐断数据源：agent 停止上报后，会话收尾期间不会有新数据涌进来。
	ctrlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.ctrl.StopCapture(ctrlCtx); err != nil {
		// 控制失败也要继续停会话：否则数据会一直往一个即将关闭的会话里灌。
		s.logger.Warn("tell agent to stop capture failed, stopping session anyway",
			"lease_id", leaseID, "error", err)
	}

	stats, serr := s.StopSession(ctx, sessionID)
	if serr != nil && !errors.Is(serr, internalipc.ErrNoActiveCapture) {
		return capturecontrol.StopLeaseCaptureResult{Message: serr.Error(), SessionID: sessionID}, serr
	}
	// 正常路径下 finalizeTask → clearLeaseCapture 已清空会话状态；
	// 这里兜底再清一次（幂等），覆盖 finalize 延迟场景。
	s.clearLeaseCapture(sessionID)

	s.logger.Info("lease capture stopped",
		"lease_id", leaseID, "session_id", sessionID,
		"raw", stats.RawPackets, "events", stats.Events, "duration_sec", stats.DurationSec)
	return capturecontrol.StopLeaseCaptureResult{
		OK:          true,
		Message:     "capture stopped; egress kept alive",
		SessionID:   sessionID,
		RawPackets:  stats.RawPackets,
		Events:      stats.Events,
		DurationSec: stats.DurationSec,
	}, nil
}

// countLeasesLocked 统计 owner 的活跃租约数（需持 leaseMu）。
func (s *pipelineService) countLeasesLocked(owner string) int {
	n := 0
	for _, l := range s.leases {
		if l.owner == owner {
			n++
		}
	}
	return n
}

// waitMobileReady 轮询拨号 mobile Source 的 gRPC 端口直至就绪；
// 期间 task 消失或非 running 状态立即报错。
func (s *pipelineService) waitMobileReady(sessionID string, grpcPort int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", grpcPort)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, ok := s.getTask(sessionID)
		if !ok {
			return fmt.Errorf("mobile session %s task not found during startup", sessionID)
		}
		if task.State() != capture.StateRunning {
			return fmt.Errorf("mobile session %s exited during startup (state %s)", sessionID, task.State())
		}
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("mobile source gRPC port %d not ready within %s", grpcPort, timeout)
}

// stopSessionQuiet 尽力停止会话（失败仅告警）：用于创建/回滚路径。
func (s *pipelineService) stopSessionQuiet(sessionID string) {
	if _, err := s.StopSession(context.Background(), sessionID); err != nil && !errors.Is(err, internalipc.ErrNoActiveCapture) {
		s.logger.Warn("stop session during lease rollback", "session_id", sessionID, "error", err)
	}
}

// ListProxyLeases 列出调用方可见的租约。owner 作用域过滤（语义同 ListPlugins）：
// 非 admin 只见自己的 + 匿名（系统）租约；admin 全可见；匿名只见匿名。
func (s *pipelineService) ListProxyLeases(ctx context.Context) ([]capturecontrol.ProxyLease, error) {
	owner := auth.OwnerFrom(ctx)
	allOwners := false
	if p, ok := auth.PrincipalFrom(ctx); ok {
		allOwners = p.IsAdmin
	}
	s.leaseMu.Lock()
	leases := make([]*proxyLease, 0, len(s.leases))
	for _, l := range s.leases {
		leases = append(leases, l)
	}
	s.leaseMu.Unlock()

	out := make([]capturecontrol.ProxyLease, 0, len(leases))
	for _, l := range leases {
		if !allOwners {
			if owner == "" {
				if l.owner != "" {
					continue // 匿名调用方只见匿名租约
				}
			} else if l.owner != "" && l.owner != owner {
				continue // 其他 owner 的租约不可见
			}
		}
		out = append(out, s.buildLeaseView(l))
	}
	return out, nil
}

// getLeaseForOwner 按 leaseID 查租约并做 owner 校验：
// 不存在、或属于他人且调用方非 admin，统一按 not found 处理（不泄露存在性）。
func (s *pipelineService) getLeaseForOwner(ctx context.Context, leaseID string) (*proxyLease, error) {
	s.leaseMu.Lock()
	lease, ok := s.leases[leaseID]
	s.leaseMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("proxy lease %q not found", leaseID)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && p.IsAdmin {
		return lease, nil
	}
	if lease.owner != auth.OwnerFrom(ctx) {
		return nil, fmt.Errorf("proxy lease %q not found", leaseID)
	}
	return lease, nil
}

// GetProxyLease 查询单个租约状态快照（owner 校验，不匹配按不存在处理）。
func (s *pipelineService) GetProxyLease(ctx context.Context, leaseID string) (capturecontrol.ProxyLease, error) {
	lease, err := s.getLeaseForOwner(ctx, leaseID)
	if err != nil {
		return capturecontrol.ProxyLease{}, err
	}
	return s.buildLeaseView(lease), nil
}

// ReleaseProxyLease 释放租约：停止当前抓包 → 杀 agent → 回收全部端口。
// 释放后二维码立即失效（端口归还端口池）。幂等。
// 注意：StopSession 必须在 leaseMu 之外调用（锁序规约）。
func (s *pipelineService) ReleaseProxyLease(ctx context.Context, leaseID string) (capturecontrol.ReleaseProxyLeaseResult, error) {
	lease, err := s.getLeaseForOwner(ctx, leaseID)
	if err != nil {
		return capturecontrol.ReleaseProxyLeaseResult{OK: false, Message: err.Error()}, err
	}

	var stoppedSession string
	s.leaseMu.Lock()
	stoppedSession = lease.sessionID
	s.leaseMu.Unlock()

	if stoppedSession != "" {
		// 先停上报，再停会话（同 StopLeaseCapture）。
		ctrlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if cerr := lease.ctrl.StopCapture(ctrlCtx); cerr != nil {
			s.logger.Warn("stop capture during lease release", "lease_id", leaseID, "error", cerr)
		}
		cancel()
		if _, serr := s.StopSession(ctx, stoppedSession); serr != nil && !errors.Is(serr, internalipc.ErrNoActiveCapture) {
			return capturecontrol.ReleaseProxyLeaseResult{
				OK: false, Message: serr.Error(), SessionID: stoppedSession,
			}, serr
		}
	}

	// 兜底回收（幂等）：覆盖会话已提前终止、finalize 尚未跑完等路径。
	s.reclaimLease(leaseID)
	s.logger.Info("proxy lease released", "lease_id", leaseID, "session_id", stoppedSession)
	return capturecontrol.ReleaseProxyLeaseResult{
		OK:        true,
		Message:   "lease released",
		SessionID: stoppedSession,
	}, nil
}

// clearLeaseCapture 清空某会话在租约上的抓包状态（幂等）。
// 由 finalizeTask 调用（会话因任何原因终止时），只回收「本次抓包」占用的
// mobile gRPC 端口，租约本身与 agent 端口、控制端口一律保留。
func (s *pipelineService) clearLeaseCapture(sessionID string) {
	s.leaseMu.Lock()
	var lease *proxyLease
	for _, l := range s.leases {
		if l.sessionID == sessionID {
			lease = l
			break
		}
	}
	if lease == nil {
		s.leaseMu.Unlock()
		return
	}
	grpcPort := lease.grpcPort
	lease.sessionID = ""
	lease.activity = nil
	lease.grpcPort = 0
	lease.lastCaptureAt = time.Now()
	s.leaseMu.Unlock()

	s.grpcPorts.release(grpcPort)
	s.logger.Info("lease capture cleared (session ended, egress kept)",
		"lease_id", lease.leaseID, "session_id", sessionID, "mobile_grpc_port", grpcPort)
}

// reclaimLease 是租约的唯一回收点（幂等）：从租约表移除、终止 agent、
// 回收全部端口（agent / 控制 / mobile gRPC）。sticky 记忆保留，
// 使同一设备下次创建租约仍能拿到同一个端口。
func (s *pipelineService) reclaimLease(leaseID string) {
	s.leaseMu.Lock()
	lease, ok := s.leases[leaseID]
	if ok {
		delete(s.leases, leaseID)
	}
	s.leaseMu.Unlock()
	if !ok {
		return
	}
	lease.agent.stop()
	s.agentPorts.release(lease.agentPort)
	s.ctrlPorts.release(lease.ctrlPort)
	if lease.grpcPort > 0 {
		s.grpcPorts.release(lease.grpcPort)
	}
	s.logger.Info("proxy lease reclaimed",
		"lease_id", leaseID, "owner", lease.owner, "device", lease.device,
		"agent_port", lease.agentPort, "control_port", lease.ctrlPort, "mobile_grpc_port", lease.grpcPort)
}

// CleanupProxyLeases 关停兜底清扫：pipeline 退出时（StopAll 之后）调用，
// 回收所有仍在表中的租约（正常情况已由各自 release/reclaim 处理）。
func (s *pipelineService) CleanupProxyLeases() {
	s.leaseMu.Lock()
	leases := make([]*proxyLease, 0, len(s.leases))
	for _, l := range s.leases {
		leases = append(leases, l)
	}
	s.leases = make(map[string]*proxyLease)
	s.leaseMu.Unlock()
	for _, l := range leases {
		l.agent.stop()
		s.agentPorts.release(l.agentPort)
		s.ctrlPorts.release(l.ctrlPort)
		if l.grpcPort > 0 {
			s.grpcPorts.release(l.grpcPort)
		}
	}
	if len(leases) > 0 {
		s.logger.Info("proxy leases cleaned up on shutdown", "count", len(leases))
	}
}

// buildLeaseView 组装租约的配置 + 运行时状态快照：
// activity.Snapshot()（无锁原子）+ agent 存活/PID + 当前抓包会话状态。
// 未抓包时连接/字节数为零值（idle 下没有会话，也就没有会话级计数）。
func (s *pipelineService) buildLeaseView(l *proxyLease) capturecontrol.ProxyLease {
	s.leaseMu.Lock()
	sessionID := l.sessionID
	grpcPort := l.grpcPort
	activity := l.activity
	captureCount := l.captureCount
	lastCaptureAt := l.lastCaptureAt
	s.leaseMu.Unlock()

	view := capturecontrol.ProxyLease{
		LeaseID:         l.leaseID,
		Owner:           l.owner,
		ProjectID:       l.projectID,
		Plugin:          l.plugin,
		IncludeHosts:    l.includeHosts,
		IncludePorts:    l.includePorts,
		Device:          l.device,
		ListenAddr:      fmt.Sprintf("0.0.0.0:%d", l.agentPort),
		AgentListenPort: l.agentPort,
		MobileGRPCPort:  grpcPort,
		AgentRunning:    l.agent.running(),
		AgentPID:        int32(l.agent.pid()),
		SessionID:       sessionID,
		CreatedAt:       l.createdAt,
		ControlPort:     l.ctrlPort,
		CaptureCount:    captureCount,
		StickyPort:      l.sticky,
	}
	if !lastCaptureAt.IsZero() {
		view.LastCaptureAtUnix = lastCaptureAt.Unix()
	}
	if sessionID != "" {
		if task, ok := s.getTask(sessionID); ok {
			view.SessionRunning = task.State() == capture.StateRunning
		}
		view.CaptureRunning = view.SessionRunning
	}
	if activity != nil {
		act := activity.Snapshot()
		view.ActiveConns = act.ActiveConns
		view.TotalConns = act.TotalConns
		view.LastDataUnix = act.LastDataUnix
		view.TotalBytes = act.TotalBytes
	}
	return view
}

// toIntList 将 []int32 转换为 []int（nil 保持 nil）。
func toIntList(in []int32) []int {
	if in == nil {
		return nil
	}
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}
