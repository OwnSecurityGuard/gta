package auth

import (
	"net/http"
)

// Middleware 是 MCP HTTP 侧的鉴权中间件，校验 Authorization: Bearer <token>。
// 匿名模式（resolver 未配置任何 token）下直接放行并注入 local 身份，保持单机用法不变。
func Middleware(r Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, _ := parseBearer(req.Header.Get("Authorization"))
		p, ok := r.Resolve(token)
		if !ok {
			// 带上 WWW-Authenticate，否则客户端不知道该用什么方式认证。
			w.Header().Set("WWW-Authenticate", `Bearer realm="gta"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), p)))
	})
}
