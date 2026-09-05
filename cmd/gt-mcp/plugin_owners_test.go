package main

import (
	"context"
	"testing"

	"gametrace/pkg/auth"
)

// TestPluginOwnersFor 验证项目插件共享的 owner 候选集计算：
//   - 项目成员（member）能看到项目插件条目归属 owner；
//   - 项目 owner 自己也在候选内（但去重）；
//   - 无关用户拿到 nil（= 只能用自己注册的插件）；
//   - 插件条目无归属（老数据）不产生候选。
func TestPluginOwnersFor(t *testing.T) {
	m, _, _ := newInviteMCP(t)
	ctx := context.Background()

	p := &project{
		ID: "p1", Name: "P", CreatedBy: "bob", Owner: "bob",
		Plugins: []projectPlugin{
			{ID: "x1", Name: "godot-ecs", Owner: "bob"},
			{ID: "x2", Name: "legacy"}, // 老数据：无归属
		},
		Members: []projectMember{{User: "carol", Role: roleMember}},
	}
	if err := m.projects.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	// carol 是成员 → 候选 [bob]
	carolCtx := auth.WithPrincipal(ctx, &auth.Principal{Owner: "carol"})
	owners := m.pluginOwnersFor(carolCtx, "carol")
	if len(owners) != 1 || owners[0] != "bob" {
		t.Fatalf("carol owners = %v, want [bob]", owners)
	}

	// bob 自己是 owner → 候选不含自己（去重）= nil
	bobCtx := auth.WithPrincipal(ctx, &auth.Principal{Owner: "bob"})
	if owners := m.pluginOwnersFor(bobCtx, "bob"); owners != nil {
		t.Fatalf("bob owners = %v, want nil", owners)
	}

	// dave 与项目无关 → nil
	if owners := m.pluginOwnersFor(ctx, "dave"); owners != nil {
		t.Fatalf("dave owners = %v, want nil", owners)
	}
}

// TestSetProjectPlugins_RecordsOwner 验证 set_project_plugins 为无归属条目
// 记录设置者身份（幂等回显：已带 owner 的条目不被覆盖）。
func TestSetProjectPlugins_RecordsOwner(t *testing.T) {
	m, _, _ := newInviteMCP(t)
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "bob"})
	req := reqWith("name", "P")
	if _, err := m.handleCreateProject(ctx, req); err != nil {
		t.Fatal(err)
	}
	created, err := m.projects.ListVisible(ctx, "bob", false)
	if err != nil || len(created) != 1 {
		t.Fatalf("list projects: %v %v", created, err)
	}
	pid := created[0].ID

	setReq := reqWith(
		"project_id", pid,
		"plugins", `[{"id":"x1","name":"godot-ecs"},{"id":"x2","name":"other","owner":"alice"}]`,
	)
	if _, err := m.handleSetProjectPlugins(ctx, setReq); err != nil {
		t.Fatal(err)
	}
	got, err := m.projects.Get(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Plugins) != 2 {
		t.Fatalf("plugins = %+v", got.Plugins)
	}
	if got.Plugins[0].Owner != "bob" {
		t.Fatalf("entry without owner should record setter, got %q", got.Plugins[0].Owner)
	}
	if got.Plugins[1].Owner != "alice" {
		t.Fatalf("entry with explicit owner should be preserved, got %q", got.Plugins[1].Owner)
	}
}
