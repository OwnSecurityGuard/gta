import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // Streamable HTTP 传输端点
      "/mcp": {
        target: "http://localhost:8781",
        changeOrigin: true,
      },
      // 老式 SSE 传输端点（GET /sse 建立流，POST /message 发请求）。
      // 必须代理，否则经 Vite 开发服务器(5173)同源访问时会被 SPA 兜底
      // 返回 index.html(<!DOCTYPE>)，导致 MCP 客户端报 "Invalid OAuth error response"。
      "/sse": {
        target: "http://localhost:8781",
        changeOrigin: true,
        // SSE 为长连接流，禁用代理缓冲，确保事件实时透传。
        configure: (proxy) => {
          proxy.on("proxyRes", (proxyRes) => {
            if (proxyRes.headers["content-type"]?.includes("text/event-stream")) {
              // 关闭分块缓冲，直接 flush
              (proxyRes as unknown as { flushHeaders?: () => void }).flushHeaders?.();
            }
          });
        },
      },
      "/message": {
        target: "http://localhost:8781",
        changeOrigin: true,
      },
      "/events": {
        target: "http://localhost:8781",
        changeOrigin: true,
      },
      // 远程 Agent 下载（/download/agent）：同源访问时需代理到 MCP 服务端，否则被 SPA 兜底拦截。
      "/download": {
        target: "http://localhost:8781",
        changeOrigin: true,
      },
    },
  },
});
