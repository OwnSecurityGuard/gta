// project.go — 「项目」一等组织单元（Web First · P4 → 2026-09-05 权限模型重构）。
//
// 项目持久化在 control.sqlite 的 projects / project_members 表，成员角色钉死为
// Owner（projects.owner，SSOT）> Admin > Member；created_by 为审计字段。
// 所有权限判定统一走 pkg/authz（Action 驱动），handler 不再各自判断。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/authz"
	"gta/pkg/store"
)

func newProjectID() string {
	// 毫秒时间戳 + 4 位随机数，避免同毫秒内碰撞。
	return fmt.Sprintf("%s_%04d", time.Now().Format("20060102_150405.000"), randInt(10000))
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

// handleCreateProject 新建项目。Owner = 创建者（首任 Owner），租户归一 default。
func (m *mcpCapture) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner := authzPrincipal(ctx).User
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
	slog.Info("project created", "owner", owner, "project_id", p.ID, "name", name, "tenant", p.TenantID)
	return successResult(p), nil
}

// handleListProjects 列出当前可见项目（admin 可见全部；普通用户只见自己 / 所属成员的项目）。
func (m *mcpCapture) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner := authzPrincipal(ctx).User
	all := authzPrincipal(ctx).IsAdmin
	out, err := m.projects.ListVisible(ctx, owner, all)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if out == nil {
		out = []project{}
	}
	return successResult(map[string]any{"projects": out}), nil
}

// handleGetProject 查看单个项目（含其最近会话与调用者的 capabilities）。
// 项目是协作边界：成员可见项目内全部会话（不做 owner 二次过滤）。
// members 附带 registered 标注（该用户名是否已有身份：users 表或 env bootstrap），
// 项目 admin 据此能看到哪些成员还停在"预邀请"状态（对方注册同名后自动生效）。
func (m *mcpCapture) handleGetProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectRead)
	if err != nil {
		return errorResult(err), nil
	}
	sessions, err := m.controlStore.ListSessionsForProject(ctx, id, store.SessionOwnerFilter{})
	if err != nil {
		return nil, fmt.Errorf("list project sessions: %w", err)
	}
	if sessions == nil {
		sessions = []store.SessionMeta{}
	}
	// 填充 members 的 registered 标注（浅拷贝，不落库）。
	out := *p
	out.Members = make([]projectMember, len(p.Members))
	copy(out.Members, p.Members)
	for i := range out.Members {
		out.Members[i].Registered = m.isKnownUser(ctx, out.Members[i].User)
	}
	return successResult(map[string]any{
		"project":         out,
		"capabilities":    m.projectCapabilities(ctx, p),
		"recent_sessions": sessions,
	}), nil
}

// handleUpdateProject 更新项目（仅更新显式提供的字段；ActionProjectUpdate 仅 Owner）。
func (m *mcpCapture) handleUpdateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	existing, err := m.projectForAction(ctx, id, authz.ActionProjectUpdate)
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

// handleDeleteProject 删除项目（ActionProjectDelete 仅 Owner / global admin）。
func (m *mcpCapture) handleDeleteProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectDelete)
	if err != nil {
		return errorResult(err), nil
	}
	if _, err := m.projects.Delete(ctx, p.ID); err != nil {
		return nil, fmt.Errorf("delete project: %w", err)
	}
	slog.Info("project deleted", "owner", authzPrincipal(ctx).User, "project_id", id)
	return successResult(map[string]any{"id": id}), nil
}

// handleAddProjectMember 添加 / 覆盖项目成员（ActionProjectManageMembers：Owner/Admin）。
// 身份预邀请语义：允许添加尚未注册的用户名（pending=true 返回），对方注册同名
// 身份后自动生效——owner 不必等对方先注册再操作。
func (m *mcpCapture) handleAddProjectMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	user := strings.TrimSpace(req.GetString("user", ""))
	if user == "" {
		return errorResult(fmt.Errorf("user is required")), nil
	}
	if !validOwnerName(user) {
		return errorResult(fmt.Errorf("invalid user name %q: letters/digits/._- , starts with letter or digit, max 64 chars", user)), nil
	}
	roleStr := strings.TrimSpace(req.GetString("role", ""))
	role := projectRole(roleStr)
	if role != roleAdmin && role != roleMember {
		return errorResult(fmt.Errorf("role must be admin or member")), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectManageMembers)
	if err != nil {
		return errorResult(err), nil
	}
	if p.Owner == user {
		return errorResult(fmt.Errorf("user %s is the project owner; transfer ownership instead", user)), nil
	}
	if err := m.projects.AddMember(ctx, id, projectMember{User: user, Role: role}); err != nil {
		return nil, fmt.Errorf("add project member: %w", err)
	}
	updated, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	pending := !m.isKnownUser(ctx, user)
	slog.Info("project member added", "project_id", id, "user", user, "role", role, "pending", pending)
	return successResult(map[string]any{
		"project": updated,
		"pending": pending,
	}), nil
}

// handleRemoveProjectMember 移除项目成员（Owner 不在成员表，天然不可移除；
// 显式拦截以给出可操作的错误信息）。
func (m *mcpCapture) handleRemoveProjectMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	user := strings.TrimSpace(req.GetString("user", ""))
	if user == "" {
		return errorResult(fmt.Errorf("user is required")), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectManageMembers)
	if err != nil {
		return errorResult(err), nil
	}
	if p.Owner == user {
		return errorResult(fmt.Errorf("cannot remove project owner; transfer ownership first")), nil
	}
	found, err := m.projects.RemoveMember(ctx, id, user)
	if err != nil {
		return nil, fmt.Errorf("remove project member: %w", err)
	}
	if !found {
		return errorResult(fmt.Errorf("member %s not found in project %s", user, id)), nil
	}
	updated, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	slog.Info("project member removed", "project_id", id, "user", user)
	return successResult(updated), nil
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

// handleSetProjectPlugins 整体替换项目的插件关联列表（ActionProjectManagePlugins）。
func (m *mcpCapture) handleSetProjectPlugins(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	plugins, err := parseAssociatedEntries[projectPlugin](strings.TrimSpace(req.GetString("plugins", "")))
	if err != nil {
		return errorResult(err), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectManagePlugins)
	if err != nil {
		return errorResult(err), nil
	}
	// 插件条目记录设置者身份（owner）：解码插件在注册表按 owner 作用域隔离，
	// 项目成员解析项目插件时以此为跨 owner 候选。条目自带 owner 则尊重（幂等回显）。
	setter := authzPrincipal(ctx).User
	for i := range plugins {
		if plugins[i].Owner == "" {
			plugins[i].Owner = setter
		}
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

// handleSetProjectRules 整体替换项目的规则关联列表（ActionProjectManageRules）。
func (m *mcpCapture) handleSetProjectRules(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	rules, err := parseAssociatedEntries[projectRule](strings.TrimSpace(req.GetString("rules", "")))
	if err != nil {
		return errorResult(err), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectManageRules)
	if err != nil {
		return errorResult(err), nil
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

// handleTransferProjectOwner 转移项目 Owner（独立的敏感安全操作，不借道成员/更新接口）。
// 约束：caller 必须有 ActionProjectTransferOwner（Owner / global admin）；
// new_owner 必须已是本项目成员（admin/member）；CAS 更新防并发双转；
// 转移后旧 Owner 降为 admin 成员、新 Owner 移出成员表（成员表不含 Owner）。
func (m *mcpCapture) handleTransferProjectOwner(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(req.GetString("project_id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("project_id is required")), nil
	}
	newOwner := strings.TrimSpace(req.GetString("new_owner", ""))
	if newOwner == "" {
		return errorResult(fmt.Errorf("new_owner is required")), nil
	}
	p, err := m.projectForAction(ctx, id, authz.ActionProjectTransferOwner)
	if err != nil {
		return errorResult(err), nil
	}
	if p.Owner == newOwner {
		return errorResult(fmt.Errorf("user %s is already the project owner", newOwner)), nil
	}
	role, err := m.projects.RoleOf(ctx, id, newOwner)
	if err != nil {
		return nil, err
	}
	if role != authz.RoleAdmin && role != authz.RoleMember {
		return errorResult(fmt.Errorf("new_owner %s must be an existing member (admin/member) of project %s", newOwner, id)), nil
	}
	prevOwner := p.Owner
	ok, err := m.projects.TransferOwner(ctx, id, prevOwner, newOwner)
	if err != nil {
		return nil, fmt.Errorf("transfer project owner: %w", err)
	}
	if !ok {
		return errorResult(fmt.Errorf("project owner changed concurrently; retry")), nil
	}
	// 成员表调整非原子，失败仅告警：owner 字段是 SSOT，成员行可由下一次 Update 修复。
	if err := m.projects.AddMember(ctx, id, projectMember{User: prevOwner, Role: roleAdmin}); err != nil {
		slog.Warn("transfer_owner: demote previous owner failed", "project_id", id, "user", prevOwner, "error", err)
	}
	if _, err := m.projects.RemoveMember(ctx, id, newOwner); err != nil {
		slog.Warn("transfer_owner: remove new owner from members failed", "project_id", id, "user", newOwner, "error", err)
	}
	updated, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	slog.Info("project owner transferred", "project_id", id, "from", prevOwner, "to", newOwner,
		"actor", authzPrincipal(ctx).User)
	return successResult(updated), nil
}

// moveSessionToProject 把会话绑定到项目（或清空绑定）的六步收口（方案 §5.3）：
//  1. session 存在；2. caller 有 ActionSessionMoveProject；3. target project 存在；
//  4. caller 对 target project 有 ActionProjectRead（成员即可）；5. 租户一致；
//  6. 原子更新（带租户 CAS）。
func (m *mcpCapture) moveSessionToProject(ctx context.Context, sessionID, projectID string) error {
	meta, err := m.resolveSessionMeta(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := m.authz.Can(ctx, authz.ActionSessionMoveProject, sessionResource(meta)); err != nil {
		return fmt.Errorf("forbidden: cannot move session %s", sessionID)
	}
	if projectID != "" {
		p, err := m.projects.Get(ctx, projectID)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("project %s not found", projectID)
		}
		if err := m.authz.Can(ctx, authz.ActionProjectRead, projectResource(p)); err != nil {
			return fmt.Errorf("forbidden: caller is not a member of project %s", projectID)
		}
		if p.Tenant() != tenantOrDefault(meta.TenantID) {
			return fmt.Errorf("tenant mismatch: session %q vs project %q", tenantOrDefault(meta.TenantID), p.Tenant())
		}
	}
	if err := m.controlStore.MoveSessionToProject(ctx, sessionID, projectID, tenantOrDefault(meta.TenantID)); err != nil {
		return fmt.Errorf("move session to project: %w", err)
	}
	// 同步 filesystem metadata.json，供 list_all_sessions 暴露 project_id（在线/离线派生）。
	// 测试等场景 sessionMgr 可能为 nil，避免 nil 指针 panic。
	if m.sessionMgr != nil {
		if meta, err := m.sessionMgr.readSessionMetadata(sessionID, authzPrincipal(ctx).User); err == nil && meta != nil {
			meta.ProjectID = projectID
			if err := m.sessionMgr.writeSessionMetadata(sessionID, *meta); err != nil {
				slog.Warn("move_session_to_project: persist metadata.json failed", "session_id", sessionID, "error", err)
			}
			m.sessionMgr.writeCurrent(*meta)
		}
	}
	slog.Info("session moved to project", "session_id", sessionID, "project_id", projectID)
	return nil
}

// handleMoveSessionToProject MCP 工具入口：move_session_to_project。
func (m *mcpCapture) handleMoveSessionToProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := strings.TrimSpace(req.GetString("session_id", ""))
	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	projectID := strings.TrimSpace(req.GetString("project_id", ""))
	if err := m.moveSessionToProject(ctx, sessionID, projectID); err != nil {
		return errorResult(err), nil
	}
	return successResult(map[string]any{"session_id": sessionID, "project_id": projectID}), nil
}

// handleSetSessionProject 是 set_session_project 的 deprecated 别名，转调新收口。
func (m *mcpCapture) handleSetSessionProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return m.handleMoveSessionToProject(ctx, req)
}
