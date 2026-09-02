package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gta/pkg/event"
	"gta/pkg/store"
)

// captureCtxFixture 构造含两个连接、三种流分组形态的代理抓包事件集。
// 时间严格递增，避免并列时间戳下排序实现的差异影响等价性判定。
func captureCtxFixture(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "ctx.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(time.Millisecond)
	mk := func(id, conn, corr string, off time.Duration) *event.Event {
		return &event.Event{
			Identity: event.Identity{
				ID: event.EventID(id), SessionID: "s1", Type: "tcp",
				SchemaID: "tcp.v1", Source: "decoder", Timestamp: base.Add(off),
			},
			Trace:   event.TraceContext{CorrelationID: corr},
			Context: event.EventContext{ConnID: conn, Source: "mobile"},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    event.Value{Kind: event.Object, Object: map[string]event.Value{"n": {Kind: event.Int, Int: 1}}},
			},
		}
	}
	events := []*event.Event{
		mk("e1", "connA", "", 0), // 未关联 → 自成流
		mk("e2", "connA", "c1", time.Millisecond),
		mk("e3", "connA", "c1", 2*time.Millisecond), // 同流后续事件
		mk("e4", "connB", "c2", 3*time.Millisecond),
		mk("e5", "connB", "", 4*time.Millisecond), // connB 的未关联流
		mk("e6", "", "", 5*time.Millisecond),      // 无 conn → 不入 capture 上下文
	}
	if err := s.AppendEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	return s
}

// referenceBuildCaptureContext 是旧实现（基于全量解码事件）的逐行拷贝，
// 作为新 event_index 路径的等价性基准。
func referenceBuildCaptureContext(events []*event.Event) map[string]captureContextJSON {
	out := make(map[string]captureContextJSON, len(events))
	connSeqByID := make(map[string]int)
	streamSeqByConn := make(map[string]int)
	connEvents := make(map[string][]*event.Event)

	for _, ev := range events {
		connID := ev.Context.ConnID
		if connID == "" {
			continue
		}
		if _, ok := connSeqByID[connID]; !ok {
			connSeqByID[connID] = len(connSeqByID) + 1
			streamSeqByConn[connID] = 0
		}
		connEvents[connID] = append(connEvents[connID], ev)
	}

	for connID, evs := range connEvents {
		asc := make([]*event.Event, len(evs))
		for i, ev := range evs {
			asc[len(evs)-1-i] = ev
		}
		seenStream := make(map[string]bool)
		for _, ev := range asc {
			key := ev.Trace.CorrelationID
			if key == "" {
				key = string(ev.Identity.ID)
			}
			if !seenStream[key] {
				seenStream[key] = true
				streamSeqByConn[connID]++
			}
			out[string(ev.Identity.ID)] = captureContextJSON{
				CapturedBy: captureDisplayName(ev.Context.Source),
				ConnID:     connID,
				ConnSeq:    connSeqByID[connID],
				StreamID:   key,
				StreamSeq:  streamSeqByConn[connID],
				Source:     ev.Context.Source,
			}
		}
	}
	return out
}

// TestBuildCaptureContextFromIndex_Equivalence 金标准等价性测试：
// 新路径（event_index 轻量行）必须与旧路径（全量解码事件）产出完全一致的
// 连接序号/流序号/流分组键/来源字段。
func TestBuildCaptureContextFromIndex_Equivalence(t *testing.T) {
	s := captureCtxFixture(t)
	defer s.Close()

	ctx := context.Background()
	full, err := s.QueryEventsDesc(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := referenceBuildCaptureContext(full)

	got := buildCaptureContextFromIndex(ctx, s, "s1")
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("capture context mismatch:\nwant %+v\ngot  %+v", want, got)
	}

	// 语义抽查：connB（最新活跃）序号 1；connA 序号 2；
	// connA 内 e1 自成流 seq=1，e2/e3 共享 c1 流 seq=2。
	if got["e4"].ConnSeq != 1 || got["e5"].ConnSeq != 1 {
		t.Fatalf("connB seq wrong: %+v", got["e4"])
	}
	if got["e1"].ConnSeq != 2 || got["e1"].StreamSeq != 1 || got["e1"].StreamID != "e1" {
		t.Fatalf("e1 wrong: %+v", got["e1"])
	}
	if got["e2"].StreamSeq != 2 || got["e3"].StreamSeq != 2 || got["e2"].StreamID != "c1" {
		t.Fatalf("e2/e3 stream wrong: %+v %+v", got["e2"], got["e3"])
	}
	if got["e4"].StreamSeq != 1 || got["e5"].StreamSeq != 2 {
		t.Fatalf("connB streams wrong: %+v %+v", got["e4"], got["e5"])
	}
	if _, exists := got["e6"]; exists {
		t.Fatal("event without conn_id should not have capture context")
	}
	if got["e1"].CapturedBy != "Mobile Proxy" || got["e1"].Source != "mobile" {
		t.Fatalf("source fields wrong: %+v", got["e1"])
	}
}
