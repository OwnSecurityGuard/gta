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
//
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
		// 无产物则回内置提示页。index.html 直接读文件返回——http.FileServerFS
		// 会把显式 /index.html 301 到 "./"（其规范化规则），SPA 深链接会因此断掉。
		if name == "" || name == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if data, err := fs.ReadFile(fsys, "index.html"); err == nil {
				_, _ = w.Write(data)
			} else {
				_, _ = w.Write([]byte(webUIPlaceholderHTML))
			}
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
