package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gta/pkg/authz"
)

// projectRole 描述项目成员在项目内的角色。
//
// 角色语义（2026-09-05 钉死，docs/superpowers/plans/2026-09-05-tenant-project-authz.md §4.2）：
//   - Owner：projects.owner 字段（SSOT），全权含删除项目、转移 Owner；
//   - Admin：members 表 role=admin，可管成员/插件/规则/项目内会话；
//   - Member：members 表 role=member，可用项目、读数据、操作自己创建的会话。
//   - created_by 退化为审计字段（谁建的，永不变更），不参与鉴权。
type projectRole string

const (
	roleAdmin  projectRole = "admin"
	roleMember projectRole = "member"
)

// projectMember 是项目的一个成员（user + role）。Owner 不在此列，见 project.Owner。
// projectMember 是项目的一名成员（持久化在 project_members 表，不再用 JSON 列）。
// Registered 不持久化：由 get_project 在响应时填充，标注该用户名是否已有身份
//（false = 预邀请，对方注册同名身份后自动生效）。
type projectMember struct {
	User       string      `json:"user"`
	Role       projectRole `json:"role"`
	Registered bool        `json:"registered,omitempty"`
}

// projectPlugin 是项目关联的一条插件条目（轻量关联，非全量插件注册表）。
// Owner 是把该插件加入项目时设置者的身份（插件在注册表中按 owner 作用域隔离，
// 项目成员解析解码插件时以此为跨 owner 候选；老数据无此字段，成员解析不到时
// 由项目 admin 重新保存一次插件列表即可回填）。
type projectPlugin struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
}

// projectRule 是项目关联的一条规则条目。
type projectRule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// project 是持久化在 control.sqlite projects 表的一个项目。
type project struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Game          string          `json:"game,omitempty"`
	CreatedBy     string          `json:"created_by"`
	Owner         string          `json:"owner,omitempty"`
	TenantID      string          `json:"tenant_id,omitempty"`
	DefaultPlugin string          `json:"default_plugin,omitempty"`
	DefaultPort   int             `json:"default_port,omitempty"`
	Members       []projectMember `json:"members"`
	Plugins       []projectPlugin `json:"plugins"`
	Rules         []projectRule   `json:"rules"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
}

// tenantOrDefault 归一租户字段。
func tenantOrDefault(t string) string {
	if t == "" {
		return authz.DefaultTenant
	}
	return t
}

// Tenant 返回项目租户（空值归一）。
func (p *project) Tenant() string { return tenantOrDefault(p.TenantID) }

// projectStore 提供 projects / project_members 表的 SQLite 持久化访问。
// members 存 project_members 表（鉴权热路径，需要 (project_id,user) 唯一约束与索引）；
// projects.members JSON 列保留一个版本作为回滚备份（读走表、写双写）。
// plugins/rules 仍为 JSON 列：纯展示关联，无查询/鉴权需求（方案 D6）。
type projectStore struct {
	db *sql.DB
}

// newProjectStoreDB 基于已打开的 *sql.DB 构造项目存储。
func newProjectStoreDB(db *sql.DB) *projectStore {
	return &projectStore{db: db}
}

// Init 建表并执行幂等迁移（列补齐、成员 JSON → 表回填）。
func (ps *projectStore) Init() error {
	schema := `
CREATE TABLE IF NOT EXISTS projects (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    game           TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    owner          TEXT NOT NULL DEFAULT '',
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    default_plugin TEXT NOT NULL DEFAULT '',
    default_port   INTEGER NOT NULL DEFAULT 0,
    members        TEXT NOT NULL DEFAULT '[]',
    plugins        TEXT NOT NULL DEFAULT '[]',
    rules          TEXT NOT NULL DEFAULT '[]',
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS project_members (
    project_id TEXT NOT NULL,
    user       TEXT NOT NULL,
    role       TEXT NOT NULL CHECK(role IN ('admin','member')),
    created_at DATETIME NOT NULL,
    PRIMARY KEY (project_id, user)
);`
	if _, err := ps.db.Exec(schema); err != nil {
		return fmt.Errorf("create project tables: %w", err)
	}
	if _, err := ps.db.Exec(`CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user)`); err != nil {
		return fmt.Errorf("index project_members(user): %w", err)
	}
	if err := ps.migrateProjectsAddOwnerTenant(); err != nil {
		return err
	}
	if err := ps.migrateMembersJSONToTable(); err != nil {
		return err
	}
	return nil
}

// migrateProjectsAddOwnerTenant 为既有 projects 表补 owner / tenant_id 列。
// owner 回填 = created_by（创建者即首任 Owner）；tenant_id 统一 'default'。
// check-then-ALTER 非原子，并发迁移以 PRAGMA 复查兜底（与 owner 列迁移同款策略）。
func (ps *projectStore) migrateProjectsAddOwnerTenant() error {
	add := func(col, ddl string) error {
		if sqliteHasColumn(ps.db, "projects", col) {
			return nil
		}
		if _, err := ps.db.Exec(ddl); err != nil {
			if sqliteHasColumn(ps.db, "projects", col) {
				slog.Info("projects." + col + " column added concurrently; skipping migration")
				return nil
			}
			return fmt.Errorf("migrate projects.%s: %w", col, err)
		}
		return nil
	}
	if err := add("owner", `ALTER TABLE projects ADD COLUMN owner TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := add("tenant_id", `ALTER TABLE projects ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`); err != nil {
		return err
	}
	if _, err := ps.db.Exec(`UPDATE projects SET owner=created_by WHERE owner=''`); err != nil {
		return fmt.Errorf("backfill projects.owner: %w", err)
	}
	slog.Info("project store migrated: owner (SSOT) + tenant_id columns ready")
	return nil
}

// migrateMembersJSONToTable 把 projects.members JSON 回填到 project_members 表（幂等）。
// INSERT OR IGNORE 保证重复执行与并发迁移安全；JSON 列保留作为回滚备份，双写一个版本后移除。
func (ps *projectStore) migrateMembersJSONToTable() error {
	rows, err := ps.db.Query(`SELECT id, created_at, members FROM projects`)
	if err != nil {
		return fmt.Errorf("scan projects for member backfill: %w", err)
	}
	defer rows.Close()
	type row struct{ id, createdAt, membersJSON string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.createdAt, &r.membersJSON); err != nil {
			return err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var migrated int
	for _, r := range pending {
		for _, m := range unmarshalSlice[projectMember](r.membersJSON) {
			if m.User == "" {
				continue
			}
			if _, err := ps.db.Exec(
				`INSERT OR IGNORE INTO project_members(project_id, user, role, created_at) VALUES (?,?,?,?)`,
				r.id, m.User, string(m.Role), r.createdAt,
			); err != nil {
				return fmt.Errorf("backfill member %s@%s: %w", m.User, r.id, err)
			}
			migrated++
		}
	}
	if migrated > 0 {
		slog.Info("backfilled project_members from JSON", "rows", migrated)
	}
	return nil
}

// sqliteHasColumn 通过 PRAGMA table_info 判断某表是否已有指定列
// （pkg/store.hasColumn 是包内私有，此处本地复制同款实现）。
func sqliteHasColumn(db *sql.DB, table, col string) bool {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false
		}
		if name == col {
			return true
		}
	}
	return false
}

// jsonOrEmpty 将切片 marshal 成 JSON 字符串；nil/空切片落在 '[]'。
func jsonOrEmpty(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Create 插入一个新项目并写入成员表（事务）。
// Owner 默认 = CreatedBy（首任 Owner），TenantID 空值归一 'default'。
func (ps *projectStore) Create(ctx context.Context, p *project) error {
	if p.Owner == "" {
		p.Owner = p.CreatedBy
	}
	p.TenantID = tenantOrDefault(p.TenantID)
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO projects(id, name, description, game, created_by, owner, tenant_id, default_plugin, default_port,
                     members, plugins, rules, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.Game, p.CreatedBy, p.Owner, p.TenantID, p.DefaultPlugin, p.DefaultPort,
		jsonOrEmpty(p.Members), jsonOrEmpty(p.Plugins), jsonOrEmpty(p.Rules),
		p.CreatedAt, p.UpdatedAt,
	); err != nil {
		return err
	}
	if err := writeMembersTx(ctx, tx, p.ID, p.Members); err != nil {
		return err
	}
	return tx.Commit()
}

// Update 整体覆写一个已存在项目并全量重写成员表（事务，调用方负责加载后修改、设置 UpdatedAt）。
func (ps *projectStore) Update(ctx context.Context, p *project) error {
	p.TenantID = tenantOrDefault(p.TenantID)
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
UPDATE projects SET name=?, description=?, game=?, created_by=?, owner=?, tenant_id=?, default_plugin=?, default_port=?,
                    members=?, plugins=?, rules=?, updated_at=?
WHERE id=?`,
		p.Name, p.Description, p.Game, p.CreatedBy, p.Owner, p.TenantID, p.DefaultPlugin, p.DefaultPort,
		jsonOrEmpty(p.Members), jsonOrEmpty(p.Plugins), jsonOrEmpty(p.Rules),
		p.UpdatedAt, p.ID,
	); err != nil {
		return err
	}
	// JSON 备份列与成员表同步全量重写，两者由同一事务保证一致。
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_members WHERE project_id=?`, p.ID); err != nil {
		return err
	}
	if err := writeMembersTx(ctx, tx, p.ID, p.Members); err != nil {
		return err
	}
	return tx.Commit()
}

// writeMembersTx 在事务内写入成员行（JSON 备份列由调用方的 projects 写语句同步覆盖）。
func writeMembersTx(ctx context.Context, tx *sql.Tx, projectID string, members []projectMember) error {
	for _, m := range members {
		if m.User == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO project_members(project_id, user, role, created_at) VALUES (?,?,?,?)`,
			projectID, m.User, string(m.Role), time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("write member %s@%s: %w", m.User, projectID, err)
		}
	}
	return nil
}

const projectSelectCols = `id, name, description, game, created_by, owner, tenant_id, default_plugin, default_port,
	members, plugins, rules, created_at, updated_at`

// Get 查询单个项目（含成员表）；未找到返回 (nil, nil)。
func (ps *projectStore) Get(ctx context.Context, id string) (*project, error) {
	row := ps.db.QueryRowContext(ctx, `SELECT `+projectSelectCols+` FROM projects WHERE id=?`, id)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get project %s: %w", id, err)
	}
	members, err := ps.listMembers(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Members = members
	return p, nil
}

// Delete 删除项目及其成员行（事务）；返回是否删到了记录。
func (ps *projectStore) Delete(ctx context.Context, id string) (bool, error) {
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_members WHERE project_id=?`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListVisible 列出调用方可见的项目（created_at 降序）。
// all=true（admin）时返回所有行；否则仅返回 visibleTo(p, owner) 的行。
func (ps *projectStore) ListVisible(ctx context.Context, owner string, all bool) ([]project, error) {
	rows, err := ps.db.QueryContext(ctx, `SELECT `+projectSelectCols+` FROM projects ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		if all || visibleTo(p, owner) {
			result = append(result, *p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 补齐可见项目的成员表数据。
	for i := range result {
		members, err := ps.listMembers(ctx, result[i].ID)
		if err != nil {
			return nil, err
		}
		result[i].Members = members
	}
	return result, nil
}

// listMembers 读成员表，按 role、user 排序保证输出稳定。
func (ps *projectStore) listMembers(ctx context.Context, projectID string) ([]projectMember, error) {
	rows, err := ps.db.QueryContext(ctx,
		`SELECT user, role FROM project_members WHERE project_id=? ORDER BY user`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", projectID, err)
	}
	defer rows.Close()
	var out []projectMember
	for rows.Next() {
		var m projectMember
		var role string
		if err := rows.Scan(&m.User, &role); err != nil {
			return nil, err
		}
		m.Role = projectRole(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// visibleTo 判断 user 是否可见该项目：当前 Owner 或任一成员。
// 仅作 ListVisible 的 Go 侧过滤；鉴权判定统一走 authz（visibleTo 不做角色区分）。
func visibleTo(p *project, user string) bool {
	if p.Owner == user {
		return true
	}
	for _, mbr := range p.Members {
		if mbr.User == user {
			return true
		}
	}
	// 兼容 owner 列尚未回填的历史行。
	return p.Owner == "" && p.CreatedBy == user
}

// RoleOf 解析 user 在项目内的有效角色，供 authz.Decide 消费。
// 项目不存在返回 (RoleNone, nil)，由调用方决定报"不存在"还是"无权限"。
// created_by 仅在 owner 列为空的历史行兜底。
func (ps *projectStore) RoleOf(ctx context.Context, projectID, user string) (authz.Role, error) {
	var owner, createdBy string
	err := ps.db.QueryRowContext(ctx, `SELECT owner, created_by FROM projects WHERE id=?`, projectID).
		Scan(&owner, &createdBy)
	if err == sql.ErrNoRows {
		return authz.RoleNone, nil
	}
	if err != nil {
		return authz.RoleNone, fmt.Errorf("role lookup project %s: %w", projectID, err)
	}
	if (owner != "" && owner == user) || (owner == "" && createdBy == user) {
		return authz.RoleOwner, nil
	}
	var role string
	err = ps.db.QueryRowContext(ctx,
		`SELECT role FROM project_members WHERE project_id=? AND user=?`, projectID, user).Scan(&role)
	if err == sql.ErrNoRows {
		return authz.RoleNone, nil
	}
	if err != nil {
		return authz.RoleNone, fmt.Errorf("role lookup member %s@%s: %w", user, projectID, err)
	}
	if projectRole(role) == roleAdmin {
		return authz.RoleAdmin, nil
	}
	return authz.RoleMember, nil
}

// AddMember 添加 / 覆盖项目成员（禁止覆盖 Owner —— Owner 不在成员表）。
func (ps *projectStore) AddMember(ctx context.Context, projectID string, m projectMember) error {
	_, err := ps.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO project_members(project_id, user, role, created_at) VALUES (?,?,?,?)`,
		projectID, m.User, string(m.Role), time.Now().UTC().Format(time.RFC3339))
	return err
}

// RemoveMember 移除项目成员；返回是否删到了记录。
func (ps *projectStore) RemoveMember(ctx context.Context, projectID, user string) (bool, error) {
	res, err := ps.db.ExecContext(ctx,
		`DELETE FROM project_members WHERE project_id=? AND user=?`, projectID, user)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TransferOwner 以 CAS 方式转移 Owner：仅当当前 Owner 与 expectOwner 一致时生效，
// 防并发双转。返回是否成功；False 表示项目不存在或 Owner 已被并发改变。
func (ps *projectStore) TransferOwner(ctx context.Context, projectID, expectOwner, newOwner string) (bool, error) {
	res, err := ps.db.ExecContext(ctx,
		`UPDATE projects SET owner=?, updated_at=? WHERE id=? AND owner=?`,
		newOwner, time.Now().UTC().Format(time.RFC3339), projectID, expectOwner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// projectRowScanner 抽象 sql.Row / sql.Rows 的 Scan 方法。
type projectRowScanner interface {
	Scan(dest ...any) error
}

// scanProject 读取一行 project，并对 members/plugins/rules JSON 列反序列化。
// members 仅为 JSON 备份列的快照，调用方应随后用 listMembers 覆盖。
func scanProject(s projectRowScanner) (*project, error) {
	var p project
	var membersJSON, pluginsJSON, rulesJSON string
	if err := s.Scan(
		&p.ID, &p.Name, &p.Description, &p.Game, &p.CreatedBy, &p.Owner, &p.TenantID, &p.DefaultPlugin, &p.DefaultPort,
		&membersJSON, &pluginsJSON, &rulesJSON, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	p.Members = unmarshalSlice[projectMember](membersJSON)
	p.Plugins = unmarshalSlice[projectPlugin](pluginsJSON)
	p.Rules = unmarshalSlice[projectRule](rulesJSON)
	return &p, nil
}

// unmarshalSlice 将 JSON 列反序列化为切片；空串/解析失败时返回空切片。
func unmarshalSlice[T any](s string) []T {
	if s == "" {
		return []T{}
	}
	var out []T
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []T{}
	}
	if out == nil {
		return []T{}
	}
	return out
}
