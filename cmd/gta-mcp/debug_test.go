package main

import (
	"context"
	"os"
	"testing"
	"time"

	"gta/pkg/event"
	"gta/pkg/store"
)

// TestDebug_TimeFormat 验证时间戳格式与查询匹配。
func TestDebug_TimeFormat(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	sessionMgr := newSessionManager(workDir)
	sessionID := sessionMgr.generateSessionID()
	sessionDir := sessionMgr.sessionDir(sessionID)
	if err := mkdirAll(sessionDir); err != nil {
		t.Fatal(err)
	}
	st, _ := store.NewSQLiteStore(sessionMgr.absDBPath(sessionID), nil)
	defer st.Close()

	// 写入一条事件（使用 Event）
	now := time.Now()
	payload := event.Value{
		Kind: event.Object,
		Object: map[string]event.Value{
			"src":       {Kind: event.String, Str: "1.1.1.1:80"},
			"dst":       {Kind: event.String, Str: "2.2.2.2:90"},
			"flow_id":   {Kind: event.String, Str: "123"},
			"direction": {Kind: event.String, Str: "client_to_server"},
			"msg_name":  {Kind: event.String, Str: "TestReq"},
		},
	}
	events := []*event.Event{
		{
			Identity: event.Identity{
				ID:        "ev-1",
				SessionID: sessionID,
				Type:      "tcp",
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: now,
			},
			Relation: event.Relation{},
			Context: event.EventContext{
				FlowID:    "123",
				Direction: "client_to_server",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    payload,
			},
		},
	}
	if err := st.AppendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	db := st.DB()

	// 查看存储的时间戳格式
	var storedTS int64
	row := db.QueryRowContext(ctx, "SELECT timestamp FROM events WHERE id=?", "ev-1")
	if err := row.Scan(&storedTS); err != nil {
		t.Fatalf("scan timestamp: %v", err)
	}
	t.Logf("stored timestamp: %q", storedTS)
	t.Logf("now (local): %v", now)
	t.Logf("now.Format(RFC3339Nano): %q", now.Format(time.RFC3339Nano))
	t.Logf("now.UTC().Format(RFC3339Nano): %q", now.UTC().Format(time.RFC3339Nano))

	// 测试不同查询方式
	queries := []struct {
		name string
		from any
		to   any
	}{
		{"local time.Time", now.Add(-time.Hour), now.Add(time.Hour)},
		{"UTC time.Time", now.Add(-time.Hour).UTC(), now.Add(time.Hour).UTC()},
		{"RFC3339Nano local", now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano)},
		{"RFC3339Nano UTC", now.Add(-time.Hour).UTC().Format(time.RFC3339Nano), now.Add(time.Hour).UTC().Format(time.RFC3339Nano)},
	}

	for _, q := range queries {
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM events WHERE session_id=? AND timestamp BETWEEN ? AND ?",
			sessionID, q.from, q.to).Scan(&count)
		t.Logf("query [%s]: count=%d, err=%v", q.name, count, err)
	}

	// 测试不带时间过滤的查询
	var countAll int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE session_id=?", sessionID).Scan(&countAll)
	t.Logf("query [no time filter]: count=%d", countAll)

	// 验证 RunRegistry 持久化后的时间时区
	runRegistry, _ := NewRunRegistry(workDir)
	now2 := time.Now()
	rec := RunRecord{
		RunID: "run_test", SessionID: sessionID, FeatureName: "test",
		ProjectPath: "/tmp", TimeFrom: now2, IsolationMode: "time_window_only",
	}
	runRegistry.Begin(rec)
	loaded, _ := runRegistry.Get("run_test")
	t.Logf("original TimeFrom: %v (tz=%v)", now2, now2.Location())
	t.Logf("loaded  TimeFrom: %v (tz=%v)", loaded.TimeFrom, loaded.TimeFrom.Location())
	t.Logf("loaded == original? %v", loaded.TimeFrom.Equal(now2))

	// 用 loaded.TimeFrom 查询
	var count2 int
	db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM events WHERE session_id=? AND timestamp BETWEEN ? AND ?",
		sessionID, loaded.TimeFrom.Add(-time.Hour), loaded.TimeFrom.Add(time.Hour)).Scan(&count2)
	t.Logf("query with loaded.TimeFrom: count=%d", count2)
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0755)
}
