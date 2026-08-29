package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// probeHandler 记录中间件是否放行以及它注入的 owner。
func probeHandler(called *bool, owner *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		*owner = OwnerFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func do(t *testing.T, r Resolver, header string) (called bool, owner string, code int) {
	t.Helper()
	var gotOwner string
	srv := Middleware(r, probeHandler(&called, &gotOwner))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return called, gotOwner, rec.Code
}

// TestMiddleware_AnonymousPassesThrough 验证本地单机跑 MCP（不配 token）时行为完全不变。
func TestMiddleware_AnonymousPassesThrough(t *testing.T) {
	t.Parallel()
	called, owner, code := do(t, mustResolver(t, ""), "")
	if !called {
		t.Fatal("匿名模式应放行到下游")
	}
	if code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", code)
	}
	if owner != AnonymousOwner {
		t.Fatalf("owner 应为 %q，实际 %q", AnonymousOwner, owner)
	}
}

// TestMiddleware_ValidToken 验证 HTTP 侧的 Bearer 解析与 owner 注入。
func TestMiddleware_ValidToken(t *testing.T) {
	t.Parallel()
	called, owner, code := do(t, mustResolver(t, "alice=gta_aaa"), "Bearer gta_aaa")
	if !called || code != http.StatusOK {
		t.Fatalf("正确 token 应放行: called=%v code=%d", called, code)
	}
	if owner != "alice" {
		t.Fatalf("owner 应为 alice，实际 %q", owner)
	}
}

// TestMiddleware_SchemeCaseInsensitive 验证 scheme 大小写不敏感（RFC 7235 规定）。
func TestMiddleware_SchemeCaseInsensitive(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"Bearer gta_aaa", "bearer gta_aaa", "BEARER gta_aaa"} {
		called, owner, code := do(t, mustResolver(t, "alice=gta_aaa"), h)
		if !called || code != http.StatusOK || owner != "alice" {
			t.Fatalf("%q 应放行且 owner=alice: called=%v code=%d owner=%q", h, called, code, owner)
		}
	}
}

// TestMiddleware_Rejects 验证未授权请求返回 401 且不触达下游。
func TestMiddleware_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		header string
	}{
		{"完全没有 Authorization", ""},
		{"错误的 token", "Bearer gta_zzz"},
		{"空 Bearer", "Bearer "},
		{"scheme 不对", "Basic gta_aaa"},
		{"已撤销的 token", "Bearer gta_ccc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			called, _, code := do(t, mustResolver(t, "alice=gta_aaa,bob=gta_bbb:admin"), c.header)
			if called {
				t.Fatal("被拒绝时绝不能调用下游 handler")
			}
			if code != http.StatusUnauthorized {
				t.Fatalf("状态码应为 401，实际 %d", code)
			}
		})
	}
}

// TestMiddleware_SetsWWWAuthenticate 验证 401 响应带上 WWW-Authenticate，
// 否则客户端（尤其是浏览器和标准 HTTP 库）无法知道该用什么方式认证。
func TestMiddleware_SetsWWWAuthenticate(t *testing.T) {
	t.Parallel()
	var called bool
	var owner string
	srv := Middleware(mustResolver(t, "alice=gta_aaa"), probeHandler(&called, &owner))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("401 响应应带 WWW-Authenticate 头")
	}
}
