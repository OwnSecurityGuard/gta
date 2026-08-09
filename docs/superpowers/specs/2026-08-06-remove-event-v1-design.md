# 剔除 Event V1，全面迁移至 EventV2 设计文档

## 目标

完全移除旧版 `event.Event` 类型、V1 与 V2 之间的适配器、V1 Decode gRPC RPC 及其 SDK 支持。整个系统统一使用 `event.EventV2` 作为唯一事件模型。

## 背景

- `event.Event`（V1）采用 JSON 懒加载模型，字段扁平（`ID`, `Protocol`, `JSON`, `Src`, `Dst`, `FlowID`, `MsgName` 等）。
- `event.EventV2` 采用三层模型：`Identity + Relation + Context + Payload`，使用 MsgPack 序列化，支持 schema 版本和因果关系。
- 当前 `pkg/event/adapter.go` 提供 `ToV2()` 和 `ToLegacy()` 双向转换，是 V1 与 V2 共存的过渡层。
- 本次重构后，不再有 V1 概念，插件协议只保留 `DecodeV2`。

## 设计决策

| 决策 | 内容 |
|------|------|
| Event 类型 | 删除 `event.Event`，保留 `event.EventV2` 作为唯一事件类型。不重命名为 `Event`，避免无意义的全局替换。 |
| 适配器 | 删除 `ToV2()` / `ToLegacy()` / `BatchConvertToV2` / `BatchConvertToLegacy`。保留 `ExtractStateChanges()` 和 `inferSchemaID()`。 |
| gRPC 协议 | 删除 `Decoder.Decode` RPC 和 `DecodeResponse` message，只保留 `DecodeV2`。 |
| SDK | 删除 V1 `decodeFunc` 和 `Decode` 方法，`RunRegisterLoop` 改为只接受 V2 handler。 |
| Dispatcher | 删除 V1 路径（`stream pb.Decoder_DecodeClient`、`decodeV1Locked`），`DecodeV2` 作为唯一解码入口。外部需要单一事件时取 `evs[0]`。 |
| Analyze | `Engine.Process` 改为直接处理 `*event.EventV2`（由当前 `ProcessV2` 改名）。`ruleEnv` 中 `event` 类型改为 `*event.EventV2`。 |
| MCP trace | trace handler 的消息类型改为 `*event.EventV2`。`flow_id` 参数由 `int64`/`uint64` 改为 `string`，与 `EventContext.FlowID` 一致。从 `Payload.Value` 中提取业务字段。 |
| Store | 当前已以 V2 为主，确认删除 V1 残留接口（如 `AppendEvents` 若存在）。 |
| 规则表达式 | 破坏性变更：规则不能再访问 `event.Protocol` 等 V1 字段，应改用 `event.Identity.Type`、`event.Context.FlowID` 等 V2 字段。 |

## 关键文件变更

### 删除/清理

- `pkg/event/event.go`
  - 删除 `Event` 结构体及方法（`Data`, `GetTimestamp`）。
  - 保留 `Packet`, `Metric`, `Aggregator`, `TimestampedEvent`, `StateChange`, `LinkType`, `TCPFlags`。
- `pkg/event/adapter.go`
  - 删除 `ToV2`, `ToLegacy`, `BatchConvertToV2`, `BatchConvertToLegacy`。
  - 保留 `ExtractStateChanges`（后续可移到 `state_change.go`）。
- `pkg/plugin/proto/plugin.proto`
  - 删除 `rpc Decode` 和 `message DecodeResponse`。
- `pkg/plugin/sdk/decoder.go`
  - 删除 `decodeFunc` 字段、`Decode` 方法、`callWithRecover`。
- `pkg/plugin/sdk/registry.go`
  - 删除 `RunRegisterLoop`，只保留 `RunRegisterLoopV2`。
- `pkg/decode/dispatcher.go`
  - 删除 `stream` 字段、`useV2` 字段、`Decode` 方法、`decodeV1Locked`。
  - `NewDispatcher` 不再探测 V1/V2，直接创建 `DecodeV2` stream。

### 修改

- `pkg/analyze/engine.go`
  - 删除旧 `Process(*event.Event)`。
  - 将 `ProcessV2` 改名为 `Process`，签名 `Process(ctx, *event.EventV2) ([]Metric, error)`。
- `pkg/analyze/rule.go`
  - `ruleEnv["event"]` 改为 `(*event.EventV2)(nil)`。
  - 规则可访问 `event.Identity`, `event.Relation`, `event.Context`, `event.Payload`。
- `cmd/gta-mcp/trace_handler.go`
  - `flowID` 参数类型改为 `string`。
  - 消息结构体改为基于 `*event.EventV2`。
  - `queryFlowMessages` 返回 `[]*event.EventV2`。
  - `pairByMsgName` / `pairByDirection` 等从 `Payload.Value` 读取 `msg_name`, `direction`, `msg_id`。
- `plugins/http/main.go`
  - 删除 V1 `decodePacket`，`main()` 只注册 V2。
- 所有测试
  - 删除 V1 测试用例，修复编译错误。

## 数据兼容性

- 数据库中 `events` 表已经是 V2 格式（MsgPack payload + context 列），无需改动。
- 旧规则表达式需要人工迁移，这是预期的破坏性变更。

## 验收标准

1. `grep -R "event.Event\b" --include='*.go'` 在业务代码中无命中（允许注释和字符串字面量）。
2. `grep -R "ToLegacy\|ToV2\|BatchConvertTo" --include='*.go'` 无命中。
3. `grep -R "DecodeResponse\b" --include='*.go'` 无命中（`DecodeResponseV2` 除外）。
4. `go build ./cmd/... ./pkg/... ./plugins/...` 零错误。
5. `go test ./pkg/... -count=1` 中 V1 相关失败被修复或移除。
