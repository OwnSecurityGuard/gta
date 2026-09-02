> ⚠️ **归档文档**：此报告记录 2026-08-16 之前的 AI 能力审计结果。相关证据/模式分析工具已在 2026-08-22 重构中删除，正文保留历史参考，不反映当前代码状态。

# GTA 全栈 AI 能力审计与可用性报告

> 📌 **2026-08-16 复核更新**：以下发现已在本轮整改中解决，正文保留原始审计记录：
> - `query_capture_table` 已实际注册（allowlist 含 `event_index` / `plugin_debug_access`），并随 README 工具表由脚本生成而可发现；
> - `explain_plugin` 的 `verify` 参数已在工具 schema 中声明；`begin_capture_run` 的提示性参数已在描述与 `get_capabilities` 中注明"不会自动启动抓包"；
> - 新增 `get_capabilities` 自描述工具目录；README 工具表改为 `go run ./scripts/gen_tool_table` 生成，杜绝计数漂移；
> - SDK 侧 `winio.DialPipe` 已按平台拆分（Linux CI 通过），SDK tag v0.3.0 与 host `SDKVersion` 对齐；
> - `tools/hotreload` 死代码与 `docs/sdk-agents-md-patch.md` 已删除，过期设计稿移入 `docs/archive/`。
> 其余未列条目仍为有效发现。

> 审计范围：`gta`（host）、`gta-plugin-sdk`（插件 SDK）、`gta/plugins`（插件仓库）
> 方法：4 个并行 Explore agent 逐文件清点 + 代码↔文档交叉核对；报告中"发现路径/缺口"均带 file:line，关键项已人工抽样复核。
> AI 视角定义：① 控制面 AI（调 MCP 工具）② 插件作者 AI（读 SDK）③ 分析 AI（读语义/证据文档）。
> 核心问题：AI 在使用时，各个功能是否**能被注意到、被正确使用**，以及如何**确认功能完整**。

## 0. 总览：功能"AI 可发现性"分布

| 区域 | 能力/工具数 | ✅ 可发现 | ⚠️ 部分 | ❌ 盲区/矛盾 |
|---|---|---|---|---|
| MCP 控制面 | 38(+2 条件) | 36 | 2 | 0 |
| 采集/会话 | 7 | 7 | 0 | 0* |
| 插件生命周期 | 17 | 16 | 0 | 1 |
| 数据查询面 | 6 | 4 | 0 | 2 |
| 分析/语义 | 引擎+语义+证据 | 3 | 1 | 1 |
| 插件 SDK | ~30 API | 24 | 4 | 2 |
| 插件仓库/脚手架 | 3 | 2 | 1 | 1 |

\* `list_raw_packets`/`decode_raw_packets` 需 `--enable-raw-debug` 才注册，默认不可见。

**结论**：控制面 / 插件生命周期 / 语义证据面整体"AI 可发现性"良好；**真正风险集中在「文档矛盾」与「代码有、文档/工具无」的盲区**（见 §8）。这正是上一轮 `_state_changes` 碎片化问题的同类病灶，且范围更大。

## 1. MCP 控制面（38 工具，全部已实现，非桩）

**发现路径（唯一）**：MCP 协议 `tools/list`。`mcp-go` 把每个 `AddTool` 的 `WithDescription` + 各参数 `mcp.Description` 作为 JSON-RPC schema 返回给 AI。仓库内**无任何独立工具清单 / 目录 markdown**（已 Glob 确认）。因此 AI 实际看到的"文档"= 源码内联长描述（`main.go:2275-2548`）。
- 端点：SSE `GET /sse` + `POST /message`（默认 `:8781`）；另挂 `/mcp` StreamableHTTP。到 gta-pipeline 的 gRPC 是 `:9888`（**非** MCP 端口，勿混）。

**AI 使用方案（确认功能完整）**：
- 采集组：`start_capture(port,plugin?)`→`stop_capture`→`get_session_status`；`list_interfaces`/`list_live_sessions`/`list_all_sessions`/`delete_session`。
  - *自检*：拿到 `session_id`+`db_path`；造流量；`get_session_status` 断言 `packets_in>0`、`event_count>0`、`decode_errors==0`。
- 插件组（17 个，见 §3）。
- 查询组：`list_decoded_data`/`list_state_changes`/`aggregate_query`/`get_capture_schema`（见 §4）。
- 语义组：`query_evidence_graph`/`trace_event_chain`/`analyze_protocol_patterns`/`suggest_link_rules`（见 §5）。
- 行为窗口：`begin_capture_run`/`end_capture_run`/`get_run_status`/`trace_protocol_flow`。

**⚠️ 两处会让 AI 误用（已核实）**：
1. `begin_capture_run` 的 `plugin_name/device/filter/port` 是 **no-op**（`run_handlers.go:79-93`）——不会自动起抓包，AI 必须自己先 `start_capture`。按 schema 字面理解会以为传参即自动采集。
2. `explain_plugin` 偷偷读 `Arguments["verify"]`（`dev_tools.go:389`），但 `AddTool` 只声明 `name`/`action`——AI 依赖 schema 不会知道该参数。

**❌ 条件注册**：`list_raw_packets`/`decode_raw_packets` 仅 `--enable-raw-debug`（或 `GTA_MCP_ENABLE_RAW_DEBUG=1`）时注册（`main.go:2483`）。AI 默认 `tools/list` 看不到。

## 2. 采集与会话

- `start_capture`（`main.go:395`）：端口抓包或 `pcap_file` 回放，可选 `plugin`，转发 gRPC `StartCapture`。完整实现。
- `get_session_status`（`main.go:504`）：实时态失败降级到元数据。
- `list_raw_packets`/`decode_raw_packets`：原始包取证，条件注册（见 §1）。

**AI 使用方案**：`start_capture(port=8984,plugin=<your-decoder>)` → 跑 examples 造流量 → `list_decoded_data(session_id,limit=50)` 断言返回含 `event_type`/`schema_id`/`data.*`；`stop_capture` 后 `get_session_status` 断言 `decode_errors==0`。

**AI 发现路径小结**：采集/会话面全部经 MCP 工具暴露，工具描述即文档，无盲区。

## 3. 插件生命周期（17 工具，16 真实 + 1 隐性）

全部为转发层（`gta-mcp` 转发 Developer Plane `pkg/plugindev` 与 Runtime Plane `gta-pipeline`），描述详尽：
- `create_plugin`（`create_plugin.go:20`→`scaffold.go:51`）：脚手架生成，真实。
- `build_plugin`（`dev_tools.go:19`）：编译并返回 file:line:col 诊断，真实。
- `activate_plugin`（`dev_tools.go:59`）：启动二进制并注入 `GTA_REGISTRY_ADDR`，联合校验 registered+online+manifest，真实。
- `deactivate_plugin`/`status_plugin`/`explain_plugin`/`verify_plugin`/`test_plugin`：均真实（`dev_tools.go`/`verify_tools.go`）。
- `get_plugin_contract`/`get_plugin_dev_guide`/`get_plugin_manifest`/`list_plugins`/`list_registered_plugins`/`deregister_plugin`/`get_registry_addr`/`sample_bytes_plugin`：真实。
- `set_session_plugin`（`main.go:670`）：运行中会话热切换解码器，真实。

**AI 使用方案（端到端确认插件可用）**：
`create_plugin(name=my-decoder,protocol=xxx)` → `build_plugin(name)` 断言 0 错误 → `activate_plugin(name)` 断言返回 `integrated:true` → `start_capture(port,plugin=my-decoder)` 拿 `session_id` → `verify_plugin(session_id,plugin=my-decoder)` 断言 `verdict=pass` → `list_decoded_data` 断言有事件。

**⚠️ 1 处不对称**：没有 `register_plugin` 工具——注册由 `activate_plugin` 拉起 SDK `RunRegisterLoop` 隐式完成。AI 可能误以为有显式 register 工具（deregister 有、register 无）。属设计合理但需 AI 知晓。

## 4. 数据查询面

| 工具 | 落库表 | 发现路径 | 盲区 |
|---|---|---|---|
| `list_decoded_data` | events | MCP 描述 + `docs/event.md` | ⚠️ event.md DDL 与真实 schema 漂移（缺 context 列、origin_id 索引） |
| `list_state_changes` | state_changes | MCP 描述 + `get_capture_schema` | ⚠️ `before_resolved/after_resolved` 语义仅在代码注释 |
| `aggregate_query` | aggregated_metrics | MCP 描述详尽 + 动态 examples | ✅ |
| `get_capture_schema` | 全表 | MCP 描述 | ✅ |
| `query_evidence_graph`/`trace_event_chain` | evidence_nodes/edges | MCP 描述 + `semantic-evidence-v1.md`(SSOT) | ✅ |

**❌ 两处真盲区（代码有、AI 无法查询）**：
1. `event_index` 表（`projection_json`，由 schema `indexable_fields` 投影）**无任何专用 MCP 工具**，仅内部 `RawQuery` 使用（`event_writer.go:99`）。AI 想验证"索引投影是否生效"无出口。
2. `plugin_debug_access` 审计表（取证留痕，`debug_access.go:27`）写入明确，但**无对等读取工具**暴露给 AI。

**AI 使用方案**：`list_decoded_data(limit=50)` 断言 `event_type`/`schema_id`/`payload` 非空；`list_state_changes(subject_type=player)` 断言返回 `op`/`path`/`before`/`after`；`aggregate_query(expression='http_req_count')` 断言返回 `{name,window,value,group}`。

## 5. 分析与语义

- **SemanticProjector（Phase 2）**：`semantic/projector.go:46`，纯函数 `Project(ev)→SemanticEvent`，硬约束 confidence 恒 1.0 / operation 恒 "" / 不猜测。文档：✅ `semantic-evidence-v1.md §2`（与代码高度吻合，是 5 份文档中最可信的）。
- **EvidenceGraph（Phase 3/4）**：`semantic/engine.go`，建 decoded_from/response_to/correlated_with/caused_by/updates/possible_followup/contains 边，图完整性不变量强制端点存在。文档：✅ `semantic-evidence-v1.md`（Strength/Method/RuleID/RelationType 全枚举）。
- **Analyzer Engine（流式）**：`analyze/engine.go`，`Process→Flush/FinalFlush` 产出 metrics。文档：⚠️ 引擎生命周期仅代码注释，无独立文档。
- **rules.yaml 编写**：根目录仅 2 条 HTTP 示例规则；规则 schema（`analyze/rule.go:16`）无 docs，**AI 想扩展分析必须读源码**。❌ 盲区。

**AI 使用方案（确认分析完整）**：`analyze_protocol_patterns(session_id)` 断言返回流统计/消息类型/实体模式；`query_evidence_graph(session_id)` 断言 `nodes` 含 `semantic`、`edges` 含 `strength`/`method`/`rule_id`；`trace_event_chain(event_id=...)` 断言返回上下游链；`suggest_link_rules` 断言返回可落地的 link rule 建议。

**AI 发现路径小结**：查询面/语义面整体良好；分析规则编写与两张内部表是主要盲区。

## 6. 插件 SDK 能力（gta-plugin-sdk）

**✅ 已文档化（AI 可发现）**：DecodeV2 全字段（Agents.md §8/§10 + contract.yaml `rpc.decode_v2`）；保留字段 `_meta`(direction/flow_id/msg_name/is_push) 与 `_state_changes`（Agents.md §10.1 + decoder-development.md §15，上一轮已补全）；`framing.ExtractL7`/`NewReassembler`（Agents.md §8.1/§16 + decoder-development.md §2/§6）；`event.Value` 全 API（Agents.md §11）；manifest schema（Agents.md §7）；registry 主流程（Agents.md §5/§6）。
**✅ doc_ref 完整性**：contract.yaml 21 条 `rules[]` 的 `doc_ref` 全部指向真实存在的文件/小节，无悬空（含我们新增的 `state-changes-required`→`decoder-development.md#15`）。

**❌ 代码有、文档无（会漏给 AI 的碎片）**：
1. `Event.MetaValue(key)`（`event/event.go:336`）——读取 `_meta` 的唯一公开 API，三处文档均无。
2. `Reassembler.Forget`/`Reset`（`framing/framing.go:511,519`）——跨连接/新 capture 重置流状态必需，仅代码注释。
3. `FlowKey.Canonical`/`Reverse`/`String`（`framing.go:81,87,97`）——请求/响应配对常用，仅代码注释。
4. `framing.Segment`/`TCPFlags` 字段未在文档展开；Agents.md §3 包树**漏列 `framing/`** 目录。
5. `registry.go:50-51` 注释谎称"registry 必须显式设置否则 fatal"，与实际默认 `:9091` 回退（contract.yaml `registry_discovery.default`）矛盾。

**AI 使用方案（确认 SDK 能力完整）**：读 `get_plugin_dev_guide` + `get_plugin_contract`（=contract.yaml 全文）→ 构造 fixture（回环帧 `02 00 00 00` + 以太网帧各一）→ 解码后 `list_decoded_data` 断言 `event_type`/`schema_id` 非空且 framing 正确；插件内 `event.ValueFromMap` 含 `_state_changes` → `list_state_changes` 断言投影出现。

## 7. 插件仓库与脚手架（gta/plugins）

- **插件实况**：`plugins/` 下仅含本地未提交的解码器插件（不进入远程仓库），**不存在 `http` 插件**（已 ls 确认）。注意：这类本地插件不会提交远程，任何已提交文档/示例/工具都不得引用它们——示例一律用通用占位名。
- **`create_plugin` 脚手架**：`pkg/plugindev/templates/create_plugin/`，真实、对 AI 引导充分（模板注释 + `get_plugin_dev_guide`）。✅
- **`tools/hotreload/hotreload.go`**：独立 `main`，硬编码 `.\http-plugin.exe` / `E:\gta\plugins\http` / `Plugin:"http"`——**全部不存在**（真实为 `godot-gateway.exe`）。`cmd.Start()` 会 file-not-found。❌ 死代码。
- **真实热更路径**（无 "hotreload" 名义工具）：`build_plugin` → `deactivate_plugin` → `activate_plugin`（注入 `GTA_REGISTRY_ADDR`）→ 运行会话内用 `set_session_plugin` 切解码器（不停止抓包）。
- **`GTA_REGISTRY_ADDR`**：处理得当——插件启动必读；`activate_plugin` 注入、`get_registry_addr` 暴露。
- **`godot-gateway/go.mod`** 提交了 `replace => E:\ai_workspace\gta-plugin-sdk`（`go.mod:23`），破坏可移植构建；脚手架模板正确地省略 replace。⚠️ 不一致。

**AI 使用方案（确认插件可用性）**：`list_plugins` 断言含你的解码器插件；`build_plugin(name=<your-decoder>)` 断言 0 错误；`activate_plugin(name=<your-decoder>)` 断言 `integrated:true`；`start_capture(port=8984,plugin=<your-decoder>)` → `list_decoded_data` 断言有解码事件。

## 8. 关键缺口与矛盾（按严重度）

**🔴 P0 — 会直接导致写错 / 零事件 / 严重误导**
1. `troubleshooting.md` §7.B（line 134）："capture contract passes L7 payload… do not remove Ethernet/IP/TCP headers"——**与 SSOT（payload-framing-by-link-type）正面矛盾**。这是曾导致"零事件"并被废弃的 `payload-is-l7` 旧模型死灰复燃。作者按排障文档会写出零事件解码器。
2. `docs/plugin-domain-design.md`：文件头自标"设计稿（未实现）"，却描述一套完全不同的 13 工具 / 三平面 / `pkg/plugindev` gRPC 架构；实际 `main.go` 已实现 ~40 个 MCP 工具。AI 以此文档为入口会全盘误判可用工具面。
3. `http` ↔ `godot-gateway` 命名漂移 + `tools/hotreload/hotreload.go` 死代码：文档/旧工具指向不存在的 `http`/`http-plugin.exe`/`E:\gta\plugins\http`，AI 照做即失败。

**🟠 P1 — AI 不可发现的能力盲区**
4. `event_index` / `projection_json`：无专用查询工具（§4）。
5. `rules.yaml` 编写：规则 schema 无文档，AI 扩展分析须读 `pkg/analyze/rule.go`（§5）。
6. `plugin_debug_access` 审计表：写入明确、无读取工具（§4）。
7. `decoderAction` 热加载状态机（build/drop/keep/idle，`capture_task.go:476`）：仅代码注释（§3 关联）。
8. SDK 未文档 API：`MetaValue` / `Reassembler.Forget`/`Reset` / `FlowKey.Canonical`（§6）。
9. `begin_capture_run` 的 4 个 no-op 参数 + `explain_plugin` 未声明 `verify` 参数（§1）。

**🟡 P2 — 轻微不对称 / 冗余**
10. `docs/event.md` DDL 与真实 schema 漂移（缺 context 列、origin_id 索引、created_at）。
11. `registry.go` 注释谎称 fatal（§6）。
12. Agents.md §3 包树漏列 `framing/`（§6）。
13. 文档冗余：`decoder-development.md` 与 `decoder-development-guide.md` 大量重叠、`decoder-contract-v1.1.md` 与 Agents.md §8 重叠——作者难辨权威源。

## 9. 给 AI 的"功能完整性自检"方案（一键跑通确认全栈）

```
1. tools/list
   → 断言返回 38 工具 +（raw-debug 开启时）list_raw_packets/decode_raw_packets
2. start_capture(port=8984, plugin=<your-decoder>)
   → 拿到 session_id + db_path；跑 examples 造流量
3. get_session_status
   → 断言 packets_in>0 / event_count>0 / decode_errors==0
4. list_decoded_data(limit=50)
   → 断言 event_type / schema_id / data.* 非空（确认 DecodeV2 + framing 正确）
5. 用发 _state_changes 的插件
   → list_state_changes(subject_type=player) 断言 op/path/before/after 出现
6. aggregate_query(expression='http_req_count')
   → 断言返回 {name,window,value,group}
7. query_evidence_graph / trace_event_chain(event_id)
   → 断言 nodes 含 semantic、edges 含 strength/method/rule_id
8. create_plugin → build_plugin → activate_plugin(integrated:true)
   → set_session_plugin → verify_plugin 断言 verdict=pass
9. SDK 侧：读 Agents.md §8.1 + contract.yaml payload_framing
   → 确认 framing.ExtractL7 使用；fixture 回环+以太网各一均解出事件
```
任一步失败即定位到对应 § 的能力盲区。

## 10. 建议修复（可落地，按优先级）

> 约束：本地未提交插件（如 `godot-gateway`）不会进入远程仓库，以下修复均**不引用**它们；已提交文档/工具保持通用、自洽。

**P0**：
- 删除/重写 `troubleshooting.md` §7.B 矛盾段（改为"pcap 来源 payload 是完整帧，必须 ExtractL7 剥头"），并在 §1 第 5 步去掉"is payload L7"的误导性前提。✅ 已修。
- 在 `plugin-domain-design.md` 顶部显著标注"过期设计稿，实际工具面以 `cmd/gta-mcp/main.go` 的 `AddTool` 注册为准"，并把示例中的 `http` 占位名改为通用名。✅ 已修。
- 删除死代码中对不存在的 `http` 插件的引用：`tools/hotreload`、`tools/verify_http_plugin` 改为通用 flag 驱动（插件名/端口/目录可配），示例改用通用占位名；**不得**引用本地未提交插件。`examples/http` 是通用 HTTP 示例服务（与具体插件解耦），可保留。✅ 已修。

**P1**：
- 新增 `docs/rules-authoring.md`（规则 schema、expr 变量 `event/data/identity/relation/context/payload`、编写示例）。
- 为 `event_index` / `plugin_debug_access` 增加只读 MCP 出口，或至少在 `get_capture_schema` 文档化其 `RawQuery` 逃生舱用法。
- SDK 补文档：`Event.MetaValue`、`Reassembler.Forget`/`Reset`、`FlowKey.Canonical`；Agents.md §3 包树补 `framing/`。
- `begin_capture_run` 在描述中标注 no-op 参数；`explain_plugin` 把 `verify` 补进 `AddTool` schema。

**P2**：
- 修正 `docs/event.md` DDL 与真实 schema 对齐（或标"以 get_capture_schema 为准"）。
- 修正 `registry.go` 注释；合并/标注冗余的 decoder 指南，明确 Agents.md 为权威源。
- 移除 `godot-gateway/go.mod` 的 `replace`（或文档化"开发期专用，发布前删"）。


