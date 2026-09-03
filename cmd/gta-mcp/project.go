// project.go — 「项目」一等组织单元（Web First · P4）。
//
// 项目持久化在 control.sqlite 的 projects 表（取代旧的分片 JSON 文件
// projectStore），并具备轻量的项目级成员 / 插件 / 规则关联，以及会话到项目
// 的绑定。刻意不做 Workspace / Organization / 完整 RBAC 等推广结构。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
	"gta/pkg/store"
)

func newProjectID() string {
	// 毫秒时间戳 + 4 位随机数，避免同毫秒内碰撞。
	return fmt.Sprintf("%s_%04d", time.Now().Format("20060102_150405.000"), randInt(10000))
}

// ownerScope 解析当前请求的 owner；返回是否 admin（admin 可见全部 + 可管理任意项目）。
func (m *mcpCapture) ownerScope(ctx context.Context) (owner string, all bool) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		return p.Owner, p.IsAdmin
	}
	return auth.OwnerFrom(ctx), false
}

// sessionFilterForOwner 构造会话查询的 owner 可见性过滤器。
func (m *mcpCapture) sessionFilterForOwner(ctx context.Context) store.SessionOwnerFilter {
	owner, all := m.ownerScope(ctx)
	return store.SessionOwnerFilter{AllOwners: all, Owner: owner}
}

// handleProjectArgPort 读取可选的 port 参数（0=未设置）。
func handleProjectArgPort(req mcp.CallToolRequest) int {
	if args := req.GetArguments(); args != nil {
		if v, ok := args["port"]; ok && v != nil {
			var p int
			if err := json.Unmarshal([]byte(fmt.Sprintf("%v", v)), &p); err == nil {
				return p
			}
		}
	}
	return 0
}

// projectForEdit 加载目标项目并做可管理性校验，供所有需要「项目 admin」权限的 handler 复用。
func (m *mcpCapture) projectForEdit(ctx context.Context, id string) (*project, error) {
	owner, all := m.ownerScope(ctx)
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project not found")
	}
	if !m.projects.CanManage(p, owner, all) {
		return nil, fmt.Errorf("forbidden: only project admin can manage project %s", id)
	}
	return p, nil
}

// handleCreateProject 新建项目。
func (m *mcpCapture) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, _ := m.ownerScope(ctx)
	name := strings.TrimSpace(req.GetString("name", ""))
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	plugin := strings.TrimSpace(req.GetString("plugin", ""))
	port := handleProjectArgPort(req)

	now := time.Now().Format(time.RFC3339)
	p := project{
		ID:            newProjectID(),
		Name:          name,
		CreatedBy:     owner,
		DefaultPlugin: plugin,
		DefaultPort:   port,
		Members:       []projectMember{},
		Plugins:       []projectPlugin{},
		Rules:         []projectRule{},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := m.projects.Create(ctx, &p); err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	slog.Info("project created", "owner", owner, "project_id", p.ID, "name", name)
	return successResult(p), nil
}

// handleListProjects 列出当前可见项目（admin 可见全部；普通用户只见自己 / 所属成员的项目）。
func (m *mcpCapture) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, all := m.ownerScope(ctx)
	out, err := m.projects.ListVisible(ctx, owner, all)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if out == nil {
		out = []project{}
	}
	return successResult(map[string]any{"projects": out}), nil
}

// handleGetProject 查看单个项目（含其最近会话）。不可见时按未找到处理。
func (m *mcpCapture) handleGetProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, _ := m.ownerScope(ctx)
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if p == nil || !visibleTo(p, owner) {
		return errorResult(fmt.Errorf("project not found")), nil
	}
	sessions, err := m.controlStore.ListSessionsForProject(ctx, id, m.sessionFilterForOwner(ctx))
	if err != nil {
		return nil, fmt.Errorf("list project sessions: %w", err)
	}
	if sessions == nil {
		sessions = []store.SessionMeta{}
	}
	return successResult(map[string]any{
		"project":         *p,
		"recent_sessions": sessions,
	}), nil
}

// handleUpdateProject 更新项目（仅更新显式提供的字段）。
func (m *mcpCapture) handleUpdateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	existing, err := m.projectForEdit(ctx, id)
	if err != nil {
		return errorResult(err), nil
	}
	if n := strings.TrimSpace(req.GetString("name", "")); n != "" {
		existing.Name = n
	}
	args := req.GetArguments()
	if args != nil {
		if _, ok := args["description"]; ok {
			existing.Description = strings.TrimSpace(req.GetString("description", ""))
		}
		if _, ok := args["game"]; ok {
			existing.Game = strings.TrimSpace(req.GetString("game", ""))
		}
		if _, ok := args["default_plugin"]; ok {
			existing.DefaultPlugin = strings.TrimSpace(req.GetString("default_plugin", ""))
		}
		if _, ok := args["default_port"]; ok {
			existing.DefaultPort = handleProjectArgPort(req)
		}
	}
	existing.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := m.projects.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	slog.Info("project updated", "project_id", id)
	return successResult(existing), nil
}

// handleDeleteProject 删除项目。
func (m *mcpCapture) handleDeleteProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	owner, _ := m.ownerScope(ctx)
	_, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// 未找到也走 projectForEdit 的路径统一报错
	if _, err := m.projectForEdit(ctx, id); err != nil {
		return errorResult(err), nil
	}
	ok, err := m.projects.Delete(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("delete project: %w", err)
	}
	if !ok {
		return errorResult(fmt.Errorf("project not found")), nil
	}
	slog.Info("project deleted", "owner", owner, "project_id", id)
	return successResult(map[string]any{"id": id}), nil
}

// handleAddProjectMember 添加 / 覆盖项目成员。
func (m *mcpCapture) handleAddProjectMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	user := strings.TrimSpace(req.GetString("user", ""))
	if user == "" {
		return errorResult(fmt.Errorf("user is required")), nil
	}
	roleStr := strings.TrimSpace(req.GetString("role", ""))
	role := projectRole(roleStr)
	if role != roleAdmin && role != roleMember {
		return errorResult(fmt.Errorf("role must be admin or member")), nil
	}
	if _, err := m.projectForEdit(ctx, id); err != nil {
		return errorResult(err), nil
	}
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	replaced := false
	for i := range p.Members {
		if p.Members[i].User == user {
			p.Members[i].Role = role
			replaced = true
			break
		}
	}
	if !replaced {
		p.Members = append(p.Members, projectMember{User: user, Role: role})
	}
	p.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := m.projects.Update(ctx, p); err != nil {
		return nil, fmt.Errorf("add project member: %w", err)
	}
	slog.Info("project member added", "project_id", id, "user", user, "role", role)
	return successResult(p), nil
}

// handleRemoveProjectMember 移除项目成员（禁止移除创建者）。
func (m *mcpCapture) handleRemoveProjectMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	user := strings.TrimSpace(req.GetString("user", ""))
	if user == "" {
		return errorResult(fmt.Errorf("user is required")), nil
	}
	if _, err := m.projectForEdit(ctx, id); err != nil {
		return errorResult(err), nil
	}
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.CreatedBy == user {
		return errorResult(fmt.Errorf("cannot remove project creator")), nil
	}
	kept := p.Members[:0]
	found := false
	for _, mbr := range p.Members {
		if mbr.User == user {
			found = true
			continue
		}
		kept = append(kept, mbr)
	}
	if !found {
		return errorResult(fmt.Errorf("member %s not found in project %s", user, id)), nil
	}
	p.Members = kept
	p.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := m.projects.Update(ctx, p); err != nil {
		return nil, fmt.Errorf("remove project member: %w", err)
	}
	slog.Info("project member removed", "project_id", id, "user", user)
	return successResult(p), nil
}

// parseAssociatedEntries 解析 JSON 字符串数组参数（[{id,name},...]）。
func parseAssociatedEntries[T any](param string) ([]T, error) {
	var out []T
	if param == "" {
		return nil, fmt.Errorf("list is required")
	}
	if err := json.Unmarshal([]byte(param), &out); err != nil {
		return nil, fmt.Errorf("invalid JSON list: %w", err)
	}
	return out, nil
}

// handleSetProjectPlugins 整体替换项目的插件关联列表。
func (m *mcpCapture) handleSetProjectPlugins(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	plugins, err := parseAssociatedEntries[projectPlugin](strings.TrimSpace(req.GetString("plugins", "")))
	if err != nil {
		return errorResult(err), nil
	}
	if err := m.requireCanManageAndReturnProjectForEdit(ctx, id); err != nil {
		return errorResult(err), nil
	}
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Plugins = plugins
	if p.Plugins == nil {
		p.Plugins = []projectPlugin{}
	}
	p.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := m.projects.Update(ctx, p); err != nil {
		return nil, fmt.Errorf("set project plugins: %w", err)
	}
	slog.Info("project plugins set", "project_id", id, "n", len(p.Plugins))
	return successResult(p), nil
}

// handleSetProjectRules 整体替换项目的规则关联列表。
func (m *mcpCapture) handleSetProjectRules(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	rules, err := parseAssociatedEntries[projectRule](strings.TrimSpace(req.GetString("rules", "")))
	if err != nil {
		return errorResult(err), nil
	}
	if err := m.requireCanManageAndReturnProjectForEdit(ctx, id); err != nil {
		return errorResult(err), nil
	}
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Rules = rules
	if p.Rules == nil {
		p.Rules = []projectRule{}
	}
	p.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := m.projects.Update(ctx, p); err != nil {
		return nil, fmt.Errorf("set project rules: %w", err)
	}
	slog.Info("project rules set", "project_id", id, "n", len(p.Rules))
	return successResult(p), nil
}

// requireCanManageAndReturnProjectForEdit 仅做可管理性校验，返回错误给调用方转 errorResult。
func (m *mcpCapture) requireCanManageAndReturnProjectForEdit(ctx context.Context, id string) error {
	_, err := m.projectForEdit(ctx, id)
	return err
}

// handleSetSessionProject 绑定会话到项目（或清空绑定）。
func (m *mcpCapture) handleSetSessionProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := strings.TrimSpace(req.GetString("session_id", ""))
	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	projectID := strings.TrimSpace(req.GetString("project_id", ""))
	owner, all := m.ownerScope(ctx)

	meta, err := m.controlStore.GetSessionFor(ctx, sessionID, m.sessionFilterForOwner(ctx))
	if err != nil {
		return errorResult(fmt.Errorf("session not found")), nil
	}
	// 非会话 owner 且非 admin 不得操作该会话（GetSessionFor 已过滤，此处加显式防御）。
	if owner != meta.Owner && !all {
		return errorResult(fmt.Errorf("forbidden: cannot bind session %s", sessionID)), nil
	}
	if projectID != "" {
		canManageProject := false
		if p, err := m.projects.Get(ctx, projectID); err == nil && p != nil {
			canManageProject = m.projects.CanManage(p, owner, all)
		}
		if !canManageProject && owner != meta.Owner {
			return errorResult(fmt.Errorf("forbidden: cannot bind session %s to project %s", sessionID, projectID)), nil
		}
	}
	if err := m.controlStore.SetSessionProject(ctx, sessionID, projectID); err != nil {
		return nil, fmt.Errorf("set session project: %w", err)
	}
	// 同步到 filesystem metadata.json，供 list_all_sessions 暴露 project_id（在线/离线派生）。
	// 测试等场景 sessionMgr 可能为 nil，避免调用 nil 指针 panic。
	if m.sessionMgr == nil {
		slog.Info("session project set (no sessionMgr)", "session_id", sessionID, "project_id", projectID)
		return successResult(map[string]any{"session_id": sessionID, "project_id": projectID}), nil
	}
	if meta, err := m.sessionMgr.readSessionMetadata(sessionID, owner); err == nil && meta != nil {
		meta.ProjectID = projectID
		if err := m.sessionMgr.writeSessionMetadata(sessionID, *meta); err != nil {
			slog.Warn("set_session_project: persist metadata.json failed", "session_id", sessionID, "error", err)
		}
		m.sessionMgr.writeCurrent(*meta)
	}
	slog.Info("session project set", "session_id", sessionID, "project_id", projectID)
	return successResult(map[string]any{"session_id": sessionID, "project_id": projectID}), nil
}
