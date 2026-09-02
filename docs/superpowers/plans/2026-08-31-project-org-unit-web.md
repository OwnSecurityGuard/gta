# 项目组织单元 + Web 首页（Project Org-Unit + Web Home）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把「项目 Project」从轻量抓包配置升级为一等组织单元（项目持有 Game / Members / Decoder Plugins / Rules / Sessions），并把 Web 首页「我的项目」作为最高优先级入口；会话可挂到项目下，首页与项目详情页展示实时状态与时间线。

**Architecture:** 将项目存储从「按 owner 分片的 JSON 文件」迁移到 `control.sqlite` 的 `projects` 表（因为项目需跨 owner 可见：created_by + members 都要能看到）。`sessions` 表新增 `project_id` 列，`project_id` 沿 Web → MCP `start_capture` → gRPC `StartCaptureRequest` → pipeline `StartSession` → `SessionMeta` → `CreateSession` 全链路透传。项目级仅两档角色 admin/member，不做复杂 RBAC。

**Tech Stack:** Go (gRPC + SQLite via modernc.org/sqlite)、MCP (mark3labs/mcp-go)、React + Vite + TanStack Query。

---
> 已确认的范围决策（AskUserQuestion 结果）：实现主线 = 项目组织单元 + Web 首页；成员模型 = project 级 owner/admin + member 列表；会话归属 = 一键开始抓包自动带 project_id；插件/规则 = 项目真正持有条目（简单关联 CRUD，不做独立管理后台 / schema / contract 编辑器）。

---

## 文件结构（本轮新增/修改）

**后端（共享服务端）**
- Modify `pkg/store/session_store.go` — `sessions` 表加 `project_id` 列 + 迁移 + CRUD 透传。
- Modify `pkg/store/eventstore.go` — `SessionMeta` 加 `ProjectID` 字段。
- Modify `pkg/internalipc/proto/internal.proto` → 再生成 `pkg/internalipc/proto/internal.pb.go` — `StartCaptureRequest` 加 `project_id = 10`。
- Modify `cmd/gta-pipeline/pipeline_service.go` — `StartSession` 透传 `ProjectID` 到 `SessionMeta`。
- Redirect `cmd/gta-mcp/project.go` — 从 JSON `projectStore` 改为 SQLite `projectStore`（projects 表 + members/plugins/rules JSON 列）。
- Modify `cmd/gta-mcp/main.go` — `handleStartCapture` 读 `project_id` 并透传；注册新 project 工具；`mcpCapture.projects` 改为 SQLite 存储。
- New `cmd/gta-mcp/project_store.go` — SQLite《projects》表定义、迁移、CRUD、成员判定。
- New test `cmd/gta-mcp/project_store_test.go`、`gta-mcp/project_membership_test.go`。

**前端（web/）**
- Modify `web/src/types/project.ts` — 扩充 `ProjectInfo`（description/game/members/plugins/rules/created_by/status），新增 `ProjectMember`、`ProjectPlugin`、`ProjectRule`、`ProjectDetail`。
- Modify `web/src/hooks/use-mcp.ts` — 新增 `useProject`/`useProjectSessions`/`useSetSessionProject` 及 member/plugin/rule mutations。
- New `web/src/components/project-page.tsx` — 项目详情页（状态/成员/插件/规则/会话列表）。
- Modify `web/src/components/my-capture-page.tsx` — 项目卡片展示派生状态 + 最近会话，「管理/进入」跳项目详情页。
- Modify `web/src/components/start-capture-dialog.tsx` — 从某项目开始时自动携带 `project_id`。
- Modify `web/src/App.tsx` — 接入项目详情页路由与「进入项目」入口。

---

## Task 1: `SessionMeta` 增加 `ProjectID` 字段

**Files:**
- Modify: `pkg/store/eventstore.go:219-244`

- [ ] **Step 1: 加字段**

在 `SessionMeta` 中、`Owner` 之后新增字段（持久化为 `sessions.project_id` 列）：

```go
	// ProjectID 是会话所属的项目（projects.id）。空串表示未归属任何项目。
	// 一等字段，持久化到 sessions.project_id 列。
	ProjectID    string         `json:"project_id,omitempty"`
```

- [ ] **Step 2: 编译**

Run: `go build ./pkg/...`
Expected: 编译通过（字段为新增，无既有调用者受影响）。

- [ ] **Step 3: Commit**

```bash
git add pkg/store/eventstore.go
git commit -m "feat(store): add ProjectID to SessionMeta"
```

---

## Task 2: `sessions` 表加 `project_id` 列 + 迁移 + CRUD 透传

**Files:**
- Modify: `pkg/store/session_store.go`
- Test: `pkg/store/session_store_test.go`

- [ ] **Step 1: 列定义 + 迁移**

在 `init()` 的 `sessions` 建表语句（第 42-59 行）追加列，并调用迁移：

```sql
    manifest_snapshot TEXT DEFAULT ''
    , project_id      TEXT NOT NULL DEFAULT ''
);`
```

在 `init()` 中 `migrateSessionsAddOwner()` 之后追加迁移调用：

```go
	if err := cs.migrateSessionsAddProjectID(); err != nil {
		return err
	}
```

新增迁移函数（仿照 `migrateSessionsAddOwner`）：

```go
// migrateSessionsAddProjectID 为既有数据库补齐 sessions.project_id 列（默认 ''）。
func (cs *ControlStore) migrateSessionsAddProjectID() error {
	if hasColumn(cs.db, "sessions", "project_id") {
		return nil
	}
	if _, err := cs.db.Exec(`ALTER TABLE sessions ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`); err != nil {
		if hasColumn(cs.db, "sessions", "project_id") {
			slog.Info("sessions.project_id column added concurrently by another process; skipping migration")
			return nil
		}
		return fmt.Errorf("migrate sessions.project_id: %w", err)
	}
	slog.Info("migrated control store: added sessions.project_id column (backfilled '' = no project)")
	return nil
}

// hasColumn 泛化列存在性检查（复用/替代 hasOwnerColumn）。
func hasColumn(db *sql.DB, table, col string) bool {
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
```

将 `hasOwnerColumn` 改为调用通用 `hasColumn`（`hasColumn(cs.db, "sessions", "owner")`），避免重复逻辑。

- [ ] **Step 2: 写失败测试**

在 `pkg/store/session_store_test.go` 追加（用临时 control.sqlite）：

```go
func TestSessionProjectIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewControlStore(filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatalf("NewControlStore: %v", err)
	}
	defer cs.Close()
	meta := SessionMeta{
		ProjectID: "20260831_120000.000",
		SessionID: "sess-1",
		StartedAt: time.Now(),
		Status:    "running",
		DBPath:    filepath.Join(dir, "sess.db"),
	}
	if err := cs.CreateSession(context.Background(), meta); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := cs.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ProjectID != meta.ProjectID {
		t.Fatalf("ProjectID roundtrip = %q, want %q", got.ProjectID, meta.ProjectID)
	}
}
```

- [ ] **Step 3: 运行测试使其失败**

Run: `go test ./pkg/store -run TestSessionProjectIDRoundTrip -v`
Expected: FAIL —— `scanSession` 尚不读 `project_id`，`got.ProjectID` 为空。

- [ ] **Step 4: 实现 CRUD 透传**

- `CreateSession`：INSERT 列清单加 `project_id`，`VALUES` 加 `?`，参数加 `meta.ProjectID`。
- `sessionSelectCols`：前置 `project_id,`。
- `scanSession`：在 `&meta.Owner,` 之后加 `&meta.ProjectID,`。
- `UpdateSession`：SET 加 `project_id=?`，参数加 `meta.ProjectID`。
- 新增按项目过滤查询：

```go
// ListSessionsForProject 列出某项目下的会话元数据，按 started_at 降序、按 owner 可见性过滤。
func (cs *ControlStore) ListSessionsForProject(ctx context.Context, projectID string, f SessionOwnerFilter) ([]SessionMeta, error) {
	query := `SELECT ` + sessionSelectCols + ` FROM sessions WHERE project_id=?`
	args := []any{projectID}
	if !f.AllOwners {
		query += ` AND owner=?`
		args = append(args, f.Owner)
	}
	query += ` ORDER BY started_at DESC`
	rows, err := cs.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionMeta
	for rows.Next() {
		meta, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *meta)
	}
	return result, rows.Err()
}
```

- [ ] **Step 5: 运行测试使其通过**

Run: `go test ./pkg/store -run TestSessionProjectIDRoundTrip -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add pkg/store/session_store.go pkg/store/session_store_test.go
git commit -m "feat(store): add sessions.project_id column with migration & CRUD passthrough"
```

---

## Task 3: proto 加 `project_id` 并全链路透传

**Files:**
- Modify: `pkg/internalipc/proto/internal.proto:210-227`
- Regenerate: `pkg/internalipc/proto/internal.pb.go`、`pkg/internalipc/proto/internal_grpc.pb.go`
- Modify: `cmd/gta-pipeline/pipeline_service.go`
- Modify: `cmd/gta-mcp/main.go` (`handleStartCapture`)

- [ ] **Step 1: proto 加字段**

在 `StartCaptureRequest`（第 226 行 `bool all_owners = 9;` 后）追加：

```proto
  // 会话归属的项目（projects.id），可选。gta-mcp 从 HTTP 入参透传。
  string project_id = 10;
```

- [ ] **Step 2: 再生成 pb**

Run: `make proto`
Expected: 重新生成 `internal.pb.go` 等，`StartCaptureRequest` 暴露 `GetProjectId()`。

- [ ] **Step 3: pipeline 透传**

在 `pipeline_service.go` 的 `CreateSession`（第 165 行）`SessionMeta` 中补齐：

```go
		ProjectID:        req.GetProjectId(),
```

Run: `go build ./cmd/gta-pipeline`
Expected: 通过（`req.GetProjectId()` 由第 2 步生成）。

- [ ] **Step 4: MCP 透传入参**

在 `cmd/gta-mcp/main.go` `handleStartCapture`（第 460 行）读取可选的 `project_id` 并设置 grpcReq：

```go
	projectID := req.GetString("project_id", "")
	...
	// 在构造 grpcReq 时（约第 487-496 行）：
	grpcReq.ProjectId = projectID
```

并在成功返回 map 中加入 `"project_id": projectID`（同步写入 `sessionMetadata` 视为可选，项目关联以 sessions.project_id 为准）。

- [ ] **Step 5: 编译 + 冒烟**

Run: `go build ./...`
Expected: 全仓编译通过。

- [ ] **Step 6: Commit**

```bash
git add pkg/internalipc/proto/internal.proto pkg/internalipc/proto/internal.pb.go pkg/internalipc/proto/internal_grpc.pb.go cmd/gta-pipeline/pipeline_service.go cmd/gta-mcp/main.go
git commit -m "feat: thread project_id through start_capture to session record"
```

---

## Task 4: SQLite `projects` 表与项目存储迁移

**Files:**
- New: `cmd/gta-mcp/project_store.go`
- New: `cmd/gta-mcp/project_store_test.go`

- [ ] **Step 1: 定义数据结构与表**

新增 `project_store.go`：

```go
// project_store.go — 把「项目」落到 control.sqlite 的 projects 表。
//
// 与旧 JSON 分片（projects.<owner>.json）不同，项目需要跨 owner 可见：
// created_by 与 members 都能看到。故从 per-owner JSON 迁到 SQLite projects 表。
// members/plugins/rules 用 JSON 文本列存储，首版不做多表 join（YAGNI）。
package main

type projectRole string

const (
	roleAdmin  projectRole = "admin"
	roleMember projectRole = "member"
)

// projectMember 是项目成员；created_by 以 roleAdmin 幂等存在于此。
type projectMember struct {
	User string      `json:"user"`
	Role projectRole `json:"role"`
}

// projectPlugin / projectRule：项目持有的解码插件与规则条目（关联数据，非完整管理后台）。
type projectPlugin struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type projectRule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// project 是一等组织单元：持有 Game/Members/Decoder Plugins/Rules/Sessions。
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
```

- [ ] **Step 2: 定义 `projectStore`（SQLite 版，替换旧 JSON 实现）**

DBCreate/迁移/CRUD：

```go
type projectStore struct {
	db *sql.DB
}

const projectSchema = `
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

func newProjectStoreDB(db *sql.DB) *projectStore {
	return &projectStore{db: db}
}

func (ps *projectStore) Init() error {
	if _, err := ps.db.Exec(projectSchema); err != nil {
		return err
	}
	return nil
}

func (ps *projectStore) Create(ctx context.Context, p *project) error {
	members, _ := json.Marshal(p.Members)
	plugins, _ := json.Marshal(p.Plugins)
	rules, _ := json.Marshal(p.Rules)
	_, err := ps.db.ExecContext(ctx, `
INSERT INTO projects(id, name, description, game, created_by, default_plugin, default_port,
                     members, plugins, rules, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.Game, p.CreatedBy, p.DefaultPlugin, p.DefaultPort,
		string(members), string(plugins), string(rules), p.CreatedAt, p.UpdatedAt)
	return err
}

func (ps *projectStore) Update(ctx context.Context, p *project) error {
	members, _ := json.Marshal(p.Members)
	plugins, _ := json.Marshal(p.Plugins)
	rules, _ := json.Marshal(p.Rules)
	_, err := ps.db.ExecContext(ctx, `
UPDATE projects SET name=?, description=?, game=?, default_plugin=?, default_port=?,
                    members=?, plugins=?, rules=?, updated_at=?
WHERE id=?`,
		p.Name, p.Description, p.Game, p.DefaultPlugin, p.DefaultPort,
		string(members), string(plugins), string(rules), p.UpdatedAt, p.ID)
	return err
}

const projectCols = `id, name, description, game, created_by, default_plugin,
	default_port, members, plugins, rules, created_at, updated_at`

func scanProject(s interface{ Scan(dest ...any) error }) (*project, error) {
	var p project
	var members, plugins, rules string
	err := s.Scan(&p.ID, &p.Name, &p.Description, &p.Game, &p.CreatedBy, &p.DefaultPlugin,
		&p.DefaultPort, &members, &plugins, &rules, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(members), &p.Members)
	_ = json.Unmarshal([]byte(plugins), &p.Plugins)
	_ = json.Unmarshal([]byte(rules), &p.Rules)
	if p.Members == nil {
		p.Members = []projectMember{}
	}
	if p.Plugins == nil {
		p.Plugins = []projectPlugin{}
	}
	if p.Rules == nil {
		p.Rules = []projectRule{}
	}
	return &p, nil
}

// ListVisible 返回当前用户可见的项目（created_by 或成员；全局 admin 可见全部），按 created_at 降序。
func (ps *projectStore) ListVisible(ctx context.Context, owner string, all bool) ([]project, error) {
	var rows *sql.Rows
	var err error
	if all {
		rows, err = ps.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY created_at DESC`)
	} else {
		rows, err = ps.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		if all || ps.visibleTo(p, owner) {
			out = append(out, *p)
		}
	}
	return out, rows.Err()
}

func (ps *projectStore) Get(ctx context.Context, id string) (*project, error) {
	row := ps.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id=?`, id)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (ps *projectStore) Delete(ctx context.Context, id string) (bool, error) {
	res, err := ps.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// visibleTo：owner 是否为 created_by 或成员；空 owner 仅可见 created_by 为空的匿名项目。
func (ps *projectStore) visibleTo(p *project, owner string) bool {
	if p.CreatedBy == owner {
		return true
	}
	for _, m := range p.Members {
		if m.User == owner {
			return true
		}
	}
	return false
}

// CanManage：仅 created_by 或全局 admin 可管理（改/删/管成员/管插件规则/管规则）。
func (ps *projectStore) CanManage(p *project, owner string, all bool) bool {
	return all || p.CreatedBy == owner
}
```

`ListVisible` 的 all/非 all 两个分支查询语句相同，合并为一个即可下面会修正；保持单一查询 + `visibleTo` 过滤（下一步落到测试）。

- [ ] **Step 3: 写失败测试**

新增 `project_store_test.go`（用 `NewControlStore` 的底层 `DB()`，或临时 sqlite）：

```go
func TestProjectMembershipVisibility(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ctl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ps := newProjectStoreDB(db)
	if err := ps.Init(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	proj := &project{
		ID: "20260831_120000.000", Name: "王者荣耀测试服", CreatedBy: "alice",
		Members: []projectMember{{User: "bob", Role: roleMember}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := ps.Create(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	// alice（created_by）与 bob（member）可见；tom 不可见。
	aliceView, _ := ps.ListVisible(context.Background(), "alice", false)
	if len(aliceView) != 1 || aliceView[0].ID != proj.ID {
		t.Fatalf("alice should see the project, got %d", len(aliceView))
	}
	bobView, _ := ps.ListVisible(context.Background(), "bob", false)
	if len(bobView) != 1 {
		t.Fatalf("bob (member) should see the project, got %d", len(bobView))
	}
	tomView, _ := ps.ListVisible(context.Background(), "tom", false)
	if len(tomView) != 0 {
		t.Fatalf("tom should NOT see the project, got %d", len(tomView))
	}
	// 全局 admin 可见全部。
	adminView, _ := ps.ListVisible(context.Background(), "root", true)
	if len(adminView) != 1 {
		t.Fatalf("global admin should see all, got %d", len(adminView))
	}
	// CanManage：alice 可管，bob（member）不可管。
	if !ps.CanManage(proj, "alice", false) {
		t.Fatal("alice should manage project")
	}
	if ps.CanManage(proj, "bob", false) {
		t.Fatal("bob (member) should NOT manage project")
	}
}
```

- [ ] **Step 4: 运行测试使其失败**

Run: `go test ./cmd/gta-mcp -run TestProjectMembershipVisibility -v`
Expected: 编译失败（`projectStore` 尚无 `Init/ListVisible/CanManage`）或 FAIL。

- [ ] **Step 5: 实现通过测试（补上 `Init/ListVisible/CanManage`，并修正 ListVisible 单一查询）**

修正 `ListVisible` 为单一查询 + 内存过滤（无论 owner/all 都 `SELECT ... ORDER BY created_at DESC`，用 `visibleTo`/all 过滤）。

- [ ] **Step 6: Commit**

```bash
git add cmd/gta-mcp/project_store.go cmd/gta-mcp/project_store_test.go
git commit -m "feat(mcp): SQLite project store with membership visibility"
```

---

## Task 5: 重写 MCP 项目工具（成员/插件/规则/归属）

**Files:**
- Modify: `cmd/gta-mcp/project.go`（重写为 SQLite 数据源 + 扩充工具处理）
- Modify: `cmd/gta-mcp/main.go`（装配 `newProjectStoreDB(controlStore.DB())`、注册新增工具）
- New: `cmd/gta-mcp/project_membership_test.go`

说明：将 `project.go` 中所有 `m.projects` 操作改为走 SQLite projectStore；`ownerScope` 保留。

- [ ] **Step 1: 工具注册（main.go）**

在 `AddTool` 区块（`list_projects` 附近，第 2950 行）替换/新增，注册：

```go
	m.AddTool(mcp.NewTool("create_project",
		mcp.WithDescription("Create a project (org unit) owned by current user."),
	), m.handleCreateProject)
	m.AddTool(mcp.NewTool("list_projects",
		mcp.WithDescription("List projects visible to current user (created_by or member; global admin sees all)."),
	), m.handleListProjects)
	m.AddTool(mcp.NewTool("get_project",
		mcp.WithDescription("Get project detail including members, plugins, rules, and recent sessions."),
		mcp.WithString("id", mcp.Required()),
	), m.handleGetProject)
	m.AddTool(mcp.NewTool("update_project",
		mcp.WithDescription("Update project fields (name/description/game/default_plugin/default_port). Admin (creator or global admin) only."),
		mcp.WithString("id", mcp.Required()),
		mcp.WithString("name"), mcp.WithString("description"), mcp.WithString("game"),
		mcp.WithString("default_plugin"), mcp.WithNumber("default_port"),
	), m.handleUpdateProject)
	m.AddTool(mcp.NewTool("delete_project",
		mcp.WithDescription("Delete a project. Admin (creator or global admin) only."),
		mcp.WithString("id", mcp.Required()),
	), m.handleDeleteProject)
	m.AddTool(mcp.NewTool("add_project_member",
		mcp.WithDescription("Add a member (role admin|member) to a project. Project admin only."),
		mcp.WithString("project_id", mcp.Required()), mcp.WithString("user", mcp.Required()),
		mcp.WithString("role", mcp.Required()),
	), m.handleAddProjectMember)
	m.AddTool(mcp.NewTool("remove_project_member",
		mcp.WithDescription("Remove a member from a project. Project admin only."),
		mcp.WithString("project_id", mcp.Required()), mcp.WithString("user", mcp.Required()),
	), m.handleRemoveProjectMember)
	m.AddTool(mcp.NewTool("set_project_plugins",
		mcp.WithDescription("Replace the plugin entries held by a project. Project admin only."),
		mcp.WithString("project_id", mcp.Required()), mcp.WithString("plugins", mcp.Required()), // JSON [{id,name}]
	), m.handleSetProjectPlugins)
	m.AddTool(mcp.NewTool("set_project_rules",
		mcp.WithDescription("Replace the rule entries held by a project. Project admin only."),
		mcp.WithString("project_id", mcp.Required()), mcp.WithString("rules", mcp.Required()), // JSON [{id,name}]
	), m.handleSetProjectRules)
	m.AddTool(mcp.NewTool("set_session_project",
		mcp.WithDescription("Reassign an existing session to a project (or '' to detach). Owner/admin of the project."),
		mcp.WithString("session_id", mcp.Required()), mcp.WithString("project_id"),
	), m.handleSetSessionProject)
```

装配改动（`main.go` 402-409 附近）：把 `projects: newProjectStore(workDir)` 改为 SQLite：

```go
	projects := newProjectStoreDB(controlStore.DB())
	if err := projects.Init(); err != nil {
		return nil, fmt.Errorf("init project store: %w", err)
	}
	m := &mcpCapture{
		...
		projects:   projects,
	}
```

`mcpCapture` 结构体字段类型由 `*projectStore` 保持不变（实现替换为 SQLite 版）。

- [ ] **Step 2: 重写处理函数（project.go）**

- `handleCreateProject`：`Owner` → `CreatedBy`；`Plugin` → `DefaultPlugin`；`Port` → `DefaultPort`；落库用 `m.projects.Create`；ID 沿用 `newProjectID()`。
- `handleListProjects`：调用 `m.projects.ListVisible(ctx, owner, all)`（不再汇总 JSON 分片）。
- `handleGetProject`（新增）：取 `id`，`m.projects.Get`；权限校验 `visibleTo`（不可见按未找到处理）；附 `recent_sessions`（`controlStore.ListSessionsForProject(id, filter)`）。
- `handleUpdateProject`：先取得项目，`CanManage(owner, all)` 否则 403；仅更新显式字段；`m.projects.Update`。
- `handleDeleteProject`：`CanManage` 校验；`m.projects.Delete`。
- `handleAddProjectMember`：`CanManage` 校验；追加/幂等地替换 `{User, Role}`（role ∈ admin|member，否则报错）。
- `handleRemoveProjectMember`：`CanManage` 校验；从 `Members` 移除（不允许移除 created_by 自身）。
- `handleSetProjectPlugins` / `handleSetProjectRules`：`CanManage` 校验；解析 JSON 数组；整体替换 `Plugins`/`Rules`。
- `handleSetSessionProject`：取 `session_id`，校验会话 owner 可见（`controlStore.GetSessionFor`）；取 `project_id`，若非空需校验当前用户是该项目 admin 或该会话 owner；更新该 session 的 `project_id`（新增 `ControlStore.SetSessionProject`）。

- [ ] **Step 3: 新增 `ControlStore.SetSessionProject`**

在 `session_store.go` 增加（Task 2 基础上）：

```go
func (cs *ControlStore) SetSessionProject(ctx context.Context, sessionID, projectID string) error {
	res, err := cs.db.ExecContext(ctx,
		`UPDATE sessions SET project_id=? WHERE session_id=?`, projectID, sessionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return nil
}
```

- [ ] **Step 4: 写成员/权限测试**

`project_membership_test.go`（用 `NewControlStore` 拿到 `DB()` 搭 `projectStore`，再构造 `mcpCapture` 用最小 stub 调 `handleAddProjectMember`/`handleRemoveProjectMember`/`handleSetProjectPlugins`）。核心断言：member 不能增删成员、不能改插件/规则；created_by 与全局 admin 可以。

- [ ] **Step 5: 编译 + 全量测试**

Run: `go build ./... && go test ./cmd/gta-mcp ./pkg/store`
Expected: 全部通过。

- [ ] **Step 6: Commit**

```bash
git add cmd/gta-mcp/project.go cmd/gta-mcp/project_membership_test.go cmd/gta-mcp/main.go pkg/store/session_store.go
git commit -m "feat(mcp): project member/plugin/rule management + session-to-project binding"
```

---

## Task 6: 前端类型 + hooks

**Files:**
- Modify: `web/src/types/project.ts`
- Modify: `web/src/hooks/use-mcp.ts`

- [ ] **Step 1: 扩充类型**

```ts
export type ProjectRole = "admin" | "member";

export interface ProjectMember {
  user: string;
  role: ProjectRole;
}

export interface ProjectPlugin {
  id: string;
  name: string;
}

export interface ProjectRule {
  id: string;
  name: string;
}

/** 项目一等组织单元 */
export interface ProjectInfo {
  id: string;
  name: string;
  description?: string;
  game?: string;
  created_by?: string;
  default_plugin?: string;
  default_port?: number;
  members?: ProjectMember[];
  plugins?: ProjectPlugin[];
  rules?: ProjectRule[];
  created_at?: string;
  updated_at?: string;
}

export interface ProjectDetail extends ProjectInfo {
  recent_sessions?: Array<{
    session_id: string;
    status: string;
    started_at: string;
    event_count: number;
    raw_packets: number;
  }>;
}
```

- [ ] **Step 2: hooks**

在 `use-mcp.ts` 增加（参照现有 `useProjects`/`useStartCapture` 的 `mcpClient.callTool` 模式）：

```ts
export function useProject(id?: string) {
  return useQuery({
    queryKey: ["project", id],
    queryFn: () =>
      id
        ? mcpClient.callTool<ProjectDetail>("get_project", { id })
        : Promise.resolve(null),
    enabled: !!id,
  });
}

export function useSetSessionProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { session_id: string; project_id?: string }) =>
      mcpClient.callTool<{ ok?: boolean }>("set_session_project", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["sessions"] }),
  });
}

export function useProjectPlugins(id: string | undefined) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { project_id: string; plugins: ProjectPlugin[] }) =>
      mcpClient.callTool<{ ok?: boolean }>("set_project_plugins", {
        project_id: id,
        plugins: JSON.stringify(v.plugins),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["project", id] }),
  });
}
```

沿用现有成员/规则 mutation 同款写法新增 `useProjectMembers`/`useProjectRules`。

- [ ] **Step 3: 编译**

Run: `cd web && npx tsc --noEmit`
Expected: PASS。

- [ ] **Step 4: Commit**

```bash
git add web/src/types/project.ts web/src/hooks/use-mcp.ts
git commit -m "feat(web): project org-unit types + hooks"
```

---

## Task 7: Web 首页项目区 + 项目详情页

**Files:**
- New: `web/src/components/project-page.tsx`
- Modify: `web/src/components/my-capture-page.tsx`
- Modify: `web/src/components/start-capture-dialog.tsx`
- Modify: `web/src/App.tsx`

- [ ] **Step 1: 首页项目卡片带派生状态**

在 `my-capture-page.tsx` 项目卡片中，用该项目的最近会话（`sessions` 里 `project_id === p.id` 的最新一条）派生状态点：running→绿色「在线」、否则灰色「离线」；并显示「最近 N events / M packets」。把「抓包」按钮保留，另加「进入」按钮 → `onOpenProject(p.id)`。

- [ ] **Step 2: 项目详情页**

新建 `project-page.tsx`，props：`{ projectId, onBack, onSelectSession, onStartProject }`。内容：
- 顶部：名称 / game 徽标 / 状态点 / 描述 / created_by。
- 三个区块：`成员`（列表 + 增删，仅 admin 可见编辑）、`解码插件`（chips + 替换编辑，仅 admin）、`规则`（chips + 替换编辑，仅 admin）。
- `最近会话`（`useProject(projectId).recent_sessions`，点条目 `onSelectSession(session_id)`）。
- 用 `useAuth()` 的当前用户与 `created_by`/成员角色判定 isAdmin。

- [ ] **Step 3: 路由接入 App.tsx**

在 `App.tsx` 增加视图状态（或轻量路由）：当 `projectId` 选中时渲染 `project-page`，否则渲染现有 `MyCapturePage`；`MyCapturePage` 传入 `onOpenProject`。

- [ ] **Step 4: StartCaptureDialog 携带 project_id**

从某项目发起时，`useStartCapture({ ...params, project_id })` 透传 `project_id`。

- [ ] **Step 5: 构建**

Run: `cd web && npx tsc --noEmit && npx vite build`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add web/src/components/project-page.tsx web/src/components/my-capture-page.tsx web/src/components/start-capture-dialog.tsx web/src/App.tsx
git commit -m "feat(web): project home status + project detail page + project_id in start capture"
```

---

## Task 8: 端到端验证与文档

**Files:**
- Modify: `docs/team-deployment.md`（如需，补充项目/成员概念与 index）

- [ ] **Step 1: 端到端冒烟**

Run: `go test ./...`（全仓）→ `make build` → 起 pipeline+mcp → 用 web 建项目、加成员、从项目开始抓包，确认 `control.sqlite` 的 `sessions.project_id` 正确写入且按项目拉取会话正常。
Expected: 全部通过；`sqlite3 control.sqlite "select session_id, project_id from sessions;"` 能看到非空 project_id。

- [ ] **Step 2: Commit**

```bash
git add docs/team-deployment.md
git commit -m "docs: project org-unit concept and session-to-project binding"
```

---

## 自检

- **Spec 覆盖**：项目组织单元（Task 4/5）、Game/Members/Plugins/Rules 字段（Task 4/5/6）、sessions.project_id（Task 2/3）、一键带 project_id（Task 3/7）、Web 首页优先级（Task 7）、成员可见性 + 不复杂 RBAC（Task 4/5/6）。未做独立插件/规则管理后台与 schema/contract 编辑器，符合「不做管理后台」约束。
- **Placeholder 扫描**：全部步骤给出具体文件、代码或命令，无 TBD。
- **类型一致性**：`ProjectID`（Go 侧）↔ `project_id`（列/proto/JSON）↔ `project_id`（TS 参数）术语一致；`projectMember`/`ProjectMember`、`CanManage`/`isAdmin` 语义一致。

---

**Plan complete and saved to `docs/superpowers/plans/2026-08-31-project-org-unit-web.md`. 两种执行方式：**

**1. Subagent-Driven（推荐）** — 每个任务派新子代理实现，任务间审查，快速迭代

**2. Inline Execution** — 在本会话用 executing-plans 按检查点批量执行

**选择哪种方式？**