package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"gametrace/pkg/agent"
	"gametrace/pkg/auth"
	"gametrace/pkg/internalipc/capturecontrol"
)

// errAgentSpawn 是 agentSpawner 返回错误时用的哨兵错误（测试专用）。
var errAgentSpawn = errors.New("agent spawn failed")

// fakeProbePort 在测试期间替换 probeFreePortFn，绕过 GameTrace 专属端口段
// （12100-12199 / 19500-19599）。开发机常驻的 gt-singbox-agent 可能正
// 占用这些端口；测试目的是验证 CreateProxyLease 的内部状态机，
// 不该耦合到生产服务是否在跑。
func fakeProbePort(t *testing.T) {
	t.Helper()
	orig := probeFreePortFn
	probeFreePortFn = func(int) error { return nil }
	t.Cleanup(func() { probeFreePortFn = orig })
}

// TestHelperProcess 是被 proxy_lease 测试 re-exec 出来的子进程：收到
// GO_WANT_HELPER_PROCESS=1 时长期阻塞，模拟常驻的 gt-singbox-agent，
// 由父进程通过 agentProcess.stop() 的 Process.Kill() 终止。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(time.Hour)
}

// startFakeAgent 启动一个长期存活的 helper 子进程并包装成 *agentProcess，
// 供 CreateProxyLease 的 agentSpawner fake 返回（running() == true）。
func startFakeAgent(t *testing.T) *agentProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake agent: %v", err)
	}
	ap := &agentProcess{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(ap.done) }()
	t.Cleanup(func() { ap.stop() })
	return ap
}

// startFakeControlServer 在测试进程内拉起一个真实的 agent 控制服务，
// 作为常驻 agent 控制面的替身：pipeline 的 waitAgentControlReady 与
// StartLeaseCapture/StopLeaseCapture 都会真的打到这里，从而验证控制协议
// 与状态机（数据面不参与，没有真实手机流量）。
func startFakeControlServer(t *testing.T, ctrlAddr string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := agent.NewCaptureGate(agent.RelayConfig{}, logger)
	srv := agent.NewControlServer(ctrlAddr, gate, nil, logger)
	t.Cleanup(func() { _ = gate.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
}

// spawnCall 记录一次 agentSpawner 调用，用于断言 listenAddr/ctrlAddr 正确。
type spawnCall struct {
	ctrlAddr   string
	listenAddr string
}

// recordingSpawner 返回一个记录调用参数的 fake spawner：拉起常驻 helper 进程
// 作为 agent 替身，并在测试进程内监听控制端口。
func recordingSpawner(t *testing.T, calls *[]spawnCall) proxyAgentSpawner {
	return func(workDir, bin, ctrlAddr, listenAddr string) (*agentProcess, error) {
		*calls = append(*calls, spawnCall{ctrlAddr: ctrlAddr, listenAddr: listenAddr})
		startFakeControlServer(t, ctrlAddr)
		return startFakeAgent(t), nil
	}
}

// withOwner 注入 owner 身份（非 admin）。
func withOwner(ctx context.Context, owner string) context.Context {
	return auth.WithPrincipal(ctx, &auth.Principal{Owner: owner})
}

// TestPortRange_AllocateReleaseExhaust 验证端口段分配器：轮转分配、耗尽报错、
// 归还后可复用、对未占用端口归还幂等。
func TestPortRange_AllocateReleaseExhaust(t *testing.T) {
	pr := newPortRange(12100, 12103) // 4 个端口
	seen := make(map[int]bool)
	for i := 0; i < 4; i++ {
		p, err := pr.allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if p < 12100 || p > 12103 {
			t.Fatalf("port %d out of range [12100,12103]", p)
		}
		if seen[p] {
			t.Fatalf("duplicate port %d", p)
		}
		seen[p] = true
	}
	if _, err := pr.allocate(); err == nil {
		t.Fatal("allocate: expected exhaustion error")
	}

	// 归还 12101 后可重新分配到它。
	pr.release(12101)
	p, err := pr.allocate()
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if p != 12101 {
		t.Fatalf("allocate after release = %d, want 12101", p)
	}

	// 对未占用端口归还幂等（无副作用）。
	pr.release(12101)
	pr.release(999999)
}

// TestPortRange_Reserve 验证指定端口占用（sticky 复用依赖它）：
// 空闲端口可占用、已占用/越界端口返回 false。
func TestPortRange_Reserve(t *testing.T) {
	pr := newPortRange(12100, 12199)
	if !pr.reserve(12150) {
		t.Fatal("reserve free port failed")
	}
	if pr.reserve(12150) {
		t.Fatal("reserve already-used port should fail")
	}
	if pr.reserve(1) {
		t.Fatal("reserve out-of-range port should fail")
	}
	// 被 reserve 的端口不会再由轮转分配出去。
	for i := 0; i < 99; i++ {
		p, err := pr.allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if p == 12150 {
			t.Fatal("reserve'd port 12150 handed out by allocate")
		}
	}
}

// TestCreateProxyLease_Success 验证创建租约成功：入表、控制端口可拨号、
// 返回快照字段正确、spawner 收到的 listenAddr/ctrlAddr 与分配端口一致。
func TestCreateProxyLease_Success(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	var calls []spawnCall
	s.agentSpawner = recordingSpawner(t, &calls)

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{
		Plugin:       "tcp",
		Device:       "pixel-7",
		IncludeHosts: []string{"api.example.com"},
		IncludePorts: []int32{443, 80},
	})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, lease.LeaseID) })

	if lease.LeaseID == "" {
		t.Fatal("LeaseID empty")
	}
	if lease.Owner != "alice" {
		t.Errorf("Owner = %q, want alice", lease.Owner)
	}
	if lease.Device != "pixel-7" {
		t.Errorf("Device = %q, want pixel-7", lease.Device)
	}
	if lease.AgentListenPort < 12100 || lease.AgentListenPort > 12199 {
		t.Errorf("AgentListenPort = %d out of range", lease.AgentListenPort)
	}
	if lease.ControlPort < 19500 || lease.ControlPort > 19599 {
		t.Errorf("ControlPort = %d out of range", lease.ControlPort)
	}
	if lease.MobileGRPCPort < 19100 || lease.MobileGRPCPort > 19199 {
		t.Errorf("MobileGRPCPort = %d out of range", lease.MobileGRPCPort)
	}
	if !lease.AgentRunning {
		t.Error("AgentRunning = false, want true")
	}
	// 默认 auto-start：创建后应已处于抓包中。
	if !lease.SessionRunning || lease.SessionID == "" {
		t.Errorf("auto-start failed: SessionRunning=%v SessionID=%q", lease.SessionRunning, lease.SessionID)
	}
	// 租约 id 与会话 id 必须不同（租约跨会话常驻）。
	if lease.LeaseID == lease.SessionID {
		t.Errorf("LeaseID == SessionID (%q); lease must outlive capture sessions", lease.LeaseID)
	}

	// 租约已入表。
	s.leaseMu.Lock()
	_, inTable := s.leases[lease.LeaseID]
	s.leaseMu.Unlock()
	if !inTable {
		t.Fatal("lease not in table after create")
	}

	// mobile gRPC 端口可拨号（真实 mobile source 已监听）。
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(lease.MobileGRPCPort), time.Second)
	if err != nil {
		t.Fatalf("mobile gRPC port %d not dialable: %v", lease.MobileGRPCPort, err)
	}
	_ = conn.Close()

	// spawner 参数正确：agent 以常驻模式拉起（只给 listen + control）。
	if len(calls) != 1 {
		t.Fatalf("spawner calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if want := "0.0.0.0:" + strconv.Itoa(lease.AgentListenPort); c.listenAddr != want {
		t.Errorf("spawner listenAddr = %q, want %q", c.listenAddr, want)
	}
	if want := "127.0.0.1:" + strconv.Itoa(lease.ControlPort); c.ctrlAddr != want {
		t.Errorf("spawner ctrlAddr = %q, want %q", c.ctrlAddr, want)
	}
}

// TestCreateProxyLease_NoAutoStart 验证 NoAutoStart：只建出口不抓包
//（agent 运行、端口已分配，但没有会话、没有 mobile gRPC 端口占用）。
func TestCreateProxyLease_NoAutoStart(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{
		Device:      "pixel-7",
		NoAutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, lease.LeaseID) })

	if lease.SessionID != "" || lease.SessionRunning || lease.CaptureRunning {
		t.Errorf("idle lease should have no session, got id=%q running=%v capture=%v",
			lease.SessionID, lease.SessionRunning, lease.CaptureRunning)
	}
	if lease.MobileGRPCPort != 0 {
		t.Errorf("idle lease MobileGRPCPort = %d, want 0", lease.MobileGRPCPort)
	}
	if !lease.AgentRunning {
		t.Error("AgentRunning = false, want true (egress must be up even when idle)")
	}
	if lease.AgentListenPort == 0 || lease.ControlPort == 0 {
		t.Errorf("idle lease ports not allocated: agent=%d control=%d", lease.AgentListenPort, lease.ControlPort)
	}
}

// TestLeaseCapture_StartStopRestart 是常驻出口模型的核心回归：
// 反复 start/stop 抓包时，手机的代理端口必须始终不变（二维码不失效、
// VPN 不重连），且每次 start 都是一个全新的会话 id（新旧会话隔离）。
func TestLeaseCapture_StartStopRestart(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{
		Device:      "pixel-7",
		NoAutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, lease.LeaseID) })

	// 反复开始/停止三轮：端口恒定，session 每轮都是新的。
	var seenSessions []string
	for round := 1; round <= 3; round++ {
		start, err := s.StartLeaseCapture(ctx, capturecontrol.StartLeaseCaptureRequest{LeaseID: lease.LeaseID})
		if err != nil {
			t.Fatalf("round %d StartLeaseCapture: %v", round, err)
		}
		if start.SessionID == "" {
			t.Fatalf("round %d: empty session id", round)
		}
		for _, prev := range seenSessions {
			if start.SessionID == prev {
				t.Fatalf("round %d reused session id %q from a previous capture", round, prev)
			}
		}
		seenSessions = append(seenSessions, start.SessionID)

		// 端口必须与租约创建时完全一致——这是「同一二维码长期有效」的落点。
		if start.Lease.AgentListenPort != lease.AgentListenPort {
			t.Fatalf("round %d agent port changed: %d -> %d",
				round, lease.AgentListenPort, start.Lease.AgentListenPort)
		}
		if start.Lease.ControlPort != lease.ControlPort {
			t.Fatalf("round %d control port changed: %d -> %d",
				round, lease.ControlPort, start.Lease.ControlPort)
		}
		if !start.Lease.CaptureRunning {
			t.Errorf("round %d CaptureRunning = false after start", round)
		}
		if start.Lease.CaptureCount != round {
			t.Errorf("round %d CaptureCount = %d, want %d", round, start.Lease.CaptureCount, round)
		}
		// 每轮抓包占用一个 mobile gRPC 端口，抓完即释放（不累积占用端口段）。
		if start.Lease.MobileGRPCPort == 0 {
			t.Errorf("round %d MobileGRPCPort = 0 while capturing", round)
		}

		stop, err := s.StopLeaseCapture(ctx, lease.LeaseID)
		if err != nil {
			t.Fatalf("round %d StopLeaseCapture: %v", round, err)
		}
		if stop.SessionID != start.SessionID {
			t.Errorf("round %d stopped session %q, want %q", round, stop.SessionID, start.SessionID)
		}

		// 停止后回到 idle，但出口与端口全部保留。
		view, err := s.GetProxyLease(ctx, lease.LeaseID)
		if err != nil {
			t.Fatalf("round %d GetProxyLease: %v", round, err)
		}
		if view.SessionID != "" || view.CaptureRunning || view.SessionRunning {
			t.Errorf("round %d after stop: session=%q capture=%v running=%v, want idle",
				round, view.SessionID, view.CaptureRunning, view.SessionRunning)
		}
		if !view.AgentRunning {
			t.Errorf("round %d: agent must stay running after stop (no VPN reconnect)", round)
		}
		if view.AgentListenPort != lease.AgentListenPort {
			t.Errorf("round %d: agent port changed after stop", round)
		}
		if view.MobileGRPCPort != 0 {
			t.Errorf("round %d: MobileGRPCPort = %d after stop, want 0 (port released)", round, view.MobileGRPCPort)
		}
	}
}

// TestLeaseCapture_RejectsConcurrentStart 验证同一租约重复 start 被拒绝：
// 一个出口同时只属于一个抓包会话，避免两个会话抢同一条数据流。
func TestLeaseCapture_RejectsConcurrentStart(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{Device: "pixel-7"})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, lease.LeaseID) })

	if _, err := s.StartLeaseCapture(ctx, capturecontrol.StartLeaseCaptureRequest{LeaseID: lease.LeaseID}); err == nil {
		t.Fatal("second StartLeaseCapture should fail while capturing")
	}
	// 未在抓包时 stop 也应报错（不静默成功）。
	if _, err := s.StopLeaseCapture(ctx, lease.LeaseID); err != nil {
		t.Fatalf("StopLeaseCapture: %v", err)
	}
	if _, err := s.StopLeaseCapture(ctx, lease.LeaseID); err == nil {
		t.Fatal("StopLeaseCapture on idle lease should fail")
	}
}

// TestLeaseCapture_OwnerScope 验证 start/stop 的 owner 作用域：
// 他人租约按 not found 处理（不泄露存在性）。
func TestLeaseCapture_OwnerScope(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	aliceCtx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(aliceCtx, capturecontrol.CreateProxyLeaseRequest{Device: "pixel-7", NoAutoStart: true})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(aliceCtx, lease.LeaseID) })

	bobCtx := withOwner(context.Background(), "bob")
	if _, err := s.StartLeaseCapture(bobCtx, capturecontrol.StartLeaseCaptureRequest{LeaseID: lease.LeaseID}); err == nil {
		t.Error("bob StartLeaseCapture on alice lease: expected not found")
	}
	if _, err := s.StopLeaseCapture(bobCtx, lease.LeaseID); err == nil {
		t.Error("bob StopLeaseCapture on alice lease: expected not found")
	}
}

// TestProxyLease_SessionStopKeepsEgress 验证会话因任何原因终止时（此处直接
// StopSession，模拟手动停/异常退出）只清空抓包状态，出口本身保留：
// agent 进程、CONNECT 端口、控制端口全部存活，可再次 start。
func TestProxyLease_SessionStopKeepsEgress(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{Device: "pixel-7"})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, lease.LeaseID) })

	if _, err := s.StopSession(ctx, lease.SessionID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	s.leaseMu.Lock()
	l, inTable := s.leases[lease.LeaseID]
	s.leaseMu.Unlock()
	if !inTable {
		t.Fatal("lease must survive capture session termination (stable egress)")
	}
	if l.sessionID != "" {
		t.Errorf("lease sessionID = %q after session stop, want empty", l.sessionID)
	}
	// mobile gRPC 端口已回收，agent / 控制端口仍占用。
	s.grpcPorts.mu.Lock()
	_, grpcUsed := s.grpcPorts.used[lease.MobileGRPCPort]
	s.grpcPorts.mu.Unlock()
	if grpcUsed {
		t.Error("mobile gRPC port still marked used after session stop")
	}
	s.agentPorts.mu.Lock()
	_, agentUsed := s.agentPorts.used[lease.AgentListenPort]
	s.agentPorts.mu.Unlock()
	if !agentUsed {
		t.Error("agent CONNECT port released; egress must keep its port")
	}

	// 出口仍在 → 可以再开一轮抓包。
	again, err := s.StartLeaseCapture(ctx, capturecontrol.StartLeaseCaptureRequest{LeaseID: lease.LeaseID})
	if err != nil {
		t.Fatalf("restart capture after session stop: %v", err)
	}
	if again.SessionID == lease.SessionID {
		t.Error("restarted capture reused the stopped session id")
	}
}

// TestProxyLease_StickyPortReuse 验证 sticky 端口：同一 (owner, device)
// 释放后再创建仍拿到同一个代理端口，使此前扫过的二维码继续有效。
func TestProxyLease_StickyPortReuse(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	first, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{
		Device: "pixel-7", NoAutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateProxyLease #1: %v", err)
	}
	port := first.AgentListenPort
	if _, err := s.ReleaseProxyLease(ctx, first.LeaseID); err != nil {
		t.Fatalf("ReleaseProxyLease: %v", err)
	}

	second, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{
		Device: "pixel-7", NoAutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateProxyLease #2: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, second.LeaseID) })
	if second.AgentListenPort != port {
		t.Fatalf("sticky port not reused: %d -> %d (QR code would break)", port, second.AgentListenPort)
	}
	if !second.StickyPort {
		t.Error("StickyPort = false on reused port, want true")
	}
	// 不同设备不应抢走该端口（sticky 按 owner+device 隔离）。
	other, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{
		Device: "pixel-8", NoAutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateProxyLease #3: %v", err)
	}
	t.Cleanup(func() { _, _ = s.ReleaseProxyLease(ctx, other.LeaseID) })
	if other.AgentListenPort == port {
		t.Error("different device reused the same sticky port")
	}
}

// TestProxyLease_OwnerScope 验证 owner 作用域：非 admin 只见/只可操作自己的租约，
// 他人租约的 Get/Release 按 not found 处理；admin 全可见。
func TestProxyLease_OwnerScope(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	s.leaseMu.Lock()
	s.leases["lease-alice"] = &proxyLease{leaseID: "lease-alice", owner: "alice"}
	s.leases["lease-bob"] = &proxyLease{leaseID: "lease-bob", owner: "bob"}
	s.leaseMu.Unlock()

	// alice 只能 Get 自己的。
	if _, err := s.GetProxyLease(withOwner(context.Background(), "alice"), "lease-alice"); err != nil {
		t.Errorf("alice Get own: %v", err)
	}
	if _, err := s.GetProxyLease(withOwner(context.Background(), "alice"), "lease-bob"); err == nil {
		t.Error("alice Get bob: expected not found")
	}

	// admin 全可见。
	adminCtx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "admin", IsAdmin: true})
	if _, err := s.GetProxyLease(adminCtx, "lease-bob"); err != nil {
		t.Errorf("admin Get bob: %v", err)
	}

	// List 作用域：alice 只见 1 条，admin 见 2 条。
	aliceList, _ := s.ListProxyLeases(withOwner(context.Background(), "alice"))
	if len(aliceList) != 1 || aliceList[0].LeaseID != "lease-alice" {
		t.Errorf("alice list = %+v, want only lease-alice", aliceList)
	}
	adminList, _ := s.ListProxyLeases(adminCtx)
	if len(adminList) != 2 {
		t.Errorf("admin list = %d, want 2", len(adminList))
	}

	// bob Release alice 的租约 → not found（不泄露存在性）。
	if _, err := s.ReleaseProxyLease(withOwner(context.Background(), "bob"), "lease-alice"); err == nil {
		t.Error("bob Release alice: expected not found")
	}
}

// TestReleaseProxyLease_Reclaims 验证释放租约：三类端口全部回收、agent 被杀、
// 会话 stopped、任务移除。
func TestReleaseProxyLease_Reclaims(t *testing.T) {
	s, _, controlStore := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	agentPort, grpcPort, ctrlPort := lease.AgentListenPort, lease.MobileGRPCPort, lease.ControlPort
	sessionID := lease.SessionID

	res, err := s.ReleaseProxyLease(ctx, lease.LeaseID)
	if err != nil {
		t.Fatalf("ReleaseProxyLease: %v", err)
	}
	if !res.OK {
		t.Errorf("Release OK = false, want true (message %q)", res.Message)
	}
	if res.SessionID != sessionID {
		t.Errorf("released session = %q, want %q", res.SessionID, sessionID)
	}

	// 租约已从表移除。
	s.leaseMu.Lock()
	_, inTable := s.leases[lease.LeaseID]
	s.leaseMu.Unlock()
	if inTable {
		t.Error("lease still in table after release")
	}

	// 三类端口全部回收。
	for name, tc := range map[string]struct {
		pr   *portRange
		port int
	}{
		"agent":   {s.agentPorts, agentPort},
		"grpc":    {s.grpcPorts, grpcPort},
		"control": {s.ctrlPorts, ctrlPort},
	} {
		tc.pr.mu.Lock()
		_, used := tc.pr.used[tc.port]
		tc.pr.mu.Unlock()
		if used {
			t.Errorf("%s port %d still marked used after release", name, tc.port)
		}
	}

	// 会话已 stopped，任务已移除。
	meta, err := controlStore.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if meta.Status != "stopped" {
		t.Errorf("session status = %q, want stopped", meta.Status)
	}
	if _, ok := s.getTask(sessionID); ok {
		t.Error("task still in map after release")
	}
}

// TestCreateProxyLease_AgentExitedFails 验证 agent 拉起后立即退出时直接报错，
// 且不留任何租约在表。
func TestCreateProxyLease_AgentExitedFails(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	attempts := 0
	s.agentSpawner = func(workDir, bin, ctrlAddr, listenAddr string) (*agentProcess, error) {
		attempts++
		// 返回一个非 running 的 agentProcess（cmd 为 nil），模拟「立即退出」。
		return &agentProcess{}, nil
	}

	_, err := s.CreateProxyLease(withOwner(context.Background(), "alice"), capturecontrol.CreateProxyLeaseRequest{})
	if err == nil {
		t.Fatal("CreateProxyLease: expected error when agent exits immediately")
	}
	if attempts != 1 {
		t.Errorf("agentSpawner attempts = %d, want 1", attempts)
	}

	s.leaseMu.Lock()
	n := len(s.leases)
	s.leaseMu.Unlock()
	if n != 0 {
		t.Errorf("leases left in table = %d, want 0", n)
	}
}

// TestCreateProxyLease_AgentSpawnError 验证 agentSpawner 返回错误（如二进制缺失）
// 时不重试、直接报错。
func TestCreateProxyLease_AgentSpawnError(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	attempts := 0
	s.agentSpawner = func(workDir, bin, ctrlAddr, listenAddr string) (*agentProcess, error) {
		attempts++
		return nil, errAgentSpawn
	}

	if _, err := s.CreateProxyLease(withOwner(context.Background(), "alice"), capturecontrol.CreateProxyLeaseRequest{}); err == nil {
		t.Fatal("CreateProxyLease: expected error from agentSpawner")
	}
	if attempts != 1 {
		t.Errorf("agentSpawner attempts = %d, want 1 (no retry on spawn error)", attempts)
	}
}

// TestCreateProxyLease_PerOwnerLimit 验证每用户并发租约上限：预检在 spawn 前拦截。
func TestCreateProxyLease_PerOwnerLimit(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	s.leaseMu.Lock()
	for i := 0; i < maxLeasesPerOwner; i++ {
		id := "lease-" + strconv.Itoa(i)
		s.leases[id] = &proxyLease{leaseID: id, owner: "alice"}
	}
	s.leaseMu.Unlock()

	// 未注入 fake spawner：预检即失败，不会走到 agentSpawner。
	if _, err := s.CreateProxyLease(withOwner(context.Background(), "alice"), capturecontrol.CreateProxyLeaseRequest{}); err == nil {
		t.Fatal("CreateProxyLease: expected per-owner limit error")
	}
}
