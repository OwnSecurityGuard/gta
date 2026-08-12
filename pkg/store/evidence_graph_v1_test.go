package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestEvidenceGraph_V1FieldsRoundTrip 验证 Phase 3：写入的 v1 结构化字段
// （节点 Semantic、边的 Strength/Method/RuleID/EvidenceIDs）能正确持久化并读回。
func TestEvidenceGraph_V1FieldsRoundTrip(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	nodes := []EvidenceNodeRow{
		{
			ID:         "evt_1",
			SessionID:  "s1",
			Kind:       "event",
			FlowID:     "f1",
			Timestamp:  123,
			Labels:     `{"type":"http.request"}`,
			Properties: `{}`,
			Semantic:   `{"event_id":"1","session_id":"s1","kind":"request","name":"GET /","confidence":1,"source":"engine"}`,
		},
	}
	edges := []EvidenceEdgeRow{
		{
			ID:          "edge_1",
			SessionID:   "s1",
			Source:      "evt_1",
			Target:      "pkt_1",
			Type:        "decoded_from",
			Confidence:  1.0,
			Reason:      "decoded",
			Properties:  `{}`,
			Strength:    "observed",
			Method:      "plugin",
			RuleID:      "",
			EvidenceIDs: `["pkt_1"]`,
		},
	}

	if err := s.WriteEvidenceGraph(context.Background(), "s1", "s1", nodes, edges); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := s.QueryEvidenceGraph(context.Background(), EvidenceGraphQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(res.Edges))
	}
	e := res.Edges[0]
	if e.Strength != "observed" || e.Method != "plugin" {
		t.Errorf("edge Strength/Method = %q/%q, want observed/plugin", e.Strength, e.Method)
	}
	if e.EvidenceIDs != `["pkt_1"]` {
		t.Errorf("edge EvidenceIDs = %q, want [\"pkt_1\"]", e.EvidenceIDs)
	}

	if len(res.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(res.Nodes))
	}
	n := res.Nodes[0]
	if !strings.Contains(n.Semantic, `"kind":"request"`) {
		t.Errorf("node Semantic = %q, missing kind after round-trip", n.Semantic)
	}
}
