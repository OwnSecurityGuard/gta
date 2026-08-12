package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/event"
)

// TestWriteEventsV2_StructuredFields 验证 Event 结构化字段正确落库。
func TestWriteEventsV2_StructuredFields(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()

	// 创建包含结构化字段的 Event
	payload1 := event.Value{
		Kind: event.Object,
		Object: map[string]event.Value{
			"src":       {Kind: event.String, Str: "127.0.0.1:5000"},
			"dst":       {Kind: event.String, Str: "127.0.0.1:8080"},
			"flow_id":   {Kind: event.Uint, Uint: 12345},
			"direction": {Kind: event.String, Str: "client_to_server"},
			"msg_name":  {Kind: event.String, Str: "LoginReq"},
			"is_push":   {Kind: event.Bool, Bool: false},
			"type":      {Kind: event.String, Str: "request"},
			"method":    {Kind: event.String, Str: "POST"},
		},
	}

	payload2 := event.Value{
		Kind: event.Object,
		Object: map[string]event.Value{
			"src":       {Kind: event.String, Str: "127.0.0.1:8080"},
			"dst":       {Kind: event.String, Str: "127.0.0.1:5000"},
			"flow_id":   {Kind: event.Uint, Uint: 12345},
			"direction": {Kind: event.String, Str: "server_to_client"},
			"msg_name":  {Kind: event.String, Str: "LoginResp"},
			"is_push":   {Kind: event.Bool, Bool: false},
			"type":      {Kind: event.String, Str: "response"},
			"status":    {Kind: event.String, Str: "200"},
		},
	}

	events := []*event.Event{
		{
			Identity: event.Identity{
				ID:        "ev-1",
				SessionID: "test-session",
				Type:      "tcp",
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: now,
			},
			Relation: event.Relation{},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    payload1,
			},
		},
		{
			Identity: event.Identity{
				ID:        "ev-2",
				SessionID: "test-session",
				Type:      "tcp",
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: now.Add(50 * time.Millisecond),
			},
			Relation: event.Relation{},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    payload2,
			},
		},
	}

	if err := s.AppendEvents(ctx, events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// 查库验证 ev-1
	row := s.db.QueryRowContext(ctx,
		"SELECT payload FROM events WHERE id=?", "ev-1")
	var payloadBytes []byte
	if err := row.Scan(&payloadBytes); err != nil {
		t.Fatalf("scan ev-1: %v", err)
	}

	// 反序列化 payload
	storedPayload, err := event.UnmarshalValueMsgpack(payloadBytes)
	if err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	obj, ok := storedPayload.AsObject()
	if !ok {
		t.Fatal("payload is not an object")
	}

	// 验证字段
	if src, exists := obj["src"]; !exists || src.Str != "127.0.0.1:5000" {
		t.Errorf("src = %v, want 127.0.0.1:5000", src)
	}
	if direction, exists := obj["direction"]; !exists || direction.Str != "client_to_server" {
		t.Errorf("direction = %v, want client_to_server", direction)
	}
	if msgName, exists := obj["msg_name"]; !exists || msgName.Str != "LoginReq" {
		t.Errorf("msg_name = %v, want LoginReq", msgName)
	}
	if flowID, exists := obj["flow_id"]; !exists || flowID.Uint != 12345 {
		t.Errorf("flow_id = %v, want 12345", flowID)
	}
	if isPush, exists := obj["is_push"]; !exists || isPush.Bool != false {
		t.Errorf("is_push = %v, want false", isPush)
	}

	// 查库验证 ev-2
	row2 := s.db.QueryRowContext(ctx,
		"SELECT payload FROM events WHERE id=?", "ev-2")
	if err := row2.Scan(&payloadBytes); err != nil {
		t.Fatalf("scan ev-2: %v", err)
	}

	storedPayload2, err := event.UnmarshalValueMsgpack(payloadBytes)
	if err != nil {
		t.Fatalf("unmarshal payload2: %v", err)
	}

	obj2, ok := storedPayload2.AsObject()
	if !ok {
		t.Fatal("payload2 is not an object")
	}

	if direction, exists := obj2["direction"]; !exists || direction.Str != "server_to_client" {
		t.Errorf("ev-2 direction = %v, want server_to_client", direction)
	}
	if msgName, exists := obj2["msg_name"]; !exists || msgName.Str != "LoginResp" {
		t.Errorf("ev-2 msg_name = %v, want LoginResp", msgName)
	}
}

// TestWriteEventsV2_EmptyPayload 验证 Event 空 payload 能正常落库。
func TestWriteEventsV2_EmptyPayload(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// 创建空 payload 的 Event
	events := []*event.Event{
		{
			Identity: event.Identity{
				ID:        "ev-empty",
				SessionID: "test-session",
				Type:      "tcp",
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: time.Now(),
			},
			Relation: event.Relation{},
			Payload: event.Payload{
					SchemaID: "tcp.v1",
					Value: event.Value{
						Kind:   event.Object,
						Object: map[string]event.Value{},
					},
				},
		},
	}

	if err := s.AppendEvents(ctx, events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// 验证能正常读取
	rows, err := s.QueryEvents(ctx, "test-session", 100, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rows))
	}
	if string(rows[0].Identity.ID) != "ev-empty" {
		t.Errorf("expected ID 'ev-empty', got '%s'", rows[0].Identity.ID)
	}
}

// TestWriteStateChanges 验证 WriteStateChanges 写入 state_changes 表与查询。
func TestWriteStateChanges(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	flowID := "999"
	payload := event.ValueObject(map[string]event.Value{
		"_meta": event.ValueObject(map[string]event.Value{
			"flow_id": event.ValueString(flowID),
		}),
		"_state_changes": {Kind: event.Array, Array: []event.Value{
			event.ValueObject(map[string]event.Value{
				"subject_type": event.ValueString("Building"),
				"subject_id":   event.ValueString("1001"),
				"op":           event.ValueString("set"),
				"path":         event.ValueString("level"),
				"after":        event.ValueInt(3),
			}),
			event.ValueObject(map[string]event.Value{
				"subject_type": event.ValueString("Building"),
				"subject_id":   event.ValueString("1001"),
				"op":           event.ValueString("set"),
				"path":         event.ValueString("cost"),
				"after":        event.ValueInt(100),
			}),
			event.ValueObject(map[string]event.Value{
				"subject_type": event.ValueString("Hero"),
				"subject_id":   event.ValueString("500"),
				"op":           event.ValueString("delete"),
				"path":         event.ValueString("name"),
			}),
		}},
	})
	events := []*event.Event{
		event.NewEvent("test-session", "tcp", "tcp.v1", "test", payload, event.EventContext{FlowID: flowID}),
	}

	if err := s.WriteStateChanges(ctx, "test-session", events); err != nil {
		t.Fatalf("WriteStateChanges: %v", err)
	}

	// 查询 state_changes 表验证
	rows, err := s.db.QueryContext(ctx,
		"SELECT subject_type, subject_id, op, path, after_value FROM state_changes WHERE session_id=? AND flow_id=? ORDER BY subject_type, path",
		"test-session", flowID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type record struct{ subjectType, subjectID, op, path, value string }
	var records []record
	for rows.Next() {
		var subjectType, subjectID, op, path string
		var afterValue sql.NullString
		if err := rows.Scan(&subjectType, &subjectID, &op, &path, &afterValue); err != nil {
			t.Fatalf("scan: %v", err)
		}
		val := ""
		if afterValue.Valid {
			val = afterValue.String
		}
		records = append(records, record{subjectType, subjectID, op, path, val})
	}

	if len(records) != 3 {
		t.Fatalf("records count = %d, want 3", len(records))
	}

	// 验证第一条：Building/1001/cost
	r := records[0]
	if r.subjectType != "Building" || r.path != "cost" || r.op != "set" {
		t.Errorf("record[0] = %+v, want Building/cost/set", r)
	}
	if r.value != "100" {
		t.Errorf("record[0].after_value = %q, want 100", r.value)
	}

	// 验证 delete 操作 after_value 为 JSON null
	rDelete := records[2]
	if rDelete.op != "delete" {
		t.Errorf("record[2].op = %q, want delete", rDelete.op)
	}
	if rDelete.value != "null" {
		t.Errorf("record[2].after_value = %q, want null (JSON null)", rDelete.value)
	}
}

// TestWriteStateChanges_SkipsInvalid 验证不完整的 StateChange 会被跳过，不会写入投影。
func TestWriteStateChanges_SkipsInvalid(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	flowID := "999"
	payload := event.ValueObject(map[string]event.Value{
		"_state_changes": {Kind: event.Array, Array: []event.Value{
			// 有效
			event.ValueObject(map[string]event.Value{
				"subject_type": event.ValueString("Building"),
				"subject_id":   event.ValueString("1001"),
				"op":           event.ValueString("set"),
				"path":         event.ValueString("level"),
				"after":        event.ValueInt(3),
			}),
			// 无效：缺少 subject_id
			event.ValueObject(map[string]event.Value{
				"subject_type": event.ValueString("Building"),
				"op":           event.ValueString("set"),
				"path":         event.ValueString("cost"),
			}),
			// 无效：非法 op
			event.ValueObject(map[string]event.Value{
				"subject_type": event.ValueString("Hero"),
				"subject_id":   event.ValueString("500"),
				"op":           event.ValueString("patch"),
				"path":         event.ValueString("name"),
			}),
		}},
	})
	events := []*event.Event{
		event.NewEvent("test-session", "tcp", "tcp.v1", "test", payload, event.EventContext{FlowID: flowID}),
	}

	if err := s.WriteStateChanges(ctx, "test-session", events); err != nil {
		t.Fatalf("WriteStateChanges: %v", err)
	}

	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM state_changes WHERE session_id=? AND flow_id=?",
		"test-session", flowID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 valid state change, got %d", count)
	}
}

// TestSchemaMigration_Idempotent 验证 schema 迁移幂等（多次启动不报错）。
func TestSchemaMigration_Idempotent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")

	// 第一次创建
	s1, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatalf("first NewSQLiteStore: %v", err)
	}
	s1.Close()

	// 第二次打开（模拟重启），迁移应幂等
	s2, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatalf("second NewSQLiteStore: %v", err)
	}
	defer s2.Close()

	// 验证表存在
	ctx := context.Background()
	var name string
	err = s2.db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='state_changes'").Scan(&name)
	if err != nil {
		t.Errorf("state_changes table not found after migration: %v", err)
	}
}

// TestWriteEvidenceGraph 验证证据图节点和边的写入与查询。
func TestWriteEvidenceGraph(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	sessionID := "test-session"

	nodes := []EvidenceNodeRow{
		{ID: "evt_ev-1", SessionID: sessionID, Kind: "event", FlowID: "flow-1", Timestamp: 1000000, Labels: `{"type":"tcp"}`, Properties: `{"direction":"client_to_server"}`},
		{ID: "evt_ev-2", SessionID: sessionID, Kind: "event", FlowID: "flow-1", Timestamp: 2000000, Labels: `{"type":"tcp"}`, Properties: `{"direction":"server_to_client"}`},
		{ID: "ent_1001", SessionID: sessionID, Kind: "entity", FlowID: "flow-1", Timestamp: 1500000, Labels: `{"subject_type":"Building"}`, Properties: `{}`},
	}

	edges := []EvidenceEdgeRow{
		{ID: "edge-1", SessionID: sessionID, Source: "evt_ev-2", Target: "evt_ev-1", Type: "response_to", Confidence: 1.0, Reason: "RPC response", Properties: `{}`},
		{ID: "edge-2", SessionID: sessionID, Source: "evt_ev-1", Target: "ent_1001", Type: "updates", Confidence: 1.0, Reason: "state change", Properties: `{"path":"level"}`},
	}

	if err := s.WriteEvidenceGraph(ctx, sessionID, "", nodes, edges); err != nil {
		t.Fatalf("WriteEvidenceGraph: %v", err)
	}

	// 查询全部
	result, err := s.QueryEvidenceGraph(ctx, EvidenceGraphQuery{SessionID: sessionID})
	if err != nil {
		t.Fatalf("QueryEvidenceGraph: %v", err)
	}
	if len(result.Nodes) != 3 {
		t.Fatalf("nodes count = %d, want 3", len(result.Nodes))
	}
	if len(result.Edges) != 2 {
		t.Fatalf("edges count = %d, want 2", len(result.Edges))
	}

	// 验证 response_to 边
	found := false
	for _, e := range result.Edges {
		if e.Type == "response_to" {
			found = true
			if e.Source != "evt_ev-2" || e.Target != "evt_ev-1" {
				t.Errorf("response_to edge: source=%q target=%q", e.Source, e.Target)
			}
		}
	}
	if !found {
		t.Error("response_to edge not found")
	}

	// 幂等覆盖：再次写入相同 session 应替换旧数据
	nodes2 := []EvidenceNodeRow{
		{ID: "evt_new", SessionID: sessionID, Kind: "event", FlowID: "flow-1", Timestamp: 3000000, Labels: `{}`, Properties: `{}`},
	}
	if err := s.WriteEvidenceGraph(ctx, sessionID, "run-2", nodes2, nil); err != nil {
		t.Fatalf("second WriteEvidenceGraph: %v", err)
	}
	result2, err := s.QueryEvidenceGraph(ctx, EvidenceGraphQuery{SessionID: sessionID})
	if err != nil {
		t.Fatalf("second QueryEvidenceGraph: %v", err)
	}
	if len(result2.Nodes) != 1 {
		t.Fatalf("after overwrite nodes count = %d, want 1", len(result2.Nodes))
	}
	if result2.Nodes[0].ID != "evt_new" {
		t.Errorf("node id = %q, want evt_new", result2.Nodes[0].ID)
	}
	if len(result2.Edges) != 0 {
		t.Fatalf("after overwrite edges count = %d, want 0", len(result2.Edges))
	}
}

// TestQueryEvidenceGraph_Neighbourhood 验证从根节点出发的邻接子图扩展。
func TestQueryEvidenceGraph_Neighbourhood(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	sessionID := "test-session"

	nodes := []EvidenceNodeRow{
		{ID: "A", SessionID: sessionID, Kind: "event", FlowID: "f1", Timestamp: 1000, Labels: `{}`, Properties: `{}`},
		{ID: "B", SessionID: sessionID, Kind: "event", FlowID: "f1", Timestamp: 2000, Labels: `{}`, Properties: `{}`},
		{ID: "C", SessionID: sessionID, Kind: "event", FlowID: "f1", Timestamp: 3000, Labels: `{}`, Properties: `{}`},
		{ID: "D", SessionID: sessionID, Kind: "event", FlowID: "f1", Timestamp: 4000, Labels: `{}`, Properties: `{}`},
	}

	edges := []EvidenceEdgeRow{
		{ID: "e1", SessionID: sessionID, Source: "A", Target: "B", Type: "caused_by", Confidence: 1.0, Properties: `{}`},
		{ID: "e2", SessionID: sessionID, Source: "B", Target: "C", Type: "response_to", Confidence: 1.0, Properties: `{}`},
		{ID: "e3", SessionID: sessionID, Source: "C", Target: "D", Type: "possible_followup", Confidence: 0.5, Properties: `{}`},
	}

	if err := s.WriteEvidenceGraph(ctx, sessionID, "run-nbr", nodes, edges); err != nil {
		t.Fatalf("WriteEvidenceGraph: %v", err)
	}

	// 从 A 出发，depth=1 应该拿到 A、B 两个节点
	result, err := s.QueryEvidenceGraph(ctx, EvidenceGraphQuery{
		SessionID:  sessionID,
		RootNodeID: "A",
		MaxDepth:   1,
	})
	if err != nil {
		t.Fatalf("QueryEvidenceGraph depth=1: %v", err)
	}
	if len(result.Nodes) < 2 {
		t.Fatalf("depth=1 got %d nodes, want >=2", len(result.Nodes))
	}

	// 从 A 出发，depth=2 应该拿到 A、B、C 三个节点
	result2, err := s.QueryEvidenceGraph(ctx, EvidenceGraphQuery{
		SessionID:  sessionID,
		RootNodeID: "A",
		MaxDepth:   2,
	})
	if err != nil {
		t.Fatalf("QueryEvidenceGraph depth=2: %v", err)
	}
	if len(result2.Nodes) < 3 {
		t.Fatalf("depth=2 got %d nodes, want >=3", len(result2.Nodes))
	}

	// 从 A 出发，depth=3 应该拿到全部 4 个节点
	result3, err := s.QueryEvidenceGraph(ctx, EvidenceGraphQuery{
		SessionID:  sessionID,
		RootNodeID: "A",
		MaxDepth:   3,
	})
	if err != nil {
		t.Fatalf("QueryEvidenceGraph depth=3: %v", err)
	}
	if len(result3.Nodes) != 4 {
		t.Fatalf("depth=3 got %d nodes, want 4", len(result3.Nodes))
	}
}
