# Tenant → Project → Session 归属与鉴权重构方案（已执行）

> 状态：**已按本方案实施（2026-09-05，决策 D1-D8 均按"我的建议"落地）**。
> 决策记录：D1=A 硬边界；D2=`''` 保持；D3=owner 字段 SSOT；D4=只落字段+比较；
> D5=projectStore 不挪包；D6=plugins/rules 暂不表化；D7=新增 access_code Action；D8=先 slog。

---

## 0. 现状盘点（读代码所得，非推测）

### 0.1 数据落点

| 对象 | 位置 | 备注 |
|---|---|---|
| `projects` 表 | `control.sqlite`（SQLite） | `cmd/gta-mcp/project_store.go:66`，**MCP 进程独占** |
| `sessions` 表 | `control.sqlite` + PG（`pg_schema.go:123`） | 双后端，均有 `project_id TEXT NOT NULL DEFAULT ''` |
| 会话元数据副本 | `sessions/<id>/metadata.json` | `cmd/gta-mcp/main.go:286` listSessions 走文件系统 |

关键事实：**PG 后端没有 `projects` 表**。`projectStore` 活在 `cmd/gta-mcp`（package main），不在 `pkg/store`。

### 0.2 实测数据量（2026-09-05）

```
control.sqlite: sessions=8, projects=0, access_codes=0, plugin_debug_access=0
```

→ 项目/成员/启动码**零数据**，schema 改造无历史包袱，是本方案最好的执行窗口。

### 0.3 权限判定点全清单（共 9 处）

| # | 判定点 | 位置 | 作用域 |
|---|---|---|---|
| 1 | `ownerScope()` | `project.go:28` | 取 (owner, isAdmin) |
| 2 | `sessionFilterForOwner()` | `project.go:36` | → `store.SessionOwnerFilter` |
| 3 | `ownerFilterFromCtx()` | `main.go:81` | **与 #2 完全同义的重复实现** |
| 4 | `projectForEdit()` | `project.go:55` | 项目可管理性（调 CanManage） |
| 5 | `CanManage()` | `project_store.go:189` | `all \|\| created_by \|\| member.role==admin` |
| 6 | `visibleTo()` | `project_store.go:173` | `created_by \|\| 任一 member` |
| 7 | `authorizeSession()` | `main.go:2234` | 会话归属，controlStore + metadata.json 双源 |
| 8 | `SessionOwnerFilter.Matches` | `pkg/store/eventstore.go:104` | 纯 owner 相等比较 |
| 9 | **前端 `project-page.tsx:69`** | web | 前端自己算 `members[].role==="admin"` |

### 0.4 无鉴权的 Store 方法（你说的对，确认存在）

`pkg/store/session_store.go`：`GetSession()`(200) / `UpdateSession()`(274) / `DeleteSession()`(308) / `SetSessionProject()`(331) —— 全部只按 `session_id` 匹配，无 owner 无 project 条件。

### 0.5 文档与代码冲突（确认）

`docs/team-deployment.md:100`：
> 管理权：仅项目 `created_by` 或全局 `:admin` 可改/删项目、管理成员与插件/规则

`project_store.go:189` 实际允许 **project admin 成员**管理项目。文档偏严、代码偏松。

### 0.6 顺手发现的两个隐患（你未列，但影响本方案）

- **A. `authorizeSession` 兜底放行**（`main.go:2258`）：controlStore 查不到且 metadata.json 读不到时 `return nil`（放行）。本意是兼容 workDir 漂移，实际是一条越权兜底路径。
- **B. `/access/claim` 吐真实 token**（`access_code.go:221`）：未鉴权端点，凭一个 6 位短码即可换取该 owner 的**静态长期凭证**并自动开会话。码是一次性的，但泄露面 = 长期 token。你列的 Action 清单里没有 access_code 这一类。

---

## 1. 问题一：Project / Session 权限边界不一致

### 1.1 现状（确认属实）

`handleGetProject`（`project.go:114-130`）先 `visibleTo(p, owner)`，再 `ListSessionsForProject(ctx, id, sessionFilterForOwner(ctx))`。
结果：**项目成员能打开项目，但看不到项目里别人的会话**。Project 实际上不是协作边界，只是个"标签夹"。

### 1.2 两个可选语义

| | 语义 A：Project 是硬边界（推荐） | 语义 B：双层 + 可见性开关 |
|---|---|---|
| `project_id != ''` 的会话 | 可见性完全由 Project 决定：成员即可见可用 | 仍叠加 owner 过滤，由 `project.session_visibility` 字段切换 |
| `project_id = ''` 的会话 | 个人私有，仅 creator / global admin | 同 |
| 心智模型 | 一句话：进了项目就是共享的 | 需要为每个项目解释一个开关 |
| 代码复杂度 | 低：一条规则 | 高：每个查询多一个分支 |
| 后果 | 抓包数据在项目内对成员可见（需要接受） | 灵活，但"默认不可见"下项目依旧不是协作单元 |

**推荐 A。** 理由：B 的默认值如果是 private，等于维持现状（问题没解决）；如果是 shared，就是 A 多一个开关。既然 `projects` 表 0 行、没有历史承诺，直接钉死 A，不引入配置项。

### 1.3 落地要点

1. `ListSessionsForProject` 去掉 owner 过滤，改为纯 `project_id` 过滤；调用方必须先 `Can(ActionProjectRead, project)`。
2. **文件系统侧也要改**（容易漏）：`sessionManager.listSessions`（`main.go:286`）按 `metadata.json` 的 `Owner` 过滤，`list_all_sessions` 走这条路。项目成员看不到项目会话 → 前端"我的抓包"和"项目详情"两张页面会给出矛盾结果。
   - 做法：给 `SessionOwnerFilter` 加一个字段 `ProjectIDs []string`（"属于这些项目的会话同样可见"），SQL 侧拼 `OR project_id IN (...)`，`Matches` 侧同样处理。`pkg/store` 只看到 id 列表，不需要知道 project 的存在。
3. `authorizeSession`（`main.go:2234`）判定改为：`project_id != ''` → 走 project 鉴权；`project_id == ''` → 走 owner 鉴权。同时**删掉 2258 的兜底放行**（隐患 A）。

---

## 2. 问题二：成员 JSON → 表

### 2.1 拆什么

| JSON 列 | 是否表化 | 理由 |
|---|---|---|
| `members` | **是** | 鉴权热路径，每次 `Can()` 都要查；需要 `(project_id,user)` 唯一约束与索引 |
| `plugins` | 暂否 | 纯展示关联，无查询/鉴权需求 |
| `rules` | 暂否 | 同上 |

`ponytail:` 只把进鉴权路径的那份数据表化，另外两份保持 JSON，等真出现"按 plugin 反查项目"的需求再拆。

### 2.2 Schema

```sql
CREATE TABLE IF NOT EXISTS project_members (
    project_id TEXT NOT NULL,
    user       TEXT NOT NULL,
    role       TEXT NOT NULL CHECK(role IN ('admin','member')),
    created_at DATETIME NOT NULL,
    PRIMARY KEY (project_id, user)
);
CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user);
```

### 2.3 迁移策略（幂等、可回滚）

1. `migrateProjectMembersTable()`：建表。
2. 读 `projects.members` JSON → `INSERT OR IGNORE INTO project_members`，回填 `created_at = projects.created_at`。
3. **保留 `projects.members` JSON 列不删**，双写一个版本（读走表，写同时写表和 JSON），作为回滚备份。
4. 下一个版本确认无问题后 `DROP` 备份列（SQLite 3.35+ 支持 `ALTER TABLE DROP COLUMN`；PG 原生支持）。
5. `projects.owner` 不进 members 表（见 §4.3）。

---

## 3. 问题三：轻量 AuthZ 层

### 3.1 对你提议的两处修正

**修正 1：`Resource` 用引用，不用实体。**
`Can(ctx, action, session)` 传实体意味着调用方必须先 load 出来才能鉴权 —— 对 `list_projects` / `list_all_sessions` 这类"先过滤后展示"的场景不成立（拿不到实体就得先查全量，等于绕过鉴权）。改为引用式描述：

```go
package authz

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
    ActionSessionUse           Action = "session:use"      // 启停、读数据
    ActionSessionDelete        Action = "session:delete"
    ActionSessionMoveProject   Action = "session:move_project"
    ActionPluginRead           Action = "plugin:read"
    ActionPluginUse            Action = "plugin:use"
    ActionPluginManage         Action = "plugin:manage"
    ActionLeaseUse             Action = "lease:use"
    ActionLeaseRelease         Action = "lease:release"
    ActionAccessCodeCreate     Action = "access_code:create"
    ActionAccessCodeClaim      Action = "access_code:claim"
)

type Kind string
const (KindProject Kind = "project"; KindSession; KindPlugin; KindLease; KindAccessCode)

type Resource struct {
    Kind      Kind
    ID        string
    TenantID  string // 缺省 "default"
    ProjectID string // session 归属的项目，'' = 个人会话
    CreatorID string // 资源创建者（session 的 owner / project 的 created_by）
}

type Authorizer interface {
    Can(ctx context.Context, a Action, r Resource) error
}
```

**修正 2：规则与数据分离，避免包循环。**
`authz` 包放**纯策略**（不 import store、不查 DB）；`cmd/gta-mcp` 里的实现负责解析 role。这样规则 100% 可单测，且 `authz` 不依赖任何存储。

```go
// pkg/authz/policy.go —— 纯函数，无 IO
type Role int
const (RoleNone Role = iota; RoleMember; RoleAdmin; RoleOwner)

type Principal struct {
    User    string
    Tenant  string
    IsAdmin bool
}

func Decide(p Principal, a Action, r Resource, role Role) error
```

```go
// cmd/gta-mcp/authz.go —— 组合 projectStore，只做 role 解析
type projectAuthorizer struct{ projects *projectStore }

func (a *projectAuthorizer) Can(ctx context.Context, act authz.Action, res authz.Resource) error {
    p, _ := authz.PrincipalFrom(ctx)
    if res.TenantID != "" && p.Tenant != "" && res.TenantID != p.Tenant {
        return fmt.Errorf("forbidden: tenant mismatch")
    }
    role := authz.RoleNone
    if res.ProjectID != "" {
        role = a.projects.RoleOf(ctx, res.ProjectID, p.User) // 查 project_members；owner 返回 RoleOwner
    }
    return authz.Decide(p, act, res, role)
}
```

**不做**中间件自动拦截（需要反射/注册机制，属于过度设计）。收口的是**规则**，不是调用点 —— handler 第一行一行显式调用：

```go
if err := m.authz.Can(ctx, authz.ActionSessionRead, res); err != nil {
    return errorResult(err), nil
}
```

### 3.2 权限矩阵（策略表，即 `Decide` 的实现依据）

| Action | GlobalAdmin | Project Owner | Project Admin | Project Member | 个人会话 creator |
|---|:--:|:--:|:--:|:--:|:--:|
| project:read | ✓ | ✓ | ✓ | ✓ | – |
| project:update | ✓ | ✓ | ✗ | ✗ | – |
| project:delete | ✓ | ✓ | ✗ | ✗ | – |
| project:manage_members | ✓ | ✓ | ✓ | ✗ | – |
| project:manage_plugins | ✓ | ✓ | ✓ | ✗ | – |
| project:manage_rules | ✓ | ✓ | ✓ | ✗ | – |
| project:transfer_owner | ✓ | ✓ | ✗ | ✗ | – |
| session:read（项目会话） | ✓ | ✓ | ✓ | ✓ | – |
| session:use | ✓ | ✓ | ✓ | ✓ | – |
| session:delete | ✓ | ✓ | ✓ | 仅自己创建的 | ✓ |
| session:move_project | ✓ | ✓ | ✓ | 源：仅自己创建的；目标：成员即可 | ✓ |
| session:*（个人会话，`project_id=''`） | ✓ | – | – | – | ✓ |
| plugin / lease / access_code | ✓ | 项目内 ✓ | 项目内 ✓ | 项目内 ✓ | ✓ |

Actor 层级：`Global Admin → Project Owner → Project Admin → Project Member`，外加"个人资源 creator"这条独立轴（不属于项目层级）。

### 3.3 防"新 handler 忘记鉴权"的真正机制

AuthZ 层本身防不住遗忘。加一个 AST 静态护栏测试（`cmd/gta-mcp/authz_guard_test.go`，约 60 行）：

1. `go/parser` 扫描本包所有 `func (m *mcpCapture) handleXxx`；
2. 命中敏感工具名集合（`*session*`、`*project*`、`*lease*`、`*plugin*`、`*access_code*`）的函数，函数体内必须出现 `authz.Can` / `.Can(` 调用；
3. 白名单放行已确认无需鉴权的（如 `list_interfaces`）；
4. 未命中 → `t.Errorf("handler %s 缺少 authz 鉴权调用")`。

这条测试比 AuthZ 接口本身更有价值 —— 它把"靠人记得"变成"CI 报错"。

---

## 4. 问题四：Action 细化 / Role 钉死 / Owner 转移 / Tenant

### 4.1 `CanManage` 拆分

`CanManage` 保留一个版本作为 deprecated 包装（内部转 `ActionProjectManageMembers`），供既有测试过渡，随后删除。新增按 Action 调用（矩阵见 §3.2）。

### 4.2 Role 语义钉死

| Role | 来源 | 能力 |
|---|---|---|
| **Owner** | `projects.owner` 字段（当前 Owner） | 全部，含删除项目、转移 Owner |
| **Admin** | `project_members.role='admin'` | 管成员 / 插件 / 规则 / 项目内会话 |
| **Member** | `project_members.role='member'` | 使用项目、读数据、操作自己创建的会话 |

`created_by` **语义退化为审计字段**（谁建的，永不变更），不再参与鉴权。

### 4.3 Owner 的 SSOT（避免双源）

**Owner 只存 `projects.owner` 字段，`project_members` 表只存 admin/member。**
这样"一个项目恰好一个 Owner"由 schema 保证，不存在 members 表里同时出现两个 owner 或"表里说 owner 是 A、字段说 owner 是 B"的双源问题。

```sql
ALTER TABLE projects ADD COLUMN owner     TEXT NOT NULL DEFAULT '';      -- 回填 = created_by
ALTER TABLE projects ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
```

- `RoleOf()` 返回 Owner 的条件：`p.Owner == user`（优先于查 members 表）。
- `remove_project_member` 的"禁止移除创建者"改为"禁止移除 Owner"。
- 转移 Owner 是**独立工具** `transfer_project_owner`，校验：caller ∈ {global admin, 当前 owner}；目标用户必须是本项目 admin/member（不允许把项目转给项目外的人）；原子 `UPDATE projects SET owner=?, updated_at=? WHERE id=? AND owner=?`（带旧值 CAS，防并发双转）。

### 4.4 Tenant：分两步，本次只落字段

同意你的判断 —— `owner` 是"人"不是"组织"，同一个 Alice 无法分属 Company A / B。但现在**没有任何 tenant 来源**：`GTA_AUTH_TOKENS` 格式是 `owner=token[:admin]`，token 里不带组织信息。`pkg/auth.Principal` 只有 `Owner` + `IsAdmin`。

建议：

| | 本次做 | 本次不做 |
|---|---|---|
| schema | `projects.tenant_id` / `sessions.tenant_id`，默认 `'default'` | – |
| 代码 | `Principal.Tenant`（默认 `""` = 单租户通配）；`Decide` 里加 tenant 一致性比较 | tenant 实体表、组织成员管理 |
| 校验 | 跨项目/跨会话操作时 tenant 必须相等（空值视作 default） | 多 tenant 路由、跨组织邀请 |

理由：表现在是空的，写 `tenant_id` 的成本 ≈ 0；等第二次迁移时，每张表、每条 SQL、每个鉴权点都要重写。而 tenant 实体没有真实需求，现在造就是假抽象。

`ponytail:` 字段先落、比较逻辑先有、实体后补。今天不加，明天的迁移成本是今天的 20 倍。

---

## 5. 问题五：Session 模型与 `move_session_to_project`

### 5.1 Schema 决策：`project_id` 用 `''` 还是 `NULL`

你写的是 nullable。我的建议是**保持 `NOT NULL DEFAULT ''`，语义上 `''` ≡ NULL**：

- SQLite 不允许 `ALTER COLUMN` 改约束，改成 nullable 需要**重建整表**（建新表 → 拷数据 → 删旧表 → 改名）；PG 虽有 `projects` 缺失的问题，但 sessions 列也是 `NOT NULL DEFAULT ''`，两边行为不一致。
- `NOT NULL` 下"是否归属项目"就一个表达式 `project_id = ''`，不存在 `IS NULL OR = ''` 的三态判断，PG/SQLite 行为完全一致。
- 现有 8 行数据全部 `project_id=''`，重建表是纯风险无收益。

如果坚持 NULL：迁移脚本是"建新表 + 双写兼容读"，8 行数据下也安全，但要多写约 40 行迁移代码和一个版本的数据校验。**这条需要你拍板。**

### 5.2 字段对齐

```sql
ALTER TABLE sessions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
```

`created_by`：建议**沿用现有 `owner` 列**，不改名。理由：`sessions.owner` 与 `metadata.json` 的 `Owner` 字段是一对镜像，改名要同步文件系统格式、PG schema、所有 SQL 与结构体，收益为 0。语义在文档里钉死：

> `sessions.owner` = 创建者 = 归属者。对**项目会话**，它退化为审计字段（谁抓的），不参与可见性判定；对**个人会话**(`project_id=''`)，它是唯一权限依据。

### 5.3 `move_session_to_project`

`SetSessionProject` 下沉为包内私有（或加 `//deprecated: 仅内部使用`），新增 service 方法统一收口：

```go
func (m *mcpCapture) moveSessionToProject(ctx, sessionID, targetProjectID string) error {
    // 1. session 存在（按可见性 loaded）
    // 2. Can(ActionSessionMoveProject, session)
    // 3. target project 存在
    // 4. Can(ActionProjectRead | ActionSessionUse, targetProject) —— target 需成员级
    // 5. tenant 一致（session.tenant_id == project.tenant_id）
    // 6. 原子更新：UPDATE sessions SET project_id=?, tenant_id=? WHERE session_id=? AND tenant_id=?
    //    + 同步 metadata.json（失败需回滚或明确补偿，现状是只 Warn）
}
```

工具侧：新增 `move_session_to_project`；`set_session_project` 保留为 deprecated 别名并转调新方法（前端 `web/src/hooks/use-mcp.ts:478` 在用它，改名要同步）。

`UpdateSession` / `DeleteSession` / `GetSession` 三个裸方法：保留但加 doc 注释标注"仅限内部/已鉴权路径调用"，由 `authz_guard_test.go` 保证 handler 侧不绕过。

---

## 6. 需要你拍板的决策点

| # | 决策 | 我的建议 | 备选 |
|---|---|---|---|
| D1 | Session 可见性语义 | **A：Project 硬边界，成员可见项目内全部会话** | B：双层 + `session_visibility` 开关 |
| D2 | `sessions.project_id` | **保持 `NOT NULL DEFAULT ''`，`''` ≡ NULL** | 改 nullable（需重建表） |
| D3 | Owner 存储 | **`projects.owner` 字段，members 表只存 admin/member** | members 表存 `role='owner'` |
| D4 | Tenant | **本次只落字段 + 比较逻辑，不做实体** | 一并做 organization 表 |
| D5 | `projectStore` 位置 | **不动，留在 `cmd/gta-mcp`**（PG 无 projects 表，抽象不成立） | 挪到 `pkg/store` 并补 PG |
| D6 | plugins / rules | **暂不表化** | 一并拆表 |
| D7 | access_code Action | **加 `ActionAccessCodeCreate/Claim`**（`/access/claim` 会吐长期 token，属高敏感） | 维持现状 |
| D8 | Owner 转移留痕 | **先 slog，不建审计表**（目前无消费方，落表即死数据） | 建 `project_audit` 表 |

---

## 7. 执行分期

| 阶段 | 内容 | 依赖 |
|---|---|---|
| **Stage A — 数据模型** | §2 成员表 + 迁移双写；§4.2/4.3 owner/tenant 字段；§5.2 sessions.tenant_id | 无。纯 schema + store，可独立提测 |
| **Stage B — AuthZ** | §3 `pkg/authz` 策略包 + `projectAuthorizer` + 矩阵 + 护栏测试；§1 边界收口（含文件系统侧 `ProjectIDs`）；§5.3 `moveSessionToProject`；删 `authorizeSession` 兜底放行 | Stage A |
| **Stage C — 对齐** | 前端改为消费后端返回的 `capabilities`（消除 `project-page.tsx:69` 的本地判断）；`docs/team-deployment.md:100` 改为"owner/global admin 可删项目与转移 Owner；project admin 可管成员/插件/规则"；`use-mcp.ts:478` 切 `move_session_to_project` | Stage B |

**不做（P2）**：tenant 实体与跨组织成员、plugins/rules 表化、Casbin/OPA/RBAC 框架。

---

## 8. 验收清单

- [ ] `pkg/authz` 策略表 100% 表驱动单测覆盖（纯函数，无 DB）：每个 Action × 每个 Role × 全局 admin 的组合
- [ ] `authz_guard_test.go`：故意新增一个未鉴权的敏感 handler → 测试必须失败
- [ ] 既有 `project_store_test.go` / `project_membership_test.go` 从 `CanManage` 迁到 `Can`，全部通过
- [ ] 迁移幂等性测试：同一 control.sqlite 连续 `Init()` 两次，成员不重复、不丢失
- [ ] 边界用例：member 能看到项目内他人会话；非成员看不到；个人会话仍仅 creator 可见
- [ ] `move_session_to_project`：源会话无权限 / 目标项目非成员 / tenant 不一致 → 三种拒绝路径各有测试
- [ ] 手工回归：`docs/team-deployment.md:157` 的验收清单跑一遍
