package auth

import (
	"database/sql"
	"log/slog"
)

// DBResolver 从持久化用户表（users）解析身份：邀请制身份发放的运行时侧。
// 与 StaticResolver（GTA_AUTH_TOKENS bootstrap）组合使用，见 FirstResolver。
//
// 故意每次请求都查库：控制面 QPS 极低（人手操作 + agent 回连），换来的是
// 邀请发放 / 撤销即时生效，无需重启。token 是 256bit 随机值，走主键等值查询。
type DBResolver struct {
	db *sql.DB
}

// NewDBResolver 基于 users 表构造 resolver。db 为空视为不可用（Resolve 恒失败）。
func NewDBResolver(db *sql.DB) *DBResolver {
	return &DBResolver{db: db}
}

// HasUsers 报告用户表是否已有任何用户（用于启动时判断是否进入 token 模式）。
// 查询失败按"无用户"处理并记日志：宁可退回匿名模式也不要把整个服务锁死。
func (r *DBResolver) HasUsers() bool {
	if r == nil || r.db == nil {
		return false
	}
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		slog.Warn("dbresolver: count users failed", "error", err)
		return false
	}
	return n > 0
}

// Resolve 按精确 token 匹配 users 表。
func (r *DBResolver) Resolve(token string) (*Principal, bool) {
	if r == nil || r.db == nil || token == "" {
		return nil, false
	}
	var owner string
	var isAdmin int
	var tenant string
	err := r.db.QueryRow(
		`SELECT owner, is_admin, tenant_id FROM users WHERE token=?`, token,
	).Scan(&owner, &isAdmin, &tenant)
	if err != nil {
		return nil, false
	}
	return &Principal{Owner: owner, IsAdmin: isAdmin != 0, Tenant: tenant}, true
}

// FirstResolver 按顺序组合多个 resolver，第一个命中即返回。
//
// 组合语义（与匿名模式兼容）：
//   - primary（env bootstrap）配置了 token：env 命中优先；否则查 users 表；
//   - primary 为匿名模式但 users 表非空：只查 users 表（env 为空也能跑邀请制部署）；
//   - 两者皆空：纯匿名模式，Resolve 恒返回 local 身份（现状兼容底线）。
//
// Required() 报告是否处于 token 模式，供 authMiddleware 决定是否挂载 401 校验。
type FirstResolver struct {
	primary *StaticResolver
	db      *DBResolver
}

// NewFirstResolver 组合 env bootstrap 与 users 表两个身份来源。
func NewFirstResolver(primary *StaticResolver, db *DBResolver) *FirstResolver {
	return &FirstResolver{primary: primary, db: db}
}

// Required 报告是否进入 token 模式（任一来源有身份即算）。
func (r *FirstResolver) Required() bool {
	if r == nil {
		return false
	}
	return r.primary.Required() || r.db.HasUsers()
}

// Resolve 见类型注释的组合语义。
func (r *FirstResolver) Resolve(token string) (*Principal, bool) {
	if r.primary.Required() {
		if p, ok := r.primary.Resolve(token); ok {
			return p, true
		}
	}
	if p, ok := r.db.Resolve(token); ok {
		return p, true
	}
	if !r.Required() {
		return &Principal{Owner: AnonymousOwner}, true
	}
	return nil, false
}
