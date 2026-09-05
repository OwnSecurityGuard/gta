package store

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"gametrace/pkg/event"
)

// TestPGIntegration 是针对真实 PostgreSQL 后端的端到端校验，默认跳过。
//
// 运行条件：设置环境变量 GT_TEST_PG_DSN 为可写的 PG 连接串，例如：
//
//	GT_TEST_PG_DSN="postgres://user:pass@localhost:5432/gametrace?sslmode=disable" \
//	  go test ./pkg/store -run TestPGIntegration -v
//
// 该测试验证 PG 方言翻译的正确性：?→$N 占位符、ON CONFLICT  upsert、
// session_id 隔离、information_schema 表结构探测、BIGINT 时间、RETURNING id，
// 以及控制元数据（sessions / plugin_debug_access）与事件数据的双后端一致性。
// 不依赖业务代码，直接走 pkg/store 的工厂与接口。
func TestPGIntegration(t *testing.T) {
	dsn := os.Getenv("GT_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("GT_TEST_PG_DSN not set; skipping live PostgreSQL integration test")
	}
	ctx := context.Background()
	now := time.Now()

	// ---- 控制元数据后端 ----
	ctrl, err := OpenControlStore("postgres", dsn)
	if err != nil {
		t.Fatalf("OpenControlStore(postgres): %v", err)
	}
	defer ctrl.Close()

	s1, s2 := fmt.Sprintf("pg-it-%d-a", now.UnixNano()), fmt.Sprintf("pg-it-%d-b", now.UnixNano())
	meta1 := SessionMeta{
		Owner:     "pg-test",
		SessionID: s1,
		StartedAt: now,
		Status:    "running",
		Port:      8080,
		Plugin:    "tcp",
		DBPath:    "/tmp/" + s1 + "/capture.sqlite",
	}
	if err := ctrl.CreateSession(ctx, meta1); err != nil {
		t.Fatalf("CreateSession s1: %v", err)
	}
	if err := ctrl.CreateSession(ctx, SessionMeta{
		Owner:     "pg-test",
		SessionID: s2,
		StartedAt: now,
		Status:    "running",
		Port:      8081,
		Plugin:    "tcp",
		DBPath:    "/tmp/" + s2 + "/capture.sqlite",
	}); err != nil {
		t.Fatalf("CreateSession s2: %v", err)
	}
	// 清理：测试结束删掉两条会话，避免污染共享库。
	defer func() {
		_ = ctrl.DeleteSession(ctx, s1)
		_ = ctrl.DeleteSession(ctx, s2)
	}()

	got, err := ctrl.GetSession(ctx, s1)
	if err != nil {
		t.Fatalf("GetSession s1: %v", err)
	}
	if got == nil || got.SessionID != s1 || got.Port != 8080 {
		t.Fatalf("GetSession s1 mismatch: %+v", got)
	}

	// 审计：PG 后端走 RETURNING id（pgx 无 LastInsertId）。
	aid, err := ctrl.RecordDebugAccess(ctx, DebugAccess{
		Actor: "mcp", Tool: "sample_bytes", Plugin: "tcp", SessionID: s1,
		RequestedPackets: 10, ReturnedPackets: 5, ReturnedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("RecordDebugAccess: %v", err)
	}
	if aid <= 0 {
		t.Fatalf("RecordDebugAccess returned id=%d, want >0", aid)
	}
	accesses, err := ctrl.DebugAccesses(ctx, s1)
	if err != nil {
		t.Fatalf("DebugAccesses: %v", err)
	}
	if len(accesses) != 1 || accesses[0].ID != aid {
		t.Fatalf("DebugAccesses mismatch: %+v", accesses)
	}

	// ---- 事件存储后端（共享 PG 库，按 session_id 隔离）----
	st1, err := OpenCaptureStore("postgres", dsn, nil, s1)
	if err != nil {
		t.Fatalf("OpenCaptureStore s1: %v", err)
	}
	defer st1.Close()
	st2, err := OpenCaptureStore("postgres", dsn, nil, s2)
	if err != nil {
		t.Fatalf("OpenCaptureStore s2: %v", err)
	}
	defer st2.Close()

	// raw_packets
	pkts := []event.Packet{{
		ID:        "p-1",
		Timestamp: now,
		Raw:       []byte{0x01, 0x02, 0x03},
		LinkType:  event.LinkType(1),
		Src:       netip.MustParseAddrPort("127.0.0.1:5000"),
		Dst:       netip.MustParseAddrPort("127.0.0.1:8080"),
		Protocol:  "tcp",
	}}
	if err := st1.AppendRawPackets(ctx, pkts); err != nil {
		t.Fatalf("AppendRawPackets: %v", err)
	}

	// events（结构化 payload）
	val := event.Value{Kind: event.Object, Object: map[string]event.Value{
		"src": {Kind: event.String, Str: "127.0.0.1:5000"},
		"dst": {Kind: event.String, Str: "127.0.0.1:8080"},
	}}
	evs := []*event.Event{{
		Identity: event.Identity{
			ID: "ev-1", SessionID: s1, Type: "tcp", SchemaID: "tcp.v1", Source: "test", Timestamp: now,
		},
		Payload: event.Payload{SchemaID: "tcp.v1", Value: val},
	}}
	if err := st1.AppendEvents(ctx, evs); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// metrics（PG 主键 (session_id,name,window,group_json) ON CONFLICT DO UPDATE）
	if err := st1.WriteMetrics(ctx, []event.Metric{
		{Name: "count", Window: now, Group: map[string]string{"g": "a"}, Value: 7},
	}); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}

	// 查询 s1 应有数据
	rawRows, err := st1.QueryRawPackets(ctx, RawPacketQuery{})
	if err != nil {
		t.Fatalf("QueryRawPackets s1: %v", err)
	}
	if len(rawRows) != 1 {
		t.Fatalf("QueryRawPackets s1: got %d rows, want 1", len(rawRows))
	}
	evRows, err := st1.QueryEvents(ctx, s1, 100, 0)
	if err != nil {
		t.Fatalf("QueryEvents s1: %v", err)
	}
	if len(evRows) != 1 || evRows[0].Identity.ID != "ev-1" {
		t.Fatalf("QueryEvents s1: %+v", evRows)
	}
	metRows, err := st1.QueryMetrics(ctx, MetricQuery{SessionID: s1})
	if err != nil {
		t.Fatalf("QueryMetrics s1: %v", err)
	}
	if len(metRows) != 1 || metRows[0].Name != "count" {
		t.Fatalf("QueryMetrics s1: %+v", metRows)
	}
	conns, err := st1.QueryConnections(ctx, s1, 0, 0)
	if err != nil {
		t.Fatalf("QueryConnections s1: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("QueryConnections s1: got %d, want 1", len(conns))
	}

	// 隔离性：s2 必须读不到 s1 的数据
	raw2, err := st2.QueryRawPackets(ctx, RawPacketQuery{})
	if err != nil {
		t.Fatalf("QueryRawPackets s2: %v", err)
	}
	if len(raw2) != 0 {
		t.Fatalf("session isolation broken: s2 raw_packets = %d, want 0", len(raw2))
	}
	ev2, err := st2.QueryEvents(ctx, s2, 100, 0)
	if err != nil {
		t.Fatalf("QueryEvents s2: %v", err)
	}
	if len(ev2) != 0 {
		t.Fatalf("session isolation broken: s2 events = %d, want 0", len(ev2))
	}

	// ClearDecodedData：仅清 events/state_changes/event_index，保留 raw_packets（按 session_id）
	if err := st1.ClearDecodedData(ctx); err != nil {
		t.Fatalf("ClearDecodedData: %v", err)
	}
	evAfter, err := st1.QueryEvents(ctx, s1, 100, 0)
	if err != nil {
		t.Fatalf("QueryEvents after clear: %v", err)
	}
	if len(evAfter) != 0 {
		t.Fatalf("ClearDecodedData left %d events, want 0", len(evAfter))
	}
	rawAfter, err := st1.QueryRawPackets(ctx, RawPacketQuery{})
	if err != nil {
		t.Fatalf("QueryRawPackets after clear: %v", err)
	}
	if len(rawAfter) != 1 {
		t.Fatalf("ClearDecodedData wrongly removed raw_packets: %d", len(rawAfter))
	}
}
