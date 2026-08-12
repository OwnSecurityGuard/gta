# Semantic & Evidence Contract v1 (SSOT)

> **契约版本：`v1`** — 与代码常量 `semantic.SemanticContractVersion = "v1"`
> （`pkg/analyze/semantic/types.go`）严格对应。本文档描述的所有类型、枚举、
> JSON 结构均以 `v1` 为稳定边界；任何破坏性变更（字段语义改变、枚举取值删除/
> 重命名、边类型契约变化）将进入 `v2`，且会在此处显式标注迁移说明。
>
> 单一事实来源（Single Source of Truth）。本文档与 `pkg/event`（事实层）和
> `docs/event.md`（Event 契约）并列，专门定义 **Event 如何获得语义**、以及
> **语义关系如何成为 Evidence**。
>
> 本文档的 Go 类型实现位于 `pkg/analyze/semantic/types.go`。
> 文档与代码冲突时，以本文档描述的逻辑语义为准，代码负责落地。

> **Phase 状态**：Phase 1（契约冻结：仅类型 + 文档，不改 Capture/Event/Plugin
> RPC/MCP Tool/DB/UI）已完成。Phase 2（Semantic 投影）起为向后兼容增量。

---

## 0. 三层模型（铁律）

```
                    Event
                     │
              immutable fact（事实层，不改）
                     │
                     ▼
              SemanticEvent
                     │
       ┌─────────────┼─────────────┐
       │             │             │
      kind          name       operation
       │             │             │
    request       LoginReq       login
    response      LoginResp      login
    push          PushXXX        ""
       │
       ▼
                  Evidence
                     │
          ┌──────────┴──────────┐
          │                     │
        Node                   Edge
          │                     │
       EventRef          source / target
       Semantic           relation
       timestamp          confidence
                          strength
                          method
                          rule_id
                          reason
                          evidence_ids
```

三个层次严格区分，互不污染：

| 层 | 回答的问题 | 可否修改 | 典型产物 |
|---|---|---|---|
| **Event** | 发生了什么？ | 否（immutable / append-only） | `event_id / type / payload` |
| **SemanticEvent** | 这个 Event 在协议/业务上意味着什么？ | 投影，可重算 | `kind / name / operation / direction` |
| **Evidence** | 为什么认为两个 Event 有关系？ | 分析产物，可重算 | `Node + Edge（含 strength/method/rule_id）` |

**铁律：事实与推理不能污染 Event。**
Event 内不得新增 `confidence`、`semantic_role`、`response_to`、`reason` 等字段。
这些全部属于 Semantic / Evidence Projection。否则 Event Store 会退化为
「原始事实 + 当前 AI/规则认为它是什么」，最终失去稳定性。

---

## 1. Semantic Contract 的边界

**Semantic Contract 不是业务 Schema。** 它只规定 Agent / UI 如何理解一个 Event 的
「语义身份」，绝不规定：

```
Player / Inventory / HP / Gold / Quest / Guild / Item / NPC / Match ...
```

这些全部属于具体游戏领域，由插件（Plugin）在 `payload` 与 `_state_changes` 中承载，
不在核心 Contract 之内。

同理，v1 **明确不做**：统一业务 Schema、UI Component Schema、AI Reasoning Trace、
Vector / Embedding、知识图谱数据库（Neo4j）、跨 Session Knowledge Graph、
自动 AI 推理、自动生成业务 ontology、重构 Event、重构 Payload。

---

## 2. SemanticEvent v1

```go
type SemanticEvent struct {
    EventID   event.EventID    `json:"event_id"`
    SessionID string           `json:"session_id"`
    FlowID    string           `json:"flow_id,omitempty"`

    Kind      SemanticKind     `json:"kind"`
    Name      string           `json:"name,omitempty"`
    Operation string           `json:"operation,omitempty"`

    Direction string           `json:"direction,omitempty"`

    Subject   *SemanticSubject `json:"subject,omitempty"`

    Confidence float64         `json:"confidence"`

    Source    SemanticSource   `json:"source"`
}
```

### 2.1 Kind（v1 固定枚举，不再扩充）

| 值 | 含义 | 示例 |
|---|---|---|
| `message` | 仅有消息身份，尚未进一步判断语义 | 心跳包、仅透传的 `CS_Login` |
| `request` | 客户端发起的请求 | `LoginReq` 可判定为 request |
| `response` | 对请求的响应 | `LoginResp` |
| `push` | 服务端主动推送（非请求-响应模型） | `SC_PlayerInfo` |
| `state_change` | 实体状态变更 | 由 `_state_changes` 投影 |
| `transaction` | 时间聚类产生的逻辑事务组 | 一次登录流程 |

`message` 是「语义未决」的安全默认值——能判定就升格为 `request/response/push`，
判定不了就留在 `message`，**不要强行归类**。

### 2.2 Name（协议原始名称，不规范化）

`Name` 是协议原始消息名，**保留原样，不要规范化**：

```
LoginReq / LoginResp / CS_PlayerInfo / SC_PlayerInfo
```

它是最重要的原始语义证据之一。

### 2.3 Operation（可选的规范化业务操作名）

`Operation` 是可选的、跨命名风格统一的业务操作名：

```
LoginReq / LoginRequest / C2S_Login  →  operation = "login"
```

**但必须允许 `operation = ""`**。很多情况下无法可靠判断，此时留空。

> **重要规则：猜不到就不填。** 绝不能为了「完整」而强行推断。这是后续 Agent 信任模型的基础。

### 2.4 Direction（复用既有语义，不重新发明）

直接复用 Event 已有的网络上下文，**不另起一套**。契约层已在 `types.go` 中定义为常量，
全仓统一引用，避免各层各写各的字符串字面量导致漂移：

```go
const (
    DirectionClientToServer string = "client_to_server"
    DirectionServerToClient string = "server_to_client"
    DirectionUnknown       string = "unknown"
)
```

取值仅限以上三者。

### 2.9 投影来源（Phase 2 输入约定）

`SemanticEvent` 的各字段由 Phase 2 的 `SemanticProjector.Project(e *event.Event) SemanticEvent`
**纯函数**确定性映射得到（实现见 `pkg/analyze/semantic/projector.go`）。来源固定如下：

| SemanticEvent 字段 | 来源 | 说明 |
|---|---|---|
| `EventID` | `Event.Identity.ID` | 透传 |
| `SessionID` | `Event.Identity.SessionID` | 透传 |
| `FlowID` | `Event.Context.FlowID` | 五元组流标识（确定性事实） |
| `Name` | `payload._meta.msg_name` | 协议原始消息名，原样保留；缺省 `""` |
| `Direction` | `payload._meta.direction` 优先，回退 `Event.Context.Direction` | 网络方向；两者皆空则为 `unknown` |
| `Kind` | `_meta.kind`（须为合法枚举）> `Identity.Type` 后缀确定性匹配 > 默认 `message` | 语义种类，**绝不猜测** |
| `Operation` | Phase 2 一律 `""` | 无确定性规则表，禁止推测（硬约束 3） |
| `Subject` | Phase 2 一律 `nil` | 不做 player/entity 自动识别（硬约束 2） |
| `Confidence` | 固定 `1.0` | Phase 2 只做事实/确定性投影（硬约束 4） |
| `Source` | 固定 `engine`（Phase 2 冻结） | 投影本身是 Engine 的确定性投影，输出恒由引擎产生 |

`Kind` 的后缀匹配是确定性映射（事件类型是显式事实，非推断）：

| `Identity.Type` 包含 | 映射为 |
|---|---|
| `request` | `request` |
| `response` | `response` |
| `push` | `push` |
| `state_change` / `statechange` | `state_change` |
| `transaction` | `transaction`（仅当类型本身显式，事务"关系聚类"属 Phase 3） |
| 均无匹配 | `message`（中性，不猜测） |

> 这是「确定性映射」而非「AI 推理」。`Operation` 与 `Subject` 在 Phase 2 **必须留空**，
> `Confidence` **必须为 1.0**——任何 0.82 这类"推断值"都属于 Phase 3，绝不允许在此出现。
> 这是 Agent 信任模型的基础，也是三层职责干净的前提。

> **Source 冻结为 `engine`（v1 不按 `_meta` 推导来源）**：当前 `Event._meta` 承担"系统附加
> 结构化字段"（direction/flow_id/msg_name 等，由 Pipeline 补充），不等价于"插件声明语义来源"。
> 用 `_meta` 是否存在推导 `source` 会把"语义来源"与"元数据来源"混为一谈。
> 因此 Phase 2 一律 `Source = engine`。未来若需表达"插件显式声明语义
> （如 semantic.kind / semantic.operation）"，应引入**明确来源标记**，例如：
> `_meta.semantic_source = "plugin"`，或在 Event 输入契约中定义独立的语义来源字段——
> 而不是复用 `_meta` 的存在性。

### 2.5 Subject（轻量实体引用，非完整 Entity Schema）

```go
type SemanticSubject struct {
    Type string `json:"type"`
    ID   string `json:"id"`
}
```

示例：

```json
{ "type": "player", "id": "12345" }
{ "type": "session", "id": "abc" }
```

`player / guild / item / npc / match / session` 等都可作为业务实体，
但 **GTA 不规定这些 `Type` 的具体含义**——含义由各插件 / 领域自行约定。

### 2.6 Confidence（判定可信度）

统一取值 `0.0 <= confidence <= 1.0`，并有明确语义：

| 区间 | 含义 |
|---|---|
| `1.0` | 事实直接得出 |
| `0.9+` | 极强证据 |
| `0.7–0.9` | 高可信推断 |
| `0.5–0.7` | 弱推断 |
| `<0.5` | **不应作为默认 Agent 事实** |

**关键区分：`Confidence` ≠ `Evidence Strength`（见第 4 节）。**
例如「`LoginReq` 是一个 request」是 decoder 明确告知（confidence=1.0，strength=observed）；
而「`LoginResp` 是 `LoginReq` 的响应」可能是 GTA 推导出来的（即便 confidence 高，
其 strength 仍可能是 inferred）。两者必须分开记录。

### 2.7 Source（判定来源）

```go
type SemanticSource string
const (
    SourcePlugin SemanticSource = "plugin"  // 解码插件明确告知
    SourceRule   SemanticSource = "rule"    // 显式规则映射
    SourceEngine SemanticSource = "engine"  // 语义引擎推导
    SourceUser   SemanticSource = "user"    // 用户/外部标注
)
```

Agent 据此区分：

```json
{ "operation": "login", "source": "rule",     "confidence": 0.86 }
{ "operation": "login", "source": "plugin",   "confidence": 1.0  }
```

两者含义完全不同。

### 2.8 SemanticEvent JSON 示例

```json
{
  "event_id": "018f1c2a-3b4d-7e5f-9a0b-1c2d3e4f5a6b",
  "session_id": "sess-001",
  "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
  "kind": "request",
  "name": "LoginReq",
  "operation": "login",
  "direction": "client_to_server",
  "subject": { "type": "player", "id": "12345" },
  "confidence": 1.0,
  "source": "plugin"
}
```

---

## 3. Evidence Node v1

```go
type EvidenceNode struct {
    ID         string           `json:"id"`
    Kind       EvidenceNodeKind `json:"kind"`

    EventID    event.EventID    `json:"event_id,omitempty"`
    SessionID  string           `json:"session_id"`
    FlowID     string           `json:"flow_id,omitempty"`

    Semantic   *SemanticEvent   `json:"semantic,omitempty"`

    Timestamp  time.Time        `json:"timestamp"`
    Labels     map[string]string `json:"labels,omitempty"`
    Properties map[string]any   `json:"properties,omitempty"` // 兼容期保留，Phase 3 评估后清理
}
```

### 3.1 Node Kind（v1 固定，不再扩充）

| 值 | 含义 |
|---|---|
| `raw_packet` | 原始抓包 |
| `event` | 解码后的事件 |
| `state_change` | 单个状态变更投影 |
| `entity` | 业务实体（由 StateChange 聚合） |
| `transaction` | 时间聚类产生的事务组 |

### 3.2 节点 ID 命名方案（隐性契约，必须稳定）

| Node Kind | ID 方案 | 示例 |
|---|---|---|
| `event` | `evt_<eventID>` | `evt_018f...` |
| `raw_packet` | `pkt_<rawPacketID>` | `pkt_abc123` |
| `state_change` | `sc_<eventID>_<subjectType>_<path>` | `sc_018f..._player_hp` |
| `entity` | `ent_<session>_<flow>_<subjectType>_<subjectID>` | `ent_sess-001_..._player_12345` |
| `transaction` | `txn_<flow>_<seq>` | `txn_10.0.0.1..._1` |

`EventID` 字段在 event 类节点上冗余记录原始事件 ID，便于从 Evidence 回溯到 Event。

### 3.3 EvidenceNode JSON 示例

```json
{
  "id": "evt_018f1c2a-3b4d-7e5f-9a0b-1c2d3e4f5a6b",
  "kind": "event",
  "event_id": "018f1c2a-3b4d-7e5f-9a0b-1c2d3e4f5a6b",
  "session_id": "sess-001",
  "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
  "semantic": {
    "event_id": "018f1c2a-3b4d-7e5f-9a0b-1c2d3e4f5a6b",
    "session_id": "sess-001",
    "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
    "kind": "request",
    "name": "LoginReq",
    "operation": "login",
    "direction": "client_to_server",
    "confidence": 1.0,
    "source": "plugin"
  },
  "timestamp": "2026-08-12T22:00:00Z"
}
```

---

## 4. Evidence Edge v1（本契约最重要部分）

```go
type EvidenceEdge struct {
    ID          string           `json:"id"`
    Source      string           `json:"source"`
    Target      string           `json:"target"`
    Type        RelationType     `json:"type"`

    Confidence  float64          `json:"confidence"`

    Strength    EvidenceStrength `json:"strength"`
    Method      EvidenceMethod   `json:"method"`
    RuleID      string           `json:"rule_id,omitempty"`
    Reason      string           `json:"reason,omitempty"`
    EvidenceIDs []string         `json:"evidence_ids,omitempty"`

    Properties  map[string]any   `json:"properties,omitempty"` // 兼容期保留，Phase 3 评估后清理
}
```

### 4.1 RelationType（v1 稳定集合）

| 值 | 含义 | v1 状态 |
|---|---|---|
| `decoded_from` | 解码结果来自某原始包 | ✅ 稳定 |
| `contains` | 批量/容器消息包含逻辑消息 | ✅ 稳定 |
| `response_to` | RPC 响应对应请求 | ✅ 稳定 |
| `caused_by` | 高置信因果关系 | ✅ 稳定 |
| `correlated_with` | 统计或规则相关性 | ✅ 稳定 |
| `updates` | 增量更新实体字段 | ✅ 稳定 |
| `possible_followup` | 低置信时间邻近（非真实因果） | ✅ 稳定 |
| `parameter_from` | 参数来源 | ⚠️ **experimental**（易与业务领域绑定，v1 不标准化，消费者不应依赖其稳定性） |

### 4.2 EvidenceStrength（必须补齐，与 Confidence 分开）

```go
type EvidenceStrength string
const (
    EvidenceObserved EvidenceStrength = "observed" // 直接事实。例：event A decoded_from raw packet B
    EvidenceDerived  EvidenceStrength = "derived"  // 由确定规则得到。例：同一 correlation_id
    EvidenceInferred EvidenceStrength = "inferred" // 推测。例：仅凭名字与时间推断 LoginReq → LoginResp
)
```

### 4.3 EvidenceMethod（记录判定方法）

```go
type EvidenceMethod string
const (
    MethodPlugin          EvidenceMethod = "plugin"          // 插件直接声明
    MethodCorrelation     EvidenceMethod = "correlation"     // 同 correlation_key / correlation_id
    MethodNamePattern     EvidenceMethod = "name_pattern"    // 请求-响应命名模式
    MethodTemporal        EvidenceMethod = "temporal"        // 时间邻近
    MethodStateProjection EvidenceMethod = "state_projection"// 状态变更投影
    MethodTransaction     EvidenceMethod = "transaction"     // 事务聚类
)
```

### 4.4 RuleID 必须统一（接 Plugin Contract 体系）

RuleID 与 Plugin Contract 的 rule 体系贯通（brief / verify / explain 同一套词汇），
使 Agent 可 `rule_id → explain → documentation` 追溯，避免语义引擎维护第二套规则词汇。

推荐内置 rule_id：

```
response-name-pair   # 请求-响应命名对
same-correlation    # 同一 correlation_key
same-flow           # 同一 flow
temporal-followup   # 时间邻近
state-update        # 状态变更
```

### 4.5 可解释性要求（Evidence 必须可解释）

每条 Edge 必须能回答：

```
谁 → 谁（source / target）
是什么关系（type）
为什么（reason，需说明依据，而非 "matched by name"）
可信度多少（confidence ∈ [0,1]）
是事实 / 推导 / 推测（strength）
用什么方法（method）
哪条规则（rule_id）
依据哪些 Evidence（evidence_ids）
```

如果回答不了，**这条 Edge 就不应该生成。**

### 4.6 EvidenceEdge JSON 示例

```json
{
  "id": "edge_evt_aaa_evt_bbb_response_to",
  "source": "evt_018f1c2a-...-aaa",
  "target": "evt_018f1c2a-...-bbb",
  "type": "response_to",
  "confidence": 0.92,
  "strength": "inferred",
  "method": "name_pattern",
  "rule_id": "response-name-pair",
  "reason": "LoginReq and LoginResp share the same base operation and occur in the same flow",
  "evidence_ids": [
    "018f1c2a-...-aaa",
    "018f1c2a-...-bbb"
  ]
}
```

---

## 5. Evidence Graph v1

```go
type EvidenceGraph struct {
    Nodes         []EvidenceNode  `json:"nodes"`
    Edges         []EvidenceEdge  `json:"edges"`
    Uncertainties []string        `json:"uncertainties,omitempty"`
}
```

`Uncertainties` 保留用于无法建立强关系时的不确定性说明（例如
`server_to_client event X has correlation_key=Y but no pending request`）。

### 5.1 EvidenceGraph JSON 示例

```json
{
  "nodes": [
    {
      "id": "pkt_abc123",
      "kind": "raw_packet",
      "session_id": "sess-001",
      "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
      "timestamp": "2026-08-12T22:00:00.100Z"
    },
    {
      "id": "evt_018f1c2a-...-aaa",
      "kind": "event",
      "event_id": "018f1c2a-...-aaa",
      "session_id": "sess-001",
      "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
      "semantic": {
        "event_id": "018f1c2a-...-aaa",
        "session_id": "sess-001",
        "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
        "kind": "request",
        "name": "LoginReq",
        "operation": "login",
        "direction": "client_to_server",
        "confidence": 1.0,
        "source": "plugin"
      },
      "timestamp": "2026-08-12T22:00:00.120Z"
    },
    {
      "id": "evt_018f1c2a-...-bbb",
      "kind": "event",
      "event_id": "018f1c2a-...-bbb",
      "session_id": "sess-001",
      "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
      "semantic": {
        "event_id": "018f1c2a-...-bbb",
        "session_id": "sess-001",
        "flow_id": "10.0.0.1:5000->10.0.0.2:8080/tcp",
        "kind": "response",
        "name": "LoginResp",
        "operation": "login",
        "direction": "server_to_client",
        "confidence": 0.92,
        "source": "engine"
      },
      "timestamp": "2026-08-12T22:00:00.180Z"
    }
  ],
  "edges": [
    {
      "id": "edge_pkt_abc123_evt_aaa_decoded_from",
      "source": "evt_018f1c2a-...-aaa",
      "target": "pkt_abc123",
      "type": "decoded_from",
      "confidence": 1.0,
      "strength": "observed",
      "method": "plugin",
      "rule_id": "decode",
      "reason": "event decoded from raw packet abc123",
      "evidence_ids": ["abc123"]
    },
    {
      "id": "edge_evt_aaa_evt_bbb_response_to",
      "source": "evt_018f1c2a-...-bbb",
      "target": "evt_018f1c2a-...-aaa",
      "type": "response_to",
      "confidence": 0.92,
      "strength": "inferred",
      "method": "name_pattern",
      "rule_id": "response-name-pair",
      "reason": "LoginReq and LoginResp share the same base operation and occur in the same flow",
      "evidence_ids": ["018f1c2a-...-aaa", "018f1c2a-...-bbb"]
    }
  ],
  "uncertainties": []
}
```

### 5.2 Labels / Properties 收口（过渡字段，非 v1 契约）

`EvidenceNode` 与 `EvidenceEdge` 当前仍保留 `Labels` 与 `Properties` 两个自由字段，
**它们不属于 v1 契约**，仅为兼容现有 engine 的既有写入而暂存，将在 Phase 3 评估后清理：

- 它们是「过渡字段」，**不进入 v1 JSON 契约**——本文档所有 JSON 示例均不含二者。
- 消费者（Plugin / Agent / UI）**不应依赖** `Labels` / `Properties` 的存在或内容。
- v1 稳定的结构化字段（`Strength` / `Method` / `RuleID` / `Reason` / `EvidenceIDs` / `Semantic`）
  已足以承载可解释性需求，`Labels` / `Properties` 是历史包袱，最终会被移除。

---

## 6. 数据关系总览

```
Event
 │  1:1 / 1:N
 ▼
SemanticEvent
 │
 ▼
EvidenceNode
 │ ─────────────┐
 ▼             ▼
EvidenceEdge   EvidenceEdge
 │             │
 ▼             ▼
Event B       Event C
```

示例：

```
Event#1
  │
  ▼ SemanticEvent{ kind=request, name=LoginReq, operation=login }
Node#1
  │ response_to  confidence=0.92  strength=inferred
  │ method=name_pattern  rule_id=response-name-pair
  ▼
Node#2
  │
  ▼ SemanticEvent{ kind=response, name=LoginResp, operation=login }
```

---

## 7. v1 不做清单（范围控制）

- ❌ 统一游戏业务 Schema（Player / Item / Quest 等领域模型）
- ❌ UI Component Schema
- ❌ AI Reasoning Trace
- ❌ Vector / Embedding
- ❌ 知识图谱数据库 / Neo4j
- ❌ 跨 Session Knowledge Graph
- ❌ 自动 AI 推理 / 自动生成业务 ontology
- ❌ 重构 Event / 重构 Payload

---

## 8. Phase 1 验收标准（本 SSOT 对应实现已落地的判定）

1. 所有类型都有明确 JSON representation（`types.go` 中所有字段带 `json` tag）。
2. 所有 enum 都有固定值（Kind / Source / NodeKind / RelationType / Strength / Method）。
3. Event 与 SemanticEvent 明确分层（SemanticEvent 是投影，Event 不变）。
4. Evidence 与 Event 明确分层（Edge 不回写 Event）。
5. `observed / derived / inferred` 有明确语义且与 `confidence` 不混用。
6. `confidence` 与 `strength` 不混用（两个独立字段）。
7. 所有 `EvidenceEdge` 可定位到 `rule_id` / `method`（字段已存在，由 Phase 3 填充）。

> 验收底线：**任何 AI 据本文档，都能独立写出与本仓库一致的 JSON。**

---

## 9. 分阶段落地路线（与实现提交顺序对应）

| Phase | 目标 | 边界 |
|---|---|---|
| **1. Contract Freeze** | 仅定义契约 + 类型 + 本文档 | 不改 Capture / Event / Plugin RPC / MCP Tool / DB / UI |
| **2. Semantic Projection** | `Event → SemanticEvent` 确定性映射 | 不引入 AI 推理，不自动猜 player/login |
| **3. Evidence Graph v1 化** | 现有 Evidence Graph 迁移到 v1 Edge（补 strength/method/rule_id/evidence_ids） | 每条 edge 必须可解释 |
| **4. 统一 Agent/UI 输出** | 让已有 MCP Tool 输出统一 v1 Contract | 不新增 Tool |

建议提交顺序：

```
Commit 1  docs: freeze semantic-evidence v1 contract
Commit 2  feat(semantic): add semantic event contract
Commit 3  feat(semantic): add event semantic projector
Commit 4  refactor(semantic): migrate evidence graph to v1 contract
Commit 5  refactor(mcp): expose unified semantic evidence output
Commit 6  test(e2e): validate semantic evidence across real plugins
```

---

## 10. 最终世界观一句话

- **Fact**：`LoginReq` 是一个 request。（`strength=observed`）
- **Derived**：`LoginResp` 与 `LoginReq` 共享 `correlation_key`。（`strength=derived`）
- **Inferred**：`LoginResp` 仅凭 `LoginReq/LoginResp` 命名对 + 同 flow 推测为响应。（`strength=inferred`）
- **Unknown**：`possible_followup` 仅为时间邻近，绝不作为确定事实。
