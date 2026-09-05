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
	called, owner, code := do(t, mustResolver(t, "alice=gt_aaa"), "Bearer gt_aaa")
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
	for _, h := range []string{"Bearer gt_aaa", "bearer gt_aaa", "BEARER gt_aaa"} {
		called, owner, code := do(t, mustResolver(t, "alice=gt_aaa"), h)
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
		{"错误的 token", "Bearer gt_zzz"},
		{"空 Bearer", "Bearer "},
		{"scheme 不对", "Basic gt_aaa"},
		{"已撤销的 token", "Bearer gt_ccc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			called, _, code := do(t, mustResolver(t, "alice=gt_aaa,bob=gt_bbb:admin"), c.header)
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
	srv := Middleware(mustResolver(t, "alice=gt_aaa"), probeHandler(&called, &owner))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("401 响应应带 WWW-Authenticate 头")
	}
}

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
	called, owner, rec := doRequest(t, mustResolver(t, "alice=gt_aaa,bob=gt_bbb:admin"), "/mcp?token=gt_bbb", "")
	if !called {
		t.Fatal("查询参数携带合法 token 应放行")
	}
	if owner != "bob" {
		t.Fatalf("owner 应为 bob，实际 %q", owner)
	}
	if got := rec.Header().Get(HeaderAdmin); got != "true" {
		t.Fatalf("admin 应回显 X-GT-Admin: true，实际 %q", got)
	}
}

// TestMiddleware_HeaderBeatsQueryParam 验证头永远优先于查询参数。
func TestMiddleware_HeaderBeatsQueryParam(t *testing.T) {
	t.Parallel()
	called, owner, _ := doRequest(t, mustResolver(t, "alice=gt_aaa,bob=gt_bbb:admin"), "/mcp?token=gt_bbb", "Bearer gt_aaa")
	if !called || owner != "alice" {
		t.Fatalf("头应优先于查询参数: called=%v owner=%q", called, owner)
	}
}

// TestMiddleware_RejectsBadQueryParam 验证查询参数里的非法 token 同样 401。
func TestMiddleware_RejectsBadQueryParam(t *testing.T) {
	t.Parallel()
	called, _, rec := doRequest(t, mustResolver(t, "alice=gt_aaa"), "/mcp?token=gt_zzz", "")
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
	_, _, rec := doRequest(t, mustResolver(t, "alice=gt_aaa,bob=gt_bbb:admin"), "/mcp", "Bearer gt_aaa")
	if got := rec.Header().Get(HeaderOwner); got != "alice" {
		t.Fatalf("X-GT-Owner 应为 alice，实际 %q", got)
	}
	if got := rec.Header().Get(HeaderAdmin); got != "" {
		t.Fatalf("非 admin 不应回显 X-GT-Admin，实际 %q", got)
	}
}
