// proxy_lease.go — 代理抓包租约管理（按用户/设备独立会话）。
//
// 架构：手机代理软件 ── HTTP CONNECT ──▶ gta-singbox-agent（每租约独立端口）
//        ── gRPC ──▶ mobile Source（每租约独立抓包会话/端口）
//
// 每个租约 = 独立 mobile 抓包会话 + 独立 gta-singbox-agent 进程 + 私有
// plugin/筛选配置 + owner/project 归属。lease_id 与 session_id 一致（1:1 生命周期），
// 会话结束（finalizeTask）时经 reclaimLeaseForSession 自动回收：杀 agent、释放端口、
// 移出租约表。无自动过期，仅手动释放（ReleaseProxyLease）。
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
	"strconv"
	"strings"
	"sync"
	"time"

	"gta/pkg/auth"
	"gta/pkg/capture"
	"gta/pkg/capture/mobile"
	"gta/pkg/internalipc"
	"gta/pkg/internalipc/capturecontrol"
)

// 租约端口段：agent HTTP CONNECT 监听 12100-12199（0.0.0.0，手机可达）；
// mobile Source gRPC 监听 19100-19199（127.0.0.1，本机回环，agent → source）。
const (
	proxyLeaseAgentPortBase = 12100
	proxyLeaseAgentPortMax  = 12199
	proxyLeaseGRPCPortBase  = 19100
	proxyLeaseGRPCPortMax   = 19199

	maxLeasesPerOwner       = 5  // 每用户（owner）并发租约上限
	proxyLeaseCreateRetries = 3  // 端口冲突/会话启动失败时换端口重试次数
	mobileReadyTimeout      = 3 * time.Second
	agentStartupGrace       = 800 * time.Millisecond
)

// proxyAgentSpawner 拉起一个租约专属 gta-singbox-agent（测试可注入 fake）。
type proxyAgentSpawner func(workDir, bin, serverAddr, listenAddr string, hosts []string, ports []int) (*agentProcess, error)

// agentProcess 管理租约专属的 gta-singbox-agent 子进程生命周期：
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

// spawnSingboxAgentLease 以纯 HTTP CONNECT 代理模式拉起租约专属 gta-singbox-agent。
// serverAddr 是本租约 mobile Source 的 gRPC 监听地址；listenAddr 是 agent 对手机
// 暴露的 HTTP CONNECT 监听地址；hosts/ports 非空时透传 --filter-hosts/--filter-ports。
// 二进制缺失或启动失败返回 error（租约模式必须有 agent，不再静默降级）。
func spawnSingboxAgentLease(workDir, bin, serverAddr, listenAddr string, hosts []string, ports []int) (*agentProcess, error) {
	if strings.TrimSpace(bin) == "" {
		exe := "gta-singbox-agent"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		bin = filepath.Join(workDir, "bin", exe)
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("gta-singbox-agent binary not found at %s (install via `make build-agent`): %w", bin, err)
	}
	args := []string{"--server", serverAddr, "--listen", listenAddr}
	if hostList := filterHostList(hosts); hostList != "" {
		args = append(args, "--filter-hosts", hostList)
	}
	if portList := filterPortList(ports); portList != "" {
		args = append(args, "--filter-ports", portList)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn gta-singbox-agent: %w", err)
	}
	ap := &agentProcess{cmd: cmd, done: make(chan struct{})}
	slog.Info("singbox agent spawned for proxy lease",
		"bin", bin, "listen", listenAddr, "server", serverAddr, "pid", cmd.Process.Pid)
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Warn("singbox agent exited", "pid", cmd.Process.Pid, "error", err)
		}
		close(ap.done)
	}()
	return ap, nil
}

// filterHostList 把 host 筛选列表转为逗号分隔的命令行参数（空列表返回 ""）。
func filterHostList(hosts []string) string {
	trimmed := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			trimmed = append(trimmed, h)
		}
	}
	return strings.Join(trimmed, ",")
}

// filterPortList 把端口筛选列表转为逗号分隔的命令行参数（空列表返回 ""）。
func filterPortList(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		if p > 0 && p < 65536 {
			parts = append(parts, strconv.Itoa(p))
		}
	}
	return strings.Join(parts, ",")
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

// release 归还端口（幂等，未占用时无副作用）。
func (pr *portRange) release(port int) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	delete(pr.used, port)
}

// proxyLease 是一个代理抓包租约的全部状态。
type proxyLease struct {
	sessionID    string // = lease_id，与 mobile 抓包会话 1:1
	owner        string
	projectID    string
	plugin       string
	includeHosts []string
	includePorts []int
	device       string
	agentPort    int // gta-singbox-agent HTTP CONNECT 监听端口（12100-12199）
	grpcPort     int // mobile Source gRPC 监听端口（19100-19199）
	activity     *mobile.Activity
	agent        *agentProcess
	createdAt    time.Time
}

// probeFreePort 预探测端口可 bind（缓解 TOCTOU：分配与实际监听之间的窗口）。
func probeFreePort(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return ln.Close()
}

// CreateProxyLease 创建代理抓包租约：分配双端口 → 预探测 → 启动独立 mobile 会话 →
// 就绪验证 → 拉起独立 agent → 写入租约表。端口冲突/会话启动失败换端口重试（≤3 次）；
// agent 二进制缺失/启动失败不重试直接报错。
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
	var lastErr error
	for attempt := 0; attempt < proxyLeaseCreateRetries; attempt++ {
		// 1. leaseMu 下分配双端口。
		s.leaseMu.Lock()
		agentPort, err1 := s.agentPorts.allocate()
		grpcPort, err2 := s.grpcPorts.allocate()
		if err1 != nil {
			if err2 == nil {
				s.grpcPorts.release(grpcPort)
			}
			s.leaseMu.Unlock()
			return capturecontrol.ProxyLease{}, fmt.Errorf("allocate agent listen port: %w", err1)
		}
		if err2 != nil {
			s.agentPorts.release(agentPort)
			s.leaseMu.Unlock()
			return capturecontrol.ProxyLease{}, fmt.Errorf("allocate mobile gRPC port: %w", err2)
		}
		s.leaseMu.Unlock()

		releasePorts := func() {
			s.agentPorts.release(agentPort)
			s.grpcPorts.release(grpcPort)
		}

		// 2. 预探测端口可 bind。
		if err := probeFreePort(agentPort); err != nil {
			lastErr = fmt.Errorf("agent port %d not bindable: %w", agentPort, err)
			releasePorts()
			continue
		}
		if err := probeFreePort(grpcPort); err != nil {
			lastErr = fmt.Errorf("mobile gRPC port %d not bindable: %w", grpcPort, err)
			releasePorts()
			continue
		}

		// 3. leaseMu 外启动独立 mobile 会话（owner 经 ctx 自动写入 SessionMeta）。
		activity := mobile.NewActivity()
		res, err := s.StartSession(ctx, capturecontrol.StartSessionRequest{
			Plugin:    req.Plugin,
			ProjectID: req.ProjectID,
			Mobile: &capturecontrol.MobileConfig{
				ListenAddr: fmt.Sprintf("127.0.0.1:%d", grpcPort),
				Activity:   activity,
			},
		})
		if err != nil {
			lastErr = fmt.Errorf("start mobile session: %w", err)
			releasePorts()
			continue
		}
		sessionID := res.SessionID

		// 4. 就绪验证：captureTask 的监听是 run goroutine 内异步建立的，
		//    轮询拨号 gRPC 端口 + task 存活检查。
		if err := s.waitMobileReady(sessionID, grpcPort, mobileReadyTimeout); err != nil {
			lastErr = err
			s.stopSessionQuiet(sessionID)
			releasePorts()
			continue
		}

		// 5. 拉起租约专属 agent（PushClient 自动重连 source，可先于完全就绪）。
		listenAddr := fmt.Sprintf("0.0.0.0:%d", agentPort)
		serverAddr := fmt.Sprintf("127.0.0.1:%d", grpcPort)
		ap, err := s.agentSpawner(s.workDir, s.agentBin, serverAddr, listenAddr, req.IncludeHosts, includePorts)
		if err != nil {
			// 二进制缺失/启动失败：换端口无意义，直接报错。
			s.stopSessionQuiet(sessionID)
			releasePorts()
			return capturecontrol.ProxyLease{}, err
		}
		// 等 agent 存活确认；立即退出（配置/端口问题）则换端口重试。
		time.Sleep(agentStartupGrace)
		if !ap.running() {
			lastErr = fmt.Errorf("gta-singbox-agent exited immediately (agent port %d)", agentPort)
			ap.stop()
			s.stopSessionQuiet(sessionID)
			releasePorts()
			continue
		}

		lease := &proxyLease{
			sessionID:    sessionID,
			owner:        owner,
			projectID:    req.ProjectID,
			plugin:       req.Plugin,
			includeHosts: req.IncludeHosts,
			includePorts: includePorts,
			device:       req.Device,
			agentPort:    agentPort,
			grpcPort:     grpcPort,
			activity:     activity,
			agent:        ap,
			createdAt:    time.Now(),
		}

		// 6. leaseMu 下复核每用户上限并写入租约表。
		s.leaseMu.Lock()
		if s.countLeasesLocked(owner) >= maxLeasesPerOwner {
			s.leaseMu.Unlock()
			ap.stop()
			s.stopSessionQuiet(sessionID)
			releasePorts()
			return capturecontrol.ProxyLease{}, fmt.Errorf("owner %q already has %d active proxy leases (limit %d); release one first",
				owner, maxLeasesPerOwner, maxLeasesPerOwner)
		}
		s.leases[sessionID] = lease
		s.leaseMu.Unlock()

		s.logger.Info("proxy lease created",
			"lease_id", sessionID, "owner", owner, "device", req.Device,
			"agent_listen", listenAddr, "mobile_grpc", serverAddr, "plugin", req.Plugin)
		return s.buildLeaseView(lease), nil
	}
	return capturecontrol.ProxyLease{}, fmt.Errorf("create proxy lease failed after %d attempts: %w", proxyLeaseCreateRetries, lastErr)
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

// stopSessionQuiet 尽力停止会话（失败仅告警）：用于创建流程中的回滚路径。
// finalizeTask → reclaimLeaseForSession 对未入表的租约是 no-op，端口由调用方释放。
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

// ReleaseProxyLease 释放租约：停止会话（finalize → reclaimLeaseForSession 自动回收
// agent 与端口）。会话已不存在（先前已终止/回收）时直接幂等回收。
// 注意：StopSession 必须在 leaseMu 之外调用（锁序规约）。
func (s *pipelineService) ReleaseProxyLease(ctx context.Context, leaseID string) (capturecontrol.ReleaseProxyLeaseResult, error) {
	if _, err := s.getLeaseForOwner(ctx, leaseID); err != nil {
		return capturecontrol.ReleaseProxyLeaseResult{OK: false, Message: err.Error()}, err
	}
	if _, err := s.StopSession(ctx, leaseID); err != nil {
		if errors.Is(err, internalipc.ErrNoActiveCapture) {
			// 会话已终止（可能 finalize 尚未跑完或早已回收）：直接回收，幂等。
			s.reclaimLeaseForSession(leaseID)
			return capturecontrol.ReleaseProxyLeaseResult{OK: true, Message: "session already stopped; lease reclaimed", SessionID: leaseID}, nil
		}
		return capturecontrol.ReleaseProxyLeaseResult{OK: false, Message: err.Error(), SessionID: leaseID}, err
	}
	// 正常路径：StopSession 已触发 finalize，reclaim 由 finalizeTask 完成；
	// 此处兜底再调一次（幂等），覆盖 finalize 延迟场景。
	s.reclaimLeaseForSession(leaseID)
	s.logger.Info("proxy lease released", "lease_id", leaseID)
	return capturecontrol.ReleaseProxyLeaseResult{OK: true, Message: "lease released", SessionID: leaseID}, nil
}

// reclaimLeaseForSession 是租约的唯一回收点（幂等）：从租约表移除、终止 agent、
// 释放双端口。由 finalizeTask 调用（此时不持 s.mu，无锁序问题），
// 覆盖所有会话终止路径（手动 stop、自动出错、ReleaseProxyLease、StopAll 关停）。
func (s *pipelineService) reclaimLeaseForSession(sessionID string) {
	s.leaseMu.Lock()
	lease, ok := s.leases[sessionID]
	if ok {
		delete(s.leases, sessionID)
	}
	s.leaseMu.Unlock()
	if !ok {
		return
	}
	lease.agent.stop()
	s.agentPorts.release(lease.agentPort)
	s.grpcPorts.release(lease.grpcPort)
	s.logger.Info("proxy lease reclaimed",
		"lease_id", sessionID, "owner", lease.owner, "device", lease.device,
		"agent_port", lease.agentPort, "mobile_grpc_port", lease.grpcPort)
}

// CleanupProxyLeases 关停兜底清扫：pipeline 退出时（StopAll 之后）调用，
// 回收所有仍在表中的租约（正常情况已由 finalize → reclaim 处理）。
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
		s.grpcPorts.release(l.grpcPort)
	}
	if len(leases) > 0 {
		s.logger.Info("proxy leases cleaned up on shutdown", "count", len(leases))
	}
}

// buildLeaseView 组装租约的配置 + 运行时状态快照：
// activity.Snapshot()（无锁原子）+ agent 存活/PID + 会话运行状态。
func (s *pipelineService) buildLeaseView(l *proxyLease) capturecontrol.ProxyLease {
	view := capturecontrol.ProxyLease{
		LeaseID:         l.sessionID,
		Owner:           l.owner,
		ProjectID:       l.projectID,
		Plugin:          l.plugin,
		IncludeHosts:    l.includeHosts,
		IncludePorts:    l.includePorts,
		Device:          l.device,
		ListenAddr:      fmt.Sprintf("0.0.0.0:%d", l.agentPort),
		AgentListenPort: l.agentPort,
		MobileGRPCPort:  l.grpcPort,
		AgentRunning:    l.agent.running(),
		AgentPID:        int32(l.agent.pid()),
		SessionID:       l.sessionID,
		CreatedAt:       l.createdAt,
	}
	if task, ok := s.getTask(l.sessionID); ok {
		view.SessionRunning = task.State() == capture.StateRunning
	}
	if l.activity != nil {
		act := l.activity.Snapshot()
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
