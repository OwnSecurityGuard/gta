# EventV2 模型优化设计

## 背景

当前 `EventV2` 已落地为 `Identity + Relation + Payload` 三层模型，并通过 `DecodeV2` RPC 以 MsgPack 方式与插件交互。但在实际业务解码中存在以下痛点：

1. 一个输入包可能产生 0..N 条业务消息，现有 `DecodeV2` 是一对一流式响应，无法表达多结果。
2. 无法可靠按 TCP 连接重组，需要把流标识、方向、端点等信息显式交给插件，让插件按 `flow_id` 自行维护协议状态。
3. `EventV2` 缺少网络定位信息，导致下游查询/配对时需要回查 `raw_packets`。
4. `EntitySnapshot` 过于游戏专用，表达能力不足。
5. `SchemaID` 没有约束，核心无法校验插件声明的 schema。
6. 因果链依赖插件手工猜测事件 ID，应该由插件提供业务关联键，核心完成映射。
7. Payload 内字段难以查询，需要轻量投影索引机制。

## 目标

在不破坏 V1（JSON）兼容路径的前提下，演进 V2 协议与事件模型：

- `DecodeV2` 支持 0..N 条结果，每条结果带 `input_id`。
- `DecodeRequest` 增加 `flow_id`、`src`、`dst`、`direction`、`packet_id`、`timestamp`。
- `EventV2` 增加可选 `Context`（`flow_id`、`raw_packet_id`、`message_ordinal`、`direction`）。
- 用通用 `StateChange` 投影替换 `EntitySnapshot`。
- 引入内存级轻量 schema registry。
- 插件结果提供 `correlation_key`、`causation_input_id`，核心映射为 `Relation`。
- 插件在 manifest 中声明投影字段，核心写入 `event_index` 索引表。

## 非目标

- 不实现 TCP 流重组；核心只负责生成/传递 `flow_id`，重组逻辑留给插件。
- 不索引所有动态 Payload 字段；只索引 manifest 中显式声明的少量字段。
- 不校验 Payload 内部结构；schema registry 只校验 `schema_id` 已注册。

## 设计决策

经确认，采用以下决策：

1. 协议完成语义：流式多响应 + 显式 `done=true` 结束标记。
2. `flow_id` 由核心根据五元组生成字符串，传给插件；插件用它隔离 buffer、协议状态、实体基线。
3. `direction` 由核心根据五元组推断并传入 `DecodeRequest`，插件可覆盖。
4. `EntitySnapshot` 直接删除，替换为 `StateChange`。
5. Schema registry 为内存级，插件注册时声明 schema，核心运行时校验。

## 1. 插件协议

### 1.1 DecodeRequest

```protobuf
message DecodeRequest {
  string session_id     = 1;
  string protocol_hint  = 2;
  bytes  payload        = 3;
  int32  link_type      = 4;

  // 新增：输入标识与网络定位
  string input_id       = 5;  // 本次解码输入的唯一标识，复用 packet_id
  string packet_id      = 6;  // 原始抓包 ID（与 input_id 一致，便于插件日志）
  string flow_id        = 7;  // 五元组流 ID，核心生成
  string src            = 8;  // "ip:port"
  string dst            = 9;  // "ip:port"
  string direction      = 10; // "client_to_server" | "server_to_client" | "unknown"
  int64  timestamp_ns   = 11; // 包时间戳（Unix ns）
}
```

生成规则：

- `input_id` / `packet_id`：核心为每个 `event.Packet` 生成 UUIDv7。
- `flow_id`：对 `{src, dst, protocol}` 做方向无关的规范化后取 hash，字符串化。不混入 `session_id`，查询时通过 `session_id=? AND flow_id=?` 区分。
- `direction`：核心做轻量推断，默认策略：
  - 若目的端口为知名服务端口（<1024）且源端口为高端口，视为 `client_to_server`。
  - 无法判断时设为 `unknown`。
  - 插件可在 payload 中通过 `_meta.direction` 覆盖，最终写入 `EventContext.Direction`。

### 1.2 DecodeResponseV2

```protobuf
message DecodeResponseV2 {
  string input_id        = 1;  // 对应 DecodeRequest.input_id
  bool   done            = 2;  // true 表示该 input_id 的结果已全部发完

  // 当 done=true 时，以下字段为空
  string event_type      = 3;
  string schema_id       = 4;
  bytes  payload_msgpack = 5;
  string error           = 6;

  // 插件提供的因果链线索
  string correlation_key    = 7;
  string causation_input_id = 8;
}
```

行为：

- 一个 `input_id` 可产生 0 条或多条结果。
- 插件对同一个 `input_id` 按顺序发送 `N` 条结果消息，最后发送一条 `done=true` 的空结果。
- `error` 非空时，表示该条结果解码失败；可单独产出 `decode.error` 事件或计入错误统计。
- `correlation_key` 映射为 `EventV2.Relation.CorrelationID`。
- `causation_input_id` 由核心维护 `input_id -> EventID` 映射，映射为 `EventV2.Relation.CausationID`。

### 1.3 V1 兼容

V1 协议（`Decode` RPC）保持 JSON 输出不变。对于 V1 插件：

- 核心构造 `DecodeRequest` 时不填充新增字段（或只填充已知字段）。
- 一个请求仍只对应一个响应，按现有逻辑转换为单条 `EventV2`。
- `input_id` 由核心内部生成，用于统一后续处理。

## 2. Dispatcher 行为

`Dispatcher` 内部按 `input_id` 缓冲结果：

```go
type pendingInput struct {
  ctx      context.Context
  request  *pb.DecodeRequest
  results  []*pb.DecodeResponseV2
}
```

流程：

1. 发送 `DecodeRequest` 前生成 `input_id`。
2. 进入 `Recv` 循环，按 `input_id` 归类响应。
3. 遇到 `done=true` 时，一次性将该 `input_id` 的所有结果转换为 `EventV2`。
4. 转换完成后才发送下一个 `DecodeRequest`。
5. 每个 pending input 设置超时（默认 5s）；超时未收到 `done=true` 则丢弃该 input 结果并记录错误，然后继续处理下一包。

## 3. EventV2 模型

### 3.1 新增 Context

```go
// EventContext 描述事件的网络上下文，跨协议通用。
type EventContext struct {
  FlowID         string `msgpack:"flow_id,omitempty"`
  RawPacketID    string `msgpack:"raw_packet_id,omitempty"`
  MessageOrdinal int    `msgpack:"message_ordinal,omitempty"`
  Direction      string `msgpack:"direction,omitempty"`
}
```

规则：

- `FlowID`：来自 `DecodeRequest.flow_id`。
- `RawPacketID`：来自 `DecodeRequest.packet_id`。
- `MessageOrdinal`：同一 `input_id` 内的结果序号，从 0 开始。
- `Direction`：优先取插件覆盖值，否则取核心推断值。

`Context` 与 `Payload` 概念分离，不放业务数据。存储层新增 `context` BLOB 列（MsgPack 编码），与 `payload` 并列，便于下游直接按 flow/direction 过滤而无需反序列化整个 payload。

### 3.2 构造函数更新

```go
func NewEventV2(
  sessionID string,
  eventType EventType,
  schemaID string,
  source SourceID,
  ctx EventContext,
  payload Value,
) *EventV2
```

### 3.3 events 表更新

在现有 `events` 表基础上新增 `context` 列：

```sql
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    type TEXT NOT NULL,
    schema_id TEXT NOT NULL,
    source TEXT NOT NULL,
    timestamp INTEGER NOT NULL,
    causation_id TEXT,
    correlation_id TEXT,
    origin_id TEXT,
    context BLOB NOT NULL,      -- MsgPack 编码的 EventContext
    payload BLOB NOT NULL,      -- MsgPack 编码的 Payload.Value
    created_at INTEGER NOT NULL
);
```

对已存在的数据库，通过 `ALTER TABLE events ADD COLUMN context BLOB` 迁移；旧行 `context` 为空对象。

## 4. StateChange 投影

### 4.1 类型定义

替换 `EntitySnapshot`：

```go
type StateChange struct {
  SubjectType string `msgpack:"subject_type"`
  SubjectID   string `msgpack:"subject_id"`
  Op          string `msgpack:"op"`     // set | delete | merge
  Path        string `msgpack:"path"`
  Before      Value  `msgpack:"before,omitempty"`
  After       Value  `msgpack:"after,omitempty"`
  Version     int64  `msgpack:"version,omitempty"`
  Metadata    Value  `msgpack:"metadata,omitempty"`
}
```

### 4.2 插件产出方式

插件在 payload 中通过 `_state_changes` 字段提供：

```msgpack
{
  "data": { ... },
  "_state_changes": [
    {
      "subject_type": "player",
      "subject_id": "1001",
      "op": "set",
      "path": "gold",
      "after": 100,
      "version": 5
    }
  ]
}
```

### 4.3 存储表

替换 `entity_snapshots` 表：

```sql
CREATE TABLE IF NOT EXISTS state_changes (
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  flow_id TEXT,
  timestamp INTEGER NOT NULL,
  subject_type TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  op TEXT NOT NULL,
  path TEXT NOT NULL,
  before_value TEXT,
  after_value TEXT,
  version INTEGER,
  metadata TEXT
);

CREATE INDEX IF NOT EXISTS idx_state_changes_subject
  ON state_changes(session_id, flow_id, subject_type, subject_id, timestamp);
```

旧 `entity_snapshots` 表在下一版本删除；本版本先停止写入，保留表结构以便历史数据可读。

## 5. Schema Registry

### 5.1 Manifest 声明

插件 `plugin.yaml` 新增 `schemas` 段：

```yaml
schemas:
  - id: "http.request.v1"
    version: 1
    indexable_fields:
      - path: "data.method"
        type: "string"
        alias: "method"
      - path: "data.path"
        type: "string"
        alias: "path"
  - id: "http.response.v1"
    version: 1
    indexable_fields:
      - path: "data.status"
        type: "int"
        alias: "status"
```

### 5.2 运行时校验

- `PluginManager` 在注册插件时解析 `schemas` 并登记到内存 `SchemaRegistry`。
- `Dispatcher` 收到 `DecodeResponseV2` 时校验 `schema_id` 是否已注册。
- 未注册：`schema_id` 降级为 `"unknown.v1"`，记录 warning，不影响后续处理。
- Payload 结构由插件负责，核心不校验。

### 5.3 Go 类型

```go
type SchemaRegistry struct {
  mu      sync.RWMutex
  schemas map[string]*SchemaDecl
}

type SchemaDecl struct {
  ID              string
  Version         int
  IndexableFields []IndexableField
}

type IndexableField struct {
  Path  string
  Type  string // string | int | float | bool
  Alias string
}
```

## 6. 因果链映射

### 6.1 correlation_key

插件在结果中提供 `correlation_key`，核心直接写入 `EventV2.Relation.CorrelationID`。

### 6.2 causation_input_id

核心维护最近 N 个 `input_id` 到主事件 ID 的映射：

```go
type causationIndex struct {
  mu      sync.Mutex
  entries map[string]event.EventID // input_id -> EventID
  order   []string
  limit   int
}
```

映射规则：

- 一个 `input_id` 产生多条事件时，取第一条非 error 事件作为主事件 ID。
- 后续结果若 `causation_input_id` 命中该映射，则设置 `Relation.CausationID`。
- 未命中时 `CausationID` 为空。

## 7. 投影索引表

### 7.1 表结构

```sql
CREATE TABLE IF NOT EXISTS event_index (
  event_id TEXT PRIMARY KEY REFERENCES events(id),
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  timestamp INTEGER NOT NULL,
  flow_id TEXT,
  direction TEXT,
  correlation_id TEXT,
  projection_json TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_index_session_time
  ON event_index(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_event_index_flow
  ON event_index(session_id, flow_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_event_index_correlation
  ON event_index(correlation_id);
```

### 7.2 写入逻辑

`Dispatcher` 在把结果转为 `EventV2` 时，根据插件 manifest 中对应 `schema_id` 的 `indexable_fields`：

1. 按 `path` 从 `Payload.Value` 中取值。
2. 按 `type` 转换（失败则跳过并 warning）。
3. 以 `alias` 为键写入 `projection_json`。

示例 projection_json：

```json
{"method": "POST", "path": "/api/login"}
```

## 8. 数据流

```
Packet
  |
  v
Dispatcher.DecodeV2(pkt)
  - 生成 input_id / packet_id
  - 生成 flow_id / direction
  - 发送 DecodeRequest
  |
  v
Plugin (per flow_id 隔离状态)
  - 返回 DecodeResponseV2 x N
  - 最后返回 done=true
  |
  v
Dispatcher 收齐 input_id
  - 校验 schema_id
  - 构建 EventV2(Identity, Relation, Context, Payload)
  - 提取 StateChange -> state_changes 表
  - 提取 projection -> event_index 表
  |
  v
Store.AppendEventsV2(events)
```

## 9. 兼容性

| 组件 | 变化 | 兼容策略 |
|---|---|---|
| `Decode` V1 RPC | 不变 | 旧插件继续工作 |
| `DecodeV2` RPC | proto 字段新增/语义改变 | 同步升级插件；旧 V2 插件需改造 |
| `EventV2` | 新增 Context | 新代码读写；旧读取方忽略未知字段 |
| `EntitySnapshot` | 删除 | 停止写入；旧表保留只读 |
| `events` 表 | 新增 `context` BLOB 列 | 通过 ALTER TABLE 迁移；旧行为空对象 |
| `event_index` 表 | 新增 | 新查询使用；旧查询不受影响 |

## 10. 关键文件变动

- `pkg/plugin/proto/plugin.proto`：更新 `DecodeRequest`、`DecodeResponseV2`。
- `pkg/plugin/proto/plugin.pb.go` / `plugin_grpc.pb.go`：重新生成。
- `pkg/event/event_v2.go`：新增 `EventContext`。
- `pkg/event/event.go`：删除 `EntitySnapshot`，新增 `StateChange`。
- `pkg/event/adapter.go`：适配 `StateChange` 与 `Context`。
- `pkg/decode/dispatcher.go`：实现 input_id 缓冲、schema 校验、投影提取。
- `pkg/decode/fields.go`：移除 entity_snapshots 解析，新增 state_changes 解析。
- `pkg/plugin/manifest.go`：新增 `schemas` 解析。
- `pkg/schema/schema.go` 或新增 `pkg/schema/registry.go`：内存 schema registry。
- `pkg/store/sqlite.go`：新增 `state_changes`、`event_index` 表。
- `pkg/store/eventstore.go`：接口中 `WriteEntitySnapshots` 改为 `WriteStateChanges`。
- `pkg/store/event_v2_writer.go`：写入 `events` + `event_index`。
- `pkg/plugin/contract/contract.yaml`：更新契约。
- `plugins/http/plugin.yaml`：声明 schemas 与 indexable_fields。
- `plugins/http/main.go`：适配新 DecodeV2 接口（多结果 + done）。

## 11. 测试策略

- 单元测试：
  - `dispatcher_v2_test.go`：0 条、1 条、N 条结果 + done 标记。
  - `state_change_test.go`：`_state_changes` 提取与存储。
  - `schema_registry_test.go`：注册、命中、未命中降级。
  - `event_index_test.go`：投影字段提取与写入。
- 集成测试：
  - HTTP 插件产生请求+响应两个结果，验证 correlation/causation。
- 回归测试：
  - V1 插件路径不变。
  - 现有 `events` 表查询输出格式不变。

## 12. 风险与缓解

| 风险 | 缓解 |
|---|---|
| V2 协议 breaking change | 本次同时升级主程序与插件；V1 路径保留 |
| 插件未发 done 导致 Dispatcher 挂起 | 增加 per-input 超时；超时后丢弃并 error |
| flow_id 冲突 | 字符串 hash 足够大；session_id 区分 |
| projection 字段类型不匹配 | 跳过并 warning，不阻断 |
| 旧 EntitySnapshot  consumers 失效 | 同步修改 MCP 查询；旧表保留只读 |
