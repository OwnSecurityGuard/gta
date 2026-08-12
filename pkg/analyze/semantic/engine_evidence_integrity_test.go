package semantic

import (
	"strings"
	"testing"
	"time"

	"gta/pkg/event"
)

// resolvableEvidenceIDSet 从一份已产出的 EvidenceGraph 计算出所有"可被 EvidenceIDs
// 合法引用"的 ID 集合，供不变量测试使用：
//
//  1. 全部节点 ID（直接形式 evt_/pkt_/sc_/ent_/txn_）
//  2. 每个 event 节点的裸事件 ID（节点 ID 去掉 "evt_" 前缀）
//  3. 每个 raw_packet 节点的裸原始包 ID（节点 ID 去掉 "pkt_" 前缀）
//
// 这三类正好对应 v1 当前所有边写入的 EvidenceIDs 形态（裸事件 ID 或裸原始包 ID），
// 也覆盖了"直接节点 ID 形式"的未来可能写法（直接命中第 1 类）。
// 返回 hasEvent / hasRaw 标志，用于保证测试场景覆盖了两种底层证据引用，而非空跑。
func resolvableEvidenceIDSet(g *EvidenceGraph) (set map[string]struct{}, hasEvent, hasRaw bool) {
	set = make(map[string]struct{})
	for _, n := range g.Nodes {
		set[n.ID] = struct{}{}
		switch n.Kind {
		case NodeEvent:
			if bare := strings.TrimPrefix(n.ID, "evt_"); bare != "" {
				set[bare] = struct{}{}
				hasEvent = true
			}
		case NodeRawPacket:
			if bare := strings.TrimPrefix(n.ID, "pkt_"); bare != "" {
				set[bare] = struct{}{}
				hasRaw = true
			}
		}
	}
	return set, hasEvent, hasRaw
}

// TestEvidenceGraph_EvidenceIDsResolveToGraph 是 EvidenceIDs 的通用完整性不变量测试：
//
//	对于每条边，其 EvidenceIDs 中的每一个 ID 都必须能在 Graph 中解析到
//	一个真实存在的 Event 节点 / 原始包节点 / 节点 ID。
//
// 否则 Agent 据 evidence_ids 去追溯证据时会走到虚空（与悬空端点同为静默不可信问题）。
func TestEvidenceGraph_EvidenceIDsResolveToGraph(t *testing.T) {
	g, _ := buildRichGraph(t)

	resolvable, _, _ := resolvableEvidenceIDSet(g)

	edgesWithEvidence := 0
	for _, e := range g.Edges {
		if len(e.EvidenceIDs) == 0 {
			continue
		}
		edgesWithEvidence++
		for _, id := range e.EvidenceIDs {
			if _, ok := resolvable[id]; !ok {
				t.Errorf("edge %s (%s) references unresolvable evidence_id %q; graph has no matching event/raw_packet/node",
					e.ID, e.Type, id)
			}
		}
	}
	if edgesWithEvidence == 0 {
		t.Fatal("scenario produced no edges with evidence_ids; invariant test would be vacuous")
	}
}

// TestEvidenceGraph_EvidenceIDsCoverBothEvidenceKinds 保证上面的不变量测试不是空跑：
// 场景必须同时产出"引用事件 ID"与"引用原始包 ID"的边，否则某一类证据的悬空问题会被静默漏过。
func TestEvidenceGraph_EvidenceIDsCoverBothEvidenceKinds(t *testing.T) {
	g, _ := buildRichGraph(t)

	_, hasEvent, hasRaw := resolvableEvidenceIDSet(g)
	_ = g // g 仅用于驱动解析；hasEvent/hasRaw 已体现节点覆盖

	if !hasEvent {
		t.Error("scenario produced no event node; invariant coverage incomplete (no bare event id reference possible)")
	}
	if !hasRaw {
		t.Error("scenario produced no raw_packet node; invariant coverage incomplete (no bare raw packet id reference possible)")
	}

	// 确认场景中确实存在引用两种底层证据的边。
	var sawEventRef, sawRawRef bool
	resolvable, _, _ := resolvableEvidenceIDSet(g)
	for _, e := range g.Edges {
		for _, id := range e.EvidenceIDs {
			if _, ok := resolvable[id]; !ok {
				continue // 已被主测试覆盖，这里只看已解析的引用分类
			}
			// 原始包 ID 在测试场景中以 "pkt-" 前缀出现（见 buildRichGraph 的 RawPacketID）。
			if strings.HasPrefix(id, "pkt-") {
				sawRawRef = true
			} else {
				sawEventRef = true
			}
		}
	}
	if !sawEventRef {
		t.Error("no edge references an event id via evidence_ids")
	}
	if !sawRawRef {
		t.Error("no edge references a raw packet id via evidence_ids")
	}
}

// TestEngine_DropsEdgeWithUnresolvableEvidenceID 验证不变量是结构性强制的：
// 即使调用方把一个图中根本不存在的 evidence_id（如 "event-999"）传进来，
// 该边也不会进入图（被丢弃 / 明确降级），而非静默写入一个无法追溯的边。
func TestEngine_DropsEdgeWithUnresolvableEvidenceID(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	ev := mkEventAt("PingCS", time.Unix(1700000000, 0).UTC(),
		event.EventContext{FlowID: "flow-x", Direction: "client_to_server"},
		map[string]any{"_meta": map[string]any{"msg_name": "PingCS"}})
	if _, err := eng.Process(ev); err != nil {
		t.Fatalf("process: %v", err)
	}

	src := eventNodeID(ev.Identity.ID)
	before := len(eng.Graph().Edges)

	// 1) 完全凭空捏造的 evidence_id
	eng.addEdgeFromNode(src, src, CausedBy, 1.0, "bogus", nil,
		edgeMeta{EvidenceIDs: []string{"event-999"}})
	// 2) 混合：一个合法 + 一个非法 → 整条边仍必须丢弃
	eng.addEdgeFromNode(src, src, CausedBy, 1.0, "bogus", nil,
		edgeMeta{EvidenceIDs: []string{string(ev.Identity.ID), "event-999"}})

	if after := len(eng.Graph().Edges); after != before {
		t.Errorf("edge with unresolvable evidence_id was accepted: before=%d after=%d", before, after)
	}
}

// TestEngine_KeepsEdgeWithResolvableEvidenceID 反向验证：引用合法事件/原始包 ID 的边
// 不应被 EvidenceIDs 校验误伤——保证上面的"丢弃"测试不是因为校验过于激进而假阳性通过。
func TestEngine_KeepsEdgeWithResolvableEvidenceID(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	ev := mkEventAt("PingCS", time.Unix(1700000000, 0).UTC(),
		event.EventContext{FlowID: "flow-x", RawPacketID: "pkt-x", Direction: "client_to_server"},
		map[string]any{"_meta": map[string]any{"msg_name": "PingCS"}})
	if _, err := eng.Process(ev); err != nil {
		t.Fatalf("process: %v", err)
	}

	src := eventNodeID(ev.Identity.ID)
	before := len(eng.Graph().Edges)

	// 引用裸事件 ID 与裸原始包 ID（二者均存在）→ 边应保留
	eng.addEdgeFromNode(src, src, CausedBy, 1.0, "bogus", nil,
		edgeMeta{EvidenceIDs: []string{string(ev.Identity.ID), "pkt-x"}})

	if after := len(eng.Graph().Edges); after != before+1 {
		t.Errorf("edge with resolvable evidence_ids was wrongly dropped: before=%d after=%d", before, after)
	}
}
