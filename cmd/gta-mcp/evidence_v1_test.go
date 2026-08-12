package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gta/pkg/analyze/semantic"
	"gta/pkg/event"
	"gta/pkg/store"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// setupEvidenceV1Session 构造一个最小 mcpCapture，写入一份含 v1 字段的证据图，
// 供 query_evidence_graph / trace_event_chain handler 验证输出。
func setupEvidenceV1Session(t *testing.T) (m *mcpCapture, sessionID, dbPath string) {
	t.Helper()
	workDir := t.TempDir()

	sessionMgr := newSessionManager(workDir)
	runRegistry, err := NewRunRegistry(workDir)
	if err != nil {
		t.Fatalf("NewRunRegistry: %v", err)
	}
	m = &mcpCapture{
		workDir:     workDir,
		sessionMgr:  sessionMgr,
		runRegistry: runRegistry,
	}
	m.readerOpener = sqliteReaderOpener()

	sessionID = sessionMgr.generateSessionID()
	sessionDir := sessionMgr.sessionDir(sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath = sessionMgr.absDBPath(sessionID)
	st, err := store.NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer st.Close()

	if err := sessionMgr.writeCurrent(sessionMetadata{
		SessionID: sessionID,
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		DBPath:    dbPath,
	}); err != nil {
		t.Fatal(err)
	}

	proj := semantic.NewSemanticProjector()
	ev := event.NewEventWithTime("sess-1", event.EventType("http.request"), "http.request.v1",
		event.SourceID("tcp"), event.ValueFromAny(map[string]any{"_meta": map[string]any{"msg_name": "GET /", "direction": "client_to_server"}}),
		time.Now(), event.EventContext{FlowID: "flow-1"})
	ev.Identity.ID = event.EventID("req-1")
	sem, _ := json.Marshal(proj.Project(ev))
	nodes := []store.EvidenceNodeRow{
		{
			ID:        "evt_req-1",
			SessionID: sessionID,
			Kind:      "event",
			FlowID:    "flow-1",
			Timestamp: time.Now().UnixNano(),
			Semantic:  string(sem),
		},
		{
			ID:        "evt_resp-1",
			SessionID: sessionID,
			Kind:      "event",
			FlowID:    "flow-1",
			Timestamp: time.Now().UnixNano(),
		},
		{
			ID:        "pkt_pkt-1",
			SessionID: sessionID,
			Kind:      "raw_packet",
			Timestamp: time.Now().UnixNano(),
		},
	}
	evIDs, _ := json.Marshal([]string{"pkt-1"})
	edges := []store.EvidenceEdgeRow{
		{
			ID:          "edge_evt_req-1_pkt_pkt-1_decoded_from",
			SessionID:   sessionID,
			Source:      "evt_req-1",
			Target:      "pkt_pkt-1",
			Type:        "decoded_from",
			Confidence:  1.0,
			Reason:      "event decoded from raw packet pkt-1",
			Strength:    string(semantic.EvidenceObserved),
			Method:      string(semantic.MethodPlugin),
			EvidenceIDs: string(evIDs),
		},
		{
			ID:          "edge_evt_resp-1_evt_req-1_response_to",
			SessionID:   sessionID,
			Source:      "evt_resp-1",
			Target:      "evt_req-1",
			Type:        "response_to",
			Confidence:  1.0,
			Reason:      "response matched request by correlation_key=corr-1",
			Strength:    string(semantic.EvidenceDerived),
			Method:      string(semantic.MethodCorrelation),
			RuleID:      "correlation_key",
			EvidenceIDs: string(evIDs),
		},
	}
	if err := st.WriteEvidenceGraph(context.Background(), sessionID, "run-1", nodes, edges); err != nil {
		t.Fatalf("WriteEvidenceGraph: %v", err)
	}
	return m, sessionID, dbPath
}

// TestE2E_QueryEvidenceGraph_V1Fields 验证 query_evidence_graph 输出统一的 v1 Contract：
// 事件节点带 Semantic 投影，边带 Strength/Method/RuleID/EvidenceIDs。
func TestE2E_QueryEvidenceGraph_V1Fields(t *testing.T) {
	m, sessionID, _ := setupEvidenceV1Session(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"session_id": sessionID}

	res, err := m.handleQueryEvidenceGraph(context.Background(), req)
	if err != nil {
		t.Fatalf("handleQueryEvidenceGraph: %v", err)
	}

	var out struct {
		OK    bool             `json:"ok"`
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	raw := contentText(res)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	// 事件节点应携带 Semantic 投影。
	var reqNode map[string]any
	for _, n := range out.Nodes {
		if n["id"] == "evt_req-1" {
			reqNode = n
		}
	}
	if reqNode == nil {
		t.Fatal("evt_req-1 node missing from output")
	}
	semRaw, ok := reqNode["semantic"]
	if !ok {
		t.Fatal("event node missing semantic field (v1 contract)")
	}
	var sem semantic.SemanticEvent
	if b, _ := json.Marshal(semRaw); json.Unmarshal(b, &sem) != nil {
		t.Fatalf("semantic is not a valid SemanticEvent: %v", semRaw)
	}
	if sem.Kind != semantic.SemanticRequest || sem.Source != semantic.SourceEngine {
		t.Errorf("semantic not v1-conformant: kind=%q source=%q", sem.Kind, sem.Source)
	}

	// decoded_from 边应带 Strength/Method/EvidenceIDs。
	var decoded map[string]any
	for _, e := range out.Edges {
		if e["type"] == "decoded_from" {
			decoded = e
		}
	}
	if decoded == nil {
		t.Fatal("decoded_from edge missing")
	}
	assertField(t, decoded, "strength", string(semantic.EvidenceObserved))
	assertField(t, decoded, "method", string(semantic.MethodPlugin))
	if _, ok := decoded["evidence_ids"]; !ok {
		t.Error("decoded_from edge missing evidence_ids")
	}
	// Confidence 与 Strength 必须并列存在、含义不同。
	if _, ok := decoded["confidence"]; !ok {
		t.Error("edge missing confidence (must remain distinct from strength)")
	}

	// response_to 边应带 RuleID + Method=correlation。
	var respTo map[string]any
	for _, e := range out.Edges {
		if e["type"] == "response_to" {
			respTo = e
		}
	}
	if respTo == nil {
		t.Fatal("response_to edge missing")
	}
	assertField(t, respTo, "method", string(semantic.MethodCorrelation))
	assertField(t, respTo, "rule_id", "correlation_key")
}

// TestE2E_TraceEventChain_V1Fields 验证 trace_event_chain 的链路与跳点都带 v1 字段。
func TestE2E_TraceEventChain_V1Fields(t *testing.T) {
	m, sessionID, _ := setupEvidenceV1Session(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": sessionID,
		"event_id":   "resp-1",
		"max_depth":  3,
	}

	res, err := m.handleTraceEventChain(context.Background(), req)
	if err != nil {
		t.Fatalf("handleTraceEventChain: %v", err)
	}

	var out struct {
		OK         bool             `json:"ok"`
		Nodes      []map[string]any `json:"nodes"`
		Edges      []map[string]any `json:"edges"`
		Downstream []map[string]any `json:"downstream"`
	}
	if err := json.Unmarshal([]byte(contentText(res)), &out); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	if len(out.Nodes) < 2 {
		t.Fatalf("expected >=2 nodes in trace, got %d", len(out.Nodes))
	}
	// 节点应带 Semantic（evt_req-1）。
	var reqNode map[string]any
	for _, n := range out.Nodes {
		if n["id"] == "evt_req-1" {
			reqNode = n
		}
	}
	if reqNode == nil || reqNode["semantic"] == nil {
		t.Error("traced node evt_req-1 missing v1 semantic projection")
	}

	// response_to 边应带 v1 字段。
	var respTo map[string]any
	for _, e := range out.Edges {
		if e["type"] == "response_to" {
			respTo = e
		}
	}
	if respTo == nil {
		t.Fatal("response_to edge missing from trace")
	}
	assertField(t, respTo, "method", string(semantic.MethodCorrelation))
	assertField(t, respTo, "rule_id", "correlation_key")

	// response_to 边方向为 resp → req（source=resp, target=req），故从 resp 出发得到的是
	// downstream 跳点（resp → req）；该跳点应带 v1 字段，无需回查边表。
	if len(out.Downstream) == 0 {
		t.Fatal("expected at least one downstream hop (resp → req)")
	}
	assertField(t, out.Downstream[0], "method", string(semantic.MethodCorrelation))
	assertField(t, out.Downstream[0], "rule_id", "correlation_key")
}

func assertField(t *testing.T, obj map[string]any, key, want string) {
	t.Helper()
	got, ok := obj[key]
	if !ok {
		t.Errorf("missing field %q in %v", key, obj)
		return
	}
	if gs, ok := got.(string); !ok || gs != want {
		t.Errorf("field %q = %v (%T), want %q", key, got, got, want)
	}
}
