# 剔除 Event V1 全面迁移 EventV2 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完全移除 `event.Event`（V1）类型、V1/V2 适配器、V1 Decode gRPC RPC 及 SDK 支持，系统统一使用 `event.EventV2`。

**Architecture:** 从协议层（proto）开始删除 V1 RPC，再到事件模型层删除 Event 类型和适配器，然后清理 SDK/Dispatcher/Analyze/MCP 中的 V1 调用，最后修复测试。

**Tech Stack:** Go, gRPC, MsgPack, SQLite

---

## 文件变更总览

| 文件 | 动作 | 说明 |
|------|------|------|
| `pkg/plugin/proto/plugin.proto` | 修改 | 删除 `rpc Decode` 和 `message DecodeResponse` |
| `pkg/plugin/proto/*.go` | 重新生成 | 基于更新后的 proto |
| `pkg/plugin/sdk/decoder.go` | 修改 | 删除 V1 decodeFunc、Decode 方法、callWithRecover |
| `pkg/plugin/sdk/registry.go` | 修改 | 删除 `RunRegisterLoop`，`RunRegisterLoopV2` 改名为 `RunRegisterLoop` |
| `plugins/http/main.go` | 修改 | 删除 V1 `decodePacket`，`main()` 只注册 V2 |
| `pkg/event/event.go` | 修改 | 删除 `Event` 结构体及方法，保留通用类型 |
| `pkg/event/adapter.go` | 修改 | 删除 `ToV2`/`ToLegacy`/`BatchConvertTo*` |
| `pkg/decode/dispatcher.go` | 修改 | 删除 V1 路径、`Decode` 方法、`decodeV1Locked` |
| `pkg/decode/fields.go` | 修改 | 删除 V1 `ExtractedFields` 残留（若还有） |
| `pkg/analyze/engine.go` | 修改 | 删除旧 `Process`，`ProcessV2` 改名为 `Process` |
| `pkg/analyze/rule.go` | 修改 | `ruleEnv["event"]` 改为 `*event.EventV2` |
| `cmd/gta-mcp/trace_handler.go` | 修改 | 消息类型改为 `*event.EventV2`，flow_id 改 string |
| `cmd/gta-mcp/*.go` | 修改 | 所有 trace 辅助函数适配 EventV2 |
| `pkg/store/eventstore.go` | 修改 | 删除 `EntitySnapshotQuery`/`EntitySnapshotRow` 等 V1 残留 |
| `pkg/store/sqlite.go` | 修改 | 删除 `entity_snapshots` 表查询/写入 |
| 测试文件 | 修改/删除 | 修复编译，删除 V1 测试 |

---

### Task 1: Proto 删除 V1 Decode RPC

**Files:**
- Modify: `pkg/plugin/proto/plugin.proto`

- [ ] **Step 1: 删除 V1 RPC 和 message**

```protobuf
// 删除这两行
// rpc Decode(stream DecodeRequest) returns (stream DecodeResponse);
// message DecodeResponse { ... }
```

只保留：
```protobuf
service Decoder {
  rpc DecodeV2(stream DecodeRequest) returns (stream DecodeResponseV2);
}
```

- [ ] **Step 2: 重新生成 Go 代码**

Run:
```bash
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/plugin/proto/plugin.proto
```

- [ ] **Step 3: 验证 proto 包编译**

Run: `go build ./pkg/plugin/proto/...`
Expected: ok

- [ ] **Step 4: Commit**

```bash
git add pkg/plugin/proto/
git commit -m "proto!: remove Decode v1 RPC and response"
```

---

### Task 2: SDK 删除 V1 handler

**Files:**
- Modify: `pkg/plugin/sdk/decoder.go`
- Modify: `pkg/plugin/sdk/registry.go`

- [ ] **Step 1: 修改 decoder.go**

删除字段：
```go
decodeFunc   func(req *pb.DecodeRequest) ([]byte, error)
```

删除方法 `Decode` 和 `callWithRecover`。

将 `Decoder` 改为：
```go
type Decoder struct {
	pb.UnimplementedDecoderServer
	decodeFuncV2 DecodeFuncV2
}
```

- [ ] **Step 2: 修改 registry.go**

删除 `RunRegisterLoop` 函数。

将 `RunRegisterLoopV2` 重命名为 `RunRegisterLoop`，签名改为：
```go
func RunRegisterLoop(decodeFuncV2 DecodeFuncV2)
```

函数体内创建 `Decoder{decodeFuncV2: decodeFuncV2}`（删除 decodeFunc 字段）。

- [ ] **Step 3: 编译验证**

Run: `go build ./pkg/plugin/sdk/...`
Expected: ok

- [ ] **Step 4: Commit**

```bash
git add pkg/plugin/sdk/
git commit -m "sdk!: remove v1 decode handler, keep only DecodeV2"
```

---

### Task 3: HTTP 插件只注册 V2

**Files:**
- Modify: `plugins/http/main.go`

- [ ] **Step 1: 删除 V1 decodePacket 函数**

删除 `decodePacket(req *pb.DecodeRequest) ([]byte, error)` 及其所有内部逻辑（保留 V2 的 `decodePacketV2` 和 `jsonToMsgpack`）。

- [ ] **Step 2: 更新 main 函数**

```go
func main() {
	sdk.RunRegisterLoop(decodePacketV2)
}
```

- [ ] **Step 3: 编译验证**

Run: `go build ./plugins/http/...`
Expected: ok

- [ ] **Step 4: Commit**

```bash
git add plugins/http/main.go
git commit -m "plugin(http): remove v1 decode path"
```

---

### Task 4: 删除 Event V1 类型和适配器

**Files:**
- Modify: `pkg/event/event.go`
- Modify: `pkg/event/adapter.go`

- [ ] **Step 1: 修改 event.go**

删除 `Event` 结构体（第 103-135 行）以及 `GetTimestamp()` 和 `Data()` 方法（第 149-167 行）。

保留 `TimestampedEvent` 接口（因为它可以被 EventV2 实现），或在 EventV2 上实现 `GetTimestamp()`。

`EventV2.GetTimestamp()` 已存在（在 event_v2.go 中），确认可用。

- [ ] **Step 2: 修改 adapter.go**

删除 `ToV2()`、`ToLegacy()`、`BatchConvertToV2()`、`BatchConvertToLegacy()`。

保留 `ExtractStateChanges()` 和 `inferSchemaID()`。

- [ ] **Step 3: 编译验证**

Run: `go build ./pkg/event/...`
Expected: 当前会有其他包引用错误，但 event 包本身应通过

- [ ] **Step 4: Commit**

```bash
git add pkg/event/event.go pkg/event/adapter.go
git commit -m "event!: remove Event v1 type and adapters"
```

---

### Task 5: Dispatcher 删除 V1 路径

**Files:**
- Modify: `pkg/decode/dispatcher.go`

- [ ] **Step 1: 删除 V1 相关字段和方法**

删除字段：
```go
stream    pb.Decoder_DecodeClient
useV2     bool
```

删除方法 `Decode` 和 `decodeV1Locked`。

保留 `DecodeV2` 作为唯一入口。

- [ ] **Step 2: 修改 NewDispatcher**

直接创建 V2 stream，不再 probe：

```go
func NewDispatcher(client pb.DecoderClient, sessionID string, logger *slog.Logger, schemaReg *schema.Registry) (*Dispatcher, error) {
	if schemaReg == nil {
		schemaReg = schema.NewRegistry()
	}
	if logger == nil {
		logger = slog.Default()
	}

	streamV2, err := client.DecodeV2(context.Background())
	if err != nil {
		return nil, fmt.Errorf("create decode v2 stream: %w", err)
	}

	return &Dispatcher{
		client:       client,
		streamV2:     streamV2,
		sessionID:    sessionID,
		logger:       logger,
		pending:      make(map[string]*pendingInput),
		causationIdx: newCausationIndex(1024),
		schemaReg:    schemaReg,
	}, nil
}
```

- [ ] **Step 3: 删除 isUnimplementedError 和相关 probe 逻辑**

如果 `isUnimplementedError` 只用于 V1/V2 探测，一并删除。

- [ ] **Step 4: 编译验证**

Run: `go build ./pkg/decode/...`
Expected: 可能有测试编译错误，但 dispatcher.go 本身应通过

- [ ] **Step 5: Commit**

```bash
git add pkg/decode/dispatcher.go
git commit -m "decode: remove v1 decode path from dispatcher"
```

---

### Task 6: Analyze 引擎直接消费 EventV2

**Files:**
- Modify: `pkg/analyze/engine.go`
- Modify: `pkg/analyze/rule.go`

- [ ] **Step 1: 修改 engine.go**

删除旧 `Process(ctx context.Context, ev *event.Event)` 方法。

将 `ProcessV2` 改名为 `Process`：
```go
func (e *Engine) Process(ctx context.Context, ev *event.EventV2) ([]event.Metric, error)
```

删除方法内部的 `ev.ToLegacy()` 调用（因为不再需要）。规则环境直接使用 `ev`：
```go
base := map[string]any{
    "event":    ev,
    "data":     ev.Payload.Value.ToAny(),
    "identity": ev.Identity,
    "relation": ev.Relation,
    "context":  ev.Context,
    "payload":  ev.Payload,
}
```

- [ ] **Step 2: 修改 rule.go**

```go
var ruleEnv = map[string]any{
	"event": (*event.EventV2)(nil),
	"data":  map[string]any(nil),
}
```

- [ ] **Step 3: 编译验证**

Run: `go build ./pkg/analyze/...`
Expected: ok（可能有测试错误，忽略）

- [ ] **Step 4: Commit**

```bash
git add pkg/analyze/engine.go pkg/analyze/rule.go
git commit -m "analyze: process EventV2 directly, remove legacy conversion"
```

---

### Task 7: MCP trace handler 迁移到 EventV2

**Files:**
- Modify: `cmd/gta-mcp/trace_handler.go`
- Modify: `cmd/gta-mcp/message_utils.go`（若存在）
- Modify: `cmd/gta-mcp/pairing.go`（若存在）

- [ ] **Step 1: 读取并定位 V1 引用**

Run:
```bash
grep -n "event.Event\|\.JSON\|\.MsgName\|\.MsgID\|\.Direction\|flowID.*uint64\|flowID.*int64" cmd/gta-mcp/*.go
```

- [ ] **Step 2: 修改 flow_id 参数类型**

`handleTraceProtocolFlow` 中：
```go
flowID, err := req.RequireString("flow_id")
```

删除 `uint64(flowID)` 转换，直接使用 string。

- [ ] **Step 3: 修改消息结构体**

将内部消息类型（如 `TraceMessage`）改为 `*event.EventV2`。

新增辅助函数从 EventV2 Payload 提取字段：
```go
func msgNameFromEvent(ev *event.EventV2) string {
	if s, ok := ev.Identity.Type.AsString(); ok {
		return s
	}
	return ""
}

func directionFromEvent(ev *event.EventV2) string {
	return ev.Context.Direction
}

func msgIDFromEvent(ev *event.EventV2) int64 {
	// msg_id 不再由核心分配，可用 event ID 或从 payload _meta.msg_id 读取
	if obj, ok := ev.Payload.Value.AsObject(); ok {
		if m, ok := obj["_meta"]; ok {
			if mo, ok := m.AsObject(); ok {
				if v, ok := mo["msg_id"]; ok {
					if n, ok := v.AsInt(); ok {
						return n
					}
				}
			}
		}
	}
	return 0
}
```

- [ ] **Step 4: 更新 queryFlowMessages 返回类型**

```go
func queryFlowMessages(ctx context.Context, reader event.Reader, sessionID string, flowID string, from, to time.Time) ([]*event.EventV2, error)
```

过滤条件改为 `context.flow_id = ?`。

- [ ] **Step 5: 更新 pairByMsgName / pairByDirection 等函数**

从 `ev.Payload.Value` 读取 `msg_name`（或改用 `ev.Identity.Type`）和 `direction`（`ev.Context.Direction`）。

- [ ] **Step 6: 编译验证**

Run: `go build ./cmd/gta-mcp/...`
Expected: 可能有错误，逐个修复

- [ ] **Step 7: Commit**

```bash
git add cmd/gta-mcp/
git commit -m "mcp: migrate trace handler to EventV2"
```

---

### Task 8: Store 层删除 V1 残留接口

**Files:**
- Modify: `pkg/store/eventstore.go`
- Modify: `pkg/store/sqlite.go`
- Modify: `pkg/store/projection_reader.go`（若存在）

- [ ] **Step 1: 修改 eventstore.go**

删除 `EntitySnapshotQuery` 和 `EntitySnapshotRow` 类型（如果仍使用旧的 entity_snapshots 表）。

如果 `state_changes` 表已经替代 `entity_snapshots`，则删除 `QueryEntitySnapshots` 方法，或改为查询 `state_changes`。

- [ ] **Step 2: 修改 sqlite.go**

删除 `entity_snapshots` 表的创建语句、查询方法、写入方法。

保留 `state_changes` 和 `event_index`。

- [ ] **Step 3: 编译验证**

Run: `go build ./pkg/store/...`
Expected: ok

- [ ] **Step 4: Commit**

```bash
git add pkg/store/
git commit -m "store: remove entity_snapshots v1 interfaces"
```

---

### Task 9: 修复测试

**Files:**
- Modify: 所有报错的 `*_test.go`

- [ ] **Step 1: 诊断所有编译错误**

Run:
```bash
go test ./cmd/... ./pkg/... -run ^$ 2>&1 | head -80
```

- [ ] **Step 2: 分类修复**

常见错误：
- `NewDispatcher` 参数正确但返回值用法需调整（如不再调用 `Decode`，改用 `DecodeV2`）
- `event.Event` 引用改为 `event.EventV2`
- `ToLegacy()`/`ToV2()` 调用删除
- `RunRegisterLoop` 调用改为传 V2 handler
- `EntitySnapshot` 相关测试改为 `StateChange`

- [ ] **Step 3: 删除无法迁移的 V1 测试**

如果某些测试专门测试 V1 JSON 字段提取，直接删除。

- [ ] **Step 4: 新增关键测试**

在 `pkg/event/event_v2_test.go` 或 `pkg/analyze/analyze_test.go` 中新增：
- EventV2 规则表达式环境测试
- DecodeV2 0..N 结果测试（如尚未存在）

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "test: migrate tests to EventV2 and remove v1 coverage"
```

---

### Task 10: 全量验证

**Files:** 全部

- [ ] **Step 1: 全量构建**

Run:
```bash
go build ./cmd/... ./pkg/... ./plugins/...
```
Expected: 零错误

- [ ] **Step 2: 全量测试**

Run:
```bash
go test ./cmd/... ./pkg/... -count=1 -timeout 120s
```
Expected: 所有 V1 相关失败修复；预存在失败记录

- [ ] **Step 3: V1 残留扫描**

Run:
```bash
grep -R "\bevent\.Event\b" --include='*.go' pkg/ cmd/ plugins/ | grep -v "EventV2\|_test.go:\s*//"
grep -R "ToLegacy\|ToV2\|BatchConvertTo" --include='*.go' pkg/ cmd/ plugins/
grep -R "DecodeResponse\b" --include='*.go' pkg/ cmd/ plugins/ | grep -v "DecodeResponseV2"
```
Expected: 无命中

- [ ] **Step 4: go vet**

Run:
```bash
go vet ./cmd/... ./pkg/... ./plugins/...
```
Expected: 零警告

- [ ] **Step 5: Commit 最终清理**

```bash
git add -A
git commit -m "chore: final cleanup after removing Event v1"
```

---

## Self-Review Checklist

- [x] Spec coverage: 每个设计决策都对应到具体 task
- [x] Placeholder scan: 无 TBD/TODO
- [x] Type consistency: `EventV2` 名称贯穿全文，`flow_id` 统一为 string
