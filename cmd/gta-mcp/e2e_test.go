package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/event"
	"gta/pkg/store"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// sqliteReaderOpener 返回一个使用 store.NewSQLiteStore 的 readerOpener，供测试注入。
func sqliteReaderOpener() func(dbPath string) (captureReader, error) {
	return func(dbPath string) (captureReader, error) {
		return store.NewSQLiteStore(dbPath, nil)
	}
}

// contentText 从 CallToolResult 提取文本内容。
func contentText(r *mcp.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// TestE2E_RunWindowAndTrace 是 P0 端到端集成测试：
// 验证 begin_capture_run → 写入结构化事件 → end_capture_run → trace_protocol_flow 全链路。
//
// 测试场景：模拟游戏协议的 BuildingUpgrade 操作
//   - LoginReq/LoginResp（方向配对，msg_name 后缀匹配）
//   - BuildingUpgradeReq/BuildingUpgradeResp（主操作）
//   - dbstate.BuildingUpdate push（服务器推送）
//   - Building entity snapshot（实体变更）
func TestE2E_RunWindowAndTrace(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()

	// 1. 初始化 mcpCapture（绕过 newMcpCapture 的依赖，手工构造）
	sessionMgr := newSessionManager(workDir)
	runRegistry, err := NewRunRegistry(workDir)
	if err != nil {
		t.Fatalf("NewRunRegistry: %v", err)
	}
	m := &mcpCapture{
		workDir:     workDir,
		sessionMgr:  sessionMgr,
		runRegistry: runRegistry,
	}

	// 2. 创建 session 并初始化 store
	sessionID := sessionMgr.generateSessionID()
	sessionDir := sessionMgr.sessionDir(sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath := sessionMgr.absDBPath(sessionID)
	st, err := store.NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer st.Close()
	m.readerOpener = sqliteReaderOpener() // 注入 readerOpener，配合 WAL 模式避免 Windows SQLite 文件锁
	// 写入 current.json，让 handleBeginCaptureRun 的 readCurrent 能读到 sessionID，
	// 避免 begin 内部又生成新 sessionID 导致查询 session_id 不匹配。
	if err := sessionMgr.writeCurrent(sessionMetadata{
		SessionID: sessionID,
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		DBPath:    dbPath,
	}); err != nil {
		t.Fatal(err)
	}

	// 3. begin_capture_run
	beginReq := mcp.CallToolRequest{}
	beginReq.Params.Arguments = map[string]any{
		"feature_name": "upgrade_building",
		"project_path": "/tmp/loadtest",
	}
	// 由于没有运行中的 capture，预期 time_window_only 模式
	beginResult, err := m.handleBeginCaptureRun(ctx, beginReq)
	if err != nil {
		t.Fatalf("begin_capture_run: %v", err)
	}
	if beginResult.IsError {
		t.Fatalf("begin_capture_run returned error: %s", contentText(beginResult))
	}

	var beginResp map[string]any
	if err := json.Unmarshal([]byte(contentText(beginResult)), &beginResp); err != nil {
		t.Fatalf("unmarshal begin response: %v", err)
	}
	runID := beginResp["run_id"].(string)
	if beginResp["capture_isolation_mode"] != "time_window_only" {
		t.Errorf("isolation_mode = %v, want time_window_only", beginResp["capture_isolation_mode"])
	}
	t.Logf("begin_capture_run: run_id=%s, mode=%s", runID, beginResp["capture_isolation_mode"])

	// 4. 模拟 dispatcher 写入结构化事件（BuildingUpgrade 操作）
	// 事件时间必须在 run 窗口内（begin 之后，end 之前）
	// 注意：end_capture_run 在本测试中紧接着调用，时间窗口只有几毫秒，
	// 所以事件时间用 begin 的 time_from（确保在窗口起点），后续 end 的 now 会在事件之后。
	src := "127.0.0.1:5000"
	dst := "127.0.0.1:8080"
	flowIDStr := "7777"
	// 从 begin 响应解析 time_from，确保事件时间在 run 窗口内
	// 注意：time.Parse 默认返回 UTC 时区，必须 Local() 转换，否则与存储的本地时区不匹配
	// 加 1ms 偏移确保事件时间 > rec.TimeFrom，避免 BETWEEN 边界精度问题导致首条事件被排除
	baseTime, _ := time.Parse(time.RFC3339Nano, beginResp["time_from"].(string))
	baseTime = baseTime.Local().Add(1 * time.Millisecond)

	events := []*event.Event{
		// Login 请求/响应（前序操作，验证 noise_filter 不误杀）
		{
			Identity: event.Identity{
				ID: "ev-login-req", Timestamp: baseTime,
				SessionID: sessionID, Type: "tcp",
				SchemaID: "tcp.v1", Source: "test",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value: event.Value{
					Kind: event.Object,
					Object: map[string]event.Value{
						"type":      {Kind: event.String, Str: "request"},
						"method":    {Kind: event.String, Str: "POST"},
						"path":      {Kind: event.String, Str: "/login"},
						"src":       {Kind: event.String, Str: src},
						"dst":       {Kind: event.String, Str: dst},
						"flow_id":   {Kind: event.String, Str: flowIDStr},
						"direction": {Kind: event.String, Str: "client_to_server"},
						"msg_name":  {Kind: event.String, Str: "LoginReq"},
					},
				},
			},
		},
		{
			Identity: event.Identity{
				ID: "ev-login-resp", Timestamp: baseTime.Add(30 * time.Millisecond),
				SessionID: sessionID, Type: "tcp",
				SchemaID: "tcp.v1", Source: "test",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value: event.Value{
					Kind: event.Object,
					Object: map[string]event.Value{
						"type":      {Kind: event.String, Str: "response"},
						"status":    {Kind: event.String, Str: "200"},
						"src":       {Kind: event.String, Str: dst},
						"dst":       {Kind: event.String, Str: src},
						"flow_id":   {Kind: event.String, Str: flowIDStr},
						"direction": {Kind: event.String, Str: "server_to_client"},
						"msg_name":  {Kind: event.String, Str: "LoginResp"},
					},
				},
			},
		},
		// 心跳消息（验证 noise_filter 过滤）
		{
			Identity: event.Identity{
				ID: "ev-heartbeat", Timestamp: baseTime.Add(100 * time.Millisecond),
				SessionID: sessionID, Type: "tcp",
				SchemaID: "tcp.v1", Source: "test",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value: event.Value{
					Kind: event.Object,
					Object: map[string]event.Value{
						"type":      {Kind: event.String, Str: "heartbeat"},
						"src":       {Kind: event.String, Str: dst},
						"dst":       {Kind: event.String, Str: src},
						"flow_id":   {Kind: event.String, Str: flowIDStr},
						"direction": {Kind: event.String, Str: "server_to_client"},
						"msg_name":  {Kind: event.String, Str: "Heartbeat"},
						"is_push":   {Kind: event.Bool, Bool: true},
					},
				},
			},
		},
		// BuildingUpgrade 请求（主操作）
		{
			Identity: event.Identity{
				ID: "ev-upgrade-req", Timestamp: baseTime.Add(500 * time.Millisecond),
				SessionID: sessionID, Type: "tcp",
				SchemaID: "tcp.v1", Source: "test",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value: event.Value{
					Kind: event.Object,
					Object: map[string]event.Value{
						"type":         {Kind: event.String, Str: "request"},
						"building_id":  {Kind: event.String, Str: "1001"},
						"target_level": {Kind: event.Int, Int: 3},
						"src":          {Kind: event.String, Str: src},
						"dst":          {Kind: event.String, Str: dst},
						"flow_id":      {Kind: event.String, Str: flowIDStr},
						"direction":    {Kind: event.String, Str: "client_to_server"},
						"msg_name":     {Kind: event.String, Str: "BuildingUpgradeReq"},
					},
				},
			},
		},
		// BuildingUpgrade 响应
		{
			Identity: event.Identity{
				ID: "ev-upgrade-resp", Timestamp: baseTime.Add(600 * time.Millisecond),
				SessionID: sessionID, Type: "tcp",
				SchemaID: "tcp.v1", Source: "test",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value: event.Value{
					Kind: event.Object,
					Object: map[string]event.Value{
						"type":      {Kind: event.String, Str: "response"},
						"code":      {Kind: event.Int, Int: 0},
						"src":       {Kind: event.String, Str: dst},
						"dst":       {Kind: event.String, Str: src},
						"flow_id":   {Kind: event.String, Str: flowIDStr},
						"direction": {Kind: event.String, Str: "server_to_client"},
						"msg_name":  {Kind: event.String, Str: "BuildingUpgradeResp"},
					},
				},
			},
		},
		// dbstate push（服务器推送 BuildingUpdate）
		{
			Identity: event.Identity{
				ID: "ev-push", Timestamp: baseTime.Add(650 * time.Millisecond),
				SessionID: sessionID, Type: "tcp",
				SchemaID: "tcp.v1", Source: "test",
			},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value: event.Value{
					Kind: event.Object,
					Object: map[string]event.Value{
						"type":        {Kind: event.String, Str: "push"},
						"name":        {Kind: event.String, Str: "dbstate.BuildingUpdate"},
						"building_id": {Kind: event.String, Str: "1001"},
						"src":         {Kind: event.String, Str: dst},
						"dst":         {Kind: event.String, Str: src},
						"flow_id":     {Kind: event.String, Str: flowIDStr},
						"direction":   {Kind: event.String, Str: "server_to_client"},
						"msg_name":    {Kind: event.String, Str: "dbstate.BuildingUpdate"},
						"is_push":     {Kind: event.Bool, Bool: true},
						"_state_changes": {Kind: event.Array, Array: []event.Value{
							{Kind: event.Object, Object: map[string]event.Value{
								"subject_type": {Kind: event.String, Str: "Building"},
								"subject_id":   {Kind: event.String, Str: "1001"},
								"path":         {Kind: event.String, Str: "level"},
								"after":        {Kind: event.Int, Int: 3},
								"op":           {Kind: event.String, Str: "set"},
							}},
							{Kind: event.Object, Object: map[string]event.Value{
								"subject_type": {Kind: event.String, Str: "Building"},
								"subject_id":   {Kind: event.String, Str: "1001"},
								"path":         {Kind: event.String, Str: "state"},
								"after":        {Kind: event.String, Str: "upgraded"},
								"op":           {Kind: event.String, Str: "set"},
							}},
						}},
					},
				},
			},
		},
	}

	if err := st.AppendEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	// 寫入實體快照投影，供 trace_protocol_flow 計算 entity_diffs
	if err := st.WriteStateChanges(ctx, sessionID, events); err != nil {
		t.Fatalf("WriteStateChanges: %v", err)
	}
	t.Logf("wrote %d events", len(events))

	// 5. end_capture_run
	// 传入 time_to 确保窗口上界覆盖所有事件时间（最大 baseTime+650ms）。
	// 避免因 begin→end 间隔过短（仅几毫秒）导致 BETWEEN 查询漏掉事件。
	endReq := mcp.CallToolRequest{}
	endReq.Params.Arguments = map[string]any{
		"run_id":  runID,
		"time_to": baseTime.Add(1000 * time.Millisecond).Format(time.RFC3339Nano),
	}
	endResult, err := m.handleEndCaptureRun(ctx, endReq)
	if err != nil {
		t.Fatalf("end_capture_run: %v", err)
	}
	if endResult.IsError {
		t.Fatalf("end_capture_run returned error: %s", contentText(endResult))
	}
	var endResp map[string]any
	if err := json.Unmarshal([]byte(contentText(endResult)), &endResp); err != nil {
		t.Fatalf("unmarshal end response: %v", err)
	}
	summary := endResp["summary"].(map[string]any)
	t.Logf("end_capture_run: duration_ms=%v, flow_count=%v, message_count=%v, client_request=%v, server_msg=%v",
		endResp["duration_ms"], summary["captured_flow_count"], summary["captured_message_count"],
		summary["client_request_count"], summary["server_message_count"])

	// 验证 summary 统计正确
	if summary["captured_message_count"].(float64) != 6 {
		t.Errorf("captured_message_count = %v, want 6", summary["captured_message_count"])
	}
	if summary["captured_flow_count"].(float64) != 1 {
		t.Errorf("captured_flow_count = %v, want 1", summary["captured_flow_count"])
	}
	if summary["client_request_count"].(float64) != 2 {
		t.Errorf("client_request_count = %v, want 2", summary["client_request_count"])
	}

	// 6. get_capture_run_status 验证
	statusReq := mcp.CallToolRequest{}
	statusReq.Params.Arguments = map[string]any{"run_id": runID}
	statusResult, err := m.handleGetCaptureRunStatus(ctx, statusReq)
	if err != nil {
		t.Fatalf("get_capture_run_status: %v", err)
	}
	var statusResp map[string]any
	json.Unmarshal([]byte(contentText(statusResult)), &statusResp)
	if statusResp["status"] != "stopped" {
		t.Errorf("status = %v, want stopped", statusResp["status"])
	}
	t.Logf("get_capture_run_status: status=%s", statusResp["status"])

	// 7. trace_protocol_flow 主验证
	traceReq := mcp.CallToolRequest{}
	traceReq.Params.Arguments = map[string]any{
		"run_id":       runID,
		"flow_id":      "7777",
		"feature_name": "upgrade_building",
		"noise_filter": map[string]any{
			"drop_heartbeats": true,
		},
		"entity_diff": map[string]any{
			"enabled":   true,
			"window_ms": 500,
		},
	}
	traceResult, err := m.handleTraceProtocolFlow(ctx, traceReq)
	if err != nil {
		t.Fatalf("trace_protocol_flow: %v", err)
	}
	if traceResult.IsError {
		t.Fatalf("trace_protocol_flow returned error: %s", contentText(traceResult))
	}

	var traceResp map[string]any
	if err := json.Unmarshal([]byte(contentText(traceResult)), &traceResp); err != nil {
		t.Fatalf("unmarshal trace result: %v", err)
	}

	// 从 map 提取 steps（successResult 会包装为 map）
	stepsRaw, _ := traceResp["steps"].([]any)
	t.Logf("trace_protocol_flow: steps=%d, uncertainties=%v", len(stepsRaw), traceResp["uncertainties"])

	// 验证 steps（预期 2 步：Login + BuildingUpgrade，心跳被过滤）
	if len(stepsRaw) != 2 {
		t.Errorf("steps count = %d, want 2 (heartbeat filtered)", len(stepsRaw))
	}

	if len(stepsRaw) >= 1 {
		s1 := stepsRaw[0].(map[string]any)
		req := s1["request"].(map[string]any)
		if req["name"] != "LoginReq" {
			t.Errorf("step[0].request.name = %v, want LoginReq", req["name"])
		}
		resp := s1["response"].(map[string]any)
		if resp["name"] != "LoginResp" {
			t.Errorf("step[0].response.name = %v, want LoginResp", resp["name"])
		}
		if s1["why_related"] == nil || s1["why_related"] == "" {
			t.Errorf("step[0].why_related is empty")
		}
		t.Logf("step[0]: %v → %v (why: %v)", req["name"], resp["name"], s1["why_related"])
	}

	if len(stepsRaw) >= 2 {
		s2 := stepsRaw[1].(map[string]any)
		req := s2["request"].(map[string]any)
		if req["name"] != "BuildingUpgradeReq" {
			t.Errorf("step[1].request.name = %v, want BuildingUpgradeReq", req["name"])
		}
		resp := s2["response"].(map[string]any)
		if resp["name"] != "BuildingUpgradeResp" {
			t.Errorf("step[1].response.name = %v, want BuildingUpgradeResp", resp["name"])
		}
		// 验证 push 关联到 BuildingUpgrade step
		pushes, _ := s2["pushes"].([]any)
		if len(pushes) != 1 {
			t.Errorf("step[1].pushes count = %d, want 1", len(pushes))
		} else {
			p := pushes[0].(map[string]any)
			if p["name"] != "dbstate.BuildingUpdate" {
				t.Errorf("step[1].pushes[0].name = %v, want dbstate.BuildingUpdate", p["name"])
			}
			t.Logf("step[1] push: %v (summary: %v)", p["name"], p["summary"])
		}
		// 验证 entity diff
		diffs, _ := s2["entity_diffs"].([]any)
		if len(diffs) != 1 {
			t.Errorf("step[1].entity_diffs count = %d, want 1", len(diffs))
		} else {
			d := diffs[0].(map[string]any)
			if d["uri"] != "Building" || d["key"] != "1001" {
				t.Errorf("entity_diff = %+v, want Building/1001", d)
			}
			fields, _ := d["fields"].([]any)
			if len(fields) != 2 {
				t.Errorf("entity_diff fields count = %d, want 2", len(fields))
			}
			t.Logf("step[1] entity_diff: %v/%v fields=%v", d["uri"], d["key"], d["fields"])
		}
		t.Logf("step[1]: %v → %v, pushes=%d, entity_diffs=%d (why: %v)",
			req["name"], resp["name"], len(pushes), len(diffs), s2["why_related"])
	}

	// 8. 验证 run.json 持久化
	runJSON, err := os.ReadFile(filepath.Join(workDir, "runs", runID, "run.json"))
	if err != nil {
		t.Errorf("read run.json: %v", err)
	} else {
		var rec RunRecord
		if err := json.Unmarshal(runJSON, &rec); err != nil {
			t.Errorf("unmarshal run.json: %v", err)
		} else {
			if !rec.Ended {
				t.Errorf("run.json Ended = false, want true")
			}
			if rec.Summary == nil {
				t.Errorf("run.json Summary is nil")
			}
			t.Logf("run.json persisted: run_id=%s, duration=%dms, ended=%v", rec.RunID, rec.DurationMs, rec.Ended)
		}
	}

	// 9. 验证 end_capture_run 幂等性
	endResult2, err := m.handleEndCaptureRun(ctx, endReq)
	if err != nil {
		t.Errorf("second end_capture_run: %v", err)
	} else {
		var endResp2 map[string]any
		json.Unmarshal([]byte(contentText(endResult2)), &endResp2)
		if endResp2["idempotent"] != true {
			t.Errorf("second end_capture_run idempotent = %v, want true", endResp2["idempotent"])
		}
		t.Logf("second end_capture_run: idempotent=%v", endResp2["idempotent"])
	}
}

// TestE2E_TraceProtocolFlow_EmptyFlow 验证空 flow 的 trace 返回空 steps + uncertainty。
func TestE2E_TraceProtocolFlow_EmptyFlow(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	sessionMgr := newSessionManager(workDir)
	runRegistry, _ := NewRunRegistry(workDir)
	m := &mcpCapture{workDir: workDir, sessionMgr: sessionMgr, runRegistry: runRegistry}

	// 创建 session + run
	sessionID := sessionMgr.generateSessionID()
	sessionDir := sessionMgr.sessionDir(sessionID)
	os.MkdirAll(sessionDir, 0755)
	st, _ := store.NewSQLiteStore(sessionMgr.absDBPath(sessionID), nil)
	defer st.Close()
	m.readerOpener = sqliteReaderOpener() // 注入 readerOpener
	sessionMgr.writeCurrent(sessionMetadata{
		SessionID: sessionID, StartedAt: time.Now().Format(time.RFC3339),
		Status: "running", DBPath: sessionMgr.absDBPath(sessionID),
	})

	// begin_capture_run
	beginReq := mcp.CallToolRequest{}
	beginReq.Params.Arguments = map[string]any{
		"feature_name": "empty_op",
		"project_path": "/tmp",
	}
	beginResult, _ := m.handleBeginCaptureRun(ctx, beginReq)
	var beginResp map[string]any
	json.Unmarshal([]byte(contentText(beginResult)), &beginResp)
	runID := beginResp["run_id"].(string)

	// end_capture_run
	endReq := mcp.CallToolRequest{}
	endReq.Params.Arguments = map[string]any{"run_id": runID}
	m.handleEndCaptureRun(ctx, endReq)

	// trace_protocol_flow 查询不存在的 flow_id
	traceReq := mcp.CallToolRequest{}
	traceReq.Params.Arguments = map[string]any{
		"run_id":       runID,
		"flow_id":      "99999", // 不存在的 flow
		"feature_name": "empty_op",
	}
	traceResult, err := m.handleTraceProtocolFlow(ctx, traceReq)
	if err != nil {
		t.Fatalf("trace_protocol_flow: %v", err)
	}
	var resp map[string]any
	json.Unmarshal([]byte(contentText(traceResult)), &resp)
	steps, _ := resp["steps"].([]any)
	if len(steps) != 0 {
		t.Errorf("steps count = %d, want 0", len(steps))
	}
	uncertainties, _ := resp["uncertainties"].([]any)
	if len(uncertainties) == 0 {
		t.Errorf("uncertainties should not be empty for empty flow")
	}
	t.Logf("empty flow trace: steps=%d, uncertainties=%v", len(steps), uncertainties)
}

// TestE2E_TraceProtocolFlow_LargeResult 验证大结果写文件。
func TestE2E_TraceProtocolFlow_LargeResult(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	sessionMgr := newSessionManager(workDir)
	runRegistry, _ := NewRunRegistry(workDir)
	m := &mcpCapture{workDir: workDir, sessionMgr: sessionMgr, runRegistry: runRegistry}

	sessionID := sessionMgr.generateSessionID()
	sessionDir := sessionMgr.sessionDir(sessionID)
	os.MkdirAll(sessionDir, 0755)
	st, _ := store.NewSQLiteStore(sessionMgr.absDBPath(sessionID), nil)
	defer st.Close()
	m.readerOpener = sqliteReaderOpener() // 注入 readerOpener
	sessionMgr.writeCurrent(sessionMetadata{
		SessionID: sessionID, StartedAt: time.Now().Format(time.RFC3339),
		Status: "running", DBPath: sessionMgr.absDBPath(sessionID),
	})

	// begin
	beginReq := mcp.CallToolRequest{}
	beginReq.Params.Arguments = map[string]any{
		"feature_name": "bulk_op",
		"project_path": "/tmp",
	}
	beginResult, _ := m.handleBeginCaptureRun(ctx, beginReq)
	var beginResp map[string]any
	json.Unmarshal([]byte(contentText(beginResult)), &beginResp)
	runID := beginResp["run_id"].(string)

	// 写入 60 对 request/response（> 50 阈值）
	// 事件时间必须在 run 窗口内
	src := "127.0.0.1:5000"
	dst := "127.0.0.1:8080"
	flowID := "8888"
	baseTime, _ := time.Parse(time.RFC3339Nano, beginResp["time_from"].(string))
	baseTime = baseTime.Local().Add(100 * time.Millisecond)

	var events []*event.Event
	for i := 0; i < 60; i++ {
		events = append(events,
			&event.Event{
				Identity: event.Identity{
					ID:        event.EventID("ev-req-" + string(rune(i))),
					Timestamp: baseTime.Add(time.Duration(i) * 100 * time.Millisecond),
					SessionID: sessionID, Type: "tcp",
					SchemaID: "tcp.v1", Source: "test",
				},
				Payload: event.Payload{
					SchemaID: "tcp.v1",
					Value: event.Value{
						Kind: event.Object,
						Object: map[string]event.Value{
							"type":      {Kind: event.String, Str: "request"},
							"src":       {Kind: event.String, Str: src},
							"dst":       {Kind: event.String, Str: dst},
							"flow_id":   {Kind: event.String, Str: flowID},
						"direction": {Kind: event.String, Str: "client_to_server"},
						"msg_name":  {Kind: event.String, Str: "BulkReq"},
						},
					},
				},
			},
			&event.Event{
				Identity: event.Identity{
					ID:        event.EventID("ev-resp-" + string(rune(i))),
					Timestamp: baseTime.Add(time.Duration(i)*100*time.Millisecond + 30*time.Millisecond),
					SessionID: sessionID, Type: "tcp",
					SchemaID: "tcp.v1", Source: "test",
				},
				Payload: event.Payload{
					SchemaID: "tcp.v1",
					Value: event.Value{
						Kind: event.Object,
						Object: map[string]event.Value{
							"type":      {Kind: event.String, Str: "response"},
							"src":       {Kind: event.String, Str: dst},
							"dst":       {Kind: event.String, Str: src},
							"flow_id":   {Kind: event.String, Str: flowID},
						"direction": {Kind: event.String, Str: "server_to_client"},
						"msg_name":  {Kind: event.String, Str: "BulkResp"},
						},
					},
				},
			},
		)
	}
	if err := st.AppendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	// end
	// 传入 time_to 覆盖所有事件时间（60 对 × 100ms 间隔，最大 baseTime+5930ms）
	endReq := mcp.CallToolRequest{}
	endReq.Params.Arguments = map[string]any{
		"run_id":  runID,
		"time_to": baseTime.Add(7000 * time.Millisecond).Format(time.RFC3339Nano),
	}
	m.handleEndCaptureRun(ctx, endReq)

	// trace（预期写文件）
	traceReq := mcp.CallToolRequest{}
	traceReq.Params.Arguments = map[string]any{
		"run_id":       runID,
		"flow_id":      "8888",
		"feature_name": "bulk_op",
	}
	traceResult, err := m.handleTraceProtocolFlow(ctx, traceReq)
	if err != nil {
		t.Fatalf("trace_protocol_flow: %v", err)
	}

	var resp map[string]any
	json.Unmarshal([]byte(contentText(traceResult)), &resp)

	if filePath, ok := resp["file_path"]; !ok || filePath == "" {
		t.Errorf("file_path missing for large result, got: %v", resp)
	} else {
		// 验证文件存在
		if _, err := os.Stat(filePath.(string)); err != nil {
			t.Errorf("trace file not found: %v", err)
		}
		t.Logf("large result written to %s, step_count=%v", filePath, resp["step_count"])
	}

	if stepCount, ok := resp["step_count"]; !ok || stepCount.(float64) != 60 {
		t.Errorf("step_count = %v, want 60", resp["step_count"])
	}
}
