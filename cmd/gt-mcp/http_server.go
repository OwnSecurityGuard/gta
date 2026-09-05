package main

import (
	"net/http"
	"strings"

	"gametrace/pkg/auth"
)

// corsMiddleware 收紧跨域：仅对 allowlist 中的 Origin 回显
// Access-Control-Allow-Origin，其余（含未配置任何 origin 时）不 emitting CORS 头，
// 由浏览器同源策略兜底。本地同源用法不受影响（不带 Origin 头的请求原样放行）。
//
// OPTIONS 预检在鉴权之前处理（预检请求不携带 Authorization），命中 allowlist
// 时返回 204 + CORS 头；未命中时也返回 204 但无 CORS 头，浏览器自行拦截。
func corsMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			// 身份回显头默认不对跨域 JS 暴露，须显式加入 Expose-Headers 前端才读得到。
			w.Header().Set("Access-Control-Expose-Headers", auth.HeaderOwner+", "+auth.HeaderAdmin)
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Content-Type, Accept, Authorization, Mcp-Session-Id, Last-Event-ID, X-Requested-With")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requiredResolver 是带 Required() 的 auth.Resolver：报告是否配置了 token
// （与 auth.StaticResolver.Required 语义一致）。
type requiredResolver interface {
	auth.Resolver
	Required() bool
}

// authMiddleware 按需接入 Bearer 鉴权。resolver 未配置任何 token（匿名模式）
// 时直接透传、不注入身份——单机用法与 T12 之前完全一致（ctx 中无 Principal，
// owner 语义为匿名）；配置了 token 后未携带/携带无效凭证的请求返回 401。
func authMiddleware(resolver auth.Resolver, next http.Handler) http.Handler {
	if resolver == nil {
		return next
	}
	if rc, ok := resolver.(requiredResolver); ok && !rc.Required() {
		return next
	}
	return auth.Middleware(resolver, next)
}

// buildHTTPHandler 组装 MCP HTTP 服务的中间件链：CORS（外层，先处理预检）
// → 鉴权（内层）→ 路由 mux。
func buildHTTPHandler(allowedOrigins []string, resolver auth.Resolver, mux http.Handler) http.Handler {
	return corsMiddleware(allowedOrigins, authMiddleware(resolver, mux))
}
