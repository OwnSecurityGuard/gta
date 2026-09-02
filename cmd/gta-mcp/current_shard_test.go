package main

import (
	"os"
	"path/filepath"
	"testing"

	"gta/pkg/store"
)

// current.<owner>.json 分片：owner A/B 各自独立文件；匿名仍用 current.json（回归底线）。
func TestSessionManager_CurrentSharding(t *testing.T) {
	sm := newSessionManager(t.TempDir())

	if got := sm.currentPathFor(""); got != filepath.Join(sm.workDir, "current.json") {
		t.Errorf("anonymous shard = %q, want current.json", got)
	}
	if got := sm.currentPathFor("alice"); got != filepath.Join(sm.workDir, "current.alice.json") {
		t.Errorf("alice shard = %q, want current.alice.json", got)
	}

	// 匿名写入落到 current.json
	if err := sm.writeCurrent(sessionMetadata{SessionID: "anon-1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sm.workDir, "current.json")); err != nil {
		t.Fatalf("anonymous current.json missing: %v", err)
	}

	// owner A / owner B 分片互不覆盖
	if err := sm.writeCurrent(sessionMetadata{Owner: "alice", SessionID: "a-1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := sm.writeCurrent(sessionMetadata{Owner: "bob", SessionID: "b-1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	a, err := sm.readCurrent("alice")
	if err != nil || a == nil || a.SessionID != "a-1" {
		t.Fatalf("readCurrent(alice) = %v, %v; want a-1", a, err)
	}
	b, err := sm.readCurrent("bob")
	if err != nil || b == nil || b.SessionID != "b-1" {
		t.Fatalf("readCurrent(bob) = %v, %v; want b-1", b, err)
	}
	anon, err := sm.readCurrent("")
	if err != nil || anon == nil || anon.SessionID != "anon-1" {
		t.Fatalf("readCurrent(anon) = %v, %v; want anon-1", anon, err)
	}

	// alice 的分片里读不到 bob 的当前会话
	if a.SessionID == "b-1" || b.SessionID == "a-1" {
		t.Fatal("owner shards leaked into each other")
	}
}

// listSessions 按 owner 过滤：无 owner 字段的历史 metadata.json 视为匿名。
func TestSessionManager_ListSessionsOwnerFilter(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	fixtures := []struct {
		owner, sessionID string
	}{
		{"", "anon-old"}, // 模拟历史会话：metadata.json 无 owner 字段
		{"", "anon-new"},
		{"alice", "a-1"},
		{"bob", "b-1"},
	}
	for _, f := range fixtures {
		dir := sm.sessionDir(f.sessionID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := sessionMetadata{SessionID: f.sessionID, Status: "stopped", StartedAt: "2026-08-01T10:00:00Z"}
		if f.owner != "" {
			meta.Owner = f.owner
		}
		if err := sm.writeSessionMetadata(f.sessionID, meta); err != nil {
			t.Fatal(err)
		}
	}

	alice, err := sm.listSessions(store.SessionOwnerFilter{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 1 || alice[0].SessionID != "a-1" {
		t.Errorf("alice sees %v, want only a-1", alice)
	}

	anon, err := sm.listSessions(store.SessionOwnerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(anon) != 2 {
		t.Errorf("anonymous sees %d sessions, want 2 (anon-old, anon-new)", len(anon))
	}

	admin, err := sm.listSessions(store.SessionOwnerFilter{Owner: "alice", AllOwners: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(admin) != 4 {
		t.Errorf("admin sees %d sessions, want 4", len(admin))
	}
}

// 不安全字符被替换，避免路径注入/文件系统问题。
func TestCurrentShardName_Sanitize(t *testing.T) {
	cases := map[string]string{
		"alice":      "current.alice.json",
		"team/prod":  "current.team_prod.json",
		"o?..\\evil": "current.o____evil.json",
	}
	for owner, want := range cases {
		if got := currentShardName(owner); got != want {
			t.Errorf("currentShardName(%q) = %q, want %q", owner, got, want)
		}
	}
}
