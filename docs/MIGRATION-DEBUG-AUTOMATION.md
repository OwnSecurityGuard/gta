# GTA 重构迁移方案：Game Telemetry → Game Debug Automation

> 本文档记录 GTA 从「AI-native Game Telemetry Platform」重构为「Game Debug Automation Platform」
> 的概念收缩、代码改动与迁移决策。属于**保留技术资产、重新定义产品边界**，非推倒重来。

## 1. 产品边界重定义

| 维度 | 旧定位（已废弃） | 新定位（进行中） |
|------|------------------|------------------|
| 一句话 | AI-native Game Telemetry Platform | Game Debug Automation Platform |
| 核心闭环 | 抓包 → 解码 → 语义证据图 → AI 分析 | 真实游戏流量 → 协议解析 → Session 记录 → 问题定位 → 场景生成 → 自动回放 → AI 辅助调试 |
| 目标用户 | 数据分析 / 算法工程师 | QA / 压测工程师 / 客户端开发 / 服务端开发 |
| 价值主张 | 让 AI agent 做分析 | 让 AI agent 驱动调试闭环（capture once, see the whole flow） |

README 顶部 tagline、Features、Architecture、Examples、Agent self-check、Roadmap 已全部重写；
MCP 工具表由 `go run ./scripts/gen_tool_table` 从 `cmd/gta-mcp/main.go` 重新生成（40 → 37 tools）。

## 2. 删除清单（Phase 0 — cut concepts）

| 概念 | 处理 | 代码落点 |
|------|------|----------|
| **Evidence（AI 分析层）** | 删除语义证据引擎与证据图存储 | `pkg/analyze/semantic/`（10 文件全删）、`pkg/store/evidence_graph.go`（已先删）、`cmd/gta-mcp/evidence_v1_test.go` |
| **Semantic Relation（知识图谱语义）** | 删除 | `pkg/event/relation.go` 删除 |
| **Strength（观察/推导强度）** | 随 Evidence 一起删除 | semantic 包整体移除 |
| **Entity Graph** | 随 Evidence 一起删除 | semantic 包整体移除 |
| **Knowledge** | 随 Evidence 一起删除 | semantic 包整体移除 |
| **four-layer-demo 插件** | 删除（用户确认） | `plugins/four-layer-demo/`、`cmd/verify_four_layer/` |

### 关键决策：Relation 不删，改为 TraceContext（用户纠正）

原计划写"删除 Relation"，经确认这是**错误方向**：`Relation` 混淆了两个完全不同的东西：

- **Debug Trace Relation（保留）**——调试链路关系，是 Debug 平台的基础设施，对应 OpenTelemetry Trace Model：
  - `TraceID` ↔ `CorrelationID`（请求/响应聚合键）
  - `SpanID` ↔ `Event.ID`
  - `ParentSpanID` ↔ `CausationID`
  - 额外保留 `OriginID`（派生/回放来源）
- **Semantic Knowledge Relation（删除）**——面向知识推理的关系，是 Evidence 引擎的一部分。

**结论**：删除 Semantic Relation，保留 Execution Relation，并把 `Relation` 降级为 `TraceContext`
（OpenTelemetry 风格的执行追踪能力）。`TraceContext` 字段：`CausationID` / `CorrelationID` / `OriginID`。
`trace_protocol_flow`、`compare_sessions` 等核心调试能力**完全保留**，因为它们依赖执行链路。

实现：`pkg/event/trace.go`（`TraceContext` + `NewTraceContext` + `WithCausation/WithCorrelation/WithOrigin`）；
`event.go` 的 `Event.Trace` 替换原 `Event.Relation`；SDK 侧 `relation` 语义同步收缩（见 §5）。

## 3. 保留并重新语义化（Phase 0 — keep & re-semanticize）

| 资产 | 状态 | 说明 |
|------|------|------|
| **Event Store** | ✅ 保留 | → Debug Event。`Event = Identity + Trace + Context + Payload`（事件溯源，不可变、追加写、只记事实） |
| **Plugin SDK** | ✅ 保留 / 简化 | 四层契约收缩到 schema + state（见 §5，SDK 侧待完成） |
| **MCP** | ✅ 保留 | → Debug MCP。删除 4 个 evidence 工具（`query_evidence_graph` / `trace_event_chain` / `analyze_protocol_patterns` / `suggest_link_rules`），新增 `get_session_timeline` |
| **Pipeline** | ✅ 保留 | → Capture Pipeline。`capture → decode → project`（去掉 analyze 语义层） |
| **State 层** | ✅ 保留 | `pkg/state/baseline.go` 从 semantic 包抢救出的 `BaselineManager`，产出 `EnrichedStateChange`；写入 `state_changes` 投影的逻辑不丢失 |

## 4. 状态层保留的来龙去脉

`pkg/analyze/semantic/` 删除后，原本由 `semantic.Engine.Process` 产生的 `state_changes` 投影会断链。
解决：将 semantic 包里的 `BaselineManager` / `EntityKey` / `EntityBaseline` / `MemoryBaselineStore`
抽到独立、与 semantic 零耦合的 `pkg/state/baseline.go`，提供 `BaselineManager.Apply(ev, sessionID) ([]EnrichedStateChange, error)`。
写入路径改为：

- `cmd/gta-pipeline/capture_task.go`：`baseline.Apply(ev, t.sessionID)` 替代 `semanticEngine.Process(ev)`
- `cmd/gta-pipeline/decode_raw.go`：同上，直接 append

State 层是"保留项"，与 Evidence/Rule 无关，因此不受 Phase 0 删除影响。

## 5. Contract 四层 → schema + state（Phase 0 — Reduce）

| 层 | 旧 | 新 |
|----|----|----|
| schema | ✅ | ✅ 保留 |
| state | ✅ | ✅ 保留（baseline 投影） |
| evidence | ✅ | ❌ 删除（host/SDK 双侧已删） |
| rule | ✅ | ❌ 删除（host/SDK 双侧已删） |

**已完成（Task #5，2026-08-22）**：独立仓库 `gta-plugin-sdk` 已删除 `evidence/` + `rule/` 包，
`contract.yaml` 收缩到 `spec_version: 4`（schema+state），checker 移除 `checkEvidence`/`checkRule`，
`event.Relation` → `event.TraceContext`。godot-gateway / godot-world 插件已适配并编译通过；
SDK 已打 tag `v0.4.0` 并推送；宿主 `go.mod` 补 `replace` 指向本地 SDK。宿主 `go build -tags pcap ./...` 通过。

## 6. Phase 1 — Session 元数据与生命周期（本次完成）

领域模型新目标：`Packet → Message → Session → Scenario → Replay`。
Phase 1 落地"抓一次游戏：看到完整流程"。

### 6.1 已存在的基础设施（确认复用，不重复造）

- `control.sqlite` 的 `sessions` 表已含：`started_at / stopped_at / status / port / plugin / interface /
  pcap_file / raw_packets / events / metrics / decode_errors / duration_sec / db_path / manifest_snapshot`。
- `pkg/store/session_store.go`（`ControlStore`）已实现 `CreateSession / GetSession / ListSessions /
  UpdateSession / DeleteSession / ReconcileRunningSessions`。
- `list_all_sessions` / `list_live_sessions` / `get_session_status` / `delete_session` MCP 工具已存在。

### 6.2 数量自动维护（auto-maintain from write pipeline）

- `cmd/gta-pipeline/capture_task.go` 在 `run` 循环内用 `taskStats`（RawCount / EventCount / MetricCount /
  DecodeErrors 等）累计，每次 `flush` 更新 `statsSnap`（atomic，无锁）。
- `cmd/gta-pipeline/pipeline_service.go` 的 `finalizeTask`（run 退出回调，自动结束或显式停止都触发）
  将 `taskStats` 写入 `ControlStore.UpdateSession`。即：**会话结束后统计自洽**，无需手动维护。
- 运行中的会话可在 `GetStatus` 通过 `task.Snapshot()` 取实时统计（增量路径已具备，按需暴露）。

### 6.3 新增 / 增强的 MCP 工具

- **`get_session_timeline`**（新增，MVP 核心）——整 session 的 request/response 因果树：
  - 输入：`session_id`（必填）、`limit`（默认 500，上限 5000）、`offset`。
  - 算法：从 `events` 表按时间戳升序读取，复用 `eventToMessage` 适配器提取 `msg_name` / `direction` /
    `is_push`；按 `TraceContext.CausationID` 建父子树（OpenTelemetry parent span），按 `CorrelationID`
    聚合为"对话/请求-响应分组"。
  - 输出：嵌套 `roots` 树 + `conversations` 聚合视图 + 会话上下文（plugin/status）+ uncertainties。
  - 实现：`cmd/gta-mcp/trace_timeline.go`，含单元测试 `trace_timeline_test.go`（树构建、悬空 causation、
    时间戳稳定排序、对话聚合均覆盖，已通过）。
- **`list_all_sessions` 增强**——新增可选 `status` 过滤：`running | stopped | error | success | failed`
  （`failed` 映射到内部 `status="error"`）。满足原计划"get_sessions（failed/success filter）"。

## 7. 数据流（迁移后）

```
Packet（抓包，预分配 ID）
  → Dispatcher（补 flow_id / direction，gRPC DecodeV2 双向流）
  → 插件解码（MsgPack）
  → Event（Identity + Trace + Context + Payload）
  → 落库 appendEvent → 投影 appendEventIndex / WriteEnrichedStateChanges（BaselineManager）
  → MCP 查询：list_decoded_data / get_session_timeline / trace_protocol_flow / list_state_changes
```

`TraceContext` 是贯穿全链路的执行追踪主键；`scenario_id` / `replay_id` 列已预留在 `events` 表
（`sqlite.go` DDL + ALTER 迁移 + `probeTraceCols` 兼容旧库），供 Phase 2 / Phase 3 前向使用。

## 8. 开发优先级（来自原计划）

```
Phase 0  删除概念（Evidence/Relation Semantic/Strength/Entity Graph/Knowledge）     ✅ 完成
Phase 1  Session 元数据与生命周期（get_session_timeline / status 过滤）            ✅ 完成
Phase 2  Scenario —— 从 Session 抽取可回放场景
Phase 3  Replay —— 自动回放 Session / Scenario
Phase 4  MCP Agent —— AI 辅助调试闭环
```

Contract 收缩（schema+state，SDK 侧 Task #5）✅ 已完成（2026-08-22，tag v0.4.0）。Phase 0 全部收尾。

## 9. 验证状态

- `go build -tags pcap ./...` —— 通过（exit 0）
- `go vet -tags pcap ./cmd/gta-mcp/... ./pkg/store/... ./pkg/event/...` —— 通过
- `go test -tags pcap ./pkg/store/... ./cmd/gta-mcp/...` —— 通过（含 `TestBuildTimeline_*`）
- MCP 工具表重新生成：37 tools

## 10. 已知风险 / 后续

- **运行会话增量统计**：当前 counts 在 `finalizeTask` 落库；运行中会话在 `list_all_sessions` 下 counts 为 0
  直到停止。如需实时，可在 `GetStatus` 暴露 `task.Snapshot()`（已具备）。
- `events` 表 `scenario_id` / `replay_id` 当前恒为空，待 Phase 2/3 填充。
