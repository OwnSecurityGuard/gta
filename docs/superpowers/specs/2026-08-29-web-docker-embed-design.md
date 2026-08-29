# Web 前端内嵌进 gta-mcp / Docker 集成 — 设计文档

日期：2026-08-29
状态：已确认（内嵌方案，用户已批准）

## 背景

团队部署目前只有 Go 双服务（`docker-compose.yml`：pipeline + mcp 共用 `gta-server` 镜像、共卷 `gta-data`，mcp 的 8781 对外）。Web 前端（`web/`，React 19 + Vite）不在部署面内：`.dockerignore` 排除了 `web/`，`gta-mcp` 无任何静态文件能力，成员想用 Web 只能各自 `npm run dev` 走 vite 代理。

前端所有请求均为相对路径（`/mcp`、`/events/plugins`），因此**只要静态资源与 MCP API 同源，前端代码零改动、零 CORS 配置即可工作**。后端已支持 `Authorization: Bearer` 与 `?token=` 查询参数回退（为 EventSource 设计），Web 端设置弹窗可填 token。

**选定方案：`go:embed` 内嵌静态资源进 `gta-mcp`**。浏览器直接访问 `http://<host>:8781`，不加容器、不加端口；单二进制自包含（裸机部署同样自带前端）。被否决的备选：独立 nginx 反代容器（多容器多端口，SSE 反代需调缓冲）。

## 目标与非目标

**目标**

1. `docker compose up -d --build` 后浏览器打开 `http://<host>:8781` 即可用 Web UI（含 token 输入流程）。
2. 裸机 `make release-matrix` 产物同样内嵌前端。
3. 未构建前端时 `go build ./...` 依旧成功（现有开发流、CI 不被破坏）。
4. 匿名模式与 token 模式行为与此前完全一致：API 鉴权链不变；静态资源免鉴权。

**非目标**

- SPA 客户端路由 / history fallback（前端无路由）。
- RAW_DEBUG 的运行期开关（`VITE_ENABLE_RAW_DEBUG` 为 Vite 构建期静态替换；镜像提供 build arg，默认关闭）。
- CI 增加 docker build/push job、镜像 registry 约定（维持 compose 本地 `--build` 模式）。
- compose 增加服务或端口。

## 设计

### 1. 静态资源与路由（cmd/gta-mcp）

- 新建 `cmd/gta-mcp/webui/` 目录（git 仅跟踪 `.gitkeep`），`//go:embed all:webui` 嵌入；`gta-mcp` 挂静态 handler（新文件 `cmd/gta-mcp/webui.go`）。
- **未构建兜底**：handler 探测嵌入 FS 中是否存在 `index.html`——不存在（未跑过 web-build）时 `/` 返回 Go 内置的提示文本（200 text/html，"Web UI 未构建，运行 make web-build"），保证无 dist 时 `go build ./...` 成功且行为可解释。
- **静态资源挂鉴权链之外**，与 `/singbox/profile` 豁免同模式：浏览器必须能免 token 取到 index.html/js 才能弹出令牌输入框；静态资源不含敏感数据。现有 API 路由（`/mcp`、`/sse`、`/message`、`/events/plugins`）原封不动留在鉴权链内；`http.ServeMux` 精确路径注册优先于 `/` 通配，静态 handler 只会收到 API 之外的路径，互不干扰。
- 无 SPA fallback：`/` 与 `/assets/*` 按文件服务（`/` 显式回 index.html），不存在路径 404。
- 缓存策略：`assets/` 下产物带内容 hash，响应 `Cache-Control: public, max-age=31536000, immutable`；`index.html` 与未构建兜底文本为 `no-cache`（保证发版即生效）。

### 2. 构建链

- `web/vite.config.ts`：`build.outDir` 改为 `../cmd/gta-mcp/webui` 并显式 `emptyOutDir: true`（outDir 在 root 之外，vite 默认会警告；产物目录被清空重建不影响 git——目录内 tracked 的只有 `.gitkeep`，需在 emptyOutDir 后由构建脚本恢复，见下）。
- `.gitignore`：加 `cmd/gta-mcp/webui/*` 与 `!cmd/gta-mcp/webui/.gitkeep`（构建产物不进 git；`web/dist` 原有忽略规则保留亦可）。
- **产物构建不直接写 webui/**：vite 产物先出在 `web/dist`（保持 vite 默认习惯、gitignore 已覆盖），由 `web-build` 脚本把 `web/dist` 内容同步进 `cmd/gta-mcp/webui/`（清除旧产物后复制，并确保 `.gitkeep` 仍在）。避免 vite 清空 outDir 时误删 tracked 的 `.gitkeep`，也让"构建产物污染 git 工作区"不可能发生。
- Makefile：新增 `web-build` target（`cd web && npm ci && npm run build` + 同步产物到 `cmd/gta-mcp/webui/`）；`release-matrix` 依赖 `web-build`（前置执行）。
- `Dockerfile`：新增 node 构建阶段（`node:22-bookworm-slim`，`npm ci && npm run build`），产物 `COPY --from` 进 Go builder 阶段的 `cmd/gta-mcp/webui/` 后再编译（embed 进二进制，runtime 阶段零新增文件）。新增 `ARG VITE_ENABLE_RAW_DEBUG=0` 传给 node 阶段。
- `.dockerignore`：移除对 `web/` 的整目录排除，改为排除 `web/node_modules`、`web/dist`（node 阶段自建依赖与产物）。

### 3. compose / .env

- compose 不加服务、不加端口；`mcp` 服务 8781 映射沿用。build args 透传 `VITE_ENABLE_RAW_DEBUG`（默认 0）。
- `.env.example`：补 `VITE_ENABLE_RAW_DEBUG` 注释项与"浏览器访问 8781"说明。

### 4. 测试与验证

- Go 单测（httptest，覆盖两种鉴权模式）：
  - `/` 返回 200 text/html——未构建（webui 只有 `.gitkeep`）时为内置提示文本；
  - `/assets/<不存在>` 返回 404；`/mcp` 不受静态 handler 影响（token 模式无凭证 401、有凭证 200）；
  - 匿名模式：`/` 可访问（免鉴权红线）、API 照旧放行。
- Docker 冒烟（文档记录 + 实测一次）：`docker compose up --build` 后 curl `/`（200 text/html）、`/mcp`（401/200 按凭证）、`/events/plugins?token=`（200 text/event-stream）。
- 文档：`docs/team-deployment.md` 新增"浏览器访问 Web UI"一节（地址、token 输入入口、401 横幅引导）；`docs/member-onboarding.md` 补一句 Web 入口。

## 相关文件

- 新增：`cmd/gta-mcp/webui/.gitkeep`、`cmd/gta-mcp/webui.go`（embed + 静态 handler + 未构建兜底）
- 修改：`cmd/gta-mcp/main.go`（路由挂载）、`web/vite.config.ts`、`.gitignore`、`.dockerignore`、`Dockerfile`、`Makefile`、`docker-compose.yml`、`.env.example`、`docs/team-deployment.md`、`docs/member-onboarding.md`
- 测试：`cmd/gta-mcp/webui_test.go`
