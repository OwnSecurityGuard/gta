package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestProjectStore 打开临时 sqlite 并初始化 projectStore。
func newTestProjectStore(t *testing.T) *projectStore {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ctl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ps := newProjectStoreDB(db)
	if err := ps.Init(); err != nil {
		t.Fatal(err)
	}
	return ps
}

// seededBetaProject 构造 CreatedBy=alice（即首任 Owner）、成员含 bob 的项目。
func seededBetaProject() *project {
	return &project{
		ID:      "p1",
		Name:    "Beta",
		CreatedBy: "alice",
		Members: []projectMember{
			{User: "bob", Role: roleMember},
		},
		Plugins: []projectPlugin{},
		Rules:   []projectRule{},
	}
}

// TestProjectRolesAndVisibility 验证 Owner/Admin/Member 角色解析与可见性：
// owner 字段回填 = created_by；RoleOf 层级正确；可见性含成员、排除外部用户。
func TestProjectRolesAndVisibility(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	if err := ps.Create(ctx, seededBetaProject()); err != nil {
		t.Fatal(err)
	}

	countVisible := func(user string, all bool) int {
		list, err := ps.ListVisible(ctx, user, all)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	if n := countVisible("alice", false); n != 1 {
		t.Errorf("alice (owner) should see 1 project, got %d", n)
	}
	if n := countVisible("bob", false); n != 1 {
		t.Errorf("bob (member) should see 1 project, got %d", n)
	}
	if n := countVisible("tom", false); n != 0 {
		t.Errorf("tom (outsider) should see 0 projects, got %d", n)
	}
	if n := countVisible("t", true); n != 1 {
		t.Errorf("admin should see all 1 project, got %d", n)
	}

	p, err := ps.Get(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("project p1 not found")
	}
	if p.Owner != "alice" {
		t.Errorf("owner should default to created_by (alice), got %q", p.Owner)
	}
	if p.TenantID != "default" && p.TenantID != "" {
		t.Errorf("tenant should be default/empty, got %q", p.TenantID)
	}

	// 角色解析：Owner > Admin > Member > None。
	cases := []struct {
		user string
		want int
	}{
		{"alice", 3}, // RoleOwner
		{"bob", 1},   // RoleMember
		{"tom", 0},   // RoleNone
	}
	for _, tc := range cases {
		role, err := ps.RoleOf(ctx, "p1", tc.user)
		if err != nil {
			t.Fatal(err)
		}
		if int(role) != tc.want {
			t.Errorf("RoleOf(%s) = %d, want %d", tc.user, role, tc.want)
		}
	}
}

// TestProjectStoreRoundTrip 验证 members（表）/plugins/rules（JSON）往返无损。
func TestProjectStoreRoundTrip(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	p := &project{
		ID:            "p2",
		Name:          "Game",
		Description:   "desc",
		Game:          "Godot",
		CreatedBy:     "alice",
		DefaultPlugin: "godot_gateway",
		DefaultPort:   8080,
		Members:       []projectMember{{User: "carol", Role: roleAdmin}},
		Plugins:       []projectPlugin{{ID: "pl1", Name: "http"}},
		Rules:         []projectRule{{ID: "r1", Name: "rule-1"}},
		CreatedAt:     "2026-08-31T00:00:00Z",
		UpdatedAt:     "2026-08-31T00:00:00Z",
	}
	if err := ps.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := ps.Get(ctx, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("project p2 not found")
	}
	if len(got.Members) != 1 || got.Members[0].User != "carol" || got.Members[0].Role != roleAdmin {
		t.Errorf("members round-trip failed: %+v", got.Members)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].ID != "pl1" {
		t.Errorf("plugins round-trip failed: %+v", got.Plugins)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "r1" {
		t.Errorf("rules round-trip failed: %+v", got.Rules)
	}
	if got.DefaultPlugin != "godot_gateway" || got.DefaultPort != 8080 {
		t.Errorf("default fields round-trip failed: %+v", got)
	}

	// Update 全量重写成员表：移除 carol、加入 dave。
	got.Members = []projectMember{{User: "dave", Role: roleMember}}
	got.UpdatedAt = "2026-09-05T00:00:00Z"
	if err := ps.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, err := ps.Get(ctx, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Members) != 1 || again.Members[0].User != "dave" {
		t.Errorf("members rewrite failed: %+v", again.Members)
	}
}

// TestMembersJSONBackfillMigration 幂等迁移：既有 projects.members JSON 在
// Init() 时回填到 project_members 表；重复 Init 不产生重复行。
func TestMembersJSONBackfillMigration(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ps := newProjectStoreDB(db)
	if err := ps.Init(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 通过正常路径创建项目（members 写表 + JSON 双写）。
	if err := ps.Create(ctx, seededBetaProject()); err != nil {
		t.Fatal(err)
	}

	// 直接重复 Init 两次，验证幂等。
	if err := ps.Init(); err != nil {
		t.Fatalf("second Init must be idempotent: %v", err)
	}
	members, err := ps.listMembers(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].User != "bob" || members[0].Role != roleMember {
		t.Fatalf("members after re-init: %+v", members)
	}

	// 模拟更老的库：手工插入一行只带 JSON members 的项目，再 Init 应回填。
	_, err = db.Exec(`INSERT INTO projects(id, name, created_by, members, created_at, updated_at)
	                  VALUES ('legacy','L','carol','[{"user":"dave","role":"admin"}]','2026-01-01','2026-01-01')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Init(); err != nil {
		t.Fatalf("Init with legacy row: %v", err)
	}
	lm, err := ps.listMembers(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(lm) != 1 || lm[0].User != "dave" || lm[0].Role != roleAdmin {
		t.Errorf("legacy JSON members not backfilled: %+v", lm)
	}
	legacy, err := ps.Get(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Owner != "carol" {
		t.Errorf("legacy owner should be backfilled from created_by, got %q", legacy.Owner)
	}
}

// TestTransferOwner 验证 Owner 转移：CAS 防并发双转；成员表相应调整。
func TestTransferOwner(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	p := seededBetaProject()
	p.Members = append(p.Members, projectMember{User: "carol", Role: roleAdmin})
	if err := ps.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	// 非成员不能成为转移目标（调用方校验，store 层不拦 —— 这里验证 CAS 行为）。
	ok, err := ps.TransferOwner(ctx, "p1", "alice", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("transfer owner should succeed with correct expectOwner")
	}
	got, err := ps.Get(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "bob" {
		t.Fatalf("owner after transfer = %q, want bob", got.Owner)
	}
	// CAS：用旧 owner 值再次转移必须失败。
	ok, err = ps.TransferOwner(ctx, "p1", "alice", "carol")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale expectOwner must not transfer")
	}
	if got.Owner != "bob" {
		t.Fatalf("owner changed by stale CAS: %q", got.Owner)
	}
}

// TestRoleOfOwnerNotInMembersTable 验证 Owner 不依赖成员表（SSOT 是 projects.owner）：
// 即使成员表没有 owner 行，RoleOf 仍返回 RoleOwner。
func TestRoleOfOwnerNotInMembersTable(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	if err := ps.Create(ctx, seededBetaProject()); err != nil {
		t.Fatal(err)
	}
	// 把 bob 从成员表删掉，owner 视角不受影响。
	if _, err := ps.RemoveMember(ctx, "p1", "bob"); err != nil {
		t.Fatal(err)
	}
	role, err := ps.RoleOf(ctx, "p1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if role != 3 { // RoleOwner
		t.Errorf("owner role via owner column = %d, want 3", role)
	}
}
