package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestControlStore(t *testing.T) *ControlStore {
	t.Helper()
	cs, err := NewControlStore(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("NewControlStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestRecordAndListDebugAccess(t *testing.T) {
	ctx := context.Background()
	cs := newTestControlStore(t)

	id, err := cs.RecordDebugAccess(ctx, DebugAccess{
		At:               time.Now(),
		Actor:            "pipeline",
		Tool:             "sample_bytes",
		Plugin:           "http",
		SessionID:        "s1",
		RequestedPackets: 20,
		ReturnedPackets:  12,
		ReturnedBytes:    384,
		Truncated:        true,
	})
	if err != nil {
		t.Fatalf("RecordDebugAccess: %v", err)
	}
	if id <= 0 {
		t.Fatalf("audit id = %d, want > 0", id)
	}

	// 第二条：未截断。
	if _, err := cs.RecordDebugAccess(ctx, DebugAccess{
		Actor: "pipeline", Tool: "sample_bytes", Plugin: "http",
		SessionID: "s1", RequestedPackets: 5, ReturnedPackets: 5, ReturnedBytes: 100,
	}); err != nil {
		t.Fatalf("RecordDebugAccess 2: %v", err)
	}

	rows, err := cs.DebugAccesses(ctx, "s1")
	if err != nil {
		t.Fatalf("DebugAccesses: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// 最新在前。
	first := rows[0]
	if first.ReturnedPackets != 5 || first.Truncated {
		t.Errorf("first row = %+v, want returned=5 truncated=false", first)
	}
	second := rows[1]
	if second.ReturnedPackets != 12 || !second.Truncated {
		t.Errorf("second row = %+v, want returned=12 truncated=true", second)
	}
}

func TestDebugAccessRecordsRealReturnedValues(t *testing.T) {
	// 审计记真实返回量（设计 §6）：请求 20、实际返回 3、截断。
	ctx := context.Background()
	cs := newTestControlStore(t)
	id, err := cs.RecordDebugAccess(ctx, DebugAccess{
		Actor: "pipeline", Tool: "sample_bytes", SessionID: "s2",
		RequestedPackets: 20, ReturnedPackets: 3, ReturnedBytes: 64, Truncated: true,
	})
	if err != nil {
		t.Fatalf("RecordDebugAccess: %v", err)
	}
	rows, err := cs.DebugAccesses(ctx, "s2")
	if err != nil {
		t.Fatalf("DebugAccesses: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("rows = %+v, want single id %d", rows, id)
	}
	if rows[0].RequestedPackets != 20 || rows[0].ReturnedPackets != 3 || rows[0].ReturnedBytes != 64 {
		t.Errorf("audit row = %+v, want requested=20 returned=3 bytes=64", rows[0])
	}
}
