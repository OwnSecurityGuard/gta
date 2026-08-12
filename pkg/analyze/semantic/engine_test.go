package semantic

import (
	"strings"
	"testing"
	"time"

	"gta/pkg/event"
)

func TestEngine_Process_DecodedFrom(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	ev := newEvent("s1", "flow-1", "http.request", map[string]any{
		"method": "GET",
		"path":   "/api/user",
	})
	ev.Context.RawPacketID = "pkt-001"

	if _, err := eng.Process(ev); err != nil {
		t.Fatalf("process: %v", err)
	}

	g := eng.Graph()
	if len(g.Nodes) != 2 {
		t.Fatalf("expected 2 nodes (event + raw_packet), got %d", len(g.Nodes))
	}
	if len(g.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(g.Edges))
	}
	if g.Edges[0].Type != DecodedFrom {
		t.Errorf("expected decoded_from edge, got %s", g.Edges[0].Type)
	}
}

func TestEngine_Process_ResponseTo(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	req := event.NewEventWithTime(
		"s1",
		"http.request",
		"test.v1",
		"test-source",
		event.ValueFromAny(map[string]any{"method": "POST"}),
		time.Now(),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-req", Direction: "client_to_server"},
	)
	req = req.WithCorrelation("req-123")

	resp := event.NewEventWithTime(
		"s1",
		"http.response",
		"test.v1",
		"test-source",
		event.ValueFromAny(map[string]any{"status": 200}),
		time.Now().Add(50*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-resp", Direction: "server_to_client"},
	)
	resp = resp.WithCorrelation("req-123")

	if _, err := eng.Process(req); err != nil {
		t.Fatalf("process req: %v", err)
	}
	if _, err := eng.Process(resp); err != nil {
		t.Fatalf("process resp: %v", err)
	}

	g := eng.Graph()
	var found bool
	for _, edge := range g.Edges {
		if edge.Type == ResponseTo {
			found = true
			if edge.Confidence != 1.0 {
				t.Errorf("response_to confidence = %f, want 1.0", edge.Confidence)
			}
		}
	}
	if !found {
		t.Errorf("expected response_to edge")
	}
}

func TestEngine_Process_ResponseTo_NoPendingRequest(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	resp := event.NewEventWithTime(
		"s1",
		"http.response",
		"test.v1",
		"test-source",
		event.ValueFromAny(map[string]any{"status": 200}),
		time.Now(),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-resp", Direction: "server_to_client"},
	)
	resp = resp.WithCorrelation("orphan-123")

	if _, err := eng.Process(resp); err != nil {
		t.Fatalf("process resp: %v", err)
	}

	g := eng.Graph()
	for _, edge := range g.Edges {
		if edge.Type == ResponseTo {
			t.Errorf("should not create response_to without pending request")
		}
	}
	if len(g.Uncertainties) == 0 {
		t.Errorf("expected uncertainty for orphan response")
	}
}

func TestEngine_Process_PossibleFollowup(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	base := time.Now()
	e1 := event.NewEventWithTime(
		"s1", "event-a", "test.v1", "test-source",
		event.ValueFromAny(map[string]any{"n": 1}),
		base,
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-1", Direction: "unknown"},
	)
	e2 := event.NewEventWithTime(
		"s1", "event-b", "test.v1", "test-source",
		event.ValueFromAny(map[string]any{"n": 2}),
		base.Add(100*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-2", Direction: "unknown"},
	)

	if _, err := eng.Process(e1); err != nil {
		t.Fatalf("process e1: %v", err)
	}
	if _, err := eng.Process(e2); err != nil {
		t.Fatalf("process e2: %v", err)
	}

	g := eng.Graph()
	var found bool
	for _, edge := range g.Edges {
		if edge.Type == PossibleFollowup {
			found = true
			if edge.Confidence != 0.3 {
				t.Errorf("possible_followup confidence = %f, want 0.3", edge.Confidence)
			}
		}
	}
	if !found {
		t.Errorf("expected possible_followup edge")
	}
}

func TestEngine_Process_StateChange_Updates(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	ev := event.NewEventWithTime(
		"s1", "game.login", "test.v1", "test-source",
		event.ValueFromAny(map[string]any{
			"_state_changes": []any{
				map[string]any{
					"subject_type": "player",
					"subject_id":   "1001",
					"op":           "set",
					"path":         "gold",
					"after":        100,
					"version":      1,
				},
			},
		}),
		time.Now(),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-1", Direction: "unknown"},
	)

	if _, err := eng.Process(ev); err != nil {
		t.Fatalf("process: %v", err)
	}

	g := eng.Graph()
	var updates, causedBy int
	for _, edge := range g.Edges {
		switch edge.Type {
		case Updates:
			updates++
		case CausedBy:
			causedBy++
		}
	}
	if updates != 1 {
		t.Errorf("expected 1 updates edge, got %d", updates)
	}
	if causedBy != 1 {
		t.Errorf("expected 1 caused_by edge, got %d", causedBy)
	}
}

func TestEngine_Graph_Copy(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	ev := newEvent("s1", "flow-1", "http.request", map[string]any{"method": "GET"})
	if _, err := eng.Process(ev); err != nil {
		t.Fatalf("process: %v", err)
	}

	g1 := eng.Graph()
	g2 := eng.Graph()
	g1.Nodes[0].ID = "mutated"

	if g2.Nodes[0].ID == "mutated" {
		t.Errorf("Graph() should return a copy")
	}
}

// TestEngine_E2E_ProtocolFlow 端到端测试：模拟完整的游戏协议交互链路。
//
// 场景：
//  1. 客户端发送 LoginReq → 服务端返回 LoginResp（response_to）
//  2. 服务端推送 EntitySync（possible_followup）
//  3. 服务端通过 NotifyGold 更新玩家金币
//  4. 客户端发送 UpgradeReq → 服务端返回 UpgradeResp（response_to）
//
// 验证证据图包含正确的节点类型、边类型和关系。
func TestEngine_E2E_ProtocolFlow(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	base := time.Now()

	// Step 1: 登录请求
	loginReq := event.NewEventWithTime(
		"s1", "LoginReq", "game.v1", "game-plugin",
		event.ValueFromAny(map[string]any{"username": "player1", "password": "***"}),
		base,
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-login-req", Direction: "client_to_server"},
	)
	loginReq = loginReq.WithCorrelation("corr-login-001")

	// Step 2: 登录响应
	loginResp := event.NewEventWithTime(
		"s1", "LoginResp", "game.v1", "game-plugin",
		event.ValueFromAny(map[string]any{"result": "ok", "player_id": "1001"}),
		base.Add(50*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-login-resp", Direction: "server_to_client"},
	)
	loginResp = loginResp.WithCorrelation("corr-login-001")

	// Step 3: 服务端推送实体同步
	sync := event.NewEventWithTime(
		"s1", "EntitySync", "game.v1", "game-plugin",
		event.ValueFromAny(map[string]any{"entities": []any{}}),
		base.Add(100*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-sync", Direction: "server_to_client"},
	)

	// Step 4: 金币变更带 StateChange
	goldChange := event.NewEventWithTime(
		"s1", "NotifyGold", "game.v1", "game-plugin",
		event.ValueFromAny(map[string]any{
			"player_id": "1001",
			"delta":     500,
			"_state_changes": []any{
				map[string]any{
					"subject_type": "Player",
					"subject_id":   "1001",
					"op":           "set",
					"path":         "gold",
					"after":        1500,
					"version":      2,
				},
			},
		}),
		base.Add(200*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-gold", Direction: "server_to_client"},
	)

	// Step 5: 升级请求
	upgradeReq := event.NewEventWithTime(
		"s1", "UpgradeReq", "game.v1", "game-plugin",
		event.ValueFromAny(map[string]any{"building_id": "b100", "level": 3}),
		base.Add(500*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-up-req", Direction: "client_to_server"},
	)
	upgradeReq = upgradeReq.WithCorrelation("corr-upgrade-002")

	// Step 6: 升级响应
	upgradeResp := event.NewEventWithTime(
		"s1", "UpgradeResp", "game.v1", "game-plugin",
		event.ValueFromAny(map[string]any{"result": "ok", "cost": 300, "_state_changes": []any{
			map[string]any{
				"subject_type": "Building",
				"subject_id":   "b100",
				"op":           "set",
				"path":         "level",
				"after":        3,
				"version":      1,
			},
			map[string]any{
				"subject_type": "Building",
				"subject_id":   "b100",
				"op":           "set",
				"path":         "status",
				"after":        "upgrading",
				"version":      1,
			},
		}}),
		base.Add(550*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-up-resp", Direction: "server_to_client"},
	)
	upgradeResp = upgradeResp.WithCorrelation("corr-upgrade-002")

	// 处理所有事件
	for _, ev := range []*event.Event{loginReq, loginResp, sync, goldChange, upgradeReq, upgradeResp} {
		if _, err := eng.Process(ev); err != nil {
			t.Fatalf("process event %s: %v", ev.Identity.ID, err)
		}
	}

	g := eng.Graph()

	// === 验证节点 ===
	t.Run("nodes", func(t *testing.T) {
		kindCount := map[EvidenceNodeKind]int{}
		for _, n := range g.Nodes {
			kindCount[n.Kind]++
		}

		// 6 个事件节点
		if kindCount[NodeEvent] != 6 {
			t.Errorf("event nodes = %d, want 6", kindCount[NodeEvent])
		}
		// 6 个 raw_packet 节点（每个事件一个）
		if kindCount[NodeRawPacket] != 6 {
			t.Errorf("raw_packet nodes = %d, want 6", kindCount[NodeRawPacket])
		}
		// 3 个 state_change 节点（gold.x1 + building.level + building.status）
		if kindCount[NodeStateChange] != 3 {
			t.Errorf("state_change nodes = %d, want 3", kindCount[NodeStateChange])
		}
		// 2 个 entity 节点（Player + Building）
		if kindCount[NodeEntity] != 2 {
			t.Errorf("entity nodes = %d, want 2", kindCount[NodeEntity])
		}
	})

	// === 验证边 ===
	t.Run("edges", func(t *testing.T) {
		edgeTypes := map[RelationType]int{}
		for _, e := range g.Edges {
			edgeTypes[e.Type]++
		}

		// decoded_from: 6 (每个事件一个)
		if edgeTypes[DecodedFrom] != 6 {
			t.Errorf("decoded_from = %d, want 6", edgeTypes[DecodedFrom])
		}
		// response_to: 2 (login + upgrade)
		if edgeTypes[ResponseTo] != 2 {
			t.Errorf("response_to = %d, want 2", edgeTypes[ResponseTo])
		}
		// possible_followup: >= 4 (LoginReq→LoginResp, LoginResp→sync, sync→gold, loginReq→upgradeReq)
		if edgeTypes[PossibleFollowup] < 4 {
			t.Errorf("possible_followup = %d, want >= 4", edgeTypes[PossibleFollowup])
		}
		// updates: 3 (gold, level, status)
		if edgeTypes[Updates] != 3 {
			t.Errorf("updates = %d, want 3", edgeTypes[Updates])
		}
		// caused_by: 3 (每个 state_change 到其事件)
		if edgeTypes[CausedBy] != 3 {
			t.Errorf("caused_by = %d, want 3", edgeTypes[CausedBy])
		}

		// 验证 response_to confidence
		for _, e := range g.Edges {
			if e.Type == ResponseTo {
				if e.Confidence != 1.0 {
					t.Errorf("response_to edge %s confidence = %f, want 1.0", e.ID, e.Confidence)
				}
			}
		}
	})

	// === 验证 enriched state changes ===
	t.Run("enriched_sc", func(t *testing.T) {
		// 重新处理 goldChange 单独获取 enriched
		eng2 := NewEngine(DefaultConfig(), nil)
		enriched, err := eng2.Process(goldChange)
		if err != nil {
			t.Fatalf("process goldChange: %v", err)
		}
		if len(enriched) != 1 {
			t.Fatalf("expected 1 enriched state change, got %d", len(enriched))
		}
		gc := enriched[0]
		if gc.SubjectType != "Player" {
			t.Errorf("subject_type = %q, want Player", gc.SubjectType)
		}
		if gc.Path != "gold" {
			t.Errorf("path = %q, want gold", gc.Path)
		}
		// 首次出现，BeforeResolved 应为 false
		if gc.BeforeResolved {
			t.Error("first change should have BeforeResolved=false")
		}
		if !gc.AfterResolved {
			t.Error("AfterResolved should be true when after value is non-null")
		}
	})

	// === 验证图副本 ===
	t.Run("graph_copy", func(t *testing.T) {
		copy1 := eng.Graph()
		copy2 := eng.Graph()
		if len(copy1.Nodes) != len(copy2.Nodes) {
			t.Error("graph copies should have same node count")
		}
	})
}

// TestEngine_PatternMatchResponseTo 验证无 correlation_key 时通过命名模式匹配请求-响应。
func TestEngine_PatternMatchResponseTo(t *testing.T) {
	cfg := DefaultConfig()
	eng := NewEngine(cfg, nil)

	now := time.Now()

	// 模拟一个 flow：请求方向 c→s，响应方向 s→c，均无 correlation_key
	reqEv := &event.Event{
		Identity: event.Identity{
			ID:        "req-1",
			SessionID: "s1",
			Type:      "LoginReq",
			SchemaID:  "test.v1",
			Source:    "test",
			Timestamp: now,
		},
		Context: event.EventContext{
			Direction: "client_to_server",
			FlowID:    "flow-1",
		},
		Payload: event.Payload{
			SchemaID: "test.v1",
			Value:    event.ValueFromAny(map[string]any{"user": "admin"}),
		},
	}

	respEv := &event.Event{
		Identity: event.Identity{
			ID:        "resp-1",
			SessionID: "s1",
			Type:      "LoginResp",
			SchemaID:  "test.v1",
			Source:    "test",
			Timestamp: now.Add(10 * time.Millisecond),
		},
		Context: event.EventContext{
			Direction: "server_to_client",
			FlowID:    "flow-1",
		},
		Payload: event.Payload{
			SchemaID: "test.v1",
			Value:    event.ValueFromAny(map[string]any{"token": "xxx"}),
		},
	}

	_, err := eng.Process(reqEv)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}
	_, err = eng.Process(respEv)
	if err != nil {
		t.Fatalf("process response: %v", err)
	}

	g := eng.Graph()

	// 验证生成了 response_to 边。
	// Graph Integrity：边的 source/target 都必须是图中节点 ID（evt_ 前缀 + event ID），
	// 不能是裸 event.EventID，否则形成悬空边。
	var found bool
	for _, edge := range g.Edges {
		if edge.Type == ResponseTo && edge.Target == "evt_req-1" {
			found = true
			if edge.Confidence != 0.85 {
				t.Errorf("pattern match edge confidence = %f, want 0.85", edge.Confidence)
			}
			if !strings.Contains(edge.Reason, "naming pattern") {
				t.Errorf("edge reason should contain 'naming pattern', got %q", edge.Reason)
			}
		}
	}
	if !found {
		t.Error("expected a pattern-based response_to edge from resp-1 to req-1, but none found")
	}
}

// TestEngine_PatternMatchPriority 验证 correlation_key 匹配优先于命名模式。
func TestEngine_PatternMatchPriority(t *testing.T) {
	cfg := DefaultConfig()
	eng := NewEngine(cfg, nil)

	now := time.Now()

	// 请求 1：无 correlation_key，类型为 GetDataReq
	req1 := &event.Event{
		Identity: event.Identity{
			ID:        "req-no-key",
			SessionID: "s1",
			Type:      "GetDataReq",
			SchemaID:  "test.v1",
			Source:    "test",
			Timestamp: now,
		},
		Context: event.EventContext{
			Direction: "client_to_server",
			FlowID:    "flow-1",
		},
		Payload: event.Payload{
			SchemaID: "test.v1",
			Value:    event.ValueFromAny(map[string]any{}),
		},
	}

	// 请求 2：有 correlation_key=abc，类型也是 GetDataReq
	req2 := &event.Event{
		Identity: event.Identity{
			ID:        "req-with-key",
			SessionID: "s1",
			Type:      "GetDataReq",
			SchemaID:  "test.v1",
			Source:    "test",
			Timestamp: now.Add(1 * time.Millisecond),
		},
		Relation: event.Relation{
			CorrelationID: "abc",
		},
		Context: event.EventContext{
			Direction: "client_to_server",
			FlowID:    "flow-1",
		},
		Payload: event.Payload{
			SchemaID: "test.v1",
			Value:    event.ValueFromAny(map[string]any{}),
		},
	}

	// 响应：带 correlation_key=abc，类型为 GetDataResp
	resp := &event.Event{
		Identity: event.Identity{
			ID:        "resp-with-key",
			SessionID: "s1",
			Type:      "GetDataResp",
			SchemaID:  "test.v1",
			Source:    "test",
			Timestamp: now.Add(2 * time.Millisecond),
		},
		Relation: event.Relation{
			CorrelationID: "abc",
		},
		Context: event.EventContext{
			Direction: "server_to_client",
			FlowID:    "flow-1",
		},
		Payload: event.Payload{
			SchemaID: "test.v1",
			Value:    event.ValueFromAny(map[string]any{}),
		},
	}

	eng.Process(req1)
	eng.Process(req2)
	eng.Process(resp)

	g := eng.Graph()

	// 响应应该匹配 req2（correlation_key 优先），而不是 req1（命名模式）
	// target 为节点 ID 形式 evt_<event_id>。
	for _, edge := range g.Edges {
		if edge.Type == ResponseTo {
			if edge.Target != "evt_req-with-key" {
				t.Errorf("response_to target = %s, want evt_req-with-key (correlation_key match takes priority)", edge.Target)
			}
			if edge.Confidence != 1.0 {
				t.Errorf("correlation_key match confidence = %f, want 1.0", edge.Confidence)
			}
		}
	}

	// req1（无 key）应该仍然在 pending 中，后续如果有无 key 的 GetDataResp 会匹配到它
}

// TestTrySwapSuffix 单元测试后缀替换函数。
func TestTrySwapSuffix(t *testing.T) {
	cases := []struct {
		s    string
		old  string
		new  string
		want string
	}{
		{"LoginReq", "Req", "Resp", "LoginResp"},
		{"LoginReq", "Request", "Response", ""},
		{"GetDataReq", "Req", "Rsp", "GetDataRsp"},
		{"CS_Hello", "CS", "SC", "SC_Hello"},
		{"Notify", "Req", "Resp", ""},
		{"", "Req", "Resp", ""},
	}
	for _, c := range cases {
		got := trySwapSuffix(c.s, c.old, c.new)
		if got != c.want {
			t.Errorf("trySwapSuffix(%q, %q, %q) = %q, want %q", c.s, c.old, c.new, got, c.want)
		}
	}
}

// TestEngine_TransactionClustering 验证按请求边界将事件聚合成事务组。
func TestEngine_TransactionClustering_BasicRequestBoundary(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionClustering = &TransactionClusterConfig{
		NewTransactionOnRequest: true,
		MergeGap:                100 * time.Millisecond,
	}

	eng := NewEngine(cfg, nil)
	now := time.Now()

	// 事务 1：LoginReq → LoginResp
	makeEvent := func(id, typ, direction, flow string, ts time.Time) *event.Event {
		return &event.Event{
			Identity: event.Identity{
				ID:        event.EventID(id),
				SessionID: "s1",
				Type:      event.EventType(typ),
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: ts,
			},
			Context: event.EventContext{
				Direction: direction,
				FlowID:    flow,
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    event.ValueFromAny(map[string]any{}),
			},
		}
	}

	// 事务 1：LoginReq (c→s) → LoginResp (s→c)
	eng.Process(makeEvent("ev-login-req", "LoginReq", "client_to_server", "flow-1", now))
	eng.Process(makeEvent("ev-login-resp", "LoginResp", "server_to_client", "flow-1", now.Add(10*time.Millisecond)))

	// 事务 2：GetDataReq → GetDataResp（间隔 > MergeGap，新事务）
	eng.Process(makeEvent("ev-data-req", "GetDataReq", "client_to_server", "flow-1", now.Add(200*time.Millisecond)))
	eng.Process(makeEvent("ev-data-resp", "GetDataResp", "server_to_client", "flow-1", now.Add(210*time.Millisecond)))

	g := eng.Graph()

	// 验证 transaction 节点
	var txNodes []EvidenceNode
	for _, n := range g.Nodes {
		if n.Kind == NodeTransaction {
			txNodes = append(txNodes, n)
		}
	}
	if len(txNodes) != 2 {
		t.Fatalf("expected 2 transaction nodes, got %d", len(txNodes))
	}

	// 验证 contains 边
	var containsEdges []EvidenceEdge
	for _, e := range g.Edges {
		if e.Type == Contains {
			containsEdges = append(containsEdges, e)
		}
	}
	if len(containsEdges) != 4 {
		t.Fatalf("expected 4 contains edges (2 events × 2 txns), got %d", len(containsEdges))
	}

	// 验证事务节点包含事件数和时长
	txByID := make(map[string]EvidenceNode)
	for _, n := range txNodes {
		txByID[n.Labels["first_event"]] = n
	}
	tx1, ok := txByID["ev-login-req"]
	if !ok {
		t.Fatal("transaction 1 not found by first_event label")
	}
	if tx1.Labels["event_count"] != "2" {
		t.Errorf("tx1 event_count = %s, want 2", tx1.Labels["event_count"])
	}
	if dur, ok := tx1.Properties["duration_ms"]; !ok || dur.(int64) < 9 || dur.(int64) > 11 {
		t.Errorf("tx1 duration_ms should be ~10, got %v", dur)
	}
}

// TestEngine_TransactionClustering_MergeGap 验证短间隔合并。
func TestEngine_TransactionClustering_MergeGap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionClustering = &TransactionClusterConfig{
		NewTransactionOnRequest: true,
		MergeGap:                50 * time.Millisecond,
	}

	eng := NewEngine(cfg, nil)
	now := time.Now()

	makeReq := func(id, typ string, ts time.Time) *event.Event {
		return &event.Event{
			Identity: event.Identity{
				ID:        event.EventID(id),
				SessionID: "s1",
				Type:      event.EventType(typ),
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: ts,
			},
			Context: event.EventContext{
				Direction: "client_to_server",
				FlowID:    "flow-1",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    event.ValueFromAny(map[string]any{}),
			},
		}
	}

	// 两个请求间隔 20ms < MergeGap=50ms，应合并为一个事务
	eng.Process(makeReq("ev-req1", "PingReq", now))
	eng.Process(makeReq("ev-req2", "PongReq", now.Add(20*time.Millisecond)))

	// 第三个请求间隔 80ms > MergeGap，应新起事务
	eng.Process(makeReq("ev-req3", "DataReq", now.Add(100*time.Millisecond)))

	g := eng.Graph()

	var txCount int
	for _, n := range g.Nodes {
		if n.Kind == NodeTransaction {
			txCount++
		}
	}
	if txCount != 2 {
		t.Fatalf("expected 2 transactions (2 merged + 1 separate), got %d", txCount)
	}
}

// TestEngine_TransactionClustering_NoDirection 验证无方向事件归入当前事务。
func TestEngine_TransactionClustering_NoDirection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionClustering = &TransactionClusterConfig{
		NewTransactionOnRequest: true,
		MergeGap:                100 * time.Millisecond,
	}

	eng := NewEngine(cfg, nil)
	now := time.Now()

	makeEvent := func(id, typ, direction string, ts time.Time) *event.Event {
		return &event.Event{
			Identity: event.Identity{
				ID:        event.EventID(id),
				SessionID: "s1",
				Type:      event.EventType(typ),
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: ts,
			},
			Context: event.EventContext{
				Direction: direction,
				FlowID:    "flow-1",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    event.ValueFromAny(map[string]any{}),
			},
		}
	}

	// 请求 → 响应 → 无方向推送 → 无方向 ACK
	eng.Process(makeEvent("ev-req", "CmdReq", "client_to_server", now))
	eng.Process(makeEvent("ev-resp", "CmdResp", "server_to_client", now.Add(5*time.Millisecond)))
	eng.Process(makeEvent("ev-push", "Notify", "", now.Add(10*time.Millisecond)))
	eng.Process(makeEvent("ev-ack", "Ack", "", now.Add(15*time.Millisecond)))

	g := eng.Graph()

	var txCount int
	for _, n := range g.Nodes {
		if n.Kind == NodeTransaction {
			txCount++
			if n.Labels["event_count"] != "4" {
				t.Errorf("transaction should have 4 events, got %s", n.Labels["event_count"])
			}
		}
	}
	if txCount != 1 {
		t.Fatalf("expected 1 transaction covering all 4 events, got %d", txCount)
	}
}
