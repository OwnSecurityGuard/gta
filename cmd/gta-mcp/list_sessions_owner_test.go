package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
)

// TestListAllSessionsEchoesOwner 验证会话列表输出携带 owner：
// 前端 owner 徽标与 admin「只看我的」筛选都依赖这个字段。
func TestListAllSessionsEchoesOwner(t *testing.T) {
	workDir := t.TempDir()
	sm := newSessionManager(workDir)
	meta := sessionMetadata{
		Owner:     "alice",
		SessionID: "20260829_120000.000",
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		Port:      8080,
		DBPath:    filepath.Join(workDir, "sessions", "20260829_120000.000", "capture.sqlite"),
	}
	if _, err := sm.createSession(meta); err != nil {
		t.Fatal(err)
	}
	m := &mcpCapture{workDir: workDir, sessionMgr: sm}

	// alice（非 admin）视角：只能看到自己的会话，且输出带 owner。
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	res, err := m.handleListAllSessions(ctx, mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text

	var parsed struct {
		Ok       bool `json:"ok"`
		Sessions []struct {
			SessionID string `json:"session_id"`
			Owner     string `json:"owner"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("解析输出失败: %v\ntext=%s", err, text)
	}
	if !parsed.Ok || len(parsed.Sessions) != 1 {
		t.Fatalf("应恰好返回 1 个会话: ok=%v n=%d text=%s", parsed.Ok, len(parsed.Sessions), text)
	}
	if parsed.Sessions[0].Owner != "alice" {
		t.Fatalf("owner 应为 alice，实际 %q", parsed.Sessions[0].Owner)
	}
}
