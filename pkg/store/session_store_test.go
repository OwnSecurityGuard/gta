package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestControlStore_CRUD(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	// Create
	meta := SessionMeta{
		SessionID: "sess-1",
		StartedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		Status:    "running",
		Port:      8887,
		Plugin:    "tcp",
		Interface: "eth0",
		DBPath:    "/tmp/sess-1/capture.sqlite",
		Extra:     map[string]interface{}{"note": "test session"},
	}
	if err := cs.CreateSession(ctx, meta); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Get
	got, err := cs.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.SessionID != "sess-1" || got.Status != "running" || got.Port != 8887 {
		t.Errorf("got = %+v, want sess-1/running/8887", got)
	}
	if got.Extra["note"] != "test session" {
		t.Errorf("Extra[note] = %v, want 'test session'", got.Extra["note"])
	}

	// Update
	stopped := time.Date(2026, 8, 1, 10, 5, 0, 0, time.UTC)
	meta.Status = "stopped"
	meta.StoppedAt = &stopped
	meta.Events = 42
	if err := cs.UpdateSession(ctx, meta); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got2, err := cs.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetSession after update: %v", err)
	}
	if got2.Status != "stopped" || got2.Events != 42 {
		t.Errorf("after update: status=%q events=%d, want stopped/42", got2.Status, got2.Events)
	}
	if got2.StoppedAt == nil || !got2.StoppedAt.Equal(stopped) {
		t.Errorf("stopped_at = %v, want %v", got2.StoppedAt, stopped)
	}

	// List
	metas, err := cs.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 || metas[0].SessionID != "sess-1" {
		t.Errorf("ListSessions = %d items, want 1 with sess-1", len(metas))
	}

	// Delete
	if err := cs.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := cs.GetSession(ctx, "sess-1"); err == nil {
		t.Error("GetSession after delete: expected error, got nil")
	}
}

func TestControlStore_UpdateNonExistent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	err = cs.UpdateSession(ctx, SessionMeta{SessionID: "no-such", Status: "stopped"})
	if err == nil {
		t.Error("UpdateSession non-existent: expected error, got nil")
	}
}

// 确保 ControlStore 只承载控制面元数据，不混入任何事件/解码数据表。
// 白名单：sessions（会话元数据）、plugin_debug_access（sample_bytes 审计，设计 §6）。
func TestControlStore_OnlyControlPlaneTables(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	rows, err := cs.DB().QueryContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	allowed := map[string]bool{
		"sessions":            true,
		"plugin_debug_access": true,
		"sqlite_sequence":     true, // AUTOINCREMENT 自动生成
	}
	for _, name := range tables {
		if !allowed[name] {
			t.Errorf("unexpected table %q in control store", name)
		}
	}
	// 审计表必须存在，否则 sample_bytes 无处落账。
	var seenAudit bool
	for _, name := range tables {
		if name == "plugin_debug_access" {
			seenAudit = true
		}
	}
	if !seenAudit {
		t.Error("plugin_debug_access table missing from control store")
	}
}

// ReconcileRunningSessions 应仅把上一轮残留的 running 会话置为 stopped，
// 保留其余字段（started_at / 统计 / extra），且不触碰已 stopped 的会话。
func TestControlStore_ReconcileRunningSessions(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	start := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	// 两个运行中会话（其中一个带 extra 字段，验证不应被清空）
	running1 := SessionMeta{
		SessionID: "r1", StartedAt: start, Status: "running", Port: 8887,
		DBPath: "/tmp/r1/capture.sqlite", Extra: map[string]interface{}{"k": "keep"},
	}
	running2 := SessionMeta{
		SessionID: "r2", StartedAt: start, Status: "running", Port: 8888,
		DBPath: "/tmp/r2/capture.sqlite",
	}
	// 一个早已停止的会话，不应被改动
	stoppedAt := time.Date(2026, 8, 1, 10, 5, 0, 0, time.UTC)
	stopped := SessionMeta{
		SessionID: "s1", StartedAt: start, StoppedAt: &stoppedAt, Status: "stopped",
		Port: 8889, DBPath: "/tmp/s1/capture.sqlite",
	}
	for _, m := range []SessionMeta{running1, running2, stopped} {
		if err := cs.CreateSession(ctx, m); err != nil {
			t.Fatalf("CreateSession %s: %v", m.SessionID, err)
		}
	}

	reconciledAt := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	n, err := cs.ReconcileRunningSessions(ctx, reconciledAt)
	if err != nil {
		t.Fatalf("ReconcileRunningSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("affected rows = %d, want 2", n)
	}

	// r1 / r2 应变为 stopped 且 stopped_at 已写入
	for _, id := range []string{"r1", "r2"} {
		got, err := cs.GetSession(ctx, id)
		if err != nil {
			t.Fatalf("GetSession %s: %v", id, err)
		}
		if got.Status != "stopped" {
			t.Errorf("%s status = %q, want stopped", id, got.Status)
		}
		if got.StoppedAt == nil || !got.StoppedAt.Equal(reconciledAt) {
			t.Errorf("%s stopped_at = %v, want %v", id, got.StoppedAt, reconciledAt)
		}
	}
	// r1 的 extra 应被保留
	r1, _ := cs.GetSession(ctx, "r1")
	if r1.Extra["k"] != "keep" {
		t.Errorf("r1 extra[k] = %v, want 'keep' (must be preserved)", r1.Extra["k"])
	}
	// stopped 会话保持不变
	s1, err := cs.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession s1: %v", err)
	}
	if s1.Status != "stopped" || s1.StoppedAt == nil || !s1.StoppedAt.Equal(stoppedAt) {
		t.Errorf("s1 不应被 reconcile 改动: status=%q stopped_at=%v", s1.Status, s1.StoppedAt)
	}
}

// ===== T9: SessionMeta.Owner / owner 过滤 / 老库迁移 =====

// 老库（无 owner 列）打开时应自动补列并回填 ''，既有会话归匿名且可正常读写。
func TestControlStore_MigrateLegacyDBAddsOwner(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	// 用旧 schema 手工建库：没有 owner 列，且预置一行会话
	legacy, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := `
CREATE TABLE sessions (
    session_id    TEXT PRIMARY KEY,
    started_at    DATETIME NOT NULL,
    stopped_at    DATETIME,
    status        TEXT NOT NULL,
    port          INTEGER,
    plugin        TEXT,
    interface     TEXT,
    pcap_file     TEXT,
    raw_packets   INTEGER DEFAULT 0,
    events        INTEGER DEFAULT 0,
    metrics       INTEGER DEFAULT 0,
    decode_errors INTEGER DEFAULT 0,
    duration_sec  REAL,
    db_path       TEXT NOT NULL,
    extra         TEXT,
    manifest_snapshot TEXT DEFAULT ''
);`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO sessions(session_id, started_at, status, port, plugin, interface, pcap_file, duration_sec, db_path)
VALUES ('legacy-1', '2026-08-01 10:00:00', 'stopped', 0, '', '', '', 0, '/tmp/legacy-1/capture.sqlite')`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatalf("NewControlStore on legacy db: %v", err)
	}
	defer cs.Close()
	ctx := context.Background()

	// 迁移后 owner 列存在
	rows, err := cs.DB().QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hasOwner := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "owner" {
			hasOwner = true
		}
	}
	if !hasOwner {
		t.Fatal("owner column missing after migration")
	}

	// 老数据回填 ''（匿名）
	got, err := cs.GetSession(ctx, "legacy-1")
	if err != nil {
		t.Fatalf("GetSession legacy-1: %v", err)
	}
	if got.Owner != "" {
		t.Errorf("legacy row owner = %q, want '' (anonymous)", got.Owner)
	}
	// 匿名过滤器可见
	if _, err := cs.GetSessionFor(ctx, "legacy-1", SessionOwnerFilter{}); err != nil {
		t.Errorf("anonymous GetSessionFor on legacy row: %v", err)
	}
}

// owner 一等字段持久化往返；UpdateSession 不改 owner（owner 不可变）。
func TestControlStore_OwnerRoundTrip(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	if err := cs.CreateSession(ctx, SessionMeta{
		Owner: "alice", SessionID: "s-alice", Status: "running",
		DBPath: "/tmp/s-alice/capture.sqlite",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := cs.GetSession(ctx, "s-alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "alice" {
		t.Errorf("owner = %q, want alice", got.Owner)
	}

	// 更新其他字段不应清空 owner
	got.Status = "stopped"
	if err := cs.UpdateSession(ctx, *got); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got2, _ := cs.GetSession(ctx, "s-alice")
	if got2.Owner != "alice" {
		t.Errorf("owner after update = %q, want alice (owner is immutable)", got2.Owner)
	}
	if got2.Status != "stopped" {
		t.Errorf("status after update = %q, want stopped", got2.Status)
	}
}

// alice 看不到 bob 的会话；匿名只看匿名；admin 看全部；无过滤（ListSessions）等价 admin。
func TestControlStore_OwnerFilter(t *testing.T) {
	db := filepath.Join(t.TempDir(), "control.db")
	cs, err := NewControlStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	metas := []SessionMeta{
		{Owner: "alice", SessionID: "a1", StartedAt: start, Status: "stopped", DBPath: "/a1"},
		{Owner: "alice", SessionID: "a2", StartedAt: start, Status: "stopped", DBPath: "/a2"},
		{Owner: "bob", SessionID: "b1", StartedAt: start, Status: "stopped", DBPath: "/b1"},
		{Owner: "", SessionID: "anon1", StartedAt: start, Status: "stopped", DBPath: "/anon1"},
	}
	for _, m := range metas {
		if err := cs.CreateSession(ctx, m); err != nil {
			t.Fatalf("CreateSession %s: %v", m.SessionID, err)
		}
	}

	assertIDs := func(name string, got []SessionMeta, want ...string) {
		t.Helper()
		idSet := map[string]bool{}
		for _, m := range got {
			idSet[m.SessionID] = true
		}
		if len(idSet) != len(want) {
			t.Errorf("%s: got %v, want %v", name, idSet, want)
		}
		for _, w := range want {
			if !idSet[w] {
				t.Errorf("%s: missing %s (got %v)", name, w, idSet)
			}
		}
	}

	alice, err := cs.ListSessionsFor(ctx, SessionOwnerFilter{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("alice", alice, "a1", "a2")

	bob, err := cs.ListSessionsFor(ctx, SessionOwnerFilter{Owner: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("bob", bob, "b1")

	anon, err := cs.ListSessionsFor(ctx, SessionOwnerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("anonymous", anon, "anon1")

	admin, err := cs.ListSessionsFor(ctx, SessionOwnerFilter{Owner: "alice", AllOwners: true})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("admin", admin, "a1", "a2", "b1", "anon1")

	all, err := cs.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("ListSessions (unfiltered)", all, "a1", "a2", "b1", "anon1")

	// GetSessionFor：alice 不能读 bob 的会话（按未找到处理）
	if _, err := cs.GetSessionFor(ctx, "b1", SessionOwnerFilter{Owner: "alice"}); err == nil {
		t.Error("alice reading bob's session: expected error")
	}
	if _, err := cs.GetSessionFor(ctx, "b1", SessionOwnerFilter{Owner: "bob"}); err != nil {
		t.Errorf("bob reading own session: %v", err)
	}
	if _, err := cs.GetSessionFor(ctx, "b1", SessionOwnerFilter{AllOwners: true}); err != nil {
		t.Errorf("admin reading bob's session: %v", err)
	}
}

func TestSessionProjectIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewControlStore(filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatalf("NewControlStore: %v", err)
	}
	defer cs.Close()
	meta := SessionMeta{
		ProjectID: "20260831_120000.000",
		SessionID: "sess-1",
		StartedAt: time.Now(),
		Status:    "running",
		DBPath:    filepath.Join(dir, "sess.db"),
	}
	if err := cs.CreateSession(context.Background(), meta); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := cs.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ProjectID != meta.ProjectID {
		t.Fatalf("ProjectID roundtrip = %q, want %q", got.ProjectID, meta.ProjectID)
	}
}
