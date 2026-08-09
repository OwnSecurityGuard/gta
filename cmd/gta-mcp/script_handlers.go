package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleListScripts 处理 list_scripts 请求
func (m *mcpCapture) handleListScripts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "global")
	sessionID := req.GetString("session_id", "")

	if scope == "session" && sessionID == "" {
		return errorResult(fmt.Errorf("session_id required for session scope")), nil
	}

	scripts, err := m.scriptMgr.ListScripts(scope, sessionID)
	if err != nil {
		return errorResult(err), nil
	}

	return successResult(map[string]any{
		"count":   len(scripts),
		"scripts": scripts,
	}), nil
}

// handleRunScript 处理 run_script 请求
func (m *mcpCapture) handleRunScript(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	scope := req.GetString("scope", "global")
	sessionID := req.GetString("session_id", "")
	argsJSON := req.GetString("args", "{}")

	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}

	if scope == "session" && sessionID == "" {
		return errorResult(fmt.Errorf("session_id required for session scope")), nil
	}

	// 解析参数
	var args map[string]string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errorResult(fmt.Errorf("invalid args JSON: %w", err)), nil
	}

	// 获取脚本路径
	scriptPath, err := m.scriptMgr.GetScriptPath(name, scope, sessionID)
	if err != nil {
		return errorResult(err), nil
	}

	// 设置环境变量：会话 ID 由调用方传入，其他 GTA_* 变量已由 Executor 统一注入。
	env := map[string]string{
		"GTA_SESSION_ID": sessionID,
	}

	// 执行脚本
	result, err := m.executor.Execute(ctx, scriptPath, args, env)
	if err != nil {
		return errorResult(fmt.Errorf("execute script: %w", err)), nil
	}

	// 构建返回结果
	return successResult(map[string]any{
		"success":     result.ExitCode == 0,
		"exit_code":   result.ExitCode,
		"stdout":      result.Stdout,
		"stderr":      result.Stderr,
		"duration_ms": result.Duration.Milliseconds(),
		"start_time":  result.StartTime.Format(time.RFC3339),
		"end_time":    result.EndTime.Format(time.RFC3339),
	}), nil
}

// handleSaveScript 处理 save_script 请求
func (m *mcpCapture) handleSaveScript(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	code := req.GetString("code", "")
	scope := req.GetString("scope", "global")
	sessionID := req.GetString("session_id", "")

	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	if code == "" {
		return errorResult(fmt.Errorf("code is required")), nil
	}

	if scope == "session" && sessionID == "" {
		return errorResult(fmt.Errorf("session_id required for session scope")), nil
	}

	// 保存脚本
	info, err := m.scriptMgr.SaveScript(name, code, scope, sessionID)
	if err != nil {
		return errorResult(err), nil
	}

	return successResult(map[string]any{
		"script": info,
	}), nil
}

// handleDeleteScript 处理 delete_script 请求
func (m *mcpCapture) handleDeleteScript(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	scope := req.GetString("scope", "global")
	sessionID := req.GetString("session_id", "")

	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}

	if scope == "session" && sessionID == "" {
		return errorResult(fmt.Errorf("session_id required for session scope")), nil
	}

	// 删除脚本
	err := m.scriptMgr.DeleteScript(name, scope, sessionID)
	if err != nil {
		return errorResult(err), nil
	}

	return successResult(map[string]any{
		"deleted": name,
		"scope":   scope,
	}), nil
}
