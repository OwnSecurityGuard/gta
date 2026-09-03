package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"gta/pkg/auth"
)

// mustResolver 构造测试用 resolver。pkg/auth 的 interceptor_test.go 里有同名助手，
// 但 _test.go 的符号不跨包可见，这里基于导出的 auth.ParseTokens 造本地版本。
func mustResolver(t *testing.T, spec string) *auth.StaticResolver {
	t.Helper()
	r, err := auth.ParseTokens(spec)
	if err != nil {
		t.Fatalf("构造 resolver 失败: %v", err)
	}
	return r
}

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

// fallbackProbe 记录是否被兜底调用（＝请求交回了鉴权链）。
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

// TestWebUI_UnknownPathSPAFallback 验证未命中嵌入文件且非 API 的路径
// 按 SPA 深链接回退到 index.html（前端路由接管，而非 401/404）。
func TestWebUI_UnknownPathSPAFallback(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(builtFS(), probe)
	rec := doGet(t, h, "/no-such-path")
	if probe.called {
		t.Fatal("SPA 深链接不应兜底到鉴权链")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("SPA 深链接应 200 返回 index.html，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); body != "<html>index</html>" {
		t.Fatalf("SPA 深链接应返回 index.html，实际 %q", body)
	}
}

// TestWebUI_APIEndpointNotSwallowed 验证 API/流式端点（无扩展名）不回退到
// index.html，而是交给下游 authed 处理器——否则 EventSource 会因 MIME 不
// 匹配（text/html vs text/event-stream）中止连接。
func TestWebUI_APIEndpointNotSwallowed(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(builtFS(), probe)
	for _, path := range []string{"/sse", "/message", "/mcp", "/events/plugins", "/download/agent"} {
		probe.called = false
		rec := doGet(t, h, path)
		if !probe.called || probe.path != path {
			t.Fatalf("%s 应交给下游 API 处理器，实际 called=%v path=%q", path, probe.called, probe.path)
		}
		if rec.Code != http.StatusTeapot {
			t.Fatalf("%s 应透传下游响应，实际 %d", path, rec.Code)
		}
	}
}

// TestWebUI_StaticFileNotFound 验证带扩展名的静态资源不存在时返回 404
// （而非 401 或 index.html 200）。
func TestWebUI_StaticFileNotFound(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(builtFS(), probe)
	rec := doGet(t, h, "/vite.svg")
	if probe.called {
		t.Fatal("静态资源不应兜底到鉴权链")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("缺失的静态资源应 404，实际 %d", rec.Code)
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
// no-cache、含"未构建"字样），静态路径（带扩展名）返回 404（而非推进鉴权链）。
func TestWebUI_PlaceholderWithoutIndex(t *testing.T) {
	t.Parallel()
	probe := &fallbackProbe{}
	h := serveWebOrAPI(emptyFS(), probe)
	rec := doGet(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("未构建时 GET / 应 200 提示页，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "未构建") {
		t.Fatalf("提示页应包含『未构建』，实际 %q", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("提示页应 no-cache，实际 %q", cc)
	}
	rec2 := doGet(t, h, "/assets/app-1.js")
	if probe.called {
		t.Fatal("未构建时静态路径不应兜底到鉴权链")
	}
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("未构建时缺失的静态资源应 404，实际 %d", rec2.Code)
	}
}
