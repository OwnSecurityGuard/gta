# HTTP Decoder Plugin 设计

## 背景

项目 `examples/http/` 已配套测试服务、分析规则 (`rules.yaml`) 与 schema (`http-schema.json`)，且 `cmd/mcp-test/main.go` 直接以 `"plugin": "http"` 启动抓包。`plugins/` 目录目前为空，需要补一个最小可用的 HTTP 解码插件。

## 目标

实现 `plugins/http`，让 `gt-pipeline` 在 `start_capture` 指定 `plugin=http` 时，能把 TCP payload 解析为 HTTP 请求/响应事件，写入 `events` 与 `entity_snapshots`。

## 设计决策

- **协议版本**：V1 JSON（`Decode` RPC），复用 `pkg/plugin/sdk` 的 `RunRegisterLoop` 与 `Decoder` 封装。`gt-pipeline` 的 Dispatcher 会优先探测 V2，失败后回退到 V1 并自动转成 `EventV2`。
- **解析策略**：逐包解析，不处理 TCP 流重组。先尝试 `http.ReadRequest`，失败再尝试 `http.ReadResponse`。
- **Body 处理**：读取完整 body 文本，上限 64KB，超限截断并标记 `body_truncated`。
- **输出字段**：
  - `data.type`: `"request"` / `"response"`
  - `data.method`, `data.path`, `data.version`, `data.host`
  - `data.status`, `data.reason`
  - `data.content_length`, `data.body_len`
  - `data.body`: body 文本
  - `data.headers`: 全部 header 的 map
  - `_fields.direction`: 由 type 推断
  - `_fields.msg_name`: `"POST /api/login"` 或 `"resp 200"`
- **错误处理**：malformed 包返回 `DecodeResponse.error`；`decodeFunc` 内用 `defer recover()` 兜底 panic。

## 文件

- `plugins/http/main.go`
- `plugins/http/plugin.yaml`
- `plugins/http/go.mod`

## 依赖

- `gametrace/pkg/plugin/sdk`
- `gametrace/pkg/plugin/proto`
- 标准库 `net/http`、`bufio`、`bytes`、`io`、`strings`、`log/slog`

## 测试计划

1. `go build ./plugins/http` 通过
2. 启动 `examples/http/server` 与 `examples/http/client` 产生流量
3. 用 `gt-pipeline` + `gt-mcp` 抓包并验证 `events` 表出现 `data.type == "request"/"response"` 的记录
