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

// seededBetaProject 构造 CreatedBy=alice、成员含 bob 的项目。
func seededBetaProject() *project {
	return &project{
		ID:        "p1",
		Name:      "Beta",
		CreatedBy: "alice",
		Members: []projectMember{
			{User: "bob", Role: roleMember},
		},
		Plugins: []projectPlugin{},
		Rules:   []projectRule{},
	}
}

// TestProjectMembershipVisibility 验证成员可见性：创建者与成员可见，外部不可见，
// admin 可见全部；CanManage 仅创建者与 admin 角色成员（非全局 admin）成立。
func TestProjectMembershipVisibility(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	if err := ps.Create(ctx, seededBetaProject()); err != nil {
		t.Fatal(err)
	}

	countVisible := func(owner string, all bool) int {
		list, err := ps.ListVisible(ctx, owner, all)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	if n := countVisible("alice", false); n != 1 {
		t.Errorf("alice (creator) should see 1 project, got %d", n)
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
	if !ps.CanManage(p, "alice", false) {
		t.Error("creator alice should manage project")
	}
	if ps.CanManage(p, "bob", false) {
		t.Error("member bob must not manage project (creator-only)")
	}
}

// TestProjectStoreRoundTrip 验证 members/plugins/rules 以 JSON 列往返无损。
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
}