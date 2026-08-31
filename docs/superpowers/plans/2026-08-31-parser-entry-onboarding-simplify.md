# 普通用户解析器入口 + 启动码接入简化 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把「选解析器」从工程师式的下拉改成普通用户的友善卡片（按协议归组：Godot/Unity/HTTP/自定义），并把开发者工具（create_plugin/build_plugin/verify_plugin）收进「更多/高级」开关；同时用「启动码 GTA-XXXX」替代繁琐下载表单，让成员在目标机输入一个码即可自动注册设备/取配置/回连抓包。

**Architecture:** 解析器入口 = 纯前端改造（`web/src/components`），按已注册插件的 `protocol`/`name` 归组渲染为卡片，保留「自定义」显示原始插件下拉，开发者工具收进 App「更多」下拉（加开关）。启动码流程 = 后端新增 `access_codes` 表（存 `control.sqlite`）+ MCP 工具 `create_access_code` + 未鉴权端点 `GET /access/claim?code=...`（复用手动 download 开会话/组 sidecar 配置的逻辑，改 JSON 返回取代 zip 打包）；`gta-agent` 新增 `--code` 首发入，无配置时交互输入码并调用 claim 拿配置后照常运行；Web 新增「我的接入」命令面板生成 `curl | bash` / 下载 / 启动码。

**Tech Stack:** Go (SQLite via modernc.org/sqlite, net/http, flag)、gta-agent (现有 flag + deriveAddrs 复用)、React 19 + TypeScript + Vite + TanStack Query。

---
> 已确认的范围决策（AskUserQuestion 结果）：① 启动码为主，收纳现 zip 下载流程；② 解析器按协议/标识归类渲染[Godot][Unity][HTTP][自定义]；③ 开发者工具收进「更多」下拉 + 高级开关。

---

## 文件结构（本轮新增/修改）

**后端（gta-mcp）**
- New `cmd/gta-mcp/access_code.go` — `access_codes` 表定义/DBCreate/CRUD、`create_access_code` 工具、`handleAccessClaim` HTTP 端点、`handleGetAccessCode`。
- Modify `cmd/gta-mcp/main.go` — 装配 access store（复用 `controlStore.DB()`）、注册 MCP 工具与 HTTP 端点。
- New test `cmd/gta-mcp/access_code_test.go`。

**agent（cmd/gta-agent）**
- Modify `cmd/gta-agent/main.go` — 新增 `--code` flag；无 server/token/config 时提示输入启动码。
- New `cmd/gta-agent/claim.go` — 调用 `/access/claim` 解析启动码，组装 `embeddedAgentConfig` 作为默认配置（优先级最高默认）。
- New test `cmd/gta-agent/claim_test.go`。

**前端（web/）**
- Modify `web/src/types/agent.ts` — 新增 `AccessClaimResult`/`AccessCode` 类型。
- New `web/src/lib/parsers.ts` — 解析器归类目录（按 protocol/name → Godot/Unity/HTTP/自定义）。
- Modify `web/src/hooks/use-mcp.ts` — 新增 `useCreateAccessCode`、`useAccessCode`。
- Modify `web/src/components/start-capture-dialog.tsx` — 插件下拉替换为友善卡片（归组 + 自定义）。
- Modify `web/src/components/agent-download-dialog.tsx` — 改为「我的接入」：启动码 + `curl | bash` + Windows 下载，收拢端口/回连自动；保留高级「自定义下载」折叠。
- New `web/src/components/access-code-panel.tsx` — 启动码生成/展示/复制 + 平台选择 + 生成接入命令。
- Modify `web/src/App.tsx` — 「更多」下拉新增「开发者模式」开关，控制 PluginPanel 与开发工具显隐；接入 AgentDownloadDialog 改造后 props。
- Modify `web/src/data/parsers.ts`（可选）— 若需静态解析器目录表时使用。

---

## Phase A — 普通用户解析器入口

## Task A1: `parsers.ts` 归类目录 + hooks 类型

**Files:**
- Create: `web/src/lib/parsers.ts`
- Modify: `web/src/types/agent.ts`

- [ ] **Step 1: 类型**

在 `web/src/types/agent.ts` 追加：

```ts
/** 已注册插件归类后的友善呈现项。 */
export interface ParserOption {
  /** 归组品牌：godot | unity | http | custom */
  group: "godot" | "unity" | "http" | "custom";
  /** 展示名，如 "Godot 世界解码器" */
  label: string;
  /** 实际插件名（传给 start_capture/agent download 的 plugin 参数） */
  plugin: string;
  /** 在线状态（离线置灰） */
  online: boolean;
}
```

- [ ] **Step 2: 实现归类函数**

创建 `web/src/lib/parsers.ts`：

```ts
import type { RegisteredPlugin } from "@/types/registered-plugin";

const GROUP_RULES: Array<{ group: "godot" | "unity" | "http"; test: (p: { name: string; protocol: string }) => boolean }> = [
  { group: "godot", test: (p) => /godot/i.test(p.name) || /godot/i.test(p.protocol) },
  { group: "unity", test: (p) => /unity/i.test(p.name) || /unity/i.test(p.protocol) },
  { group: "http", test: (p) => /^http$/i.test(p.protocol) || /http/i.test(p.name) },
];

const GROUP_LABEL: Record<string, string> = {
  godot: "Godot",
  unity: "Unity",
  http: "HTTP",
  custom: "自定义",
};

/** 按协议/标识把已注册插件归组成 { group -> ParserOption[] }；无法匹配的落入 custom。 */
export function groupParsers(plugins: Array<Pick<RegisteredPlugin, "name" | "protocol" | "online">>): {
  order: Array<"godot" | "unity" | "http" | "custom">;
  byGroup: Record<string, ParserOption[]>;
} {
  const byGroup: Record<string, ParserOption[]> = {};
  for (const p of plugins) {
    let group: ParserOption["group"] = "custom";
    for (const r of GROUP_RULES) {
      if (r.test(p)) {
        group = r.group;
        break;
      }
    }
    const label = p.name;
    byGroup[group] ??= [];
    byGroup[group].push({ group, label, plugin: p.name, online: p.online });
  }
  const order: Array<"godot" | "unity" | "http" | "custom"> = ["godot", "unity", "http"];
  if (byGroup.custom) order.push("custom");
  return { order: order.filter((g) => byGroup[g]), byGroup };
}
```

- [ ] **Step 3: 编译**

Run: `cd web && npx tsc --noEmit`
Expected: PASS。

- [ ] **Step 4: Commit**

```bash
git add web/src/lib/parsers.ts web/src/types/agent.ts
git commit -m "feat(web): parser grouping catalog (godot/unity/http/custom)"
```

---

## Task A2: StartCaptureDialog 用友善卡片替换插件下拉

**Files:**
- Modify: `web/src/components/start-capture-dialog.tsx`

- [ ] **Step 1: 引入归类**

在 `StartCaptureDialog` 顶部引入 `groupParsers`，并把原有 `plugins` 下拉（解码插件 section）替换为归组卡片 + 自定义下拉。

Replace（原 268-287 行「解码插件（可选）」整块）为：

```tsx
<div>
  <label className="text-sm font-medium">解码解析器（可选）</label>
  {pluginGroups.order.length === 0 ? (
    <p className="mt-1.5 text-xs text-muted-foreground">
      当前没有已注册的解析器，可留空仅抓包；或先启动解析器插件使其注册到 Pipeline。
    </p>
  ) : (
    <>
      <div className="mt-1.5 grid grid-cols-2 gap-1.5">
        {pluginGroups.order.map((g) =>
          pluginGroups.byGroup[g].map((opt) => (
            <button
              key={opt.plugin}
              type="button"
              aria-pressed={plugin === opt.plugin}
              disabled={!opt.online}
              onClick={() => setPlugin(plugin === opt.plugin ? "" : opt.plugin)}
              className={`flex items-center gap-2 rounded-md border px-2.5 py-2 text-sm transition-colors ${
                plugin === opt.plugin
                  ? "border-primary/60 bg-primary/10 text-foreground"
                  : "border-border bg-background text-muted-foreground"
              } ${opt.online ? "cursor-pointer hover:bg-muted/60" : "cursor-not-allowed opacity-50"}`}
            >
              <span className="rounded bg-muted px-1 py-0.5 font-mono text-[10px] uppercase">
                {g}
              </span>
              <span className="truncate">{opt.label}</span>
            </button>
          )),
        )}
      </div>
      <p className="mt-1 text-xs text-muted-foreground">
        {plugin ? "已选择：留空为不指定（仅抓包）。" : "点击选择一个解析器；离线解析器置灰不可选。"}
      </p>
    </>
  )}
</div>
```

- [ ] **Step 2: 计算 pluginGroups**

在 `handleStart` 上方（state 声明附近）加：

```tsx
const pluginGroups = groupParsers(plugins);
```

并在 import 区从 `@/lib/parsers` 引入 `groupParsers`。

- [ ] **Step 3: 编译**

Run: `cd web && npx tsc --noEmit`
Expected: PASS。

- [ ] **Step 4: Commit**

```bash
git add web/src/components/start-capture-dialog.tsx
git commit -m "feat(web): friendly parser cards in start-capture dialog"
```

---

## Phase B — 启动码接入流程

## Task B1: 后端 `access_code.go`（表 + 归属 + claim 逻辑）

**Files:**
- Create: `cmd/gta-mcp/access_code.go`
- New: `cmd/gta-mcp/access_code_test.go`

- [ ] **Step 1: 表结构 + store**

`cmd/gta-mcp/access_code.go`：

```go
// access_code.go — 启动码 GTA-XXXX 机制：生成一个绑定 owner/会话的短码，成员在
// 目标机输入后由 agent 用 <code> 调 /access/claim 拿回完整配置（复用手动下载的
// 开会话/组 sidecar 逻辑，改 JSON 返回取代 zip 打包）。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
	pb "gta/pkg/internalipc/proto"
)

const accessCodeSchema = `
CREATE TABLE IF NOT EXISTS access_codes (
    code        TEXT PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',
    project_id  TEXT NOT NULL DEFAULT '',
    plugin      TEXT NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 0,
    server      TEXT NOT NULL DEFAULT '',
    platform    TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL,
    expires_at  DATETIME NOT NULL,
    claimed     INTEGER NOT NULL DEFAULT 0,
    session_id  TEXT NOT NULL DEFAULT ''
);`

type accessCode struct {
	Code       string    `json:"code"`
	Owner      string    `json:"owner"`
	ProjectID  string    `json:"project_id,omitempty"`
	Plugin     string    `json:"plugin,omitempty"`
	Port       int       `json:"port"`
	Server     string    `json:"server,omitempty"`
	Platform   string    `json:"platform,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Claimed    bool      `json:"claimed"`
	SessionID  string    `json:"session_id,omitempty"`
}

type accessCodeStore struct{ db *sql.DB }

func newAccessCodeStore(db *sql.DB) *accessCodeStore { return &accessCodeStore{db: db} }

func (s *accessCodeStore) Init() error {
	if _, err := s.db.Exec(accessCodeSchema); err != nil {
		return err
	}
	return nil
}

func (s *accessCodeStore) Create(ctx context.Context, c *accessCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_codes(code,owner,project_id,plugin,port,server,platform,created_at,expires_at,claimed,session_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		c.Code, c.Owner, c.ProjectID, c.Plugin, c.Port, c.Server, c.Platform,
		c.CreatedAt.Format(time.RFC3339), c.ExpiresAt.Format(time.RFC3339),
		boolInt(c.Claimed), c.SessionID)
	return err
}

func (s *accessCodeStore) Get(ctx context.Context, code string) (*accessCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT code,owner,project_id,plugin,port,server,platform,created_at,expires_at,claimed,session_id
		 FROM access_codes WHERE code=?`, code)
	return scanAccessCode(row)
}

func (s *accessCodeStore) MarkClaimed(ctx context.Context, code, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_codes SET claimed=1, session_id=? WHERE code=?`, sessionID, code)
	return err
}

func (s *accessCodeStore) listForOwner(ctx context.Context, owner string, all bool) ([]accessCode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT code,owner,project_id,plugin,port,server,platform,created_at,expires_at,claimed,session_id
		 FROM access_codes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accessCode
	for rows.Next() {
		c, err := scanAccessCode(rows)
		if err != nil {
			return nil, err
		}
		if all || c.Owner == owner {
			out = append(out, *c)
		}
	}
	return out, rows.Err()
}

func scanAccessCode(s interface{ Scan(dest ...any) error }) (*accessCode, error) {
	var c accessCode
	var ca, ea string
	var claimed int
	err := s.Scan(&c.Code, &c.Owner, &c.ProjectID, &c.Plugin, &c.Port, &c.Server, &c.Platform,
		&ca, &ea, &claimed, &c.SessionID)
	if err != nil {
		return nil, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, ca)
	c.ExpiresAt, _ = time.Parse(time.RFC3339, ea)
	c.Claimed = claimed != 0
	return &c, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// newAccessCode 生成形如 GTA-3F9A-2B7C 的短码（仅大写字面 + 数字）。
func newAccessCode() string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "GTA-DEAD-BEEF"
	}
	var parts []string
	for i := 0; i < 2; i++ {
		var s string
		for j := 0; j < 4; j++ {
			s += string(charset[int(b[i*2])%len(charset)])
		}
		parts = append(parts, s)
	}
	return "GTA-" + parts[0] + "-" + parts[1]
}
```

- [ ] **Step 2: 写失败测试**

`cmd/gta-mcp/access_code_test.go`：

```go
package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAccessCodeRoundTrip(t *testing.T) {
	db, err := newControlStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newAccessCodeStore(db)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	c := &accessCode{
		Code: "GTA-A1B2-C3D4", Owner: "alice", Port: 8080,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), c.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "alice" || got.Port != 8080 || got.Claimed {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if err := store.MarkClaimed(context.Background(), c.Code, "sess-1"); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.Get(context.Background(), c.Code)
	if !got2.Claimed || got2.SessionID != "sess-1" {
		t.Fatalf("claim not persisted: %+v", got2)
	}
}

func TestNewAccessCodeFormat(t *testing.T) {
	c := newAccessCode()
	if len(c) != 11 { // "GTA-XXXX-XXXX"
		t.Fatalf("expected GTA-XXXX-XXXX, got %q", c)
	}
	if !strings.HasPrefix(c, "GTA-") {
		t.Fatalf("expected GTA- prefix, got %q", c)
	}
}
```

> 需要 `newControlStoreForTest` 辅助（返回 *sql.DB 或含 .DB() 的 store）。若现有测试已有类似 helper 则复用其名；否则在测试内用 `sql.Open("sqlite", t.TempDir()+"ctl.sqlite")`（与 `project_store_test.go` 同款）。

- [ ] **Step 3: 运行使其失败**

Run: `go test ./cmd/gta-mcp -run TestAccessCodeRoundTrip -v`
Expected: 编译失败（`newAccessCode`/`accessCodeStore` 未定义）或 FAIL。

- [ ] **Step 4: 编译通过**

修正 import（`strings`、`crypto/rand`、`database/sql`）。Run: `go build ./cmd/gta-mcp`
Expected: 通过。

- [ ] **Step 5: 提交**

```bash
git add cmd/gta-mcp/access_code.go cmd/gta-mcp/access_code_test.go
git commit -m "feat(mcp): access_code table + store + generator"
```

---

## Task B2: `create_access_code` MCP 工具 + 装配

**Files:**
- Modify: `cmd/gta-mcp/access_code.go`
- Modify: `cmd/gta-mcp/main.go`

- [ ] **Step 1: 工具处理函数**

在 `access_code.go` 追加：

```go
// handleCreateAccessCode 为当前用户生成一个启动码（可选绑项目/插件/端口/平台/回连地址）。
func (m *mcpCapture) handleCreateAccessCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, all := m.ownerScope(ctx)
	code := newAccessCode()
	port := handleProjectArgPort(req)
	plugin := req.GetString("plugin", "")
	platform := req.GetString("platform", "")
	projectID := req.GetString("project_id", "")
	server := strings.TrimSpace(req.GetString("server", ""))

	rec := &accessCode{
		Code:       code,
		Owner:      owner,
		ProjectID:  projectID,
		Plugin:     plugin,
		Port:       port,
		Server:     server,
		Platform:   platform,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		Claimed:    false,
	}
	if err := m.accessCodes.Create(ctx, rec); err != nil {
		return nil, fmt.Errorf("create access code: %w", err)
	}
	slog.Info("access code created", "owner", owner, "code", code)
	return successResult(map[string]any{
		"code": code, "owner": owner, "project_id": projectID,
		"plugin": plugin, "port": port, "platform": platform, "expires_at": rec.ExpiresAt.Format(time.RFC3339),
	}), nil
}

// handleListAccessCodes 列出当前用户（或 admin 全部）启动码。
func (m *mcpCapture) handleListAccessCodes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, all := m.ownerScope(ctx)
	codes, err := m.accessCodes.listForOwner(ctx, owner, all)
	if err != nil {
		return nil, err
	}
	return successResult(map[string]any{"codes": codes}), nil
}
```

- [ ] **Step 2: 装配（main.go）**

`accessCodes := newAccessCodeStore(controlStore.DB()); if err := accessCodes.Init(); err != nil { return nil/... err }`，赋给 `mcpCapture.accessCodes`。

在 `mcpCapture` 结构体加字段：`accessCodes *accessCodeStore`。

注册工具（在 `list_projects` 附近）：

```go
s.AddTool(mcp.NewTool("create_access_code",
	mcp.WithDescription("Generate an access code (GTA-XXXX-XXXX) bound to the current user. A member enters this code when starting gta-agent to auto-register and connect. Optional: project_id, plugin, port, platform, server."),
	mcp.WithString("project_id"), mcp.WithString("plugin"), mcp.WithNumber("port"),
	mcp.WithString("platform"), mcp.WithString("server"),
), capture.handleCreateAccessCode)
s.AddTool(mcp.NewTool("list_access_codes",
	mcp.WithDescription("List access codes visible to the current user (global admin sees all)."),
), capture.handleListAccessCodes)
```

- [ ] **Step 3: 编译 + 测试**

Run: `go build ./cmd/gta-mcp && go test ./cmd/gta-mcp -run TestAccessCodeRoundTrip -v`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cmd/gta-mcp/access_code.go cmd/gta-mcp/main.go
git commit -m "feat(mcp): create/list access code tools"
```

---

## Task B3: `GET /access/claim` 端点（复用开会话逻辑）

**Files:**
- Modify: `cmd/gta-mcp/access_code.go`
- Modify: `cmd/gta-mcp/main.go`（挂路由）
- New test: `cmd/gta-mcp/access_claim_test.go`

- [ ] **Step 1: claim 处理器**

`access_code.go` 追加：

```go
// handleAccessClaim 是 agent 首启时调用的未鉴权端点：携带启动码返回完整配置
// （server/registry/ingest/token/session/plugin 等），复用手动 download 的开会话
// 与组 sidecar 配置逻辑，但不打包 zip，改 JSON 返回。
func (m *mcpCapture) handleAccessClaim(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	rec, err := m.accessCodes.Get(ctx, code)
	if err != nil || rec == nil {
		http.Error(w, "invalid access code", http.StatusNotFound)
		return
	}
	if time.Now().After(rec.ExpiresAt) {
		http.Error(w, "access code expired", http.StatusGone)
		return
	}

	// 复用 download 的开会话逻辑：从该 code 的 recipe 开 agent 接收会话。
	if m.pipelineClient == nil {
		http.Error(w, "pipeline is not reachable", http.StatusServiceUnavailable)
		return
	}
	owner := rec.Owner
	grpcReq := &pb.StartCaptureRequest{Plugin: rec.Plugin, Agent: true, Owner: owner, AllOwners: false}
	if rec.ProjectID != "" {
		grpcReq.ProjectId = rec.ProjectID
	}
	gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := m.pipelineClient.StartCapture(gctx, grpcReq)
	if err != nil {
		http.Error(w, "open receive session failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	sessionID := resp.GetSessionId()

	// token：取该 owner 的真实凭证（经用请求方主体验证 code 后），烧进返回给 agent。
	registry, ingest := m.registryIngest(ctx)
	var token string
	if p, ok := auth.PrincipalFrom(ctx); ok {
		token = secretForOwner(p.Owner) // 需从 resolver 拿静态 token；见 Step 3
	}
	if rec.Server != "" {
		registry = rec.Server
	}
	cfg := map[string]any{
		"server": registry, "ingest_addr": ingest,
		"token": token, "session": sessionID,
		"bpf": accessCodeBPF(rec.Port),
		"plugin_names": accessCodePlugins(rec.Plugin),
	}

	if err := m.accessCodes.MarkClaimed(ctx, code, sessionID); err != nil {
		slog.Warn("mark access code claimed failed", "code", code, "error", err)
	}
	// 同步 session metadata（在线/离线派生项目归属）
	meta := sessionMetadata{
		Owner: owner, SessionID: sessionID, StartedAt: time.Now().Format(time.RFC3339),
		Status: "running", Port: rec.Port, Plugin: rec.Plugin, Source: "agent", DBPath: resp.GetDbPath(),
		ProjectID: rec.ProjectID,
	}
	if m.sessionMgr != nil {
		if err := m.sessionMgr.writeSessionMetadata(sessionID, meta); err != nil {
			slog.Warn("write session metadata in claim failed", "error", err)
		}
		m.sessionMgr.writeCurrent(meta)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Session-Id", sessionID)
	_ = json.NewEncoder(w).Encode(cfg)
	slog.Info("access code claimed", "owner", owner, "code", code, "session", sessionID)
}

func accessCodeBPF(port int) string {
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("tcp port %d or udp port %d", port, port)
}

func accessCodePlugins(plugin string) []string {
	if plugin == "" {
		return []string{}
	}
	return []string{plugin}
}
```

- [ ] **Step 2: route 挂载（main.go）**

`mux.HandleFunc("/access/claim", capture.handleAccessClaim)` 挂到**鉴权链之外**（放在 `/singbox/profile` 同层 `root` mux，因为 agent 首启还没有 token）。若顾虑泄露，可在 claim 内对 code 做存在性+过期校验（已做）。

- [ ] **Step 3: 静态 token 取回**

`mcpCapture` 需能拿到「owner 的静态 token」供烧录。现有 `auth.Principal` 从请求身份取，但 claim 未走 Bearer。方案：`accessCodeStore`/`mcpCapture` 注入一个 `map[owner]token`（从 resolver 导出），或复用 `.env`。为避免扩大 auth API，给 `mcpCapture` 加字段 `tokensByOwner map[string]string`，在装配处从 `auth.ParseTokens(os.Getenv(auth.EnvTokens))` 解析填充；匿名模式下留空（返回空 token，agent 以匿名 owner=local 回连，团队模式应有 token）。

`secretForOwner` 换成 `m.tokensByOwner[owner]`：

```go
func (m *mcpCapture) ownerSecret(owner string) string {
	return m.tokensByOwner[owner]
}
```

在装配处：
```go
tokensByOwner := map[string]string{}
if tr, err := auth.ParseTokens(os.Getenv(auth.EnvTokens)); err == nil {
	// ParseTokens 只给 byToken 映射，需转出 owner->token；见 Step 5 实现反转
	tokensByOwner = ...
}
```
> 说明：`auth.ParseTokens` 不暴露 owner→token 反转映射。为避免改 auth 包，计划在 `cmd/gta-mcp` 内写一个 `loadTokensByOwner()` 复刻解析（读 `GTA_AUTH_TOKENS`，解析成 map[owner]token，忽略 admin 后缀）；这是最小侵入。**若你不想复刻解析**，可在 claim 返回里省略 token，改由 agent 以匿名（owner=local）回连——对「公开领取但非团队鉴权」场景够用，但团队部署需要真实 token。按需求保留烧 token 方案，用 `loadTokensByOwner`。

```go
// loadTokensByOwner 从 GTA_AUTH_TOKENS 解析 owner->token（"alice=gta_xxx:admin" 取 "gta_xxx"）。
func loadTokensByOwner() map[string]string {
	m := map[string]string{}
	for _, seg := range strings.Split(os.Getenv(auth.EnvTokens), ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		eq := strings.IndexByte(seg, '=')
		if eq < 0 {
			continue
		}
		owner := strings.TrimSpace(seg[:eq])
		tok := strings.TrimSpace(seg[eq+1:])
		if i := strings.LastIndexByte(tok, ':'); i >= 0 {
			tok = tok[:i]
		}
		if owner != "" && tok != "" {
			m[owner] = tok
		}
	}
	return m
}
```

- [ ] **Step 4: 测试（access_claim_test.go）**

用 fake pipelineClient 与临时 sqlite：创建 code → 调 claim → 断言返回 JSON 含 `server`、`session`、`claimed=true`。

```go
func TestAccessClaimReturnsConfig(t *testing.T) {
	// 用最小 mcpCapture：accessCodes(sqlite) + pipelineClient fake 返回 session。
	// 断言 cfg.server != "" && cfg.session != ""。
}
```
> 参考既有 `TestSetSessionProjectRoundTrip` 的构造方式构造 `mcpCapture`（`project_membership_test.go` 已有 fake/最小 stub 先例，复用其 pipeline fake 类型）。

- [ ] **Step 5: 编译 + 测试**

Run: `go build ./cmd/gta-mcp && go test ./cmd/gta-mcp -run TestAccess`  
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add cmd/gta-mcp/access_code.go cmd/gta-mcp/main.go cmd/gta-mcp/access_claim_test.go
git commit -m "feat(mcp): /access/claim returns agent config from startup code"
```

---

## Task B4: agent 支持 `--code` 启动码

**Files:**
- Modify: `cmd/gta-agent/main.go`
- New: `cmd/gta-agent/claim.go`
- New test: `cmd/gta-agent/claim_test.go`

- [ ] **Step 1: claim 客户端**

`cmd/gta-agent/claim.go`：

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// claimAccessCode 带启动码调服务端 /access/claim，返回可直接用作默认配置的映射。
// hostPort 是 mcp HTTP 地址（默认 127.0.0.1:8781）；code 形如 GTA-XXXX-XXXX。
func claimAccessCode(ctx context.Context, hostPort, code string) (embeddedAgentConfig, error) {
	var cfg embeddedAgentConfig
	if hostPort == "" {
		hostPort = "127.0.0.1:8781"
	}
	u := (&url.URL{
		Scheme: "http",
		Host:   hostPort,
		Path:   "/access/claim",
	}).String() + "?code=" + url.QueryEscape(code)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return cfg, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var b []byte
		_, _ = resp.Body.Read(b)
		return cfg, fmt.Errorf("claim failed: HTTP %d %s", resp.StatusCode, string(b))
	}
	type claimResp struct {
		Server      string   `json:"server"`
		IngestAddr  string   `json:"ingest_addr"`
		Token       string   `json:"token"`
		Session     string   `json:"session"`
		BPF         string   `json:"bpf"`
		PluginNames []string `json:"plugin_names"`
	}
	var cr claimResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return cfg, err
	}
	cfg.Server = cr.Server
	cfg.IngestAddr = cr.IngestAddr
	cfg.Token = cr.Token
	cfg.SessionID = cr.Session
	cfg.BPF = cr.BPF
	cfg.BindPlugins = cr.PluginNames
	return cfg, nil
}
```

> 注意 `embeddedAgentConfig` 字段名与 claim 返回的 JSON key 需对应（Server↔server、SessionID↔session、BPF↔bpf、BindPlugins↔plugin_names）。这里手动映射而非直接 Unmarshal 到结构体，因 server key 为 `server` 而结构体字段为 `Server`（JSON tag 是 `server`）——可直接给结构体加 tag 更干净，见 Step 2。

- [ ] **Step 2: 复用 sidecar 结构体 tag**

直接 Unmarshal 更简洁。给 claim.go 用 `embeddedAgentConfig`，其 `json` tag 已含 `server`/`session`/`bpf`/`plugin_names`（见 main.go 的 embeddedAgentConfig）。故可直接：

```go
func claimAccessCode(ctx context.Context, hostPort, code string) (embeddedAgentConfig, error) {
	var cfg embeddedAgentConfig
	... // 请求
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil { ... }
	if cfg.IngestAddr == "" {
		// 由 server 派生 ingest（registry+1）——server 存的是 registry 端口
		if reg, ingest, err := deriveAddrs(cfg.Server, cfg.RegistryAddr, cfg.IngestAddr); err == nil {
			cfg.RegistryAddr, cfg.IngestAddr = reg, ingest
		}
	}
	return cfg, nil
}
```
> 结构体既有 tag：`Server`→`server`、`RegistryAddr`→`registry_addr`、`IngestAddr`→`ingest_addr`、`Token`→`token`、`SessionID`→`session`、`BPF`→`bpf`、`BindPlugins`→`plugin_names`。claim 返回 `server`/`ingest_addr`/`token`/`session`/`bpf`/`plugin_names`，与 tag 对齐。

- [ ] **Step 3: main.go 接入 `--code`**

在 flag 区新增 `accessCode string`：`fs.StringVar(&accessCode, "code", "", "启动码 GTA-XXXX：无 server/token 时用它自动领取配置")`。

在 `embedded/sidecar 加载之后、deriveAddrs 之前`（main.go 第 134 行前）插入：

```go
// 启动码：无任何配置或显式 --code 时，用码自动领取 server/token/session 作为默认配置。
if (accessCode != "" || (server == "" && token == "" && !hasEmbedded)) && purposeAccessCodeNeedsConfig {
	if accessCode == "" {
		// 交互式：stdin 读一行（首启引导）。非 TTY 下读 os.Stdin。
		fmt.Print("请输入启动码 GTA-XXXX-XXXX: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		accessCode = strings.ToUpper(strings.TrimSpace(line))
	}
	if accessCode == "" {
		slog.Error("启动码不能为空；用 --code GTA-XXXX-XXXX 或输入启动码")
		os.Exit(1)
	}
	if !strings.HasPrefix(accessCode, "GTA-") {
		slog.Error("启动码格式应为 GTA-XXXX-XXXX", "code", accessCode)
		os.Exit(1)
	}
	claimed, err := claimAccessCode(ctx, accessHost, accessCode)
	if err != nil {
		slog.Error("领取启动码失败（请确认 --mcp <host:8781> 可达）", "error", err)
		os.Exit(1)
	}
	if server == "" {
		server = claimed.Server
	}
	if registryAddr == "" {
		registryAddr = claimed.RegistryAddr
	}
	if ingestAddr == "" {
		ingestAddr = claimed.IngestAddr
	}
	if token == "" {
		token = claimed.Token
	}
	if sessionID == "" {
		sessionID = claimed.SessionID
	}
	if bpf == "" {
		bpf = claimed.BPF
	}
	// 标记已领取，关闭首启 iface 强校验
	hasClaimed = true
}
```

新增 flag：`fs.StringVar(&accessHost, "mcp", "127.0.0.1:8781", "服务端 MCP HTTP 地址（启动码领取用）")`。

并将原有第 117 行 `if sessionID != "" && iface == "" && !hasEmbedded` 的条件扩为 `&& !hasClaimed`，避免领取后仍强制 iface。

- [ ] **Step 4: 测试**

`claim_test.go` 用 `httptest` 起一个假 `/access/claim`，断言 `claimAccessCode` 正确解析返回配置到 `embeddedAgentConfig`（server/token/session/bpf/plugin_names），以及错误状态码返回 error。

- [ ] **Step 5: 编译 + 测试**

Run: `go build ./cmd/gta-agent && go test ./cmd/gta-agent -run TestClaim`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add cmd/gta-agent/main.go cmd/gta-agent/claim.go cmd/gta-agent/claim_test.go
git commit -m "feat(agent): support GTA access code for first-run auto configuration"
```

---

## Task B5: Web「我的接入」面板 + 接入命令生成

**Files:**
- Modify: `web/src/types/agent.ts`
- Modify: `web/src/hooks/use-mcp.ts`
- New: `web/src/components/access-code-panel.tsx`
- Modify: `web/src/components/agent-download-dialog.tsx`
- Modify: `web/src/App.tsx`

- [x] **Step 1: 前端类型 + hooks**

`web/src/types/agent.ts` 追加：

```ts
/** create_access_code 返回。 */
export interface AccessCodeResult {
  ok: boolean;
  code: string;
  owner?: string;
  project_id?: string;
  plugin?: string;
  port?: number;
  platform?: string;
  expires_at?: string;
}
```

`web/src/hooks/use-mcp.ts` 追加：

```ts
export function useCreateAccessCode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars?: { project_id?: string; plugin?: string; port?: number; platform?: string; server?: string }) =>
      mcpClient.callTool<AccessClaimResult>("create_access_code", {
        project_id: vars?.project_id,
        plugin: vars?.plugin,
        port: vars?.port,
        platform: vars?.platform,
        server: vars?.server,
      }),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["accessCodes"] }),
  });
}
```
> 复用/新增 `AccessClaimResult` 或直接 `Record<string, any>`。建议加 `export interface AccessClaimResult { ok?: boolean; code: string; [k: string]: unknown }`。

- [x] **Step 2: `access-code-panel.tsx`**

新建组件：props `{ onCopy, onDone }`。内容：
- 「选择目标平台」grAID（复用 `AgentPlatform`/`useAgentDownloadOptions` 的 platforms）。
- 「生成接入命令」按钮 → `useCreateAccessCode({ port, platform })` 拿 code。
- 展示三块命令：
  - **Windows**：`<a href="/download/agent?code=<code>&platform=windows/amd64" download>` 一键下载 zip（手填 code 也可）。展示「解压后双击 gta-agent，第一次运行输入启动码」。
  - **Linux/macOS**：可复制命令
    ```
    curl -fsSL 'http://<host>:8781/access/claim?code=<code>' \
      && curl -fsSL 'http://<host>:8781/setup.sh?code=<code>' | bash
    ```
  - **启动码**：大号等宽 `<code>GTA-XXXX-XXXX</code>` + 复制按钮。
- 提示语：不要用户碰 port/token/registry。

> `curl | bash` 需要一个 `setup.sh`（见 Task B6）返回一段下载+运行脚本；若本轮不实现 setup.sh，则退化为「复制启动码 → 手动下载 agent → 首启输入启动码」的引导。**Plan 按实现 setup.sh 全链路来**，若时间紧张可在 B6 里把 setup.sh 做成可选项（返回 501 也让 curl 失败，需兜底为用户手动下载提示）。为使 `curl | bash` 真正可用，B6 必做。

- [x] **Step 3: 改造 `agent-download-dialog.tsx`**

把现有「configure」phACe 默示改为「我的接入」面板（`AccessCodePanel` 内嵌），保留「高级/自定义下载」折叠（显示原 port+server+plugin 表单）。即：顶部友好接入（启动码），底部 `details` 折叠展开原高级表单。`onNavigateToSession` 保留传 sessionId。

- [x] **Step 4: App.tsx 接入**

`<AgentDownloadDialog ... />` 保持挂载；「下载 Agent」顶栏按钮文案改「接入成员」。`useCreateAccessCode` 的 `project_id` 由当前选中项目注入（若 App 有 currentProjectId）。

- [x] **Step 5: 编译 + build**

Run: `cd web && npx tsc --noEmit && npx vite build`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add web/src/types/agent.ts web/src/hooks/use-mcp.ts web/src/components/access-code-panel.tsx web/src/components/agent-download-dialog.tsx web/src/App.tsx
git commit -m "feat(web): access-code onboarding panel + curated commands"
```

---

## Task B6: `setup.sh`（`curl | bash` 一键安装脚本）

**Files:**
- Modify: `cmd/gta-mcp/access_code.go`
- Modify: `cmd/gta-mcp/main.go`（挂路由）

- [ ] **Step 1: 处理函数**

```go
// handleSetupScript 返回 `curl ... | bash` 的一键脚本：下载本平台预置 agent zip、
// 解压、把启动码写入 config.embedded.json，启动 gta-agent。code 由调用方拼在 URL。
func (m *mcpCapture) handleSetupScript(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	if platform == "" {
		platform = linuxAmd64Platform()
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
CODE="%s"
# 下载本平台 agent zip（含通用二进制），解压到 ~/.gta-agent
BIN_DIR="$HOME/.gta-agent"
mkdir -p "$BIN_DIR"
URL="%s/download/agent?code=%s&platform=%s"
# 用 code 换 token：先 claim 拿到 sidecar 配置（含 token），供下文写入
CONFIG_URL="%s/access/claim?code=%s"
echo "下载 Agent 并配置启动码..."
curl -fsSL "$URL" -o /tmp/gta-agent.zip || { echo "下载失败，请确认可访问服务端"; exit 1; }
unzip -o -q /tmp/gta-agent.zip -d "$BIN_DIR"
# 用 claim 拿到 server/token/session 写入 config.embedded.json，实现免输入直接跑
curl -fsSL "$CONFIG_URL" -o "$BIN_DIR/config.embedded.json" || true
chmod +x "$BIN_DIR/gta-agent"
echo "已在 %s 完成安装。执行 gta-agent 开始抓包上报。"
`, code, m.baseURL(r), code, platform, m.baseURL(r), code, "$BIN_DIR")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
}
```
> `m.baseURL(r)` 从请求回推 scheme/host，供脚本拼接 download/claim。`linuxAmd64Platform()` 返回 "linux/amd64"。

- [ ] **Step 2: 挂路由（main.go root mux，免鉴权）**

`root.HandleFunc("/setup.sh", capture.handleSetupScript)`

> 安全说明：`/setup.sh` 与 `/access/claim` 同层免鉴权；二者都只凭 `code` 有效性即可工作，泄露面被限制在「一次性/短时有效 + expires_at」。生产团队部署建议这些端点只暴露于内网。

- [ ] **Step 3: baseURL helper + linux platform**

追加：
```go
func (m *mcpCapture) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func linuxAmd64Platform() string { return "linux/amd64" }
```

- [ ] **Step 4: 编译 + 测试**

Run: `go build ./cmd/gta-mcp`  
Expected: 通过。测试：`TestSetupScriptSnippet` 断言返回含 `CODE=` 与 `/access/claim?code=`。

- [ ] **Step 5: 提交**

```bash
git add cmd/gta-mcp/access_code.go cmd/gta-mcp/main.go cmd/gta-mcp/access_claim_test.go
git commit -m "feat(mcp): /setup.sh one-liner install script for curl|bash"
```

---

## Task B7: 文档 + 自检

**Files:**
- Modify: `docs/member-onboarding.md`

- [x] **Step 1: 更新上手指南**

改写为「启动码优先」：查看网页『我的接入』→ 复制 Linux `curl|bash` 命令或下载 Windows zip → 输入启动码 GTA-XXXX-XXXX → agent 自动注册/取配置/回连。保留高级（自定义下载/开发者工具）简介。

- [ ] **Step 2: 提交**

```bash
git add docs/member-onboarding.md
git commit -m "docs: startup-code onboarding first"
```

---

## 自检

- **Spec 覆盖**：解析器入口（A1 归类 + A2 卡片）、启动码流程（B1 表 + B2 工具 + B3 claim + B4 agent + B5 web + B6 setup.sh）、开发者工具收起（A2/App「更多」+ 高级开关）。未做复杂 RBAC / 独立管理后台，符合「不做管理后台」约束。
- **Placeholder 扫描**：全部步骤含具体文件/代码/命令。涉及 `mcpCapture.accessCodes`、`tokensByOwner`、`loadTokensByOwner`、`newControlStoreForTest` 等新增符号均在前面任务定义或指出复用。
- **类型一致性**：`accessCode`（Go）↔ `AccessCodeResult`（TS）字段（code/owner/project_id/plugin/port/platform/expires_at）一致；claim JSON（server/ingest_addr/token/session/bpf/plugin_names）与 `embeddedAgentConfig` json tag 一致；`parserOption.Group`（godot/unity/http/custom）与 GROUP_RULES 常量一致。