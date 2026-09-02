package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/store"
)

// makeRetentionSession 在 sm 下创建一个带指定属性的会话：
// mtime 控制数据写入活跃度（touch 将 capture.sqlite mtime 设为指定时间）。
func makeRetentionSession(t *testing.T, sm *sessionManager, id string, status string, touch time.Time) sessionMetadata {
	t.Helper()
	dir := sm.sessionDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "capture.sqlite")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := sessionMetadata{
		SessionID: id,
		StartedAt: touch.Format(time.RFC3339),
		Status:    status,
		DBPath:    db,
	}
	if status == "stopped" {
		meta.StoppedAt = touch.Format(time.RFC3339)
	}
	if err := sm.writeSessionMetadata(id, meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(db, touch, touch); err != nil {
		t.Fatal(err)
	}
	// 目录 mtime 同步回拨，避免 MkdirAll 的当前时间让会话"看起来活跃"。
	if err := os.Chtimes(dir, touch, touch); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestEnforceRetention_TTLDeletesStaleSessions(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	old := time.Now().Add(-10 * 24 * time.Hour)
	fresh := time.Now().Add(-time.Hour)

	makeRetentionSession(t, sm, "20200101_000000.000", "stopped", old)
	makeRetentionSession(t, sm, "20260830_000000.000", "stopped", fresh)

	deleted := sm.enforceRetention(retentionPolicy{TTL: 7 * 24 * time.Hour})

	if len(deleted) != 1 || deleted[0] != "20200101_000000.000" {
		t.Fatalf("expected stale session deleted, got %v", deleted)
	}
	if _, err := os.Stat(sm.sessionDir("20200101_000000.000")); !os.IsNotExist(err) {
		t.Fatal("stale session dir should be removed")
	}
	if _, err := os.Stat(sm.sessionDir("20260830_000000.000")); err != nil {
		t.Fatal("fresh session dir should survive")
	}
}

func TestEnforceRetention_TTLCoversStaleRunningSession(t *testing.T) {
	// 崩溃残留 / agent 下载后从未上报的会话 status 仍是 running，
	// 但长期无写入活跃度 → 超期必须清理（这正是无限膨胀的来源之一）。
	sm := newSessionManager(t.TempDir())
	stale := time.Now().Add(-30 * 24 * time.Hour)
	makeRetentionSession(t, sm, "20200101_000000.000", "running", stale)

	deleted := sm.enforceRetention(retentionPolicy{TTL: 7 * 24 * time.Hour})
	if len(deleted) != 1 {
		t.Fatalf("expected stale running session deleted, got %v", deleted)
	}
}

func TestEnforceRetention_TTLSkipsActiveCapture(t *testing.T) {
	// 运行中且持续写入（mtime 新）的会话受活跃度保护，TTL 不删。
	sm := newSessionManager(t.TempDir())
	makeRetentionSession(t, sm, "20260830_000000.000", "running", time.Now().Add(-time.Minute))

	deleted := sm.enforceRetention(retentionPolicy{TTL: 7 * 24 * time.Hour})
	if len(deleted) != 0 {
		t.Fatalf("expected no deletion, got %v", deleted)
	}
}

func TestEnforceRetention_MaxSessionsKeepsNewest(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		id := time.Now().Add(time.Duration(i) * time.Minute).Format(sessionIDLayout)
		makeRetentionSession(t, sm, id, "stopped", base.Add(time.Duration(i)*time.Minute))
	}

	// 保留最新 3 个：最旧 2 个被删。
	deleted := sm.enforceRetention(retentionPolicy{MaxSessions: 3})
	if len(deleted) != 2 {
		t.Fatalf("expected 2 deletions, got %v", deleted)
	}

	sessions, err := sm.listSessions(store.SessionOwnerFilter{AllOwners: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 survivors, got %d", len(sessions))
	}
	// 幸存者应是最新 3 个（StartedAt 最大）。
	for _, s := range sessions {
		if s.StartedAt == base.Format(time.RFC3339) {
			t.Fatalf("oldest session should have been deleted, got %v", s.SessionID)
		}
	}
}

func TestEnforceRetention_MaxSessionsSkipsRunning(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	base := time.Now().Add(-time.Hour)
	// 4 个 stopped + 1 个 running（无 TTL，仅数量上限），running 最新。
	ids := []string{}
	for i := 0; i < 5; i++ {
		id := time.Now().Add(time.Duration(i) * time.Minute).Format(sessionIDLayout)
		status := "stopped"
		if i == 4 {
			status = "running"
		}
		makeRetentionSession(t, sm, id, status, base.Add(time.Duration(i)*time.Minute))
		ids = append(ids, id)
	}

	// MaxSessions=2：running 不参与数量裁剪，溢出的 stopped 被删。
	deleted := sm.enforceRetention(retentionPolicy{MaxSessions: 2})
	runningID := ids[4]
	for _, id := range deleted {
		if id == runningID {
			t.Fatalf("running session %s should not be deleted by max-sessions trim", id)
		}
	}
	if _, err := os.Stat(sm.sessionDir(runningID)); err != nil {
		t.Fatalf("running session dir must survive: %v", err)
	}
}

func TestEnforceRetention_ResetsCurrentPointer(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	stale := time.Now().Add(-30 * 24 * time.Hour)
	meta := makeRetentionSession(t, sm, "20200101_000000.000", "stopped", stale)

	// 该会话是某 owner 的 current 指针目标。
	shard := sm.currentPathFor(meta.Owner)
	if err := sm.writeCurrent(meta); err != nil {
		t.Fatal(err)
	}

	deleted := sm.enforceRetention(retentionPolicy{TTL: 7 * 24 * time.Hour})
	if len(deleted) != 1 {
		t.Fatalf("expected deletion, got %v", deleted)
	}
	data, err := os.ReadFile(shard)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Fatalf("current shard should be reset to {}, got %q", data)
	}
}

func TestEnforceRetention_DisabledPolicyNoop(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	stale := time.Now().Add(-365 * 24 * time.Hour)
	makeRetentionSession(t, sm, "20200101_000000.000", "stopped", stale)

	if deleted := sm.enforceRetention(retentionPolicy{}); len(deleted) != 0 {
		t.Fatalf("expected no deletion when policy disabled, got %v", deleted)
	}
	if _, err := os.Stat(sm.sessionDir("20200101_000000.000")); err != nil {
		t.Fatal("session dir should survive")
	}
}

func TestEnforceRetention_MetadataMissingFallsBackToFileTimes(t *testing.T) {
	// 历史会话可能没有 metadata.json：readSessionMetadata 用 capture.sqlite 的
	// mtime 合成元数据（status=stopped），TTL 仍能正确判定并清理。
	sm := newSessionManager(t.TempDir())
	id := "20200101_000000.000"
	dir := sm.sessionDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "capture.sqlite")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(db, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	deleted := sm.enforceRetention(retentionPolicy{TTL: 7 * 24 * time.Hour})
	if len(deleted) != 1 || deleted[0] != id {
		t.Fatalf("expected legacy session (no metadata.json) deleted, got %v", deleted)
	}
}

func TestSessionActivity_UsesAllSignals(t *testing.T) {
	sm := newSessionManager(t.TempDir())
	now := time.Now()
	older := now.Add(-48 * time.Hour)

	// started_at/stopped_at 很旧，但 db mtime 是新 → 活跃。
	meta := makeRetentionSession(t, sm, "20200101_000000.000", "stopped", older)
	if err := os.Chtimes(sm.dbPath(meta.SessionID), now, now); err != nil {
		t.Fatal(err)
	}
	if got := sm.sessionActivity(meta); now.Sub(got) > time.Minute {
		t.Fatalf("activity should follow db mtime, got %v", got)
	}
}
