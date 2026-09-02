package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// projectRole 描述项目成员在项目内的角色。
type projectRole string

const (
	roleAdmin  projectRole = "admin"
	roleMember projectRole = "member"
)

// projectMember 是项目的一个成员（user + role）。
type projectMember struct {
	User string      `json:"user"`
	Role projectRole `json:"role"`
}

// projectPlugin 是项目关联的一条插件条目（轻量关联，非全量插件注册表）。
type projectPlugin struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
	DefaultPlugin string          `json:"default_plugin,omitempty"`
	DefaultPort   int             `json:"default_port,omitempty"`
	Members       []projectMember `json:"members"`
	Plugins       []projectPlugin `json:"plugins"`
	Rules         []projectRule   `json:"rules"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
}

// projectStore 提供 projects 表的 SQLite 持久化访问。
// members/plugins/rules 以 JSON 文本列存储；读写时 marshal/unmarshal，默认空切片。
type projectStore struct {
	db *sql.DB
}

// newProjectStoreDB 基于已打开的 *sql.DB 构造项目存储。
func newProjectStoreDB(db *sql.DB) *projectStore {
	return &projectStore{db: db}
}

// Init 确保 projects 表存在。
func (ps *projectStore) Init() error {
	schema := `
CREATE TABLE IF NOT EXISTS projects (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    game           TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    default_plugin TEXT NOT NULL DEFAULT '',
    default_port   INTEGER NOT NULL DEFAULT 0,
    members        TEXT NOT NULL DEFAULT '[]',
    plugins        TEXT NOT NULL DEFAULT '[]',
    rules          TEXT NOT NULL DEFAULT '[]',
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL
);`
	if _, err := ps.db.Exec(schema); err != nil {
		return fmt.Errorf("create projects table: %w", err)
	}
	return nil
}

// jsonOrEmpty 将切片 marshal 成 JSON 字符串；nil/空切片落在 '[]'。
func jsonOrEmpty(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Create 插入一个新项目。
func (ps *projectStore) Create(ctx context.Context, p *project) error {
	_, err := ps.db.ExecContext(ctx, `
INSERT INTO projects(id, name, description, game, created_by, default_plugin, default_port,
                     members, plugins, rules, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.Game, p.CreatedBy, p.DefaultPlugin, p.DefaultPort,
		jsonOrEmpty(p.Members), jsonOrEmpty(p.Plugins), jsonOrEmpty(p.Rules),
		p.CreatedAt, p.UpdatedAt,
	)
	return err
}

// Update 整体覆写一个已存在项目（调用方负责加载后修改、设置 UpdatedAt）。
func (ps *projectStore) Update(ctx context.Context, p *project) error {
	_, err := ps.db.ExecContext(ctx, `
UPDATE projects SET name=?, description=?, game=?, created_by=?, default_plugin=?, default_port=?,
                    members=?, plugins=?, rules=?, updated_at=?
WHERE id=?`,
		p.Name, p.Description, p.Game, p.CreatedBy, p.DefaultPlugin, p.DefaultPort,
		jsonOrEmpty(p.Members), jsonOrEmpty(p.Plugins), jsonOrEmpty(p.Rules),
		p.UpdatedAt, p.ID,
	)
	return err
}

// Get 查询单个项目；未找到返回 (nil, nil)。
func (ps *projectStore) Get(ctx context.Context, id string) (*project, error) {
	row := ps.db.QueryRowContext(ctx, `
SELECT id, name, description, game, created_by, default_plugin, default_port,
       members, plugins, rules, created_at, updated_at
FROM projects WHERE id=?`, id)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get project %s: %w", id, err)
	}
	return p, nil
}

// Delete 删除项目；返回是否删到了记录。
func (ps *projectStore) Delete(ctx context.Context, id string) (bool, error) {
	res, err := ps.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListVisible 列出调用方可见的项目（created_at 降序）。
// all=true（admin）时返回所有行；否则仅返回 visibleTo(p, owner) 的行。
func (ps *projectStore) ListVisible(ctx context.Context, owner string, all bool) ([]project, error) {
	rows, err := ps.db.QueryContext(ctx, `
SELECT id, name, description, game, created_by, default_plugin, default_port,
       members, plugins, rules, created_at, updated_at
FROM projects ORDER BY created_at DESC`)
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
	return result, rows.Err()
}

// visibleTo 判断 owner 是否可见该项目：创建者本人或任一成员的 User 匹配。
func visibleTo(p *project, owner string) bool {
	if p.CreatedBy == owner {
		return true
	}
	for _, mbr := range p.Members {
		if mbr.User == owner {
			return true
		}
	}
	return false
}

// CanManage 判断 owner 是否可管理项目：全局 admin、项目创建者，或项目内 admin 角色成员。
// 角色层级：global admin > project owner(created_by) > project admin > project member；
// member 角色仅可查看/使用项目（开始抓包、查看会话、分析数据），不具管理权。
// Owner 不单列角色字段：created_by 即 owner，成员角色用 members[].role 表达。
func (ps *projectStore) CanManage(p *project, owner string, all bool) bool {
	if all || p.CreatedBy == owner {
		return true
	}
	for _, mbr := range p.Members {
		if mbr.User == owner && mbr.Role == roleAdmin {
			return true
		}
	}
	return false
}

// projectRowScanner 抽象 sql.Row / sql.Rows 的 Scan 方法。
type projectRowScanner interface {
	Scan(dest ...any) error
}

// scanProject 读取一行 project，并对 members/plugins/rules JSON 列反序列化。
func scanProject(s projectRowScanner) (*project, error) {
	var p project
	var membersJSON, pluginsJSON, rulesJSON string
	if err := s.Scan(
		&p.ID, &p.Name, &p.Description, &p.Game, &p.CreatedBy, &p.DefaultPlugin, &p.DefaultPort,
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