package semantic

import (
	"testing"

	"gta/pkg/event"
)

// TestSemanticEngine_V1EdgeAndNodeFields 验证 Phase 3：引擎产出的 EvidenceGraph
// 已填充 v1 结构化字段——边的 Strength/Method/RuleID/EvidenceIDs，以及事件节点的 Semantic。
func TestSemanticEngine_V1EdgeAndNodeFields(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	req := mkEvent("http.request",
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-1", Direction: "client_to_server"},
		map[string]any{"_meta": map[string]any{"msg_name": "GET /", "direction": "client_to_server"}})
	req.Relation = req.Relation.WithCorrelation("corr-1")
	if _, err := eng.Process(req); err != nil {
		t.Fatalf("process req: %v", err)
	}

	resp := mkEvent("http.response",
		event.EventContext{FlowID: "flow-1", RawPacketID: "pkt-2", Direction: "server_to_client"},
		map[string]any{"_meta": map[string]any{"msg_name": "resp 200", "direction": "server_to_client"}})
	resp.Relation = resp.Relation.WithCorrelation("corr-1")
	if _, err := eng.Process(resp); err != nil {
		t.Fatalf("process resp: %v", err)
	}

	g := eng.Graph()

	// 事件节点应携带 Semantic（Phase 2 确定性投影）。
	var reqNode *EvidenceNode
	for i := range g.Nodes {
		if g.Nodes[i].ID == "evt_"+string(req.Identity.ID) {
			reqNode = &g.Nodes[i]
		}
	}
	if reqNode == nil || reqNode.Semantic == nil {
		t.Fatal("request event node missing Semantic")
	}
	if reqNode.Semantic.Kind != SemanticRequest {
		t.Errorf("req Semantic.Kind = %q, want request", reqNode.Semantic.Kind)
	}
	if reqNode.Semantic.Source != SourceEngine {
		t.Errorf("req Semantic.Source = %q, want engine", reqNode.Semantic.Source)
	}

	var decoded, respTo *EvidenceEdge
	for i := range g.Edges {
		e := &g.Edges[i]
		switch e.Type {
		case DecodedFrom:
			// 选取请求事件对应的 decoded_from 边（其 EvidenceIDs[0] == pkt-1）。
			if len(e.EvidenceIDs) > 0 && e.EvidenceIDs[0] == "pkt-1" {
				decoded = e
			}
		case ResponseTo:
			respTo = e
		}
	}

	if decoded == nil {
		t.Fatal("missing decoded_from edge")
	}
	if decoded.Strength != EvidenceObserved || decoded.Method != MethodPlugin {
		t.Errorf("decoded_from Strength/Method = %q/%q, want observed/plugin", decoded.Strength, decoded.Method)
	}
	if len(decoded.EvidenceIDs) != 1 || decoded.EvidenceIDs[0] != "pkt-1" {
		t.Errorf("decoded_from EvidenceIDs = %v, want [pkt-1]", decoded.EvidenceIDs)
	}

	if respTo == nil {
		t.Fatal("missing response_to edge")
	}
	// Confidence（判定可信度）与 Strength（证据强度）必须分离。
	if respTo.Confidence != 1.0 {
		t.Errorf("response_to Confidence = %v, want 1.0 (credibility stays separate from Strength)", respTo.Confidence)
	}
	if respTo.Strength != EvidenceDerived || respTo.Method != MethodCorrelation {
		t.Errorf("response_to Strength/Method = %q/%q, want derived/correlation", respTo.Strength, respTo.Method)
	}
	if respTo.RuleID != "correlation_key" {
		t.Errorf("response_to RuleID = %q, want correlation_key", respTo.RuleID)
	}
	if len(respTo.EvidenceIDs) != 2 {
		t.Errorf("response_to EvidenceIDs = %v, want [reqID, respID]", respTo.EvidenceIDs)
	}
}
