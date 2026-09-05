# 插件测试功能设计（隐私安全的原始包测试通道）

> 关联文档：`frontend-plugin-hotreload-design.md`（前端热更/切换整体方案）。
> 目标：在「插件管理」页面增加**测试插件**能力——**在不完整暴露原始包**的前提下，把原始包临时交给插件解码，并只把**插件解出来的相关数据**（解码事件）展示出来。

## 1. 为什么需要新通道，而不复用 `decode_raw_packets`

| 维度 | `decode_raw_packets`（现状） | `test_plugin`（本设计，新增） |
|---|---|---|
| 目的 | 离线重解码并落库 | 验证插件解码质量 |
| 是否落库 | 写 `events` 表（**污染真实解码数据**） | **不落库**，仅采样返回 |
| 原始字节去向 | 服务端读取，不返回 | 服务端读取，不返回 |
| 门控 | 需 `--enable-raw-debug` | **常驻可用**（不回传原始字节，安全） |
| 返回内容 | 仅计数（total/decoded/errors） | 计数 + **事件类型分布** + **采样解码事件** + **错误样例** |

核心判断：原始包字节只在 `gt-pipeline` 进程内被 `QueryRawPackets` 读取并喂给插件 `DecodeV2`，**从未序列化回 MCP / 前端**。插件输出的是「协议语义层事件」（`data.*`），不含链路层原始字节。因此该能力是**隐私安全**的，应作为常驻功能，而非 dev 调试开关。

## 2. 端到端数据流

```
[前端 PluginPanel 测试区]
   配置：目标插件 + 来源会话(offline) + 可选过滤 + 包上限
        │  test_plugin(session_id, plugin, filter, limit, sample_limit)
        ▼
[gt-mcp]  ──gRPC──►  [gt-pipeline / CaptureControl]
                          │
                          │  ① QueryRawPackets()  ← 原始字节，仅进程内使用
                          │        │  raw bytes → 绝不外传
                          │        ▼
                          │  ② Dispatcher.DecodeV2(pkt)  → []*event.Event
                          │        │  累积：类型直方图 + 前 sample_limit 个事件 + 前若干错误
                          │        ▼
                          │  ③ 组装 TestPluginResponse（不含任何 raw）
                          ▼
[前端展示]  计数 + 类型分布 + 采样事件(时间/类型/schema/data) + 错误样例(id/src/dst/err)
            🔒 提示：原始包未传前端 / 未修改会话真实解码数据
```

## 3. 后端改动

### 3.1 proto（`pkg/internalipc/proto/internal.proto`）
在 `CaptureControl` 服务新增：
```proto
rpc TestPlugin(TestPluginRequest) returns (TestPluginResponse);

message TestPluginRequest {
  string session_id = 1;
  string plugin     = 2;   // 被测插件名（按名路由，可不同于会话原插件）
  string protocol   = 3;   // 可选过滤
  string src        = 4;   // 可选过滤
  string dst        = 5;   // 可选过滤
  int64  limit      = 6;   // 测试包上限，0=全部
  int64  sample_limit = 7; // 返回的解码事件采样上限（建议默认 50）
}

message TestEventLite {
  string id            = 1;
  int64  timestamp_unix = 2;
  string type          = 3;
  string schema_id     = 4;
  string data_json     = 5;  // 拍平后的关键 data.* 字段 JSON（仅取前若干 key）
}

message TestErrorLite {
  string raw_packet_id = 1;
  string src           = 2;
  string dst           = 3;
  string error         = 4;  // 仅错误信息，不含 raw
}

message TestPluginResponse {
  int64              total_raw      = 1;
  int64              decoded        = 2;
  int64              decode_errors  = 3;
  map<string,int64>  type_histogram = 4;   // 事件类型 → 计数（全量，类型数有限）
  repeated TestEventLite  sample_events = 5;
  repeated TestErrorLite  error_samples = 6;
}
```
`make proto` 重新生成。

### 3.2 `capturecontrol/server.go`
- `CaptureEngine` 接口新增 `TestPlugin(ctx, TestPluginRequest) (TestPluginResult, error)`。
- 新增类型 `TestPluginRequest` / `TestPluginResult`（含 `map[string]int64` 直方图、采样切片），与 proto 对应。
- `Server.TestPlugin` 适配：填字段 → 调引擎 → 组装 `pb.TestPluginResponse`。

### 3.3 `cmd/gt-pipeline/decode_raw.go`（或新 `test_plugin.go`）
- **抽取共享解码循环**：把 `DecodeRawPackets` 中「分批读取 + `DecodeV2` + 累积」的逻辑抽成 helper（如 `decodeRawLoop(ctx, st, dispatcher, req, onEvent, onErr)`），两个入口复用，避免重复。
- `pipelineService.TestPlugin`：
  1. 拒绝运行中的 session（与 `DecodeRawPackets` 一致，避免与 captureTask 写冲突）。
  2. 按名路由插件（`FindByName` → 退化 `Find`）。
  3. 分批 `QueryRawPackets`（原始字节仅此处存在于内存，**不回传**）。
  4. 每个解码事件累加直方图；保留前 `sample_limit` 个事件（含 `data.*` 拍平 JSON，截断大字段）；保留前若干错误（id/src/dst/err）。
  5. **不调用 `AppendEvents`/`WriteStateChanges`**（隔离测试，不污染真实数据）。
  6. 返回 `TestPluginResult`。

### 3.4 `cmd/gt-mcp/main.go`
- 新增 MCP 工具 `test_plugin`（**不**用 `--enable-raw-debug` 门控，因不回传原始字节）。
- `handleTestPlugin`：参数 `session_id/plugin/protocol/src/dst/limit/sample_limit` → 调 `pipelineClient.TestPlugin` → 返回结构化 JSON（summary + histogram + sample_events + error_samples）。后端 `err != nil` 走 `errorResult`，避免 `successResult` 误判 `ok`。

## 4. 前端改动（PluginPanel）

### 4.1 配置区（测试输入）
- **目标插件**：下拉（注册插件列表，默认选中当前卡片插件 / 第一个在线）。
- **原始包来源会话**：下拉（仅 `stopped` 会话可选；运行时过滤掉 `running`）。
- **可选过滤（可折叠）**：协议 / 源 IP / 目的 IP。
- **测试包上限**：select `50 / 100 / 500 / 全部`。
- **[运行测试]** 按钮（加载态；禁用条件：缺插件或会话）。

每个 `PluginCard` 增加 **「测试」** 按钮，点击把该插件预填进测试区并滚动定位。

### 4.2 展示区（插件解出来的相关数据）
- **状态条**：`成功解码 X / 失败 Y（共 Z 包）` + 隐私徽标 `🔒 原始包未传前端`。
- **事件类型分布**：横向条形/小表格（type → count），是「解出来的相关数据」的概览。
- **采样事件预览**：表格（时间、类型、schema_id、`data.*` 关键字段），最多 `sample_limit` 条；行展开看 `data_json` 详情。
- **解码错误样例**（折叠）：表格（包ID、src→dst、错误原因），**不含 raw**。
- **隔离提示**：`本测试不修改会话真实解码数据（只读采样）`。

### 4.3 类型与 hook
- `web/src/types/plugin-test.ts`（新增）：`TestPluginRequest`/`TestPluginResult`/`TestEventLite`/`TestErrorLite`/`TestPluginResult`。
- `web/src/hooks/use-mcp.ts`：新增 `useTestPlugin()`（`useMutation` 调 `test_plugin`）。
- 复用已有 `useRegisteredPlugins` / `useSessions` 驱动下拉。

## 5. 隐私 / 安全要点
- `test_plugin` **不读** `--enable-raw-debug`；原始字节零路径回前端。
- 解码事件（`data.*`）属协议语义层，可展示，与「不暴露原始包」不冲突。
- `sample_limit` 默认 50，防大数据量返回爆炸；`type_histogram` 全量（类型数有限）。
- 错误样例只含 `id/src/dst/error`，**不含 raw payload**。

## 6. 风险 / 限制
- 测试用 **offline（stopped）** 会话的原始包；运行中的会话不接测试（与 `decode_raw_packets` 一致）。
- 测试结果**不持久化**（刷新/关闭即丢）。如需保留，可加 `run_id` 暂存（后续增强，本设计不做）。
- 解码事件若含敏感业务字段，仍属「解出来的数据」，由插件 schema 决定；这与「不暴露原始包」不冲突（原始包=链路层字节，解码数据=应用层语义）。

## 7. 实施阶段（待确认）
- **T1** proto 增 `TestPlugin` + 重新生成。
- **T2** `capturecontrol` 接口与 `Server` 适配。
- **T3** 抽共享解码循环 + `pipelineService.TestPlugin`（不落库、收集采样）。
- **T4** `gt-mcp` 暴露 `test_plugin`（非门控）。
- **T5** 前端类型/hook + PluginPanel 配置区与展示区 + PluginCard「测试」按钮。
- **T6** 后端编译 `go build -tags pcap ./cmd/... ./pkg/...` + 前端 `tsc -b` / `vite build`。

## 8. 实施状态（2026-08-08 已完成）
全部 T1–T6 落地并通过编译/类型检查/构建：
- 后端 `go build -tags pcap ./cmd/... ./pkg/...` 通过；`capturecontrol` 单测通过（补 `fakeEngine.SetSessionPlugin/SubscribePlugins/TestPlugin`）。
- 前端 `tsc -b` 通过、`vite build` 1857 模块通过。
- 行为：仅 stopped 会话可测；原始包仅服务端解码，不回传、不落库；返回计数+事件类型分布+采样事件(data_json 截断 4KB)+错误样例。
- `test_plugin` 为常驻工具，**不**依赖 `--enable-raw-debug`（与 `list_raw_packets`/`decode_raw_packets` 的 dev 门控区分开）。
