// authz.go — 轻量鉴权装配层（2026-09-05 方案 §3）。
//
// 策略判定全部在 pkg/authz（纯函数，表驱动）；本文件只做两件事：
//  1. 组合 projectStore 完成 role 解析（Owner/Admin/Member → authz.Role）；
//  2. 提供 Resource 构造 helper，让 handler 一行完成鉴权。
//
// 不做中间件自动拦截：handler 第一行显式 `m.authz.Can(...)`，
// 遗漏由 authz_guard_test.go 的 AST 护栏兜底。
package main

import (
	"context"
	"fmt"
	"log/slog"

	"gta/pkg/auth"
	"gta/pkg/authz"
	"gta/pkg/store"
)

// projectAuthorizer 是 authz.Authorizer 的项目实现：role 来自 projectStore，
// 判定委托 authz.Decide。
type projectAuthorizer struct {
	projects *projectStore
}

// newProjectAuthorizer 构造鉴权器。
func newProjectAuthorizer(ps *projectStore) *projectAuthorizer {
	return &projectAuthorizer{projects: ps}
}

// authzPrincipal 把 pkg/auth 的请求身份转成鉴权身份。
func authzPrincipal(ctx context.Context) authz.Principal {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		return authz.Principal{User: p.Owner, Tenant: p.Tenant, IsAdmin: p.IsAdmin}
	}
	return authz.Principal{}
}

// Can 实现 authz.Authorizer：解析调用者在资源所属项目内的角色后交由 Decide。
// Kind=Project 的资源以 res.ID 为项目 id；其余以 res.ProjectID（” = 个人资源，无项目角色）。
func (a *projectAuthorizer) Can(ctx context.Context, act authz.Action, res authz.Resource) error {
	p := authzPrincipal(ctx)
	projectID := res.ProjectID
	if projectID == "" && res.Kind == authz.KindProject {
		projectID = res.ID
	}
	role := authz.RoleNone
	if projectID != "" && a.projects != nil {
		r, err := a.projects.RoleOf(ctx, projectID, p.User)
		if err != nil {
			return err
		}
		role = r
	}
	return authz.Decide(p, act, res, role)
}

// projectResource 构造项目资源引用。
func projectResource(p *project) authz.Resource {
	return authz.Resource{
		Kind:      authz.KindProject,
		ID:        p.ID,
		TenantID:  p.TenantID,
		CreatorID: p.CreatedBy,
	}
}

// sessionResource 构造会话资源引用（含归属项目与创建者）。
func sessionResource(meta *store.SessionMeta) authz.Resource {
	return authz.Resource{
		Kind:      authz.KindSession,
		ID:        meta.SessionID,
		TenantID:  meta.TenantID,
		ProjectID: meta.ProjectID,
		CreatorID: meta.Owner,
	}
}

// projectForAction 加载项目并校验指定 Action 的权限，供需要项目级授权的 handler 复用。
// 项目不存在与不可见统一按 not found 处理，避免向非成员泄露项目存在性。
func (m *mcpCapture) projectForAction(ctx context.Context, id string, act authz.Action) (*project, error) {
	if id == "" {
		return nil, fmt.Errorf("project id is required")
	}
	p, err := m.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project not found")
	}
	if err := m.authz.Can(ctx, act, projectResource(p)); err != nil {
		return nil, fmt.Errorf("project not found")
	}
	return p, nil
}

// resolveSessionMeta 按 session_id 解析会话元数据（controlStore 优先，metadata.json 兜底）。
// 只用于鉴权输入，错误统一 "not found"，不向调用方区分"不存在"与"不可见"。
func (m *mcpCapture) resolveSessionMeta(ctx context.Context, sessionID string) (*store.SessionMeta, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if m.controlStore != nil {
		if meta, err := m.controlStore.GetSession(ctx, sessionID); err == nil && meta != nil {
			return meta, nil
		}
	}
	if m.sessionMgr != nil {
		if meta, err := m.sessionMgr.readSessionMetadata(sessionID, auth.OwnerFrom(ctx)); err == nil && meta != nil {
			return &store.SessionMeta{
				SessionID: sessionID,
				Owner:     meta.Owner,
				TenantID:  meta.TenantID,
				ProjectID: meta.ProjectID,
			}, nil
		}
	}
	return nil, fmt.Errorf("session %s not found", sessionID)
}

// authorizeSession 校验调用方对指定会话的读取权限（ActionSessionRead）。
// 2026-09-05 语义修正：
//   - 项目会话（project_id != ”）由 Project 决定可见性（成员即可见）；
//   - 个人会话仅 creator / global admin；
//   - controlStore 与 metadata.json 均查不到时**拒绝**（旧实现在此兜底放行，是越权口子，已修复）。
func (m *mcpCapture) authorizeSession(ctx context.Context, sessionID string) error {
	meta, err := m.resolveSessionMeta(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := m.authz.Can(ctx, authz.ActionSessionRead, sessionResource(meta)); err != nil {
		return fmt.Errorf("session %s not found or not owned by you", sessionID)
	}
	return nil
}

// visibleSessionFilter 构造列表查询的可见性过滤器：
// admin 全可见；普通用户 = 自己 owner 的会话 OR 归属其可见项目的会话（项目协作边界）。
func (m *mcpCapture) visibleSessionFilter(ctx context.Context) (store.SessionOwnerFilter, error) {
	if p, ok := auth.PrincipalFrom(ctx); ok && p.IsAdmin {
		return store.SessionOwnerFilter{AllOwners: true}, nil
	}
	owner := auth.OwnerFrom(ctx)
	f := store.SessionOwnerFilter{Owner: owner}
	if m.projects == nil {
		return f, nil
	}
	visible, err := m.projects.ListVisible(ctx, owner, false)
	if err != nil {
		return f, fmt.Errorf("resolve visible projects: %w", err)
	}
	for i := range visible {
		f.ProjectIDs = append(f.ProjectIDs, visible[i].ID)
	}
	return f, nil
}

// projectCapabilities 计算当前调用者在项目上被放行的管理动作列表，
// 供前端替代本地 role 判断（消除权限逻辑在前端的重复实现）。
func (m *mcpCapture) projectCapabilities(ctx context.Context, p *project) []string {
	candidates := []authz.Action{
		authz.ActionProjectUpdate,
		authz.ActionProjectDelete,
		authz.ActionProjectManageMembers,
		authz.ActionProjectManagePlugins,
		authz.ActionProjectManageRules,
		authz.ActionProjectTransferOwner,
	}
	var caps []string
	for _, a := range candidates {
		if err := m.authz.Can(ctx, a, projectResource(p)); err == nil {
			caps = append(caps, string(a))
		}
	}
	return caps
}

// pluginOwnersFor 返回调用者按名解析解码插件时可用的额外 owner 候选集：
// 自己所属项目（任意角色，owner ∪ 成员）里插件条目声明的归属 owner。
// 解码插件在注册表按 owner 作用域隔离（pkg/plugin FindByNameFor），新用户天然
// 看不到别人的插件；这里把"项目成员共用项目插件"作为唯一例外白名单交给
// pipeline（StartCaptureRequest.plugin_owners / SetSessionPlugin / 租约抓包）。
// 查询失败或无命中返回 nil（= 仅会话 owner 自己的插件，行为与旧版一致）。
func (m *mcpCapture) pluginOwnersFor(ctx context.Context, user string) []string {
	if user == "" || m.projects == nil {
		return nil
	}
	visible, err := m.projects.ListVisible(ctx, user, false)
	if err != nil {
		slog.Warn("pluginOwnersFor: resolve visible projects failed", "user", user, "error", err)
		return nil
	}
	seen := map[string]bool{user: true}
	var out []string
	for i := range visible {
		for _, pp := range visible[i].Plugins {
			if pp.Owner == "" || seen[pp.Owner] {
				continue
			}
			seen[pp.Owner] = true
			out = append(out, pp.Owner)
		}
	}
	return out
}

// isKnownUser 报告用户名当前是否已有可用身份：邀请制 users 表或 env bootstrap。
// 用于项目成员的"预邀请"标注：false 表示对方还没注册同名身份，加入即 pending。
// 匿名部署（users 空 + env 空）恒返回 false，前端据此不展示注册提示。
func (m *mcpCapture) isKnownUser(ctx context.Context, user string) bool {
	if user == "" {
		return false
	}
	if m.envResolver != nil && m.envResolver.HasOwner(user) {
		return true
	}
	if m.users == nil {
		return false
	}
	exists, err := m.users.OwnerExists(ctx, user)
	if err != nil {
		slog.Warn("isKnownUser: lookup failed", "user", user, "error", err)
		return false
	}
	return exists
}
