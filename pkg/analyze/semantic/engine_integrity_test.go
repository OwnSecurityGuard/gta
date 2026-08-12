package semantic

import (
	"strings"
	"testing"
	"time"

	"gta/pkg/event"
)

// mkEventAt 构造带指定时间戳的事件（mkEvent 固定为 Unix(0,0)，无法驱动
// possible_followup / transaction 的时间逻辑）。
func mkEventAt(eventType string, ts time.Time, ctx event.EventContext, payload map[string]any) *event.Event {
	return event.NewEventWithTime(
		"sess-integrity",
		event.EventType(eventType),
		eventType+".v1",
		event.SourceID("tcp"),
		event.ValueFromAny(payload),
		ts,
		ctx,
	)
}

// buildRichGraph 跑一段覆盖全部边类型的场景，返回引擎产出的证据图。
//
// 覆盖：decoded_from / caused_by / updates / response_to(correlation)
// / response_to(name_pattern) / possible_followup / contains。
func buildRichGraph(t *testing.T) (*EvidenceGraph, map[string]*event.Event) {
	t.Helper()

	cfg := DefaultConfig()
	cfg.TransactionClustering = &TransactionClusterConfig{
		NewTransactionOnRequest: true,
		MergeGap:                100 * time.Millisecond,
	}
	eng := NewEngine(cfg, nil)

	base := time.Unix(1700000000, 0).UTC()
	events := make(map[string]*event.Event)

	process := func(name string, ev *event.Event) {
		t.Helper()
		events[name] = ev
		if _, err := eng.Process(ev); err != nil {
			t.Fatalf("process %s: %v", name, err)
		}
	}

	// 1) 请求（带 correlation_key）→ decoded_from + 开启事务
	req1 := mkEventAt("LoginCS", base,
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-1", Direction: "client_to_server"},
		map[string]any{"_meta": map[string]any{"msg_name": "LoginCS", "direction": "client_to_server"}})
	req1.Relation = req1.Relation.WithCorrelation("corr-1")
	process("req1", req1)

	// 2) 响应（同 correlation_key）→ decoded_from + response_to(correlation) + possible_followup
	resp1 := mkEventAt("LoginSC", base.Add(10*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-2", Direction: "server_to_client"},
		map[string]any{"_meta": map[string]any{"msg_name": "LoginSC", "direction": "server_to_client"}})
	resp1.Relation = resp1.Relation.WithCorrelation("corr-1")
	process("resp1", resp1)

	// 3) 携带 _state_changes 的推送 → caused_by + updates
	push := mkEventAt("PlayerUpdateSC", base.Add(20*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-3", Direction: "server_to_client"},
		map[string]any{
			"_meta": map[string]any{"msg_name": "PlayerUpdateSC", "direction": "server_to_client"},
			"_state_changes": []any{
				map[string]any{
					"subject_type": "player",
					"subject_id":   "p-1",
					"op":           "set",
					"path":         "hp",
					"before":       90,
					"after":        75,
				},
			},
		})
	process("push", push)

	// 4) 间隔超过 MergeGap 的新请求 → 关闭上一个事务（contains 边）并开启新事务
	req2 := mkEventAt("ItemReq", base.Add(2*time.Second),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-4", Direction: "client_to_server"},
		map[string]any{"_meta": map[string]any{"msg_name": "ItemReq", "direction": "client_to_server"}})
	process("req2", req2)

	// 5) 无 correlation_key 的响应 → response_to(name_pattern)
	resp2 := mkEventAt("ItemResp", base.Add(2*time.Second+10*time.Millisecond),
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-5", Direction: "server_to_client"},
		map[string]any{"_meta": map[string]any{"msg_name": "ItemResp", "direction": "server_to_client"}})
	process("resp2", resp2)

	// Graph() 会收尾剩余活跃事务，补齐 contains 边。
	return eng.Graph(), events
}

// TestEvidenceGraph_EdgeEndpointsAlwaysExist 是 Evidence Graph 的通用完整性不变量测试：
//
//	所有 EvidenceEdge.Source 必须存在于 Nodes
//	所有 EvidenceEdge.Target 必须存在于 Nodes
//
// 悬空端点会直接破坏 trace_event_chain、query_evidence_graph、BFS 遍历与
// UI 图渲染，属于 Graph Integrity 问题，而非代码风格问题。
func TestEvidenceGraph_EdgeEndpointsAlwaysExist(t *testing.T) {
	g, _ := buildRichGraph(t)

	nodeIDs := make(map[string]struct{}, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.ID == "" {
			t.Errorf("node with empty ID: kind=%s", n.Kind)
			continue
		}
		if _, dup := nodeIDs[n.ID]; dup {
			t.Errorf("duplicate node ID %q", n.ID)
		}
		nodeIDs[n.ID] = struct{}{}
	}

	if len(g.Edges) == 0 {
		t.Fatal("scenario produced no edges; invariant test would be vacuous")
	}

	for _, e := range g.Edges {
		if _, ok := nodeIDs[e.Source]; !ok {
			t.Errorf("dangling edge source: edge=%s type=%s source=%q not in Nodes", e.ID, e.Type, e.Source)
		}
		if _, ok := nodeIDs[e.Target]; !ok {
			t.Errorf("dangling edge target: edge=%s type=%s target=%q not in Nodes", e.ID, e.Type, e.Target)
		}
	}
}

// TestEvidenceGraph_ScenarioCoversAllEdgeTypes 保证上面的不变量测试不是空跑：
// 场景必须真的产出每一种边类型，否则某类边的悬空问题会被静默漏过。
func TestEvidenceGraph_ScenarioCoversAllEdgeTypes(t *testing.T) {
	g, _ := buildRichGraph(t)

	seen := make(map[RelationType]int)
	for _, e := range g.Edges {
		seen[e.Type]++
	}

	want := []RelationType{DecodedFrom, CausedBy, Updates, ResponseTo, PossibleFollowup, Contains}
	for _, rt := range want {
		if seen[rt] == 0 {
			t.Errorf("scenario did not produce any %q edge; invariant coverage incomplete (seen=%v)", rt, seen)
		}
	}

	// response_to 应同时出现 correlation 与 name_pattern 两种来源。
	methods := make(map[EvidenceMethod]int)
	for _, e := range g.Edges {
		if e.Type == ResponseTo {
			methods[e.Method]++
		}
	}
	if methods[MethodCorrelation] == 0 {
		t.Error("missing response_to edge produced by correlation_key")
	}
	if methods[MethodNamePattern] == 0 {
		t.Error("missing response_to edge produced by name pattern")
	}
}

// TestEvidenceGraph_EventTargetsUseNodeIDForm 回归测试：指向事件的边，其端点必须是
// 事件节点 ID（evt_<id>），不能是裸 event.EventID。
func TestEvidenceGraph_EventTargetsUseNodeIDForm(t *testing.T) {
	g, events := buildRichGraph(t)

	eventNodeIDs := make(map[string]struct{})
	bareEventIDs := make(map[string]string) // 裸 UUID -> 事件名
	for name, ev := range events {
		eventNodeIDs[eventNodeID(ev.Identity.ID)] = struct{}{}
		bareEventIDs[string(ev.Identity.ID)] = name
	}

	var sawCausedBy, sawResponseTo, sawFollowup bool
	for _, e := range g.Edges {
		for _, endpoint := range []struct {
			role string
			id   string
		}{{"source", e.Source}, {"target", e.Target}} {
			if name, bare := bareEventIDs[endpoint.id]; bare {
				t.Errorf("edge %s (%s) %s is a bare EventID of %s (%q); expected node ID form %q",
					e.ID, e.Type, endpoint.role, name, endpoint.id, eventNodeID(event.EventID(endpoint.id)))
			}
		}

		switch e.Type {
		case CausedBy:
			sawCausedBy = true
			if !strings.HasPrefix(e.Target, "evt_") {
				t.Errorf("caused_by target %q must be an event node ID (evt_<id>)", e.Target)
			}
			if _, ok := eventNodeIDs[e.Target]; !ok {
				t.Errorf("caused_by target %q is not a known event node", e.Target)
			}
		case ResponseTo:
			sawResponseTo = true
			if _, ok := eventNodeIDs[e.Target]; !ok {
				t.Errorf("response_to target %q is not a known event node", e.Target)
			}
		case PossibleFollowup:
			sawFollowup = true
			if _, ok := eventNodeIDs[e.Target]; !ok {
				t.Errorf("possible_followup target %q is not a known event node", e.Target)
			}
		}
	}

	if !sawCausedBy || !sawResponseTo || !sawFollowup {
		t.Errorf("regression scenario incomplete: caused_by=%v response_to=%v possible_followup=%v",
			sawCausedBy, sawResponseTo, sawFollowup)
	}
}

// TestEngine_DropsEdgeWithUnknownEndpoint 验证不变量是结构性强制的：
// 即使调用方误传了图中不存在的节点 ID，边也不会进入图。
func TestEngine_DropsEdgeWithUnknownEndpoint(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	ev := mkEventAt("PingCS", time.Unix(1700000000, 0).UTC(),
		event.EventContext{FlowID: "flow-x", Direction: "client_to_server"},
		map[string]any{"_meta": map[string]any{"msg_name": "PingCS"}})
	if _, err := eng.Process(ev); err != nil {
		t.Fatalf("process: %v", err)
	}

	src := eventNodeID(ev.Identity.ID)
	before := len(eng.Graph().Edges)

	// target 不存在
	eng.addEdgeFromNode(src, "evt_does-not-exist", CausedBy, 1.0, "bogus", nil, edgeMeta{})
	// source 不存在
	eng.addEdgeFromNode("evt_also-missing", src, CausedBy, 1.0, "bogus", nil, edgeMeta{})
	// 裸 EventID 作为 target（历史 bug 形态）
	eng.addEdgeFromNode(src, string(ev.Identity.ID), CausedBy, 1.0, "bogus", nil, edgeMeta{})

	if after := len(eng.Graph().Edges); after != before {
		t.Errorf("edges with unknown endpoints were accepted: before=%d after=%d", before, after)
	}
}
