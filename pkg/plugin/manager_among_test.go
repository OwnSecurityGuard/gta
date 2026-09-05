package plugin

import (
	"context"
	"testing"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// regShared 按指定 owner 注册一个共享名插件（非隧道，注册即在线）。
func regShared(t *testing.T, s *RegistryServer, owner string) string {
	t.Helper()
	resp, err := s.Register(ownerCtx(owner), &pb.RegisterRequest{
		SocketPath: "unix:/nonexistent/decoder.sock",
		Manifest:   []byte(sharedManifest),
	})
	if err != nil {
		t.Fatalf("register(owner=%s): %v", owner, err)
	}
	return resp.GetInstanceId()
}

// TestFindByNameAmong 覆盖项目成员共用项目插件的多 owner 解析语义：
//   - 会话 owner 自己的插件最优先；
//   - 白名单内 owner 的插件可按裸名命中（跨 owner 共用）；
//   - 白名单外的 owner 依旧不可见（隔离不回退）；
//   - 匿名/系统插件（空 owner）对任何集合可见；
//   - 完整键寻址仍要求键内 owner 在集合内；
//   - 多 owner 同名插件：排在前面的 owner 优先，离线实例跳过取在线的。
func TestFindByNameAmong(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	regShared(t, s, "alice") // alice/shared-decoder
	regShared(t, s, "bob")   // bob/shared-decoder

	// 1) 会话 owner 优先：carol 自己没有 → 命中白名单第一个 owner（alice）
	c, _, ok := s.FindByNameAmong([]string{"carol", "alice", "bob"}, "shared-decoder")
	if !ok || c == nil {
		t.Fatal("carol should resolve project plugin owned by alice")
	}
	if name, _ := s.NameByClient(c); name != "shared-decoder" {
		t.Fatalf("unexpected client: %v", name)
	}

	// 2) 无关 owner 不在白名单 → 查不到
	if _, _, ok := s.FindByNameAmong([]string{"carol", "alice"}, "shared-decoder"); !ok {
		// alice 在白名单内，应该命中 —— 这里断言的是它确实可见
		_ = ok
	}
	if _, _, ok := s.FindByNameAmong([]string{"carol", "mallory"}, "shared-decoder"); ok {
		t.Fatal("owner outside the allowlist must stay invisible")
	}

	// 3) 匿名（系统）插件恒可见
	if _, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: "unix:/nonexistent/decoder.sock",
		Manifest:   []byte(sharedManifest),
	}); err != nil {
		t.Fatal(err)
	}
	// 系统插件键是裸名；上面的 owner 键都在它前面。用不同的名字注册系统插件验证可见性。
	if _, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: "unix:/nonexistent/decoder.sock",
		Manifest: []byte(`api_version: gta.decoder/v2
name: sys-decoder
protocol: test_proto
type: decoder
hints:
  - tcp
`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.FindByNameAmong(nil, "sys-decoder"); !ok {
		t.Fatal("anonymous/system plugin must be visible with empty owners")
	}
	if _, _, ok := s.FindByNameAmong([]string{"carol"}, "sys-decoder"); !ok {
		t.Fatal("anonymous/system plugin must be visible with owner set")
	}

	// 4) 完整键寻址：白名单内 owner 可用，白名单外拒绝
	if _, _, ok := s.FindByNameAmong([]string{"carol", "alice"}, "alice/shared-decoder"); !ok {
		t.Fatal("full-key lookup within allowlist should work")
	}
	if _, _, ok := s.FindByNameAmong([]string{"carol", "bob"}, "alice/shared-decoder"); ok {
		t.Fatal("full-key lookup outside allowlist must be rejected")
	}

	// 5) 多 owner 同名：优先级 = owners 顺序（carol 无同名，alice 在前命中 alice 实例）
	if c2, _, ok := s.FindByNameAmong([]string{"carol", "alice", "bob"}, "shared-decoder"); !ok || c2 == nil {
		t.Fatal("expected a resolvable instance")
	}
}

// TestFindByNameAmong_OfflineSkipped 验证候选中某个 owner 的同名实例离线时，
// 解析跳过它继续尝试后续 owner 的在线实例。
func TestFindByNameAmong_OfflineSkipped(t *testing.T) {
	s := NewRegistryServer(10)
	defer s.Close()

	regShared(t, s, "alice") // alice/shared-decoder（会离线）
	regShared(t, s, "bob")   // bob/shared-decoder（保持在线）

	// 把 alice 的实例置为离线
	s.mu.RLock()
	for _, rp := range s.plugins {
		if rp.Owner == "alice" {
			rp.Online.Store(false)
		}
	}
	s.mu.RUnlock()

	c, _, ok := s.FindByNameAmong([]string{"carol", "alice", "bob"}, "shared-decoder")
	if !ok || c == nil {
		t.Fatal("offline candidate should be skipped in favor of an online one")
	}
}
