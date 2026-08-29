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
			// 经查询参数携带 token。仅头缺失或无法解析时回退，头永远优先于查询参数。
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
