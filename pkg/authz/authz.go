// Package authz 是 GTA 的轻量鉴权策略层。
//
// 设计约束（2026-09-05 方案，docs/superpowers/plans/2026-09-05-tenant-project-authz.md）：
//   - 只放纯策略（Decide），不做 IO、不 import 任何存储包 —— 规则 100% 表驱动可单测；
//   - Resource 用引用式描述（kind/id/...），不传实体，list 类场景才能"先过滤后鉴权"；
//   - Action 多、Role 少：细粒度体现在 Action 上，角色恒定为 Owner > Admin > Member 三档；
//   - 不上 Casbin / OPA / 复杂 RBAC，也不做中间件自动拦截 —— 收口的是规则，不是调用点。
package authz

import (
	"context"
	"fmt"
)

// Action 是一次受控操作的标识。命名约定 "<kind>:<verb>"。
type Action string

const (
	ActionProjectRead          Action = "project:read"
	ActionProjectUpdate        Action = "project:update"
	ActionProjectDelete        Action = "project:delete"
	ActionProjectManageMembers Action = "project:manage_members"
	ActionProjectManagePlugins Action = "project:manage_plugins"
	ActionProjectManageRules   Action = "project:manage_rules"
	ActionProjectTransferOwner Action = "project:transfer_owner"
	ActionSessionRead          Action = "session:read"
	ActionSessionUse           Action = "session:use"
	ActionSessionDelete        Action = "session:delete"
	ActionSessionMoveProject   Action = "session:move_project"
	ActionPluginRead           Action = "plugin:read"
	ActionPluginUse            Action = "plugin:use"
	ActionPluginManage         Action = "plugin:manage"
	ActionLeaseUse             Action = "lease:use"
	ActionLeaseRelease         Action = "lease:release"
	ActionAccessCodeCreate     Action = "access_code:create"
	ActionAccessCodeClaim      Action = "access_code:claim"
	ActionProbeRead            Action = "probe:read"
	ActionProbeUse             Action = "probe:use"
	ActionProbeControl         Action = "probe:control"
	ActionProbeManage          Action = "probe:manage"
	ActionUserManage           Action = "user:manage"
)

// Kind 是资源的顶层类别。
type Kind string

const (
	KindProject    Kind = "project"
	KindSession    Kind = "session"
	KindPlugin     Kind = "plugin"
	KindLease      Kind = "lease"
	KindAccessCode Kind = "access_code"
	KindProbe      Kind = "probe"
	KindUser       Kind = "user"
)

// DefaultTenant 是所有未显式标注租户的资源的归属租户。
// 当前部署没有组织实体，全部资源与用户都落在这里。
const DefaultTenant = "default"

// Resource 是资源的引用式描述，由调用方构造。
// 各字段按 Kind 取舍：project 填 ID/TenantID；session 额外填 ProjectID/CreatorID；
// plugin/lease/access_code 视归属填 ProjectID 或 CreatorID。
type Resource struct {
	Kind      Kind
	ID        string
	TenantID  string // 空 = DefaultTenant
	ProjectID string // session/plugin 等归属的项目；'' = 个人资源
	CreatorID string // 资源创建者（session 的 owner / project 的 created_by）
}

// Tenant 解析资源租户，空值归一为 DefaultTenant。
func (r Resource) Tenant() string {
	if r.TenantID == "" {
		return DefaultTenant
	}
	return r.TenantID
}

// InProject 报告资源是否归属某个项目（而非个人资源）。
func (r Resource) InProject() bool { return r.ProjectID != "" }

// Role 是调用者在某个项目内的有效角色。RoleNone 表示与项目无任何关系。
type Role int

const (
	RoleNone Role = iota
	RoleMember
	RoleAdmin
	RoleOwner
)

// Principal 是鉴权视角下的调用者身份。
// 与 pkg/auth.Principal 解耦：auth 管凭证解析，authz 只关心判定输入。
type Principal struct {
	User    string
	Tenant  string // 空 = DefaultTenant
	IsAdmin bool
}

// Tenant 解析调用者租户，空值归一为 DefaultTenant。
func (p Principal) TenantID() string {
	if p.Tenant == "" {
		return DefaultTenant
	}
	return p.Tenant
}

// PrincipalFrom 从 ctx 取出鉴权身份；取不到时返回匿名身份（非 admin）。
// 匿名身份的 User 为空串，Decide 会按"无归属者"拒绝绝大多数 Action，
// 与既有"空 owner 视为无主"的约定一致。
func PrincipalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(*Principal); ok && p != nil {
		return *p
	}
	return Principal{}
}

type principalKey struct{}

// WithPrincipal 把鉴权身份注入 ctx。
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Authorizer 是鉴权入口。实现方负责从存储解析 role，再委托 Decide。
type Authorizer interface {
	Can(ctx context.Context, a Action, r Resource) error
}

// ErrForbidden 是所有拒绝的哨兵错误；调用方可用 errors.Is 区分"没权限"与"不存在"。
var ErrForbidden = fmt.Errorf("forbidden")

// roleLevel 把 Role 映射为可比较的层级，owner > admin > member > none。
func roleLevel(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	default:
		return 0
	}
}

// roleAtLeast 报告实际角色是否达到要求的最低档位。
func roleAtLeast(got, want Role) bool { return roleLevel(got) >= roleLevel(want) }

// Decide 是唯一判定入口：给定身份、动作、资源与调用者在资源项目内的角色，
// 返回 nil（放行）或包装 ErrForbidden 的错误。
//
// 判定顺序（与权限矩阵一致）：
//  1. tenant 一致性 —— 跨租户一律拒绝；
//  2. global admin —— 全部放行；
//  3. 项目层级 —— project:* 与归属项目的资源按角色矩阵放行；
//  4. 个人资源轴 —— 不归属项目的资源仅 creator 本人可操作。
func Decide(p Principal, a Action, r Resource, role Role) error {
	if p.TenantID() != r.Tenant() {
		return fmt.Errorf("%w: tenant %q does not match resource tenant %q", ErrForbidden, p.TenantID(), r.Tenant())
	}
	if p.IsAdmin {
		return nil
	}

	// 项目层级动作：project:* 一律要求 role 达到矩阵档位。
	minRole, ok := projectActionRole[a]
	if ok {
		if roleAtLeast(role, minRole) {
			return nil
		}
		return fmt.Errorf("%w: action %s requires role %v, caller has %v", ErrForbidden, a, minRole, role)
	}

	switch r.Kind {
	case KindSession:
		return decideSession(p, a, r, role)
	case KindPlugin, KindLease:
		return decideProjectScoped(p, a, r, role)
	case KindProbe:
		return decideProbe(p, a, r)
	case KindUser:
		// 用户管理（发放邀请之外的列表 / 撤销）仅 global admin；
		// admin 已在 Decide 开头放行，落到这里的一律拒绝。
		return fmt.Errorf("%w: action %s requires global admin", ErrForbidden, a)
	case KindProject:
		// project:read 之外未在 projectActionRole 注册的 project 动作不应存在。
		return fmt.Errorf("%w: unsupported project action %s", ErrForbidden, a)
	default:
		return fmt.Errorf("%w: unsupported resource kind %s", ErrForbidden, r.Kind)
	}
}

// projectActionRole 是 project:* 动作的最低角色档位（权限矩阵的代码形态）。
var projectActionRole = map[Action]Role{
	ActionProjectRead:          RoleMember,
	ActionProjectUpdate:        RoleOwner,
	ActionProjectDelete:        RoleOwner,
	ActionProjectManageMembers: RoleAdmin,
	ActionProjectManagePlugins: RoleAdmin,
	ActionProjectManageRules:   RoleAdmin,
	ActionProjectTransferOwner: RoleOwner,
}

// decideSession 处理会话动作：项目会话走项目层级，个人会话走 creator 轴。
func decideSession(p Principal, a Action, r Resource, role Role) error {
	if r.InProject() {
		minRole, ok := sessionProjectActionRole[a]
		if !ok {
			return fmt.Errorf("%w: unsupported session action %s", ErrForbidden, a)
		}
		if roleAtLeast(role, minRole) {
			return nil
		}
		// session:delete / session:move_project 对 member 有 creator 例外。
		if memberCreatorAllowed(a) && r.CreatorID == p.User && p.User != "" {
			return nil
		}
		return fmt.Errorf("%w: action %s requires role %v on project %s", ErrForbidden, a, minRole, r.ProjectID)
	}
	// 个人会话：仅 creator（admin 已在上面放行）。
	// 匿名对匿名（双方空串）是既有契约（本地单机用法 / T13 回归底线）：
	// metadata.json 缺失的历史会话合成 Owner=""，匿名调用方必须可见；
	// 有主资源对匿名调用方仍拒绝（"alice" != ""）。
	if a == ActionSessionRead || a == ActionSessionUse || a == ActionSessionDelete || a == ActionSessionMoveProject {
		if r.CreatorID == p.User {
			return nil
		}
	}
	return fmt.Errorf("%w: session %s is not accessible to %s", ErrForbidden, r.ID, p.User)
}

// sessionProjectActionRole 是项目会话动作的最低角色档位。
var sessionProjectActionRole = map[Action]Role{
	ActionSessionRead:        RoleMember,
	ActionSessionUse:         RoleMember,
	ActionSessionDelete:      RoleAdmin,
	ActionSessionMoveProject: RoleAdmin,
}

// memberCreatorAllowed 报告某动作是否允许 member 操作"自己创建的"资源。
func memberCreatorAllowed(a Action) bool {
	return a == ActionSessionDelete || a == ActionSessionMoveProject
}

// decideProbe 处理探针动作：探针是个人资源，仅注册者（creator）本人可用，
// 不做项目轴与角色矩阵（2026-09-05 review 定稿：默认只能自己使用，别人无法使用）。
// global admin 已在 Decide 开头放行；Resource.CreatorID 填 probes.owner。
func decideProbe(p Principal, a Action, r Resource) error {
	switch a {
	case ActionProbeRead, ActionProbeUse, ActionProbeControl, ActionProbeManage:
	default:
		return fmt.Errorf("%w: unsupported probe action %s", ErrForbidden, a)
	}
	if r.CreatorID != "" && r.CreatorID == p.User {
		return nil
	}
	return fmt.Errorf("%w: probe %s is not accessible to %s", ErrForbidden, r.ID, p.User)
}

// decideProjectScoped 处理 plugin / lease 这类"项目内使用、项目外归 creator"的资源。
func decideProjectScoped(p Principal, a Action, r Resource, role Role) error {
	switch a {
	case ActionPluginRead:
		return nil // 插件目录全局可读，沿用现状
	case ActionPluginUse, ActionLeaseUse, ActionLeaseRelease:
		if r.InProject() {
			if roleAtLeast(role, RoleMember) {
				return nil
			}
			return fmt.Errorf("%w: action %s requires project membership in %s", ErrForbidden, a, r.ProjectID)
		}
		if r.CreatorID == p.User { // 匿名对匿名沿用同一契约
			return nil
		}
		return fmt.Errorf("%w: %s %s is not accessible to %s", ErrForbidden, r.Kind, r.ID, p.User)
	case ActionPluginManage:
		if r.InProject() {
			if roleAtLeast(role, RoleAdmin) {
				return nil
			}
			return fmt.Errorf("%w: action %s requires role admin on project %s", ErrForbidden, a, r.ProjectID)
		}
		if r.CreatorID == p.User {
			return nil
		}
		return fmt.Errorf("%w: %s %s is not manageable by %s", ErrForbidden, r.Kind, r.ID, p.User)
	default:
		return fmt.Errorf("%w: unsupported action %s", ErrForbidden, a)
	}
}

// AccessCode 特例：create 归 creator 本人；claim 是未鉴权端点的内部动作，
// 由启动码一次性 + 24h 过期兜底，不走 Decide 的用户轴（claim 时没有用户身份）。
// 保留这两个 Action 是为了护栏测试能覆盖 access_code 相关 handler。
func AccessCodeActionAllowed(p Principal, a Action, creatorID string) bool {
	if p.IsAdmin {
		return true
	}
	switch a {
	case ActionAccessCodeCreate:
		return creatorID == "" || creatorID == p.User
	case ActionAccessCodeClaim:
		return true
	default:
		return false
	}
}
