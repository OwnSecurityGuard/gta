package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"

	"gta/pkg/auth"
	pb "gta/pkg/internalipc/proto"
	"gta/pkg/store"
)

// ---- fake 扩展：ListCaptureSessions ----

// ListCaptureSessions 返回预设的 live 会话摘要。proto 请求没有 owner 字段，
// pipeline 会返回所有会话——这正是 handleListAllSessions live-only 兜底循环
// 必须自行做 owner 可见性校验的原因。
func (f *fakeCaptureClient) ListCaptureSessions(_ context.Context, _ *pb.ListCaptureSessionsRequest, _ ...grpc.CallOption) (*pb.ListCaptureSessionsResponse, error) {
	return f.liveSessions, nil
}

// TestListAllSessionsLiveOnlyOwnerFilter 验证 live-only 兜底循环的 owner 可见性：
// 会话仅登记在 controlStore（sessionMgr 本地无目录/metadata，模拟 workDir 漂移），
// 只能经 pipeline live 列表兜底浮出。pipeline 无 owner 概念，若兜底循环不加
// authorizeSession 校验，非 admin 调用方将看到他人运行中会话的
// session_id/port/plugin/interface/db_path（与 authorizeSession 防泄露规则冲突）。
func TestListAllSessionsLiveOnlyOwnerFilter(t *testing.T) {
	workDir := t.TempDir()
	cs, err := store.NewControlStore(filepath.Join(workDir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// bob 与 alice 各登记一个会话：只写 controlStore，不在 workDir/sessions
	// 建目录，因此 sessionMgr.listSessions 枚举不到 → 只能走 live-only 兜底。
	for _, meta := range []store.SessionMeta{
		{Owner: "bob", SessionID: "bob-live-1", Status: "running", DBPath: filepath.Join(workDir, "sessions", "bob-live-1", "capture.sqlite")},
		{Owner: "alice", SessionID: "alice-live-1", Status: "running", DBPath: filepath.Join(workDir, "sessions", "alice-live-1", "capture.sqlite")},
	} {
		if err := cs.CreateSession(ctx, meta); err != nil {
			t.Fatal(err)
		}
	}

	fc := &fakeCaptureClient{liveSessions: &pb.ListCaptureSessionsResponse{Sessions: []*pb.CaptureSessionSummary{
		{SessionId: "bob-live-1", State: "running", Port: 9001, Plugin: "http", Interface: "eth0"},
		{SessionId: "alice-live-1", State: "running", Port: 9002, Plugin: "dns", Interface: "eth1"},
	}}}
	m := &mcpCapture{sessionMgr: newSessionManager(workDir), controlStore: cs, pipelineClient: fc}

	// alice（非 admin）视角
	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	res, err := m.handleListAllSessions(alice, mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text

	var parsed struct {
		Ok       bool `json:"ok"`
		Sessions []struct {
			SessionID string `json:"session_id"`
			Status    string `json:"status"`
			Port      int    `json:"port"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("解析输出失败: %v\ntext=%s", err, text)
	}

	// bob 的 live-only 会话不得出现（跨 owner 泄露）
	for _, s := range parsed.Sessions {
		if s.SessionID == "bob-live-1" {
			t.Fatalf("live-only 兜底泄露了 bob 的会话: %s", text)
		}
	}
	// 对照组：alice 自己的 live-only 会话（workDir 漂移场景）必须可见且带 live 状态
	foundAlice := false
	for _, s := range parsed.Sessions {
		if s.SessionID == "alice-live-1" {
			foundAlice = true
			if s.Status != "running" {
				t.Errorf("alice 的 live-only 会话 status 应为 running，实际 %q", s.Status)
			}
			if s.Port != 9002 {
				t.Errorf("alice 的 live-only 会话 port 应取 live 值 9002，实际 %d", s.Port)
			}
		}
	}
	if !foundAlice {
		t.Fatalf("alice 自己的 live-only 会话被误杀: %s", text)
	}
}

// TestListAllSessionsLiveOnlyAdminSeesAll 验证 admin 经 live-only 兜底仍全可见。
func TestListAllSessionsLiveOnlyAdminSeesAll(t *testing.T) {
	workDir := t.TempDir()
	cs, err := store.NewControlStore(filepath.Join(workDir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "bob", SessionID: "bob-live-2", Status: "running",
		DBPath: filepath.Join(workDir, "sessions", "bob-live-2", "capture.sqlite"),
	}); err != nil {
		t.Fatal(err)
	}

	fc := &fakeCaptureClient{liveSessions: &pb.ListCaptureSessionsResponse{Sessions: []*pb.CaptureSessionSummary{
		{SessionId: "bob-live-2", State: "running", Port: 9003, Plugin: "http", Interface: "eth0"},
	}}}
	m := &mcpCapture{sessionMgr: newSessionManager(workDir), controlStore: cs, pipelineClient: fc}

	admin := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "root", IsAdmin: true})
	res, err := m.handleListAllSessions(admin, mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !containsSessionID(text, "bob-live-2") {
		t.Fatalf("admin 应经 live-only 兜底看到 bob 的会话: %s", text)
	}
}

// TestListAllSessionsLiveOnlyAnonymousUnchanged 匿名模式回归底线（双向锁定）：
// 无 Principal 的 ctx（ownerFilterFromCtx 返回 SessionOwnerFilter 零值，
// Matches 即 meta.Owner == 空串）下，controlStore 中 owner 为空串的会话经
// live-only 兜底仍出现在输出——与修复前行为一致；而具名 owner（bob）的
// 会话不得泄露给匿名调用方（收紧后的新行为，与 authorizeSession 防泄露
// 规则在其他访问路径上的既有语义一致）。
func TestListAllSessionsLiveOnlyAnonymousUnchanged(t *testing.T) {
	workDir := t.TempDir()
	cs, err := store.NewControlStore(filepath.Join(workDir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "", SessionID: "anon-live-1", Status: "running",
		DBPath: filepath.Join(workDir, "sessions", "anon-live-1", "capture.sqlite"),
	}); err != nil {
		t.Fatal(err)
	}
	// bob 的会话：零值 filter（只看 owner 为空串）不得看到。
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "bob", SessionID: "bob-live-anon", Status: "running",
		DBPath: filepath.Join(workDir, "sessions", "bob-live-anon", "capture.sqlite"),
	}); err != nil {
		t.Fatal(err)
	}

	fc := &fakeCaptureClient{liveSessions: &pb.ListCaptureSessionsResponse{Sessions: []*pb.CaptureSessionSummary{
		{SessionId: "anon-live-1", State: "running", Port: 9004, Plugin: "http", Interface: "eth0"},
		{SessionId: "bob-live-anon", State: "running", Port: 9005, Plugin: "dns", Interface: "eth1"},
	}}}
	m := &mcpCapture{sessionMgr: newSessionManager(workDir), controlStore: cs, pipelineClient: fc}

	res, err := m.handleListAllSessions(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !containsSessionID(text, "anon-live-1") {
		t.Fatalf("匿名调用方经 live-only 兜底仍应看到 owner 为空串的会话: %s", text)
	}
	if containsSessionID(text, "bob-live-anon") {
		t.Fatalf("匿名调用方不得经 live-only 兜底看到 bob 的会话: %s", text)
	}
}

// containsSessionID 判断 list_all_sessions 的 JSON 输出是否包含指定 session_id。
func containsSessionID(text, sessionID string) bool {
	var parsed struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return false
	}
	for _, s := range parsed.Sessions {
		if s.SessionID == sessionID {
			return true
		}
	}
	return false
}
