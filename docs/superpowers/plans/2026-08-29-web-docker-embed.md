# Web 前端内嵌 gta-mcp / Docker 集成 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Web 前端以 `go:embed` 内嵌进 gta-mcp 并打通 Docker 构建链，浏览器直接访问 `http://<host>:8781` 即可用 Web UI。

**Architecture:** `cmd/gta-mcp/webui.go` 提供 embed FS 上的静态 handler，以"命中嵌入文件才返回静态、其余原样进鉴权链"的方式与现有 authed handler 组合（静态免鉴权、API 语义零变化）；构建链为 vite 产物（`web/dist`）→ `make web-build` 同步到 `cmd/gta-mcp/webui/`（git 只跟踪 `.gitkeep`）/ Dockerfile 新增 node 阶段 → COPY 进 builder 后编译。

**Tech Stack:** Go 1.25（`embed`、`io/fs`、`http.FileServerFS`、`testing/fstest`）；Node 22 + npm（vite 6 build）；Docker 多阶段；Makefile；docker-compose。

**Spec:** `docs/superpowers/specs/2026-08-29-web-docker-embed-design.md`

**关键事实（已核实，勿重复调查）：**
- `cmd/gta-mcp/main.go` 约 2890 行的路由组装是：`root := http.NewServeMux(); root.HandleFunc("/singbox/profile", ...); root.Handle("/", authed); handler := http.Handler(root)`。`authed = corsMiddleware(allowedOrigins, authMiddleware(resolver, mux))`（`cmd/gta-mcp/http_server.go:63`），内层 `mux` 精确注册 `/sse` `/message` `/mcp` `/events/plugins`，未知路径落到 mux 的 404。
- `.dockerignore` 现在整目录排除 `web/`（第 20 行）——node 阶段需要 web 源码进上下文，必须改。
- 根 `.gitignore` 已有 `dist/`（第 42 行，全局）与 `web/dist/`（第 58 行）；`cmd/gta-mcp/webui/` 目前不存在、也不被忽略。
- Makefile：`release-matrix` 是 `.PHONY` target（第 97 行），CI release job（`.github/workflows/ci.yml:59-88`，ubuntu-latest + setup-go）调用它；ubuntu runner 自带 node，但计划仍显式加 `actions/setup-node@v4` 保证版本确定。
- `web/package.json` 的 `build = tsc -b && vite build`，产物在 `web/dist`（gitignore 已覆盖）。前端请求全部相对路径（`/mcp`、`/events/plugins`），同源即工作。
- 测试助手 `mustResolver(t, spec)` 在 `pkg/auth/interceptor_test.go`；`buildHTTPHandler(allowedOrigins, resolver, mux)` 在 `cmd/gta-mcp/http_server.go`。

---

### Task 1: webui 嵌入目录 + 静态 handler（含未构建兜底）

**Files:**
- Create: `cmd/gta-mcp/webui/.gitkeep`（空文件）
- Create: `cmd/gta-mcp/webui.go`
- Create: `cmd/gta-mcp/webui_test.go`
- Modify: `cmd/gta-mcp/main.go`（root mux 的 `"/"` 注册处，约 2890 行）

- [ ] **Step 1: 创建嵌入目录与空 `.gitkeep`**

```bash
cd E:/gta && mkdir -p cmd/gta-mcp/webui && touch cmd/gta-mcp/webui/.gitkeep
```

（`//go:embed` 要求目录至少含一个文件；用 `all:webui` 前缀才能包含点开头文件，见 Step 3。）

- [ ] **Step 2: 写失败测试 `cmd/gta-mcp/webui_test.go`**

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"gta/pkg/auth"
)

// builtFS 模拟"已执行 make web-build"的嵌入目录（vite 产物形态）。
func builtFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte("<html>index</html>")},
		"assets/app-1.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

// emptyFS 模拟"未构建"：只有 .gitkeep（仓库里 tracked 的状态）。
func emptyFS() fstest.MapFS {
	return fstest.MapFS{
		".gitkeep": &fstest.MapFile{Data: []byte("")},
	}
}

// apiHandler 记录是否被兜底调用（＝请求交回了鉴权链）。
type fallbackProbe struct {
	called bool
	path   string
}

func (p *fallbackProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.called = true
	p.path = r.URL.Path
	w.WriteHeader(http.StatusTeapot)
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestWebUI_IndexServedNoCache 验证 "/" 返回 index.html 且 no-cache（发版即生效）。
func TestWebUI_IndexServedNoCache(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(builtFS(), probe)
	rec := doGet(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); body != "<html>index</html>" {
		t.Fatalf("GET / 应返回 index.html 内容，实际 %q", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index 应 no-cache，实际 %q", cc)
	}
	if probe.called {
		t.Fatal("命中静态资源时不得兜底到鉴权链")
	}
}

// TestWebUI_IndexExplicitPath 验证显式 /index.html 同样命中。
func TestWebUI_IndexExplicitPath(t *testing.T) {
	t.Parallel()
	h := serveWebOrAPI(builtFS(), &fallbackProbe{})
	if rec := doGet(t, h, "/index.html"); rec.Code != http.StatusOK || rec.Body.String() != "<html>index</html>" {
		t.Fatalf("GET /index.html 应 200 且返回 index 内容，实际 %d %q", rec.Code, rec.Body.String())
	}
}

// TestWebUI_AssetImmutable 验证带 hash 的 assets 产物用长缓存。
func TestWebUI_AssetImmutable(t *testing.T) {
	t.Parallel()
	h := serveWebOrAPI(builtFS(), &fallbackProbe{})
	rec := doGet(t, h, "/assets/app-1.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("GET /assets/app-1.js 应 200，实际 %d %q", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("assets 应 immutable，实际 %q", cc)
	}
}

// TestWebUI_UnknownPathFallsThrough 验证未命中嵌入文件的路径原样进鉴权链
// （保持与静态集成前完全一致的 401/404 语义）。
func TestWebUI_UnknownPathFallsThrough(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(builtFS(), probe)
	rec := doGet(t, h, "/no-such-path")
	if !probe.called || probe.path != "/no-such-path" {
		t.Fatal("未知路径应兜底到鉴权链")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("应透传兜底 handler 的响应，实际 %d", rec.Code)
	}
}

// TestWebUI_APIGoesThroughAuth 端到端验证 API 路由不被静态吞掉：
// token 模式下无凭证 401、带凭证 200（与静态集成前一致）。
func TestWebUI_APIGoesThroughAuth(t *testing.T) {
	t.Parallel()
	inner := http.NewServeMux()
	inner.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	authed := buildHTTPHandler(nil, mustResolver(t, "alice=gta_aaa"), inner)
	h := serveWebOrAPI(builtFS(), authed)

	rec := doGet(t, h, "/mcp")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭证 GET /mcp 应 401，实际 %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer gta_aaa")
		return req
	}())
	if rec2.Code != http.StatusOK {
		t.Fatalf("带凭证 GET /mcp 应 200，实际 %d", rec2.Code)
	}
}

// TestWebUI_PlaceholderWithoutIndex 验证未构建时 "/" 返回内置提示页（200、
// no-cache、含"未构建"字样），其余路径仍兜底到鉴权链。
func TestWebUI_PlaceholderWithoutIndex(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(emptyFS(), probe)
	rec := doGet(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("未构建时 GET / 应 200 提示页，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); !contains(body, "未构建") {
		t.Fatalf("提示页应包含『未构建』，实际 %q", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("提示页应 no-cache，实际 %q", cc)
	}
	if rec2 := doGet(t, h, "/assets/app-1.js"); !probe.called {
		t.Fatal("未构建时静态路径也应兜底到鉴权链（无产物可服务）")
	}
	_ = rec2
}

// contains 是 strings.Contains 的本地别名，避免仅为一处使用导入 strings。
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// 编译期防呆：确认 auth 包仍被引用（buildHTTPHandler 测试用到 mustResolver 所在包语义）。
var _ = auth.AnonymousOwner
```

（说明：`contains/indexOf` 两个手写助手只是为了让测试文件少一个 import——若你觉得别扭，直接 `import "strings"` 用 `strings.Contains` 更常规，**推荐直接用 strings.Contains 并删掉这两个助手与 `var _ = auth.AnonymousOwner`**；保留 `auth` import 与 `mustResolver` 即可。）

- [ ] **Step 3: 运行测试确认失败**

Run: `cd E:/gta && go test ./cmd/gta-mcp/ -run TestWebUI -v`
Expected: 编译失败，`undefined: serveWebOrAPI`

- [ ] **Step 4: 创建 `cmd/gta-mcp/webui.go`**

```go
package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webui 是前端构建产物目录（make web-build / Dockerfile webui 阶段生成）。
// 仓库只跟踪 .gitkeep：go:embed 要求目录非空，all: 前缀让点开头文件也被嵌入。
//go:embed all:webui
var webUIEmbed embed.FS

// webUIPlaceholderHTML 是未构建前端的兜底页（webui 里没有 index.html 时）。
// 保证"任何情况下 go build ./... 都成功"——未构建也能起服务，给出可操作的提示。
const webUIPlaceholderHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>GTA Web UI</title></head>
<body style="font-family: system-ui, sans-serif; padding: 2rem;">
<h1>GTA Web UI 未构建</h1>
<p>当前二进制未嵌入前端静态资源。请在仓库根目录执行 <code>make web-build</code> 后重新编译（或重新构建 Docker 镜像）。</p>
</body>
</html>`

// mustWebUIFS 返回嵌入的 webui 子文件系统。embed 指令保证目录存在，
// 失败只可能是编译环境异常，panic 比静默 500 更早暴露问题。
func mustWebUIFS() fs.FS {
	sub, err := fs.Sub(webUIEmbed, "webui")
	if err != nil {
		panic("webui embed broken: " + err.Error())
	}
	return sub
}

// serveWebOrAPI 把 Web UI 静态资源（免鉴权）与既有鉴权链组合到同一个 "/":
// 命中嵌入文件（或未构建兜底）才返回静态，其余请求原样交给 authed——
// /mcp、/sse、/message、/events/plugins 的鉴权语义与静态集成前完全一致。
// 静态资源免鉴权与 /singbox/profile 豁免同理由：浏览器必须能免 token 拿到
// index.html 才能弹出令牌输入框，而静态资源不含敏感数据。
// fsys 与 authed 均为参数注入，便于用 fstest.MapFS 单测。
func serveWebOrAPI(fsys fs.FS, authed http.Handler) http.Handler {
	fileServer := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeMux 已对脏路径做 301 清理，这里再防御性清理一次；
		// io/fs 的路径不允许 "."/".."，fs.Stat 对其只会报错（等于未命中）。
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

		// "/" 与 /index.html：有产物则回 index.html（no-cache，发版即生效），
		// 无产物则回内置提示页。
		if name == "" || name == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
			if _, err := fs.Stat(fsys, "index.html"); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(webUIPlaceholderHTML))
			return
		}

		// 其余路径：命中嵌入文件（且非目录）才服务静态。
		if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
			// vite 产物文件名带内容 hash，可长缓存；其他文件 no-cache。
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		authed.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd E:/gta && go test ./cmd/gta-mcp/ -run TestWebUI -v -count=1`
Expected: 6 个测试全 PASS

- [ ] **Step 6: 在 main.go 挂载**

在 `cmd/gta-mcp/main.go` 找到（约 2889-2891 行）：

```go
	root := http.NewServeMux()
	root.HandleFunc("/singbox/profile", capture.handleSingboxProfile)
	root.Handle("/", authed)
```

改为：

```go
	root := http.NewServeMux()
	root.HandleFunc("/singbox/profile", capture.handleSingboxProfile)
	// Web UI 静态资源（免鉴权）兜在 "/" 上：命中嵌入文件才返回静态，其余请求
	// （含 /mcp 等 API 与未知路径）原样进入鉴权链，语义与集成前一致。
	root.Handle("/", serveWebOrAPI(mustWebUIFS(), authed))
```

- [ ] **Step 7: 回归验证**

Run: `cd E:/gta && go build ./... && go test ./cmd/gta-mcp/ -count=1 2>&1 | tail -2`
Expected: 编译通过、包测试 ok

- [ ] **Step 8: Commit**

```bash
cd E:/gta && git add cmd/gta-mcp/webui/.gitkeep cmd/gta-mcp/webui.go cmd/gta-mcp/webui_test.go cmd/gta-mcp/main.go && git commit -m "feat(mcp): 内嵌 Web UI 静态资源（go:embed），未构建时返回内置提示；静态免鉴权、API 鉴权语义不变"
```

---

### Task 2: Makefile `web-build` + `.gitignore`

**Files:**
- Modify: `Makefile`（.PHONY 行、release-matrix 之前、release-matrix 依赖）
- Modify: `.gitignore`

- [ ] **Step 1: `.gitignore` 追加 webui 忽略规则**

在根 `.gitignore` 的 "Frontend build artifacts" 段（`web/.vite/` 之后）追加：

```gitignore

# gta-mcp 内嵌 Web UI 产物（make web-build / Docker webui 阶段生成；仅 .gitkeep 入库）
cmd/gta-mcp/webui/*
!cmd/gta-mcp/webui/.gitkeep
```

- [ ] **Step 2: Makefile 新增 web-build 并挂到 release-matrix**

2a. `.PHONY` 行（第 1 行）改为（追加 `web-build`）：

```make
.PHONY: proto test web-build build build-mcp build-pipeline build-plugin-dev build-agent build-examples run-mcp run-pipeline run-plugin-dev release release-matrix docs
```

2b. 在 `release-matrix` target 之前插入：

```make
# web-build：构建前端并把产物同步进 cmd/gta-mcp/webui/（go:embed 嵌入目录）。
# 产物出在 web/dist（vite 默认、已被 gitignore），同步时清掉 webui 下旧产物但
# 保留 .gitkeep（go:embed 要求目录非空、且它是仓库唯一 tracked 文件）。
web-build:
	set -e; \
	cd web && npm ci && npm run build; \
	cd ..; \
	find cmd/gta-mcp/webui -mindepth 1 ! -name .gitkeep -delete; \
	cp -r web/dist/. cmd/gta-mcp/webui/; \
	echo "webui assets:"; ls cmd/gta-mcp/webui
```

2c. `release-matrix:` 行（第 97 行）改为（加前置依赖，release 产物内嵌前端）：

```make
release-matrix: web-build
```

- [ ] **Step 3: 验证**

Run: `cd E:/gta && make web-build 2>&1 | tail -8`
Expected: npm ci/build 成功，列出的 webui 目录含 `index.html`、`assets/` 等

Run: `cd E:/gta && git status --short web/ cmd/gta-mcp/webui/`
Expected: 干净（产物全被忽略，`.gitkeep` 仍在）

Run: `cd E:/gta && go run ./cmd/gta-mcp -work-dir "$(mktemp -d)" -addr 127.0.0.1:0 & sleep 8` 后按 Task 12 记录的方式从 `<workdir>/addr.mcp.json` 拿地址，`curl -s http://<addr>/ | head -c 200` → 应返回真实前端 index.html（含 `<div id="root">` 或 vite 产物特征），`curl -s -o /dev/null -w "%{http_code}" -X POST http://<addr>/mcp` → 401（未配 token 时应为 200——注意：本地起服务若没配 GTA_AUTH_TOKENS 则 /mcp 是 200，此时验证的是"API 不被静态吞掉"即可）。验证完 kill 进程。

（若 `go run` 前台挂住影响执行，用后台 + kill；与 Task 12 冒烟同款手法。）

- [ ] **Step 4: Commit**

```bash
cd E:/gta && git add Makefile .gitignore && git commit -m "build: 新增 make web-build（vite 产物同步进 webui embed 目录），release-matrix 前置依赖它"
```

---

### Task 3: `.dockerignore` + Dockerfile webui 阶段 + compose/.env

**Files:**
- Modify: `.dockerignore`（第 20 行 `web/`）
- Modify: `Dockerfile`（头部注释、builder 前、builder 内 COPY）
- Modify: `docker-compose.yml`（pipeline 的 build.args）
- Modify: `.env.example`（GTA_MCP_PORT 注释、VITE_ENABLE_RAW_DEBUG）

- [ ] **Step 1: `.dockerignore` 放开 web 源码**

把第 20 行的 `web/` 替换为：

```gitignore
web/node_modules/
web/dist/
```

（node 阶段需要 web 源码进上下文；依赖与产物在阶段内自建。其余行不动。）

- [ ] **Step 2: Dockerfile 加 webui 阶段**

2a. 在 "阶段 1：builder" 的 `FROM golang:1.25-bookworm AS builder` 之前插入：

```dockerfile
# ============================================================================
# 阶段 0：webui（前端静态资源 → go:embed 进 gta-mcp）
# ============================================================================
FROM node:22-bookworm-slim AS webui

# npm 镜像：与 Go 侧 GOPROXY 同取向（境内直连 registry.npmjs.org 常失败）。
# 境外环境可覆盖：docker build --build-arg NPM_REGISTRY=https://registry.npmjs.org
ARG NPM_REGISTRY=https://registry.npmmirror.com
RUN npm config set registry ${NPM_REGISTRY}

WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# 前端构建期门控（Vite 静态替换进产物）：默认关闭原始包调试面板。
ARG VITE_ENABLE_RAW_DEBUG=0
ENV VITE_ENABLE_RAW_DEBUG=${VITE_ENABLE_RAW_DEBUG}
RUN npm run build
# 产物位于 /web/dist
```

2b. 在 builder 阶段的 `COPY . .`（第 42 行）之后、`RUN CGO_ENABLED=1 ...` 之前插入：

```dockerfile
# 前端产物覆盖进 embed 目录（webui/.gitkeep 会一并保留，无碍），gta-mcp 编译时嵌入。
COPY --from=webui /web/dist/ ./cmd/gta-mcp/webui/
```

2c. 头部注释块（第 1-12 行）末尾追加一行说明：

```dockerfile
# webui 阶段构建前端静态资源，编译期以 go:embed 嵌入 gta-mcp（浏览器直接访问 8781）。
```

- [ ] **Step 3: compose 透传前端 build arg**

`docker-compose.yml` 的 pipeline 服务 `build.args`（第 20-22 行）追加一行：

```yaml
    build:
      context: .
      args:
        VERSION: ${GTA_VERSION:-dev}
        GIT_COMMIT: ${GTA_GIT_COMMIT:-unknown}
        # 前端构建期门控（嵌入 gta-mcp 的 Web UI）：默认关；调试原始包面板时置 1 重建镜像
        VITE_ENABLE_RAW_DEBUG: ${VITE_ENABLE_RAW_DEBUG:-0}
```

- [ ] **Step 4: `.env.example` 更新**

4a. `GTA_MCP_PORT` 的注释（第 22 行）改为：

```bash
# MCP HTTP/SSE（团队客户端连接）；同一端口同时提供 Web UI（浏览器打开 http://<host>:8781）
GTA_MCP_PORT=8781
```

4b. 在 "CORS / 版本注入" 段（`GTA_GIT_COMMIT=unknown` 之后）追加：

```bash

# 前端构建期门控（嵌入 Web UI 的「原始包」调试面板；Vite 静态替换进产物，
# 改动后需 docker compose build 重建镜像才生效）
VITE_ENABLE_RAW_DEBUG=0
```

- [ ] **Step 5: Docker 构建冒烟（本机 docker 可用时执行；不可用则记录跳过、留 Task 6）**

Run: `cd E:/gta && docker build --build-arg VITE_ENABLE_RAW_DEBUG=0 -t gta-server:webui-test . 2>&1 | tail -5`
Expected: 全阶段成功（含 webui 的 npm ci/build 与 Go 编译）

- [ ] **Step 6: Commit**

```bash
cd E:/gta && git add .dockerignore Dockerfile docker-compose.yml .env.example && git commit -m "build(docker): webui 阶段构建前端并嵌入 gta-mcp；compose/.env 透传 VITE_ENABLE_RAW_DEBUG"
```

---

### Task 4: CI release job 安装 node

**Files:**
- Modify: `.github/workflows/ci.yml`（release job，第 64-72 行附近）

- [ ] **Step 1: 在 release job 的 setup-go 之后、Build release matrix 之前插入**

```yaml
      # release-matrix 现以前置依赖 make web-build 构建前端（嵌入 gta-mcp）。
      - uses: actions/setup-node@v4
        with:
          node-version: "22"
          cache: npm
          cache-dependency-path: web/package-lock.json
```

（`build-test` job 不需要 node：未构建前端时 `go build` 走 webui.go 的占位兜底，照样编译通过。）

- [ ] **Step 2: 验证**

Run: `cd E:/gta && git diff --stat`
Expected: 仅 ci.yml 变更

- [ ] **Step 3: Commit**

```bash
cd E:/gta && git add .github/workflows/ci.yml && git commit -m "ci: release job 安装 node 22（make web-build 构建 webui 嵌入产物）"
```

---

### Task 5: 部署文档补 Web UI 访问章节

**Files:**
- Modify: `docs/team-deployment.md`（文末追加第 8 节）
- Modify: `docs/member-onboarding.md`（"你需要拿到两样东西"小节末尾追加一段）

- [ ] **Step 1: `docs/team-deployment.md` 文末追加**

```markdown
## 8. 浏览器访问 Web UI

gta-mcp 内嵌了 Web 前端（`make web-build` 或 Docker `webui` 构建阶段生成，编译期
`go:embed` 进二进制）。compose 部署完成后，浏览器直接打开：

    http://<服务器IP>:8781

- 配置了 `GTA_AUTH_TOKENS` 时首次访问会出现 401 横幅并自动弹出设置：在「访问令牌」
  填入你的 token（与 gta-agent 用的同一份）保存即可；令牌只保存在本机浏览器。
- 会话/插件列表显示归属徽标；admin 可在会话列表顶部切换「只看我的 / 全部」。
- 「开始抓包」支持本机网卡与远程 agent 两种来源；远程 agent 会给出可直接复制的
  `gta-agent` 启动命令。
- 用 `go build` 直接编译的裸二进制若未构建前端，打开 8781 会显示「Web UI 未构建」
  提示：执行 `make web-build` 后重新编译即可。
- SSE（插件事件实时推送）经查询参数携带 token（`/events/plugins?token=...`），
  若在 gta-mcp 前面加反向代理，注意对 `token` 查询参数脱敏或关闭 access log。
```

- [ ] **Step 2: `docs/member-onboarding.md` 补 Web 入口**

先读该文件，在 "## 你需要拿到两样东西" 小节的最后一个列表项/段落后追加：

```markdown
另外，浏览器打开 `http://<服务器IP>:8781` 即可使用 Web 控制台：首次访问在
「设置 → 访问令牌」中填入你的 token，即可查看会话/插件列表与抓包入口
（详见 `docs/team-deployment.md` 第 8 节）。
```

- [ ] **Step 3: Commit**

```bash
cd E:/gta && git add docs/team-deployment.md docs/member-onboarding.md && git commit -m "docs: 部署/成员指南补 Web UI 访问章节（8781、令牌输入、SSE token 脱敏提醒）"
```

---

### Task 6: 全量验证

**Files:** 无新改动，仅验证（发现问题回对应任务修，不留在此任务提交）

- [ ] **Step 1: Go 全量**

Run: `cd E:/gta && go build ./... && go test ./... -count=1 2>&1 | grep -Ev "^ok|no test files" | head -10`
Expected: 无 FAIL 输出

- [ ] **Step 2: 前端测试**

Run: `cd E:/gta/web && npm test 2>&1 | tail -4`
Expected: 24/24（本任务未改前端代码，防误伤）

- [ ] **Step 3: 真实产物端到端（本地，不起 Docker）**

```bash
cd E:/gta && make web-build 2>&1 | tail -3
go build -o bin/gta-mcp-webui-test.exe ./cmd/gta-mcp
WORK=$(mktemp -d)
./bin/gta-mcp-webui-test.exe -work-dir "$WORK" -addr 127.0.0.1:0 > "$WORK/mcp.log" 2>&1 &
sleep 6
ADDR=$(node -e "console.log(JSON.parse(require('fs').readFileSync(process.argv[1],'utf8')).addr)" "$WORK/addr.mcp.json" 2>/dev/null || grep -o '127.0.0.1:[0-9]*' "$WORK/mcp.log" | head -1)
echo "addr=$ADDR"
curl -s -o /dev/null -w "root:%{http_code} %{content_type}\n" "http://$ADDR/"
curl -s "http://$ADDR/" | head -c 200; echo
ASSET=$(curl -s "http://$ADDR/" | grep -o '/assets/[^"]*\.js' | head -1)
curl -s -o /dev/null -w "asset:%{http_code} %{content_type}\n" "http://$ADDR$ASSET"
curl -s -D - -o /dev/null "http://$ADDR$ASSET" | grep -i cache-control
curl -s -o /dev/null -w "mcp-anon:%{http_code}\n" -X POST -H "Content-Type: application/json" -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_all_sessions","arguments":{}}}' "http://$ADDR/mcp"
kill %1
```
Expected: `root:200 text/html`（内容含 vite 产物特征如 `assets/` 引用）；`asset:200` 且 Cache-Control immutable；`mcp-anon:200`（匿名模式，验证 API 未被静态吞掉）。

再验证 token 模式（起第二个实例）：

```bash
WORK2=$(mktemp -d)
GTA_AUTH_TOKENS="alice=gta_tok_aaa:admin" ./bin/gta-mcp-webui-test.exe -work-dir "$WORK2" -addr 127.0.0.1:0 > "$WORK2/mcp.log" 2>&1 &
sleep 6
ADDR2=$(node -e "..." "$WORK2/addr.mcp.json")
curl -s -o /dev/null -w "root:%{http_code}\n" "http://$ADDR2/"            # 200（静态免鉴权）
curl -s -o /dev/null -w "mcp-noauth:%{http_code}\n" -X POST "http://$ADDR2/mcp" -H "Content-Type: application/json" -d '{}'   # 401
curl -s -o /dev/null -w "sse-token:%{http_code}\n" --max-time 20 "http://$ADDR2/events/plugins?token=gta_tok_aaa"             # 200
kill %2
```
（`node -e "..."` 处填与上面相同的取址表达式。）

清理：`rm bin/gta-mcp-webui-test.exe`。

- [ ] **Step 4: Docker compose 端到端（本机 docker 可用时）**

```bash
cd E:/gta && cp .env.example .env && docker compose up -d --build 2>&1 | tail -3
curl -s -o /dev/null -w "root:%{http_code}\n" http://127.0.0.1:8781/    # 200 text/html
curl -s -o /dev/null -w "mcp:%{http_code}\n" -X POST http://127.0.0.1:8781/mcp -H "Content-Type: application/json" -d '{}'  # 401（.env 有 token）
docker compose down
rm .env
```
（docker 不可用则记录"跳过，留人工"，把命令原样写进报告的后续清单。）

- [ ] **Step 5: git 状态核对**

Run: `cd E:/gta && git status --short`
Expected: 干净（无产物泄漏进 git；`.env` 已删）

---

## Self-Review 记录

- **Spec 覆盖**：§1 静态/路由/免鉴权/缓存/未构建兜底（Task 1）；§2 .gitignore、web-build、release-matrix 依赖、Dockerfile node 阶段 + ARG、.dockerignore（Task 2/3）；§3 compose/.env（Task 3）；§4 Go 测试（Task 1）、Docker 冒烟（Task 3/6）、文档（Task 5）。无缺口。
- **占位符扫描**：无 TBD/TODO；Task 6 的取址 `node -e "..."` 在 Step 3 有完整表达式、Step 4 明确指复用同一表达式并给出替代 grep 方案。
- **类型/命名一致性**：`serveWebOrAPI(fsys fs.FS, authed http.Handler)`、`mustWebUIFS()`、`webUIPlaceholderHTML` 在 Task 1 内定义并被 main.go 挂载引用；测试用 `builtFS()/emptyFS()/fallbackProbe` 均本地定义。
