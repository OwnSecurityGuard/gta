package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"gta/pkg/auth"
	"gta/pkg/internalipc/capturecontrol"
)

// errAgentSpawn 是 agentSpawner 返回错误时用的哨兵错误（测试专用）。
var errAgentSpawn = errors.New("agent spawn failed")

// TestHelperProcess 是被 proxy_lease 测试 re-exec 出来的子进程：收到
// GO_WANT_HELPER_PROCESS=1 时长期阻塞，模拟常驻的 gta-singbox-agent，
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

// spawnCall 记录一次 agentSpawner 调用，用于断言 serverAddr/listenAddr 正确。
type spawnCall struct {
	serverAddr string
	listenAddr string
	hosts      []string
	ports      []int
}

// recordingSpawner 返回一个记录调用参数、且总是拉起常驻 helper 进程的 fake spawner。
func recordingSpawner(t *testing.T, calls *[]spawnCall) proxyAgentSpawner {
	return func(workDir, bin, serverAddr, listenAddr string, hosts []string, ports []int) (*agentProcess, error) {
		*calls = append(*calls, spawnCall{serverAddr: serverAddr, listenAddr: listenAddr, hosts: hosts, ports: ports})
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

// TestCreateProxyLease_Success 验证创建租约成功：入表、mobile gRPC 端口可拨号、
// 返回快照字段正确、spawner 收到的 serverAddr/listenAddr 与分配端口一致。
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
	// 测试结束前停掉会话，释放 mobile source 与 sqlite store，避免 TempDir 清理失败。
	t.Cleanup(func() { _, _ = s.StopSession(context.Background(), lease.LeaseID) })

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
	if lease.MobileGRPCPort < 19100 || lease.MobileGRPCPort > 19199 {
		t.Errorf("MobileGRPCPort = %d out of range", lease.MobileGRPCPort)
	}
	if !lease.AgentRunning {
		t.Error("AgentRunning = false, want true")
	}
	if !lease.SessionRunning {
		t.Error("SessionRunning = false, want true")
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

	// spawner 参数正确。
	if len(calls) != 1 {
		t.Fatalf("spawner calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if want := "127.0.0.1:" + strconv.Itoa(lease.MobileGRPCPort); c.serverAddr != want {
		t.Errorf("spawner serverAddr = %q, want %q", c.serverAddr, want)
	}
	if want := "0.0.0.0:" + strconv.Itoa(lease.AgentListenPort); c.listenAddr != want {
		t.Errorf("spawner listenAddr = %q, want %q", c.listenAddr, want)
	}
}

// TestProxyLease_OwnerScope 验证 owner 作用域：非 admin 只见/只可操作自己的租约，
// 他人租约的 Get/Release 按 not found 处理；admin 全可见。
func TestProxyLease_OwnerScope(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	s.leaseMu.Lock()
	s.leases["lease-alice"] = &proxyLease{sessionID: "lease-alice", owner: "alice"}
	s.leases["lease-bob"] = &proxyLease{sessionID: "lease-bob", owner: "bob"}
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

// TestReleaseProxyLease_Reclaims 验证释放租约：端口回收、会话 stopped、任务移除。
func TestReleaseProxyLease_Reclaims(t *testing.T) {
	s, _, controlStore := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}
	agentPort, grpcPort := lease.AgentListenPort, lease.MobileGRPCPort

	res, err := s.ReleaseProxyLease(ctx, lease.LeaseID)
	if err != nil {
		t.Fatalf("ReleaseProxyLease: %v", err)
	}
	if !res.OK {
		t.Errorf("Release OK = false, want true (message %q)", res.Message)
	}

	// 租约已从表移除。
	s.leaseMu.Lock()
	_, inTable := s.leases[lease.LeaseID]
	s.leaseMu.Unlock()
	if inTable {
		t.Error("lease still in table after release")
	}

	// 端口已从端口段回收（used 表中不再标记）。
	s.agentPorts.mu.Lock()
	_, agentUsed := s.agentPorts.used[agentPort]
	s.agentPorts.mu.Unlock()
	if agentUsed {
		t.Errorf("agentPort %d still marked used after release", agentPort)
	}

	s.grpcPorts.mu.Lock()
	_, grpcUsed := s.grpcPorts.used[grpcPort]
	s.grpcPorts.mu.Unlock()
	if grpcUsed {
		t.Errorf("grpcPort %d still marked used after release", grpcPort)
	}

	// 会话已 stopped，任务已移除。
	meta, err := controlStore.GetSession(ctx, lease.LeaseID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if meta.Status != "stopped" {
		t.Errorf("session status = %q, want stopped", meta.Status)
	}
	if _, ok := s.getTask(lease.LeaseID); ok {
		t.Error("task still in map after release")
	}
}

// TestProxyLease_AutoReclaimOnSessionStop 验证直接 StopSession（不经 ReleaseProxyLease）
// 时，finalizeTask → reclaimLeaseForSession 自动回收租约。
func TestProxyLease_AutoReclaimOnSessionStop(t *testing.T) {
	s, _, controlStore := newTestPipelineService(t)
	s.agentSpawner = recordingSpawner(t, &[]spawnCall{})

	ctx := withOwner(context.Background(), "alice")
	lease, err := s.CreateProxyLease(ctx, capturecontrol.CreateProxyLeaseRequest{})
	if err != nil {
		t.Fatalf("CreateProxyLease: %v", err)
	}

	// 直接 StopSession（不经 ReleaseProxyLease）→ finalize → 自动回收。
	if _, err := s.StopSession(ctx, lease.LeaseID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	s.leaseMu.Lock()
	_, inTable := s.leases[lease.LeaseID]
	s.leaseMu.Unlock()
	if inTable {
		t.Error("lease still in table after direct stop")
	}

	meta, err := controlStore.GetSession(ctx, lease.LeaseID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if meta.Status != "stopped" {
		t.Errorf("session status = %q, want stopped", meta.Status)
	}
}

// TestCreateProxyLease_PerOwnerLimit 验证每用户并发租约上限：预检在 spawn 前拦截。
func TestCreateProxyLease_PerOwnerLimit(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	s.leaseMu.Lock()
	for i := 0; i < maxLeasesPerOwner; i++ {
		id := "lease-" + strconv.Itoa(i)
		s.leases[id] = &proxyLease{sessionID: id, owner: "alice"}
	}
	s.leaseMu.Unlock()

	// 未注入 fake spawner：预检即失败，不会走到 agentSpawner。
	if _, err := s.CreateProxyLease(withOwner(context.Background(), "alice"), capturecontrol.CreateProxyLeaseRequest{}); err == nil {
		t.Fatal("CreateProxyLease: expected per-owner limit error")
	}
}

// TestCreateProxyLease_AgentExitedRetries 验证 agent 拉起后立即退出时换端口重试，
// 耗尽 proxyLeaseCreateRetries 次后报错，且不留任何租约在表。
func TestCreateProxyLease_AgentExitedRetries(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	attempts := 0
	s.agentSpawner = func(workDir, bin, serverAddr, listenAddr string, hosts []string, ports []int) (*agentProcess, error) {
		attempts++
		// 返回一个非 running 的 agentProcess（cmd 为 nil），模拟「立即退出」。
		return &agentProcess{}, nil
	}

	_, err := s.CreateProxyLease(withOwner(context.Background(), "alice"), capturecontrol.CreateProxyLeaseRequest{})
	if err == nil {
		t.Fatal("CreateProxyLease: expected error after agent exits on every attempt")
	}
	if attempts != proxyLeaseCreateRetries {
		t.Errorf("agentSpawner attempts = %d, want %d", attempts, proxyLeaseCreateRetries)
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
	s.agentSpawner = func(workDir, bin, serverAddr, listenAddr string, hosts []string, ports []int) (*agentProcess, error) {
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
