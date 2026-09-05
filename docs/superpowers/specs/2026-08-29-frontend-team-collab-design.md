# 前端对齐团队协作改造 — 设计文档

日期：2026-08-29
状态：已确认（方案 A，用户已批准）

## 背景

团队协作改造（`docs/plans/2026-08-29-team-collaboration-design.md`）为后端引入了：

- **共享服务端**：团队共用一套 `gt-pipeline` + `gt-mcp`（Docker Compose 部署）。
- **鉴权**：`GT_AUTH_TOKENS=alice=gt_tok_xxx:admin,bob=gt_tok_yyy`；未配置即匿名模式（owner 空、全放行，行为与旧单机一致）。HTTP `auth.Middleware` 与 gRPC 拦截器解析 `Authorization: Bearer <token>`，注入 `Principal{Owner, IsAdmin}`。
- **owner 隔离**：会话/插件按 owner 过滤（admin 可见全部），服务端完成过滤，无需客户端传参。
- **agent 抓包源**：`start_capture` 新增 `source=agent`（远程 `gt-agent` 推流）。

前端（`web/`，React 19 + TS + Vite + Tailwind 4 + TanStack Query，经 `src/lib/mcp-client.ts` 调 MCP `POST /mcp`）完全未跟上：无 token 概念（token 模式下全部 401）、抓包源硬编码 `nic`、无 owner 展示。本轮补齐前端功能，不做结构重构。

## 目标与非目标

**目标**

1. token 模式下前端完全可用：token 输入/存储、请求头注入、401 处理、SSE 鉴权。
2. 支持远程 agent 抓包源。
3. 展示会话/插件归属（owner），admin 可全量查看。
4. 匿名模式行为与现状完全一致（回归底线）。

**非目标**

- 登录页、路由化、状态库引入。
- 后端新增校验/whoami 端点。
- 前端结构重构。

## 设计

### 1. 身份与 token

- 新增 `web/src/lib/auth.ts`：
  - token 存取：localStorage 键 `gt_auth_token`（与现有服务器地址设置同模式）。
  - `authHeaders()`：有 token 返回 `{ Authorization: "Bearer <token>" }`，无 token 返回空对象。
- `McpClient` 每次 `POST /mcp` 附带上述头。无 token 时不带头（匿名模式零变化）。
- HTTP 401 或 JSON-RPC 鉴权错误 → 抛专门的 `AuthError`；App 层全局捕获：顶部显示"需要访问令牌"横幅，点击自动打开设置弹窗。
- 设置弹窗（`settings-dialog.tsx`，现有服务器地址设置处）新增"访问令牌"输入：password 型、可清空；保存后立即生效并刷新所有 Query。
- **后端身份回显**：`auth.Middleware` 对 `/mcp` 响应附加 `X-GT-Owner`（匿名时为空/省略）与 `X-GT-Admin`（admin 时 `true`）响应头。前端从任意一次调用的响应头读取当前身份，不新增 whoami 端点。

### 2. SSE 鉴权

- **后端**（`pkg/auth` http.go）：`auth.Middleware` 在 Authorization 头缺失/无法解析时，回退解析查询参数 `?token=`，成功则同样注入 Principal。仅此一处回退，其余端点语义不变。
- **前端**：`/events/plugins` 连接 URL 在有 token 时拼 `?token=`；错误提示与日志不回显完整 token（截断/掩码）。

### 3. agent 抓包源

- `start-capture-dialog.tsx` 的抓包源改为选择项：
  - 本机网卡（`source=nic`，现有表单与逻辑不变）；
  - 远程 agent（`source=agent`）。
- 选 agent 时展示说明文案（需在成员机上运行 `gt-agent` 连接服务端）；其余参数沿用现有表单。

### 4. owner 展示与 admin 视图

- **后端**：`list_all_sessions` 输出 map 增加 `"owner": sess.Owner`（插件列表 `list_registered_plugins` 已回传 `owner`，无需改动）。
- 会话侧栏（`session-sidebar.tsx`）与插件面板（`plugin-panel.tsx`）：数据返回带非空 `owner` 字段时显示归属徽标（用户名缩写）。
- 当前身份：从 `X-GT-Owner` 响应头读取并展示；无 token / 匿名时不显示。
- admin 视图：`X-GT-Admin: true`，或（头不可用时的兜底）会话列表出现非本人 owner 的会话 → 侧栏出现"只看我的 / 全部"本地筛选，默认"全部"。过滤纯前端完成（服务端已按身份过滤）。

### 错误处理

- 401 → `AuthError` → 横幅 + 引导设置 token；token 被撤销时同样触发。
- 网络错误沿用现有处理；SSE 断线沿用 EventSource 自动重连（重连 URL 始终重新拼接当前 token）。

### 测试

- 前端单测：`auth.ts`（存取/清空/headers）、`mcp-client`（带头/不带头/401 → AuthError）。
- 后端单测：`auth.Middleware` 的 `?token=` 回退（合法 token 注入、非法拒绝、匿名模式不受影响、头优先于查询参数）与 `X-GT-Owner` / `X-GT-Admin` 响应头；`list_all_sessions` 输出含 `owner` 字段。
- 回归验证：不配置 token 时，前端请求头、SSE URL、会话列表与现状一致。

## 相关文件

- 后端：`pkg/auth/http.go`（`?token=` 回退 + 身份响应头，+测试）、`cmd/gt-mcp/main.go`（`list_all_sessions` 输出补 `owner`）
- 前端：`web/src/lib/auth.ts`（新增）、`web/src/lib/mcp-client.ts`、`web/src/App.tsx`、`web/src/components/settings-dialog.tsx`、`web/src/components/start-capture-dialog.tsx`、`web/src/components/session-sidebar.tsx`、`web/src/components/plugin-panel.tsx`
