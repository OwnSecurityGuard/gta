package plugin

import (
	"context"
	"testing"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"

	"gametrace/pkg/auth"
)

// ownerCtx 返回注入指定属主身份的上下文。
func ownerCtx(owner string) context.Context {
	if owner == "" {
		return context.Background()
	}
	return auth.WithPrincipal(context.Background(), &auth.Principal{Owner: owner})
}

const sharedManifest = `api_version: gta.decoder/v2
name: shared-decoder
protocol: test_proto
type: decoder
hints:
  - tcp
`

// TestRegister_OwnerScopedCoexistence 验证 owner/name 键语义：
// 不同 owner 注册同名插件互不覆盖（都能按各自作用域找到）；
// 同一 owner 重复注册替换自己的旧实例。
func TestRegister_OwnerScopedCoexistence(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	reg := func(owner string) string {
		resp, err := s.Register(ownerCtx(owner), &pb.RegisterRequest{
			SocketPath: "unix:/nonexistent/decoder.sock",
			Manifest:   []byte(sharedManifest),
		})
		if err != nil {
			t.Fatalf("register(owner=%s): %v", owner, err)
		}
		return resp.GetInstanceId()
	}

	aliceA := reg("alice")
	bobA := reg("bob")
	if aliceA == bobA {
		t.Fatal("expected distinct instances for different owners")
	}
	// 同 owner 重复注册替换自己的实例
	aliceA2 := reg("alice")
	if aliceA2 == aliceA {
		t.Fatal("same-owner re-register should replace with a new instance")
	}

	// 各 owner 只在自己的作用域找到；无主第三方 carol 看不到
	for _, owner := range []string{"alice", "bob"} {
		c, _, ok := s.FindByNameFor(owner, "shared-decoder")
		if !ok || c == nil {
			t.Fatalf("FindByNameFor(%q) should find the plugin", owner)
		}
	}
	if _, _, ok := s.FindByNameFor("carol", "shared-decoder"); ok {
		t.Fatal("unrelated owner must not see others' plugins")
	}
	// 匿名视角看不到有主插件
	if _, _, ok := s.FindByName("shared-decoder"); ok {
		t.Fatal("anonymous FindByName must not see owner-scoped plugins")
	}

	// 替换后旧实例被移除：只剩 alice 新实例 + bob 实例
	if got := len(s.List()); got != 2 {
		t.Fatalf("expected 2 surviving instances, got %d: %+v", got, s.List())
	}
	for _, p := range s.List() {
		if p.Owner != "alice" && p.Owner != "bob" {
			t.Fatalf("unexpected owner %q", p.Owner)
		}
	}
}

// TestRegister_RejectsForeignNameLookup 验证完整键（含 "/"）可精确寻址其他
// owner 的插件，而无主调用方按裸名查不到有主插件。
func TestRegister_FullKeyLookup(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	if _, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		SocketPath: "unix:/nonexistent/decoder.sock",
		Manifest:   []byte(sharedManifest),
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := s.FindByNameFor("bob", "alice/shared-decoder"); ok {
		t.Fatal("bob must not address alice's plugin via full key")
	}
	if c, _, ok := s.FindByNameFor("alice", "alice/shared-decoder"); !ok || c == nil {
		t.Fatal("alice should address own plugin via full key")
	}
}

// TestTunnelRegisterAndBind 覆盖 tunnel=true 注册的绑定与生命周期：
//   - Register(tunnel=true) 先到：实例等待隧道（离线、不可查）；
//   - Connect 到达后绑定：在线、可 FindByNameFor、DecodeV2 往返可用；
//   - 隧道断开：offline 事件、不可查；
//   - 重连后重新注册：替换旧实例并重新绑定；
//   - 隧道插件不参与 CheckOffline 心跳超时。
func TestTunnelRegisterLifecycle(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	// Register 先到：等待隧道
	resp, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatalf("tunnel register: %v", err)
	}
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); ok {
		t.Fatal("unbound tunnel plugin must not be findable")
	}

	// Connect 到达：绑定
	ctx, cancel := context.WithCancel(ownerCtx("alice"))
	p := newTunnelHubPipe(ctx)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p}) }()
	waitTunnelBound(t, s, resp.GetInstanceId())

	c, _, ok := s.FindByNameFor("alice", "shared-decoder")
	if !ok || c == nil {
		t.Fatal("bound tunnel plugin should be findable via owner scope")
	}
	// 其他 owner 不可见
	if _, _, ok := s.FindByNameFor("bob", "shared-decoder"); ok {
		t.Fatal("bob must not see alice's tunnel plugin")
	}

	// Decode 往返走隧道
	pe := pluginEnd{p}
	stream, err := c.DecodeV2(ctx)
	if err != nil {
		t.Fatalf("DecodeV2: %v", err)
	}
	go func() { _ = stream.Send(&pb.DecodeRequest{InputId: "t-1", Payload: []byte("x")}) }()
	pe.respond(t, 1, "tunnel.event")
	if r, err := stream.Recv(); err != nil || r.EventType != "tunnel.event" {
		t.Fatalf("recv: %v, %+v", err, r)
	}
	if r, err := stream.Recv(); err != nil || !r.Done {
		t.Fatalf("recv done: %v, %+v", err, r)
	}

	// 隧道插件不受 CheckOffline 影响（LastHeartbeat 拨老也不判离线）
	s.mu.Lock()
	s.plugins[resp.GetInstanceId()].LastHeartbeat = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	s.CheckOffline(time.Second)
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); !ok {
		t.Fatal("tunnel plugin must not be killed by heartbeat CheckOffline")
	}

	// 断开隧道：offline 事件 + 不可查
	events, unsub := s.Subscribe()
	defer unsub()
	p.close()
	waitEvent(t, events, PluginEventOffline)
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); ok {
		t.Fatal("tunnel plugin should be offline after disconnect")
	}

	// 重连：重新注册（替换旧实例）→ Connect → 重新绑定可用
	cancel()
	resp2, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if resp2.GetInstanceId() == resp.GetInstanceId() {
		t.Fatal("re-register should allocate a new instance")
	}
	ctx2, cancel2 := context.WithCancel(ownerCtx("alice"))
	defer cancel2()
	p2 := newTunnelHubPipe(ctx2)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p2}) }()
	waitTunnelBound(t, s, resp2.GetInstanceId())
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); !ok {
		t.Fatal("re-registered tunnel plugin should be findable after reconnect")
	}
}

// TestTunnelConnectBeforeRegister 覆盖 Connect 先于 Register 的时序：
// pending 隧道在注册到达时被认领。
func TestTunnelConnectBeforeRegister(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	ctx, cancel := context.WithCancel(ownerCtx("alice"))
	defer cancel()
	p := newTunnelHubPipe(ctx)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p}) }()

	// 等 pending 出现（connect hook 已入队）
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		n := len(s.tunnelPending["alice"])
		s.mu.RUnlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending tunnel client not queued")
		}
		time.Sleep(5 * time.Millisecond)
	}

	resp, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatalf("tunnel register: %v", err)
	}
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); !ok {
		t.Fatal("register after connect should bind the pending tunnel immediately")
	}
	_ = resp
}

// TestAnonymousTunnelRegisterUnchanged 验证匿名（本地单机）隧道注册同样可用：
// owner 为空时注册与查找键均为裸名。
func TestAnonymousTunnelRegisterUnchanged(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	resp, err := s.Register(context.Background(), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatalf("anonymous tunnel register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p}) }()
	waitTunnelBound(t, s, resp.GetInstanceId())
	if _, _, ok := s.FindByNameFor("", "shared-decoder"); !ok {
		t.Fatal("anonymous tunnel plugin should be findable by bare name")
	}
}

// waitTunnelBound 轮询等待指定实例的隧道绑定完成（在线且 Client 非空）。
func waitTunnelBound(t *testing.T, s *RegistryServer, instanceID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		rp, ok := s.plugins[instanceID]
		bound := ok && rp.Client != nil && rp.Online.Load()
		s.mu.RUnlock()
		if bound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel not bound to instance %s in time", instanceID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitEvent 等待指定类型事件。
func waitEvent(t *testing.T, ch <-chan PluginEvent, typ PluginEventType) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				return
			}
		case <-deadline:
			t.Fatalf("event %s not received in time", typ)
		}
	}
}

// TestTunnelUnboundReaped 验证从未绑定隧道的注册（插件崩溃在 Connect 前/
// 从未 Connect）超过宽限期（2×timeout）后被 CheckOffline 回收。
func TestTunnelUnboundReaped(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	resp, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 未超宽限：仍在表
	s.CheckOffline(50 * time.Millisecond)
	if _, ok := s.plugins[resp.GetInstanceId()]; !ok {
		t.Fatal("unbound tunnel registration should survive within grace period")
	}

	// 推进时间超过 2×timeout：应被回收并推 deregister 事件
	events, unsub := s.Subscribe()
	defer unsub()
	s.mu.Lock()
	s.plugins[resp.GetInstanceId()].LastHeartbeat = time.Now().Add(-time.Second)
	s.mu.Unlock()
	s.CheckOffline(50 * time.Millisecond)
	if _, ok := s.plugins[resp.GetInstanceId()]; ok {
		t.Fatal("unbound tunnel registration should be reaped after grace period")
	}
	waitEvent(t, events, PluginEventDeregister)
}

// TestTunnelBoundDeadClientOffline 兜底路径：绑定后断开钩子若丢失，
// CheckOffline 也应通过 tunnelClientClosed 把实例判离线。
func TestTunnelBoundDeadClientOffline(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	resp, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ownerCtx("alice"))
	p := newTunnelHubPipe(ctx)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p}) }()
	waitTunnelBound(t, s, resp.GetInstanceId())

	events, unsub := s.Subscribe()
	defer unsub()
	p.close()
	// 不依赖断开钩子（钩子本身也会触发，这里断言最终一致：判离线即可）
	waitEvent(t, events, PluginEventOffline)
	s.CheckOffline(time.Second)
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); ok {
		t.Fatal("tunnel plugin with dead session should be offline")
	}
	cancel()
}

// TestHeartbeatIgnoredForTunnel 验证隧道实例收到心跳不复活：
// 会话已死的离线实例收到 Heartbeat 不会回到在线。
func TestHeartbeatIgnoredForTunnel(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	resp, err := s.Register(ownerCtx("alice"), &pb.RegisterRequest{
		Tunnel:   true,
		Manifest: []byte(sharedManifest),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ownerCtx("alice"))
	p := newTunnelHubPipe(ctx)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p}) }()
	waitTunnelBound(t, s, resp.GetInstanceId())
	p.close()
	// 等断开钩子判离线
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		offline := !s.plugins[resp.GetInstanceId()].Online.Load()
		s.mu.RUnlock()
		if offline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("plugin not marked offline after tunnel close")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := s.Heartbeat(context.Background(), &pb.HeartbeatRequest{InstanceId: resp.GetInstanceId()}); err != nil {
		t.Fatalf("heartbeat should be acked (ignored), got %v", err)
	}
	if _, _, ok := s.FindByNameFor("alice", "shared-decoder"); ok {
		t.Fatal("heartbeat must not resurrect a tunnel instance with a dead session")
	}
	cancel()
}

// TestCloseQuiescesTunnelHooks 验证 Close 后迟到的 Connect 钩子不会重新
// 填充 tunnelPending（注册表已关闭，绑定/入队为 no-op）。
func TestCloseQuiescesTunnelHooks(t *testing.T) {
	s := NewRegistryServer(10)
	s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTunnelHubPipe(ctx)
	go func() { _ = s.tunnelHub.Connect(hubEnd{p}) }()
	time.Sleep(100 * time.Millisecond)

	s.mu.RLock()
	pending := len(s.tunnelPending[""])
	s.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("hooks must be no-op after Close, got %d pending clients", pending)
	}
}

// TestConcurrentRegisterAndLookup 压力验证 owner 作用域查找与注册并发安全
// （resolvePluginKey 曾在无锁状态下读 byName，go test -race / 手动压力可复现）。
func TestConcurrentRegisterAndLookup(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			owner := string(rune('a' + n%4))
			for j := 0; j < 50; j++ {
				_, _ = s.Register(ownerCtx(owner), &pb.RegisterRequest{
					SocketPath: "unix:/nonexistent/decoder.sock",
					Manifest:   []byte(sharedManifest),
				})
				_, _, _ = s.FindByNameFor(owner, "shared-decoder")
				_, _, _ = s.Find("tcp")
				_, _ = s.GetPluginManifestFor(owner, "shared-decoder")
				s.List()
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
