# 前端对齐团队协作改造 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Web 前端支持团队协作改造后的后端：token 鉴权（含 SSE）、远程 agent 抓包源、owner 展示与 admin 筛选，且匿名模式行为零变化。

**Architecture:** 后端在 `auth.Middleware` 内做两处小改动（`?token=` 查询参数回退、身份回显响应头）并在 `list_all_sessions` 输出补 `owner` 字段；前端新增 `src/lib/auth.ts`（token/身份/401 三个轻量 store），`mcp-client` 注入 Bearer 头并同步身份，设置弹窗填 token，开始抓包弹窗加源选择，侧栏/插件面板显示归属。无路由、无状态库，沿用 TanStack Query 轮询。

**Tech Stack:** Go（pkg/auth、cmd/gta-mcp，testify 不用、标准库 testing）；React 19 + TypeScript + Vite 6 + TanStack Query 5；新增 devDependency：vitest。

**Spec:** `docs/superpowers/specs/2026-08-29-frontend-team-collab-design.md`

**关键事实（探索已确认，不要重复验证）：**
- 匿名模式（未配置 `GTA_AUTH_TOKENS`）时 `cmd/gta-mcp/http_server.go` 的 `authMiddleware` 直接透传，**根本不会进入** `auth.Middleware` —— 所以匿名模式天然没有身份响应头，前端把"无 `X-GTA-Owner` 头"当作匿名。
- `pkg/auth` 现有 API：`Resolver`/`StaticResolver`/`Principal{Owner string; IsAdmin bool}`/`WithPrincipal(ctx, *Principal)`/`AnonymousOwner = "local"`；测试助手 `mustResolver(t, spec)` 在 `interceptor_test.go`。
- `list_registered_plugins` 已回传 `owner`（main.go:756），前端类型缺字段而已；`list_all_sessions` 的输出 map **没有** `owner`。
- `start_capture` 的 `source=agent` 分支：**不要求 port**（仅 nic 要求 port>0），可与 `pcap_file` 组合；pipeline 侧只订阅 agent hub。
- 前端 `mcp-client.ts` 全部请求走 `POST /mcp`，SSE 在 `hooks/use-mcp.ts` 的 `usePluginEventStream` 里 `new EventSource("/events/plugins")`。
- web 无测试框架，需引入 vitest（仅 devDependency）。

---

### Task 1: 后端 — auth.Middleware 支持 `?token=` 回退 + 身份回显响应头

**Files:**
- Modify: `pkg/auth/http.go`
- Test: `pkg/auth/http_test.go`

- [ ] **Step 1: 在 `pkg/auth/http_test.go` 末尾追加失败测试**

在文件末尾（`TestMiddleware_SetsWWWAuthenticate` 之后）追加：

```go
// doRequest 按完整目标 URL 发请求并返回 recorder，用于断言响应头。
func doRequest(t *testing.T, r Resolver, target, header string) (called bool, owner string, rec *httptest.ResponseRecorder) {
	t.Helper()
	var gotOwner string
	srv := Middleware(r, probeHandler(&called, &gotOwner))
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return called, gotOwner, rec
}

// TestMiddleware_QueryParamToken 验证 EventSource 场景：浏览器 SSE 无法携带自定义头，
// 头缺失时回退解析查询参数 ?token=（admin 身份一并回显）。
func TestMiddleware_QueryParamToken(t *testing.T) {
	t.Parallel()
	called, owner, rec := doRequest(t, mustResolver(t, "alice=gta_aaa,bob=gta_bbb:admin"), "/mcp?token=gta_bbb", "")
	if !called {
		t.Fatal("查询参数携带合法 token 应放行")
	}
	if owner != "bob" {
		t.Fatalf("owner 应为 bob，实际 %q", owner)
	}
	if got := rec.Header().Get(HeaderAdmin); got != "true" {
		t.Fatalf("admin 应回显 X-GTA-Admin: true，实际 %q", got)
	}
}

// TestMiddleware_HeaderBeatsQueryParam 验证头永远优先于查询参数。
func TestMiddleware_HeaderBeatsQueryParam(t *testing.T) {
	t.Parallel()
	called, owner, _ := doRequest(t, mustResolver(t, "alice=gta_aaa,bob=gta_bbb:admin"), "/mcp?token=gta_bbb", "Bearer gta_aaa")
	if !called || owner != "alice" {
		t.Fatalf("头应优先于查询参数: called=%v owner=%q", called, owner)
	}
}

// TestMiddleware_RejectsBadQueryParam 验证查询参数里的非法 token 同样 401。
func TestMiddleware_RejectsBadQueryParam(t *testing.T) {
	t.Parallel()
	called, _, rec := doRequest(t, mustResolver(t, "alice=gta_aaa"), "/mcp?token=gta_zzz", "")
	if called {
		t.Fatal("非法查询参数 token 必须拒绝且不触达下游")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码应为 401，实际 %d", rec.Code)
	}
}

// TestMiddleware_IdentityHeaders 验证身份回显头：owner 恒回显，admin 仅 admin 回显。
func TestMiddleware_IdentityHeaders(t *testing.T) {
	t.Parallel()
	_, _, rec := doRequest(t, mustResolver(t, "alice=gta_aaa,bob=gta_bbb:admin"), "/mcp", "Bearer gta_aaa")
	if got := rec.Header().Get(HeaderOwner); got != "alice" {
		t.Fatalf("X-GTA-Owner 应为 alice，实际 %q", got)
	}
	if got := rec.Header().Get(HeaderAdmin); got != "" {
		t.Fatalf("非 admin 不应回显 X-GTA-Admin，实际 %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认编译失败**

Run: `cd E:/gta && go test ./pkg/auth/ -run TestMiddleware_ -v`
Expected: FAIL，`undefined: HeaderAdmin` / `undefined: HeaderOwner`

- [ ] **Step 3: 用完整实现替换 `pkg/auth/http.go`**

```go
package auth

import (
	"net/http"
)

// 身份回显响应头。跨域直连时默认不对页面 JS 暴露，需在 CORS
// Access-Control-Expose-Headers 中放行（见 cmd/gta-mcp/http_server.go）。
const (
	HeaderOwner = "X-GTA-Owner"
	HeaderAdmin = "X-GTA-Admin"
)

// Middleware 是 MCP HTTP 侧的鉴权中间件，校验 Authorization: Bearer <token>。
// 匿名模式（resolver 未配置任何 token）下调用方（http_server.go 的 authMiddleware）
// 直接透传、不挂载本中间件，单机用法完全不变（响应上也不会有身份回显头）。
func Middleware(r Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, _ := parseBearer(req.Header.Get("Authorization"))
		if token == "" {
			// 浏览器 EventSource 无法携带自定义请求头，SSE（/events/plugins）只能
			// 经查询参数携带 token。仅头缺失时回退，头永远优先于查询参数。
			token = req.URL.Query().Get("token")
		}
		p, ok := r.Resolve(token)
		if !ok {
			// 带上 WWW-Authenticate，否则客户端不知道该用什么方式认证。
			w.Header().Set("WWW-Authenticate", `Bearer realm="gta"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// 身份回显：前端没有 whoami 端点，从响应头读取当前用户名与 admin 态。
		w.Header().Set(HeaderOwner, p.Owner)
		if p.IsAdmin {
			w.Header().Set(HeaderAdmin, "true")
		}
		next.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), p)))
	})
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd E:/gta && go test ./pkg/auth/ -v`
Expected: 全部 PASS（含既有匿名/Bearer/WWW-Authenticate 测试）

- [ ] **Step 5: Commit**

```bash
cd E:/gta && git add pkg/auth/http.go pkg/auth/http_test.go && git commit -m "feat(auth): Middleware 支持 ?token= 查询参数回退（SSE 场景）并回显 X-GTA-Owner/X-GTA-Admin 身份头"
```

---

### Task 2: 后端 — CORS 放行身份回显头（跨域直连可读）

**Files:**
- Modify: `cmd/gta-mcp/http_server.go:24-28`（corsMiddleware 内 echo Origin 处）
- Test: `cmd/gta-mcp/t13_test.go`

- [ ] **Step 1: 在 `cmd/gta-mcp/t13_test.go` 的 `TestCORSMiddlewareAllowlist` 函数之后追加失败测试**

```go
// TestCORSExposeIdentityHeaders 验证命中 allowlist 的跨域响应暴露身份回显头，
// 否则前端 JS 在跨域直连场景下读不到 X-GTA-Owner / X-GTA-Admin。
func TestCORSExposeIdentityHeaders(t *testing.T) {
	h := buildHTTPHandler([]string{"http://good.example.com"}, nil, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Origin", "http://good.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expose := rec.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, auth.HeaderOwner) || !strings.Contains(expose, auth.HeaderAdmin) {
		t.Fatalf("应暴露 X-GTA-Owner/X-GTA-Admin，实际 %q", expose)
	}
}
```

（`strings`、`auth` 已在该文件 import；无需新增 import。）

- [ ] **Step 2: 运行测试确认失败**

Run: `cd E:/gta && go test ./cmd/gta-mcp/ -run TestCORSExposeIdentityHeaders -v`
Expected: FAIL（Expose-Headers 为空）

- [ ] **Step 3: 修改 `cmd/gta-mcp/http_server.go` 的 corsMiddleware**

把：

```go
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
```

改为：

```go
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			// 身份回显头默认不对跨域 JS 暴露，须显式加入 Expose-Headers 前端才读得到。
			w.Header().Set("Access-Control-Expose-Headers", auth.HeaderOwner+", "+auth.HeaderAdmin)
		}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd E:/gta && go test ./cmd/gta-mcp/ -run "TestCORS" -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
cd E:/gta && git add cmd/gta-mcp/http_server.go cmd/gta-mcp/t13_test.go && git commit -m "feat(mcp): CORS 命中 allowlist 时暴露 X-GTA-Owner/X-GTA-Admin 身份回显头"
```

---

### Task 3: 后端 — list_all_sessions 输出补 owner 字段

**Files:**
- Modify: `cmd/gta-mcp/main.go`（`handleListAllSessions` 内 `out = append(out, map[string]any{...})`，约 2219 行）
- Create: `cmd/gta-mcp/list_sessions_owner_test.go`

- [ ] **Step 1: 创建 `cmd/gta-mcp/list_sessions_owner_test.go`**

```go
package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
)

// TestListAllSessionsEchoesOwner 验证会话列表输出携带 owner：
// 前端 owner 徽标与 admin「只看我的」筛选都依赖这个字段。
func TestListAllSessionsEchoesOwner(t *testing.T) {
	workDir := t.TempDir()
	sm := newSessionManager(workDir)
	meta := sessionMetadata{
		Owner:     "alice",
		SessionID: "20260829_120000.000",
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		Port:      8080,
		DBPath:    filepath.Join(workDir, "sessions", "20260829_120000.000", "capture.sqlite"),
	}
	if _, err := sm.createSession(meta); err != nil {
		t.Fatal(err)
	}
	m := &mcpCapture{workDir: workDir, sessionMgr: sm}

	// alice（非 admin）视角：只能看到自己的会话，且输出带 owner。
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	res, err := m.handleListAllSessions(ctx, mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text

	var parsed struct {
		Ok       bool `json:"ok"`
		Sessions []struct {
			SessionID string `json:"session_id"`
			Owner     string `json:"owner"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("解析输出失败: %v\ntext=%s", err, text)
	}
	if !parsed.Ok || len(parsed.Sessions) != 1 {
		t.Fatalf("应恰好返回 1 个会话: ok=%v n=%d text=%s", parsed.Ok, len(parsed.Sessions), text)
	}
	if parsed.Sessions[0].Owner != "alice" {
		t.Fatalf("owner 应为 alice，实际 %q", parsed.Sessions[0].Owner)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd E:/gta && go test ./cmd/gta-mcp/ -run TestListAllSessionsEchoesOwner -v`
Expected: FAIL（`owner 应为 alice，实际 ""`）

- [ ] **Step 3: 修改 `handleListAllSessions` 的输出 map**

在 `cmd/gta-mcp/main.go` 中找到（约 2220 行）：

```go
		seen[sess.SessionID] = true
		out = append(out, map[string]any{
			"session_id":    sess.SessionID,
```

在 `"session_id":    sess.SessionID,` 下一行加入 owner：

```go
		seen[sess.SessionID] = true
		out = append(out, map[string]any{
			"session_id":    sess.SessionID,
			"owner":         sess.Owner,
```

（保持其余字段不动。）

- [ ] **Step 4: 运行测试确认通过**

Run: `cd E:/gta && go test ./cmd/gta-mcp/ -run TestListAllSessionsEchoesOwner -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd E:/gta && git add cmd/gta-mcp/main.go cmd/gta-mcp/list_sessions_owner_test.go && git commit -m "feat(mcp): list_all_sessions 输出补 owner 字段，供前端归属展示与 admin 筛选"
```

---

### Task 4: 前端 — 引入 vitest 并新建 `src/lib/auth.ts`

**Files:**
- Modify: `web/package.json`（scripts + devDependencies）
- Create: `web/vitest.config.ts`
- Create: `web/src/lib/auth.ts`
- Test: `web/src/lib/auth.test.ts`

- [ ] **Step 1: 安装 vitest**

Run: `cd E:/gta/web && npm install -D vitest`
Expected: 安装成功，package.json devDependencies 出现 vitest

- [ ] **Step 2: package.json scripts 加 test**

在 `web/package.json` 的 `"scripts"` 中加入（`"preview"` 之后）：

```json
    "test": "vitest run",
```

- [ ] **Step 3: 创建 `web/vitest.config.ts`**

```ts
import path from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// 与 vite.config.ts 保持一致的 "@" 别名，让测试能解析 src 内的导入。
const root = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  resolve: {
    alias: { "@": path.resolve(root, "src") },
  },
  test: { environment: "node" },
});
```

- [ ] **Step 4: 写失败测试 `web/src/lib/auth.test.ts`**

```ts
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  authHeaders,
  clearAuthError,
  getAuthError,
  getIdentity,
  getToken,
  notifyAuthError,
  setIdentity,
  setToken,
  withTokenParam,
} from "@/lib/auth";

// node 环境没有 localStorage，用内存 Map 桩掉（auth.ts 全部经 safe 包装访问）。
const store = new Map<string, string>();
beforeEach(() => {
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
  });
});
afterEach(() => {
  vi.unstubAllGlobals();
  store.clear();
  setToken(null);
  setIdentity(null);
  clearAuthError();
});

describe("token 存取", () => {
  it("setToken/getToken 往返并持久化到 localStorage", () => {
    setToken("gta_aaa");
    expect(getToken()).toBe("gta_aaa");
    expect(store.get("gta_auth_token")).toBe("gta_aaa");
  });

  it("空串视为清除", () => {
    setToken("gta_aaa");
    setToken("   ");
    expect(getToken()).toBeNull();
    expect(store.has("gta_auth_token")).toBe(false);
  });

  it("authHeaders：有 token 带 Bearer，无 token 不带头", () => {
    expect(authHeaders()).toEqual({});
    setToken("gta_aaa");
    expect(authHeaders()).toEqual({ Authorization: "Bearer gta_aaa" });
  });
});

describe("withTokenParam", () => {
  it("无 token 时原样返回", () => {
    expect(withTokenParam("/events/plugins")).toBe("/events/plugins");
  });

  it("有 token 时拼查询参数并编码", () => {
    setToken("gta aaa/1");
    expect(withTokenParam("/events/plugins")).toBe(
      "/events/plugins?token=gta%20aaa%2F1",
    );
    expect(withTokenParam("/events/plugins?a=1")).toBe(
      "/events/plugins?a=1&token=gta%20aaa%2F1",
    );
  });
});

describe("identity", () => {
  it("set/set null 往返", () => {
    setIdentity({ owner: "alice", isAdmin: true });
    expect(getIdentity()).toEqual({ owner: "alice", isAdmin: true });
    setIdentity(null);
    expect(getIdentity()).toBeNull();
  });
});

describe("authError", () => {
  it("notifyAuthError 置位、setToken 清除", () => {
    expect(getAuthError()).toBe(false);
    notifyAuthError();
    expect(getAuthError()).toBe(true);
    setToken("gta_new");
    expect(getAuthError()).toBe(false);
  });
});
```

- [ ] **Step 5: 运行测试确认失败**

Run: `cd E:/gta/web && npm test`
Expected: FAIL（`Failed to resolve import "@/lib/auth"`）

- [ ] **Step 6: 创建 `web/src/lib/auth.ts`**

```ts
/**
 * 访问令牌与身份状态。
 *
 * 三个极小的可订阅 store（token / identity / authError），供
 * useSyncExternalStore 的 hook（hooks/use-auth.ts）与 mcp-client 使用：
 *  - token 持久化在 localStorage（键 gta_auth_token），无 token = 匿名模式；
 *  - identity 来自后端身份回显响应头（X-GTA-Owner / X-GTA-Admin），不持久化；
 *  - authError 在收到 401 时置位，重新保存 token 即清除。
 */

const TOKEN_KEY = "gta_auth_token";

// localStorage 在隐私模式/被禁用时可能抛异常，统一吞掉降级为内存态。
function safeGet(key: string): string | null {
  try {
    return globalThis.localStorage.getItem(key);
  } catch {
    return null;
  }
}
function safeSet(key: string, value: string): void {
  try {
    globalThis.localStorage.setItem(key, value);
  } catch {
    /* 忽略：无法持久化时仅本次会话生效 */
  }
}
function safeRemove(key: string): void {
  try {
    globalThis.localStorage.removeItem(key);
  } catch {
    /* 忽略 */
  }
}

// ===== token =====

let token: string | null = safeGet(TOKEN_KEY);

const tokenListeners = new Set<() => void>();

export function getToken(): string | null {
  return token;
}

/** 保存/清除 token；null 或空白串视为清除（回到匿名模式）。 */
export function setToken(next: string | null): void {
  const v = next?.trim() || null;
  if (v === token) return;
  token = v;
  if (v) safeSet(TOKEN_KEY, v);
  else safeRemove(TOKEN_KEY);
  // 换了凭证：旧身份与 401 状态都作废，等下一次响应头重新回显。
  setIdentity(null);
  clearAuthError();
  for (const l of tokenListeners) l();
}

export function subscribeToken(listener: () => void): () => void {
  tokenListeners.add(listener);
  return () => tokenListeners.delete(listener);
}

/** MCP 请求头：有 token 时带 Bearer，无 token 返回空对象（匿名模式零变化）。 */
export function authHeaders(): Record<string, string> {
  return token ? { Authorization: `Bearer ${token}` } : {};
}

/** SSE 等无法携带自定义头的 URL：把 token 拼进查询参数（后端 Middleware 支持回退解析）。 */
export function withTokenParam(url: string): string {
  if (!token) return url;
  return `${url}${url.includes("?") ? "&" : "?"}token=${encodeURIComponent(token)}`;
}

// ===== identity（来自响应头回显）=====

export interface Identity {
  owner: string;
  isAdmin: boolean;
}

let identity: Identity | null = null;
const identityListeners = new Set<() => void>();

export function getIdentity(): Identity | null {
  return identity;
}

export function setIdentity(next: Identity | null): void {
  if (
    identity === next ||
    (identity !== null &&
      next !== null &&
      identity.owner === next.owner &&
      identity.isAdmin === next.isAdmin)
  ) {
    return;
  }
  identity = next;
  for (const l of identityListeners) l();
}

export function subscribeIdentity(listener: () => void): () => void {
  identityListeners.add(listener);
  return () => identityListeners.delete(listener);
}

// ===== authError（401 全局横幅）=====

let authError = false;
const authErrorListeners = new Set<() => void>();

export function getAuthError(): boolean {
  return authError;
}

export function notifyAuthError(): void {
  if (authError) return;
  authError = true;
  for (const l of authErrorListeners) l();
}

export function clearAuthError(): void {
  if (!authError) return;
  authError = false;
  for (const l of authErrorListeners) l();
}

export function subscribeAuthError(listener: () => void): () => void {
  authErrorListeners.add(listener);
  return () => authErrorListeners.delete(listener);
}
```

- [ ] **Step 7: 运行测试确认通过**

Run: `cd E:/gta/web && npm test`
Expected: 7 个测试全 PASS

- [ ] **Step 8: Commit**

```bash
cd E:/gta && git add web/package.json web/package-lock.json web/vitest.config.ts web/src/lib/auth.ts web/src/lib/auth.test.ts && git commit -m "feat(web): 新增 lib/auth.ts（token/身份/401 三个可订阅 store）并引入 vitest"
```

---

### Task 5: 前端 — mcp-client 注入 Bearer 头、401→AuthError、同步身份回显

**Files:**
- Modify: `web/src/lib/mcp-client.ts`
- Test: `web/src/lib/mcp-client.test.ts`

- [ ] **Step 1: 写失败测试 `web/src/lib/mcp-client.test.ts`**

```ts
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  clearAuthError,
  getAuthError,
  getIdentity,
  setToken,
} from "@/lib/auth";
import { AuthError, mcpClient } from "@/lib/mcp-client";

/** 构造一个够用的 Response 桩（node 环境无全局 Response 的 headers 语义细节）。 */
function jsonResponse(
  body: unknown,
  status = 200,
  headers: Record<string, string> = {},
) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 401 ? "Unauthorized" : "OK",
    headers: { get: (k: string) => headers[k.toLowerCase()] ?? null },
    json: async () => body,
  };
}

/** 包一层 MCP 双层 JSON：result.content[0].text 内是业务 JSON。 */
function rpcOk(payload: Record<string, unknown>) {
  return jsonResponse({
    jsonrpc: "2.0",
    id: 1,
    result: { content: [{ type: "text", text: JSON.stringify(payload) }] },
  });
}

let fetchCalls: { url: string; init: RequestInit }[] = [];

beforeEach(() => {
  fetchCalls = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    fetchCalls.push({ url, init });
    return rpcOk({ ok: true });
  });
});
afterEach(() => {
  vi.unstubAllGlobals();
  setToken(null);
  clearAuthError();
});

describe("callTool", () => {
  it("有 token 时请求带 Authorization: Bearer", async () => {
    setToken("gta_aaa");
    await mcpClient.callTool("list_all_sessions");
    const header = (fetchCalls[0]!.init.headers as Record<string, string>)[
      "Authorization"
    ];
    expect(header).toBe("Bearer gta_aaa");
  });

  it("无 token 时不带 Authorization 头（匿名模式零变化）", async () => {
    await mcpClient.callTool("list_all_sessions");
    expect(
      (fetchCalls[0]!.init.headers as Record<string, string>)[
        "Authorization"
      ],
    ).toBeUndefined();
  });

  it("HTTP 401 抛 AuthError 并置位全局 401 状态", async () => {
    vi.stubGlobal("fetch", async () => jsonResponse({}, 401));
    await expect(mcpClient.callTool("list_all_sessions")).rejects.toBeInstanceOf(
      AuthError,
    );
    expect(getAuthError()).toBe(true);
  });

  it("从响应头同步身份回显", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        rpcOk({ ok: true }) as unknown as Response & {
          headers: { get: (k: string) => string | null };
        },
    );
    // 重新桩一个带头版本的 fetch：直接改 fetchCalls 不可行，这里用第二个桩。
    vi.stubGlobal("fetch", async () =>
      jsonResponse(
        {
          jsonrpc: "2.0",
          id: 1,
          result: {
            content: [{ type: "text", text: JSON.stringify({ ok: true }) }],
          },
        },
        200,
        { "X-GTA-Owner": "bob", "X-GTA-Admin": "true" },
      ),
    );
    await mcpClient.callTool("list_all_sessions");
    expect(getIdentity()).toEqual({ owner: "bob", isAdmin: true });
  });

  it("JSON-RPC error 含 unauthorized 时抛 AuthError", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        jsonResponse({
          jsonrpc: "2.0",
          id: 1,
          error: { code: -32000, message: "unauthorized" },
        }),
    );
    await expect(mcpClient.callTool("list_all_sessions")).rejects.toBeInstanceOf(
      AuthError,
    );
  });
});
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd E:/gta/web && npm test`
Expected: FAIL（`AuthError` 未导出、无 Authorization 头断言不通过）

- [ ] **Step 3: 修改 `web/src/lib/mcp-client.ts`**

完整替换为：

```ts
import type { JsonRpcRequest, JsonRpcResponse, McpToolResult } from "@/types/mcp";
import { authHeaders, notifyAuthError, setIdentity } from "@/lib/auth";

/** 自增 ID 生成器 */
let nextId = 1;

/** 服务器开启 token 校验而本地未携带/凭证失效时抛出；App 层据此弹出设置引导。 */
export class AuthError extends Error {
  constructor(message = "需要访问令牌（HTTP 401）") {
    super(message);
    this.name = "AuthError";
  }
}

/** 从响应头读取身份回显（后端 auth.Middleware 注入；匿名模式无此头 → 清空身份）。 */
function syncIdentityFromHeaders(headers: { get(k: string): string | null }): void {
  const owner = headers.get("X-GTA-Owner");
  if (!owner) {
    setIdentity(null);
    return;
  }
  setIdentity({ owner, isAdmin: headers.get("X-GTA-Admin") === "true" });
}

/**
 * MCP JSON-RPC 客户端
 * 与 game-traffic-analysis 的 POST /mcp 端点通信
 */
export class McpClient {
  private baseUrl: string;

  constructor(baseUrl = "/mcp") {
    this.baseUrl = baseUrl;
  }

  setBaseUrl(url: string) {
    this.baseUrl = url;
  }

  getBaseUrl(): string {
    return this.baseUrl;
  }

  /**
   * 发送 JSON-RPC 请求并返回业务层结果
   * 自动处理 MCP 协议的双层 JSON 解析 (response → content[0].text)
   */
  async callTool<T = McpToolResult>(
    name: string,
    args: Record<string, unknown> = {},
  ): Promise<T> {
    const id = nextId++;
    const request: JsonRpcRequest = {
      jsonrpc: "2.0",
      id,
      method: "tools/call",
      params: {
        name,
        arguments: args,
      },
    };

    const response = await fetch(this.baseUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(request),
    });

    if (response.status === 401) {
      notifyAuthError();
      throw new AuthError();
    }
    if (!response.ok) {
      throw new Error(`MCP server error: ${response.status} ${response.statusText}`);
    }
    syncIdentityFromHeaders(response.headers);

    const rpcRes: JsonRpcResponse = await response.json() as JsonRpcResponse;

    if ("error" in rpcRes) {
      const msg = String(rpcRes.error.message ?? "");
      if (/unauthorized|401/i.test(msg)) {
        notifyAuthError();
        throw new AuthError(msg);
      }
      throw new Error(`MCP RPC error [${rpcRes.error.code}]: ${msg}`);
    }

    // 提取 content[0].text 并二次解析
    const content = rpcRes.result?.content;
    if (!content || !content[0]?.text) {
      throw new Error("MCP server returned empty content");
    }

    const parsed: McpToolResult = JSON.parse(content[0].text) as McpToolResult;

    if (!parsed.ok) {
      throw new Error(parsed.error ?? "MCP tool returned ok=false");
    }

    return parsed as T;
  }

  /**
   * 初始化 MCP 连接（可选，stateless 模式下非必须）
   */
  async initialize(): Promise<void> {
    const id = nextId++;
    const request: JsonRpcRequest = {
      jsonrpc: "2.0",
      id,
      method: "initialize",
      params: {
        protocolVersion: "2024-11-05",
        capabilities: {},
        clientInfo: {
          name: "game-traffic-analysis-web",
          version: "0.1.0",
        },
      },
    };

    const response = await fetch(this.baseUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(request),
    });

    if (response.status === 401) {
      notifyAuthError();
      throw new AuthError();
    }
    if (!response.ok) {
      throw new Error(`MCP initialize failed: ${response.status}`);
    }
    syncIdentityFromHeaders(response.headers);
  }
}

/** 全局 MCP 客户端单例 */
export const mcpClient = new McpClient();
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd E:/gta/web && npm test`
Expected: 全部 PASS（含 Task 4 的 auth 测试）

- [ ] **Step 5: Commit**

```bash
cd E:/gta && git add web/src/lib/mcp-client.ts web/src/lib/mcp-client.test.ts && git commit -m "feat(web): mcp-client 注入 Bearer 头、401 抛 AuthError 并同步身份回显头"
```

---

### Task 6: 前端 — use-auth hooks + SSE 连接带 token 并随 token 变化重连

**Files:**
- Create: `web/src/hooks/use-auth.ts`
- Modify: `web/src/hooks/use-mcp.ts`（`usePluginEventStream`，约 259-274 行）

- [ ] **Step 1: 创建 `web/src/hooks/use-auth.ts`**

```ts
import { useSyncExternalStore } from "react";
import {
  getAuthError,
  getIdentity,
  getToken,
  subscribeAuthError,
  subscribeIdentity,
  subscribeToken,
  type Identity,
} from "@/lib/auth";

/** 当前访问令牌（token 变化时组件重渲染，SSE 由此触发重连）。 */
export function useAuthToken(): string | null {
  return useSyncExternalStore(subscribeToken, getToken);
}

/** 当前身份（来自后端 X-GTA-Owner/X-GTA-Admin 响应头回显；匿名模式为 null）。 */
export function useIdentity(): Identity | null {
  return useSyncExternalStore(subscribeIdentity, getIdentity);
}

/** 是否处于 401 待补令牌状态（App 层显示横幅）。 */
export function useAuthError(): boolean {
  return useSyncExternalStore(subscribeAuthError, getAuthError);
}
```

- [ ] **Step 2: 修改 `web/src/hooks/use-mcp.ts` 的 usePluginEventStream**

把（约 259-274 行）：

```ts
export function usePluginEventStream() {
  const queryClient = useQueryClient();
  useEffect(() => {
    const es = new EventSource("/events/plugins");
    es.addEventListener("plugin", () => {
      void queryClient.invalidateQueries({ queryKey: ["registeredPlugins"] });
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    });
    es.onerror = () => {
      // EventSource 默认会自动重连；此处仅记录，无需手动处理。
      // 轮询兜底维持面板在重连间隙内的基本可用性。
      slogError("plugin event stream error, relying on polling fallback");
    };
    return () => es.close();
  }, [queryClient]);
}
```

改为：

```ts
export function usePluginEventStream() {
  const queryClient = useQueryClient();
  // token 变化时重建连接：EventSource 无法中途补头，也读不到新 token。
  const token = useAuthToken();
  useEffect(() => {
    const es = new EventSource(withTokenParam("/events/plugins"));
    es.addEventListener("plugin", () => {
      void queryClient.invalidateQueries({ queryKey: ["registeredPlugins"] });
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    });
    es.onerror = () => {
      // EventSource 默认会自动重连；此处仅记录，无需手动处理。
      // 轮询兜底维持面板在重连间隙内的基本可用性。
      slogError("plugin event stream error, relying on polling fallback");
    };
    return () => es.close();
  }, [queryClient, token]);
}
```

并在 `web/src/hooks/use-mcp.ts` 文件头部的 import 区加入：

```ts
import { useAuthToken } from "@/hooks/use-auth";
import { withTokenParam } from "@/lib/auth";
```

- [ ] **Step 3: 运行测试 + 类型检查**

Run: `cd E:/gta/web && npm test && npx tsc -b`
Expected: 测试 PASS、tsc 无错误

- [ ] **Step 4: Commit**

```bash
cd E:/gta && git add web/src/hooks/use-auth.ts web/src/hooks/use-mcp.ts && git commit -m "feat(web): use-auth 身份 hooks；插件事件 SSE 携带 token 并随 token 变化重连"
```

---

### Task 7: 前端 — 设置弹窗新增「访问令牌」输入

**Files:**
- Modify: `web/src/components/settings-dialog.tsx`

- [ ] **Step 1: 完整替换 `web/src/components/settings-dialog.tsx`**

```tsx
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { mcpClient } from "@/lib/mcp-client";
import { getToken, setToken } from "@/lib/auth";
import { Settings, Check, X, KeyRound } from "lucide-react";

interface SettingsDialogProps {
  open: boolean;
  onClose: () => void;
}

export function SettingsDialog({ open, onClose }: SettingsDialogProps) {
  const [url, setUrl] = useState(mcpClient.getBaseUrl());
  // 空输入 = 清除 token（回到匿名模式）。
  const [token, setTokenInput] = useState(getToken() ?? "");
  const [saved, setSaved] = useState(false);
  const queryClient = useQueryClient();

  function handleSave() {
    mcpClient.setBaseUrl(url);
    setToken(token || null);
    // 凭证/地址可能变了：立即重刷全部查询，让身份回显与会话列表马上生效。
    void queryClient.invalidateQueries();
    setSaved(true);
    setTimeout(() => {
      setSaved(false);
      onClose();
    }, 800);
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Settings className="h-5 w-5" />}
      title="设置"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            取消
          </Button>
          <Button onClick={handleSave}>
            {saved ? (
              <>
                <Check className="h-4 w-4" />
                已保存
              </>
            ) : (
              "保存"
            )}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div>
          <label className="text-sm font-medium">MCP Server 地址</label>
          <Input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            aria-label="MCP Server 地址"
            placeholder="/mcp（通过 Vite 代理）或 http://其他地址/mcp"
            className="mt-1.5 font-mono"
          />
          <p className="mt-1 text-xs text-muted-foreground">
            默认 /mcp 通过 Vite dev server 代理到 localhost:8781。如需直连其他地址则填完整 URL。
          </p>
        </div>
        <div>
          <label className="flex items-center gap-1.5 text-sm font-medium">
            <KeyRound className="h-3.5 w-3.5 text-muted-foreground" />
            访问令牌（可选）
          </label>
          <Input
            type="password"
            value={token}
            onChange={(e) => setTokenInput(e.target.value)}
            aria-label="访问令牌"
            placeholder="服务器未开启令牌校验时留空"
            autoComplete="off"
            className="mt-1.5 font-mono"
          />
          <p className="mt-1 text-xs text-muted-foreground">
            团队共享服务端开启令牌校验时，向管理员领取你的 token（形如
            <code className="font-mono"> gta_…</code>）填入；留空则按匿名/单机模式访问。保存后立即生效。
          </p>
        </div>
      </div>
    </Dialog>
  );
}
```

- [ ] **Step 2: 类型检查**

Run: `cd E:/gta/web && npx tsc -b`
Expected: 无错误

- [ ] **Step 3: Commit**

```bash
cd E:/gta && git add web/src/components/settings-dialog.tsx && git commit -m "feat(web): 设置弹窗新增访问令牌输入，保存后立即刷新全部查询"
```

---

### Task 8: 前端 — App 层 401 横幅 + 自动打开设置

**Files:**
- Modify: `web/src/App.tsx`

- [ ] **Step 1: 修改 imports**

在 `web/src/App.tsx` 头部：

`lucide-react` 一行加入 `KeyRound`：

```ts
import { Sun, Moon, Settings, Play, Square, Cable, KeyRound } from "lucide-react";
```

新增一行 hook import（`usePluginEventStream` 那行之后）：

```ts
import { useAuthError } from "@/hooks/use-auth";
```

- [ ] **Step 2: 在 App 组件内接入横幅状态**

在 `const [settingsOpen, setSettingsOpen] = useState(false);`（约 62 行）之后加：

```tsx
  // 服务器开启令牌校验且本地无有效凭证（401）：横幅提示并自动打开设置。
  const authError = useAuthError();
```

在「若原始包调试未开启，避免停留在 raw Tab」那个 useEffect 之后加：

```tsx
  // 401 发生时自动打开设置弹窗，引导填入访问令牌（横幅常驻直至保存新 token）。
  useEffect(() => {
    if (authError) setSettingsOpen(true);
  }, [authError]);
```

- [ ] **Step 3: 在 main 顶部渲染横幅**

在 `<main className="flex min-w-0 flex-1 flex-col">` 之后、`<header` 之前插入：

```tsx
        {/* 401 横幅：服务器要求访问令牌而本地未配置/已失效 */}
        {authError && (
          <div className="flex items-center gap-2 border-b border-amber-300 bg-amber-50 px-4 py-2 text-sm text-amber-900 dark:border-amber-500/40 dark:bg-amber-500/10 dark:text-amber-200">
            <KeyRound className="h-4 w-4 shrink-0" />
            <span className="flex-1">
              服务器开启了访问令牌校验，请在设置中填入访问令牌后重试。
            </span>
            <Button
              size="sm"
              variant="outline"
              className="h-7"
              onClick={() => setSettingsOpen(true)}
            >
              打开设置
            </Button>
          </div>
        )}
```

- [ ] **Step 4: 类型检查**

Run: `cd E:/gta/web && npx tsc -b`
Expected: 无错误

- [ ] **Step 5: Commit**

```bash
cd E:/gta && git add web/src/App.tsx && git commit -m "feat(web): 401 全局横幅 + 自动打开设置引导填入访问令牌"
```

---

### Task 9: 前端 — 开始抓包弹窗支持远程 agent 源

**Files:**
- Modify: `web/src/components/start-capture-dialog.tsx`
- Modify: `web/src/App.tsx`（按钮文案）

- [ ] **Step 1: 修改 `web/src/components/start-capture-dialog.tsx`**

1a. 组件注释与 `useState` 区（约 18-21 行）改为：

```tsx
/** 开始抓包对话框：本机网卡抓包 / 远程 agent 推流（移动代理抓包为常驻服务，见「代理服务器配置」页）。 */
export function StartCaptureDialog({ open, onClose, onStarted, onRunLinked }: StartCaptureDialogProps) {
  const [source, setSource] = useState<"nic" | "agent">("nic");
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  const [started, setStarted] = useState(false);
```

1b. `handleStart`（约 38-84 行）整体替换为：

```tsx
  function handleStart() {
    const p = parseInt(port, 10);
    // 仅本机网卡抓包要求端口（BPF 过滤用）；远程 agent 由 agent 侧自行过滤，端口可留空。
    if (source === "nic" && (!p || p <= 0)) return;
    start.mutate(
      {
        port: p > 0 ? p : 0,
        plugin: plugin || undefined,
        source,
      },
      {
        onSuccess: (data) => {
          const sessionId = data?.session_id ?? "";
          if (sessionId) onStarted?.(sessionId);
          toast.success("抓包会话已启动", `端口 ${port}${plugin ? ` · 插件 ${plugin}` : ""}`);
          // 自动开启行为窗口，与本次抓包会话联动（start_capture 已写入 current.json）。
          // 仅本机网卡抓包联动（有明确端口可生成 BPF 过滤）；agent 源的窗口由 agent 侧另行开启。
          if (source !== "nic") {
            setStarted(true);
            setTimeout(() => {
              setStarted(false);
              onClose();
            }, 800);
            return;
          }
          const dbPath = data?.db_path ?? "";
          const sessionDir = dbPath.replace(/[\\/][^\\/]+$/, "") || `session-${sessionId}`;
          begin.mutate(
            {
              featureName: plugin ? `capture-${plugin}` : "capture",
              projectPath: sessionDir,
              pluginName: plugin || undefined,
              port: p,
              filter: `tcp port ${p}`,
            },
            {
              onSuccess: (runData) => {
                if (runData?.run_id) {
                  onRunLinked?.(runData.run_id, runData.session_id ?? sessionId);
                  toast.success("已开启行为窗口", `run ${runData.run_id}`);
                }
              },
              onError: (err) => {
                // 抓包已成功，行为窗口失败仅告警，不阻断抓包。
                toast.info("行为窗口开启失败（抓包仍在进行）", err.message);
              },
            },
          );
          setStarted(true);
          setTimeout(() => {
            setStarted(false);
            onClose();
          }, 800);
        },
      },
    );
  }
```

1c. Dialog 标题与描述（约 90-92 行）改为：

```tsx
      title="开始抓包"
      description="本机网卡抓包，或由远程 agent 推流；移动代理抓包为常驻服务，请在「代理服务器配置」中查看连接二维码。"
```

1d. 在 `<div className="space-y-3">` 内、端口字段之前插入抓包源选择：

```tsx
        <div>
          <label className="text-sm font-medium">抓包源</label>
          <div className="mt-1.5 flex items-center gap-1 rounded-lg bg-muted p-1">
            {(
              [
                { id: "nic", label: "本机网卡" },
                { id: "agent", label: "远程 agent" },
              ] as const
            ).map((opt) => (
              <button
                key={opt.id}
                type="button"
                role="radio"
                aria-checked={source === opt.id}
                onClick={() => setSource(opt.id)}
                className={
                  "flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-[background-color,color] " +
                  (source === opt.id
                    ? "bg-card text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground")
                }
              >
                {opt.label}
              </button>
            ))}
          </div>
          {source === "agent" && (
            <p className="mt-1.5 text-xs text-muted-foreground">
              需在成员机运行 <code className="font-mono">gta-agent --server &lt;服务端&gt;:9091 --token &lt;令牌&gt;</code>
              ，agent 会抓取本机流量并推流到此会话（端口可留空）。
            </p>
          )}
        </div>
```

1e. 端口字段的 placeholder 与说明（约 116-125 行）改为：

```tsx
          <Input
            value={port}
            onChange={(e) => setPort(e.target.value)}
            aria-label="监听端口"
            inputMode="numeric"
            placeholder={source === "nic" ? "8080" : "可留空"}
            className="mt-1.5 font-mono"
          />
```

- [ ] **Step 2: `web/src/App.tsx` 顶部按钮文案**

把（约 237-247 行）：

```tsx
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={() => setStartOpen(true)}
              title="开始网卡抓包"
              aria-label="开始网卡抓包"
            >
              <Play className="h-4 w-4" />
              网卡抓包
            </Button>
```

改为：

```tsx
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={() => setStartOpen(true)}
              title="开始抓包（本机网卡 / 远程 agent）"
              aria-label="开始抓包"
            >
              <Play className="h-4 w-4" />
              开始抓包
            </Button>
```

- [ ] **Step 3: 类型检查**

Run: `cd E:/gta/web && npx tsc -b`
Expected: 无错误

- [ ] **Step 4: Commit**

```bash
cd E:/gta && git add web/src/components/start-capture-dialog.tsx web/src/App.tsx && git commit -m "feat(web): 开始抓包弹窗支持远程 agent 源（端口可留空，不联动行为窗口）"
```

---

### Task 10: 前端 — 会话侧栏 owner 徽标 + admin「只看我的」筛选

**Files:**
- Modify: `web/src/types/session.ts`
- Modify: `web/src/components/session-sidebar.tsx`

- [ ] **Step 1: `web/src/types/session.ts` 的 SessionInfo 加 owner**

在 `session_id: string;` 之前插入：

```ts
  /** 会话归属者（团队模式下的用户名；匿名/本地单机为空） */
  owner?: string;
```

- [ ] **Step 2: `web/src/components/session-sidebar.tsx` 接入筛选与徽标**

2a. imports 区加入：

```ts
import { useIdentity } from "@/hooks/use-auth";
```

2b. `SessionItem` 的 props 增加 owner 徽标标签。把签名（约 101-116 行）改为：

```tsx
function SessionItem({
  session,
  isSelected,
  onClick,
  onSwitch,
  onDeleted,
  liveStatus,
  ownerBadge,
}: {
  session: SessionInfo;
  isSelected: boolean;
  onClick: () => void;
  onSwitch?: (session: SessionInfo) => void;
  onDeleted?: (sessionId: string) => void;
  /** 仅对“当前选中会话”注入 get_session_status 实时态；其余为 undefined */
  liveStatus?: SessionStatusResult | null;
  /** 归属徽标文案（undefined = 不显示） */
  ownerBadge?: string;
}) {
```

2c. 在 `SessionItem` 头部行（`<Badge variant={isRunning ? "default" : "secondary"}` 之前）插入徽标。把头部行改为：

```tsx
      {/* 头部：状态点 + 开始时间 + 归属徽标 + Badge */}
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <span
            className={cn(
              "inline-block h-2 w-2 rounded-full shrink-0",
              isRunning ? "gta-live-dot" : "bg-muted-foreground/50",
            )}
          />
          <span className="text-sm font-medium truncate font-mono">
            {formatTime(session.started_at)}
          </span>
          {ownerBadge && (
            <span
              className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground"
              title={`归属：${session.owner}`}
            >
              {ownerBadge}
            </span>
          )}
        </div>
        <Badge variant={isRunning ? "default" : "secondary"} className="shrink-0">
          {isRunning ? "运行中" : "已停止"}
        </Badge>
      </div>
```

2d. `SessionSidebar` 组件接入身份与筛选。把（约 307-395 行）整体替换为：

```tsx
export function SessionSidebar({
  selectedSessionId,
  onSelectSession,
  onDeleted,
}: SessionSidebarProps) {
  const { data, isLoading, isError, error, refetch } = useSessions();
  // 仅对当前选中会话拉取 get_session_status（5s 轮询），使其统计与状态点保持“实时”。
  const liveStatus = useSessionStatus(selectedSessionId);
  const [switchTarget, setSwitchTarget] = useState<SessionInfo | null>(null);
  const identity = useIdentity();
  const [ownerView, setOwnerView] = useState<"all" | "mine">("all");

  const sessions = data?.sessions ?? [];

  // admin 可见态：身份头标记 admin，或列表里出现了非本人归属的会话（兜底）。
  // 出现他人会话说明当前身份能跨 owner 查看（服务端已按身份过滤）。
  const foreignOwners = sessions.some(
    (s) => !!s.owner && (!identity || s.owner !== identity.owner),
  );
  const showOwnerFilter = identity?.isAdmin === true || (identity !== null && foreignOwners);

  // 排序：运行中优先，然后按 started_at 倒序（最新的在上）
  const sortedSessions = [...sessions].sort((a, b) => {
    const aRunning = a.status === "running" ? 1 : 0;
    const bRunning = b.status === "running" ? 1 : 0;
    if (aRunning !== bRunning) return bRunning - aRunning;
    return b.started_at.localeCompare(a.started_at);
  });

  // 「只看我的」为纯前端过滤（服务端已按身份过滤，这里只是收窄视图）。
  const visibleSessions =
    ownerView === "mine" && identity
      ? sortedSessions.filter((s) => !s.owner || s.owner === identity.owner)
      : sortedSessions;

  const ownerBadgeOf = (s: SessionInfo): string | undefined => {
    if (!s.owner) return undefined;
    return identity && s.owner === identity.owner ? "我" : s.owner;
  };

  const runningCount = sessions.filter((s) => s.status === "running").length;

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {/* 标题 */}
      <div className="flex items-center justify-between gap-2 px-4 py-3 border-b">
        <h2 className="text-sm font-semibold">会话列表</h2>
        {data && (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
            {runningCount > 0 && <span className="gta-live-dot" />}
            {data.count} 个会话{runningCount > 0 && ` · ${runningCount} 运行`}
          </span>
        )}
      </div>

      {/* admin 视图筛选：默认「全部」 */}
      {showOwnerFilter && (
        <div className="flex items-center gap-1 border-b px-4 py-2">
          <span className="mr-1 text-xs text-muted-foreground">视图</span>
          {(
            [
              { id: "all", label: "全部" },
              { id: "mine", label: "只看我的" },
            ] as const
          ).map((opt) => (
            <button
              key={opt.id}
              type="button"
              aria-pressed={ownerView === opt.id}
              onClick={() => setOwnerView(opt.id)}
              className={
                "rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors " +
                (ownerView === opt.id
                  ? "bg-primary text-primary-foreground"
                  : "bg-muted text-muted-foreground hover:text-foreground")
              }
            >
              {opt.label}
            </button>
          ))}
        </div>
      )}

      {/* 会话列表 */}
      <ScrollArea className="flex-1 p-3">
        {isLoading && (
          <div className="space-y-3">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-24 w-full rounded-lg" />
            ))}
          </div>
        )}

        {isError && (
          <div
            role="alert"
            className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
          >
            <AlertTriangle className="h-4 w-4 shrink-0" />
            <span className="flex-1">加载失败：{error?.message ?? "未知错误"}</span>
            <Button variant="outline" size="sm" onClick={() => refetch()} className="h-7">
              <RotateCw className="h-3.5 w-3.5" />
              重试
            </Button>
          </div>
        )}

        {!isLoading && !isError && sessions.length === 0 && (
          <EmptyState
            icon={<Inbox className="h-5 w-5" />}
            title="暂无会话"
            hint="点击右上角「开始抓包」启动一次抓包会话，或启动插件进行离线解码。"
          />
        )}

        {!isLoading && !isError && sessions.length > 0 && visibleSessions.length === 0 && (
          <EmptyState
            icon={<Inbox className="h-5 w-5" />}
            title="该视图下暂无会话"
            hint="当前为「只看我的」视图，切换回「全部」查看团队其他成员的会话。"
          />
        )}

        <div className="space-y-2">
          {visibleSessions.map((session) => (
            <SessionItem
              key={session.session_id}
              session={session}
              isSelected={session.session_id === selectedSessionId}
              onClick={() => onSelectSession(session.session_id)}
              onSwitch={setSwitchTarget}
              onDeleted={onDeleted}
              ownerBadge={ownerBadgeOf(session)}
              liveStatus={
                session.session_id === selectedSessionId ? (liveStatus.data ?? null) : null
              }
            />
          ))}
        </div>
      </ScrollArea>

      {switchTarget && (
        <SwitchPluginDialog session={switchTarget} onClose={() => setSwitchTarget(null)} />
      )}
    </div>
  );
}
```

- [ ] **Step 3: 类型检查**

Run: `cd E:/gta/web && npx tsc -b`
Expected: 无错误

- [ ] **Step 4: Commit**

```bash
cd E:/gta && git add web/src/types/session.ts web/src/components/session-sidebar.tsx && git commit -m "feat(web): 会话侧栏显示归属徽标，admin 可切换「只看我的/全部」视图"
```

---

### Task 11: 前端 — 插件面板 owner 徽标

**Files:**
- Modify: `web/src/types/registered-plugin.ts`
- Modify: `web/src/components/plugin-panel.tsx`

- [ ] **Step 1: `web/src/types/registered-plugin.ts` 的 RegisteredPlugin 加 owner**

在 `last_heartbeat: number;` 之后加：

```ts
  /** 注册者（团队模式下的用户名；匿名/系统插件为 local） */
  owner?: string;
```

- [ ] **Step 2: `web/src/components/plugin-panel.tsx` 显示徽标**

2a. imports 区加入：

```ts
import { useIdentity } from "@/hooks/use-auth";
```

2b. `PluginPanel` 组件内（`const plugins = data?.plugins ?? EMPTY_PLUGINS;` 之后）加：

```tsx
  const identity = useIdentity();
```

2c. `PluginCard` 调用处（约 234-243 行）传入 owner 徽标，改为：

```tsx
        {plugins.map((p) => (
          <PluginCard
            key={p.instance_id}
            plugin={p}
            hotReloadAt={hotReloads[p.instance_id]}
            onDeregister={() => setConfirmTarget(p)}
            deregistering={deregister.isPending && confirmTarget?.instance_id === p.instance_id}
            onTest={() => handleTestClick(p.name)}
            ownerBadge={
              p.owner
                ? identity && p.owner === identity.owner
                  ? "我"
                  : p.owner
                : undefined
            }
          />
        ))}
```

2d. `PluginCard` 签名（约 850-862 行）加 prop：

```tsx
function PluginCard({
  plugin,
  hotReloadAt,
  onDeregister,
  deregistering,
  onTest,
  ownerBadge,
}: {
  plugin: RegisteredPlugin;
  hotReloadAt?: number;
  onDeregister: () => void;
  deregistering: boolean;
  onTest: () => void;
  /** 归属徽标文案（undefined = 不显示） */
  ownerBadge?: string;
}) {
```

2e. 在 `PluginCard` 内插件名 `</span>` 之后、在线 `Badge` 之前插入：

```tsx
          {ownerBadge && (
            <span
              className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground"
              title={`注册者：${plugin.owner}`}
            >
              {ownerBadge}
            </span>
          )}
```

- [ ] **Step 3: 类型检查**

Run: `cd E:/gta/web && npx tsc -b`
Expected: 无错误

- [ ] **Step 4: Commit**

```bash
cd E:/gta && git add web/src/types/registered-plugin.ts web/src/components/plugin-panel.tsx && git commit -m "feat(web): 插件面板显示注册者归属徽标"
```

---

### Task 12: 全量回归验证

**Files:** 无新改动，仅验证

- [ ] **Step 1: Go 全量测试**

Run: `cd E:/gta && go test ./pkg/auth/... ./cmd/gta-mcp/... 2>&1 | tail -5`
Expected: `ok` 两行（pkg/auth、cmd/gta-mcp）

- [ ] **Step 2: 前端测试 + 构建**

Run: `cd E:/gta/web && npm test && npm run build`
Expected: 测试全 PASS；`tsc -b` 无错误；vite build 成功

- [ ] **Step 3: 匿名模式回归核对（不启动任何 token）**

启动本地 gta-mcp + gta-pipeline（既有方式），打开前端，逐项确认：
1. DevTools Network：`POST /mcp` 请求头**没有** `Authorization`；
2. `/events/plugins` 连接 URL **没有** `token=` 查询参数；
3. 无 401 横幅、设置弹窗令牌为空；
4. 会话/插件卡片无归属徽标、无「视图：全部/只看我的」筛选行；
5. 会话列表内容与改造前一致。

- [ ] **Step 4: token 模式联测（GTA_AUTH_TOKENS）**

用 `GTA_AUTH_TOKENS=alice=gta_tok_aaa:admin,bob=gta_tok_bbb` 启动服务端，逐项确认：
1. 无 token 打开前端 → 出现 401 横幅 + 设置弹窗自动打开；
2. 设置中填 `gta_tok_bbb` 保存 → 横幅消失、列表恢复、显示身份 bob（非 admin，无筛选行）；
3. DevTools：`/mcp` 请求带 `Authorization: Bearer gta_tok_bbb`；`/events/plugins?token=gta_tok_bbb` 连接正常（插件上下线仍实时刷新）；
4. 换 `gta_tok_aaa`（admin）→ 出现「全部/只看我的」筛选；用 bob 的账号启动一个抓包会话后，alice 在「全部」视图能看到 bob 会话并带 `bob` 徽标，「只看我的」则隐藏之；
5. 开始抓包弹窗选「远程 agent」→ 启动成功（会话出现在列表、source 为 agent）；在成员机运行 `gta-agent --server <host>:9091 --token gta_tok_bbb` 后流量入库。

- [ ] **Step 5: Commit（如联测中有修补）**

```bash
cd E:/gta && git add -A && git commit -m "fix(web): 团队模式联测修补"
```
（无修补则跳过此步。）

---

## Self-Review 记录

- **Spec 覆盖**：token 注入/存储/401（Task 4/5/7/8）、SSE `?token=`（Task 1 后端 + Task 6 前端）、身份回显（Task 1 + Task 5 + Task 6 hooks + Task 7 展示）、agent 源（Task 9）、owner 徽标与 admin 筛选（Task 3 后端 + Task 10/11 前端）、CORS expose（Task 2）、匿名回归（Task 12 Step 3）——全覆盖。
- **占位符扫描**：无 TBD/TODO；所有代码步骤给出完整代码。
- **类型一致性**：`HeaderOwner`/`HeaderAdmin`（Task 1 定义，Task 2 测试引用）；`authHeaders`/`withTokenParam`/`setToken`/`setIdentity`（Task 4 定义，Task 5/6/7 使用）；`useAuthToken`/`useIdentity`/`useAuthError`（Task 6 定义，Task 8/10/11 使用）；`owner?: string` 字段名前后端一致（后端输出 `owner`，前端 `SessionInfo.owner`/`RegisteredPlugin.owner`）。
