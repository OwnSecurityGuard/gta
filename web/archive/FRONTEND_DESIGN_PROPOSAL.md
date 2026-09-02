> ⚠️ **归档文档**：本设计提案记录 2026-08 期间的重构前页面规划。Evidence 语义引擎与关系 Tab 已于 2026-08-22 随项目重定位删除，前端已替换为「时间线」Tab。本文档保留历史参考。

# GTA 前端 · MCP 工具覆盖评估与页面设计方案

> 评估对象：`cmd/gta-mcp` 当前注册的 **37 个常驻工具 + 2 个 raw-debug 条件工具**
> 现状：前端 `web/` 仅接入 **12 个**工具，分布在「协议数据 / 插件 / 原始包」三个 Tab。
> 本次新增的「协议聚合统计」「关系补足」工具族**在 UI 中完全缺失**。
> 设计原则：在既有设计令牌（靛蓝 `#4f46e5` / 翠绿 `#059669` / 灰阶画布 `#f5f6f9`）、shadcn 风格组件、`use-mcp` hook 模式上扩展，不引入路由库，沿用 `ViewTab` + 面板组件的方式。

---

## 1. 工具 ↔ 前端覆盖现状

| 分组 | 工具 | UI 现状 |
|---|---|---|
| 抓包控制 | `start_capture` `stop_capture` `list_all_sessions` `set_session_plugin` | ✅ 已接入 |
| 抓包控制 | `get_session_status` `list_interfaces` `list_live_sessions` `delete_session` | ❌ 未接入 |
| 插件平面 | `list_plugins` `list_registered_plugins` `activate_plugin` `deactivate_plugin` `deregister_plugin` `test_plugin` | ✅ 已接入 |
| 插件平面 | `create_plugin` `build_plugin` `status_plugin` `explain_plugin` `get_plugin_manifest` `get_plugin_contract` `get_plugin_dev_guide` `get_registry_addr` | ❌ 未接入 |
| 数据查询 | `list_decoded_data` `get_capture_schema` | ✅ 已接入 |
| 数据查询 | `list_state_changes` `query_capture_table` | ❌ 未接入 |
| **聚合统计** | **`aggregate_query` `analyze_protocol_patterns`** | ❌ **未接入（本次重点）** |
| **关系/证据图** | **`query_evidence_graph` `trace_event_chain` `suggest_link_rules`** | ❌ **未接入（本次重点）** |
| raw-debug | `list_raw_packets` `decode_raw_packets` | ✅ 已接入（条件） |
| 校验/取证 | `verify_plugin` `sample_bytes_plugin` | ✅ 已接入（plugin 面板内） |
| 行为/因果 | `begin_capture_run` `end_capture_run` `get_run_status` `trace_protocol_flow` | ❌ 未接入（高级，本轮不纳入） |

---

## 2. 评估结论：哪些工具需要前端展示

### Tier 1 — 必须展示（用户明确点名的「聚合统计 / 关系补足」）

| 工具 | 归属 Tab | 前端形态 |
|---|---|---|
| `aggregate_query` | **分析** | 顶部 KPI 指标卡 + expr 查询框（复用 FilterBar 样式）+ `aggregatable_fields` 快捷 chips |
| `analyze_protocol_patterns` | **分析** | 流统计 / 消息类型分布 / 相关性流 / 状态变更模式 / 证据图结构 / 方向分布 的分区卡片 |
| `query_evidence_graph` | **关系** | 节点（按 kind 着色）+ 边（按 type 着色、置信度映射透明度）的画布，含 node_kind/edge_type/min_confidence/root+max_depth 过滤 |
| `trace_event_chain` | **关系** | 选定事件/节点后，上游（谁导致）+ 下游（导致什么）双栏按深度排列 |
| `suggest_link_rules` | **关系** | 链路规则建议表（edge_type / source·target / occurrences / avg_confidence / rule_template），含「采纳」复制到 rules.yaml |

### Tier 2 — 增强既有视图（高性价比，顺手补齐）

| 工具 | 归属 | 前端形态 |
|---|---|---|
| `get_session_status` | 侧边栏 | 每个会话行增加实时状态点（running 翠绿脉冲 / stopped 灰）+ 包/事件/指标计数 + 解码错误数 |
| `delete_session` | 侧边栏 | 会话行增加删除（垃圾桶）操作，二次确认 |
| `list_interfaces` | 开始抓包弹窗 | 网卡选择器（当前仅有 port + plugin） |
| `create_plugin` | 插件 Tab | 「新建插件」脚手架向导（name / protocol / hints） |
| `build_plugin` | 插件 Tab | 插件卡片「编译」按钮，失败时结构化 file:line:col 诊断 |
| `status_plugin` | 插件 Tab | 插件详情：制品态 + 运行时态 + 最近尝试 + 建议下一步 |
| `explain_plugin` | 插件 Tab | 构建/激活失败时的根因卡片（category + rule_id + why + fix） |
| `get_plugin_manifest` | 插件 Tab | 详情抽屉内展示原始 plugin.yaml |

### Tier 3 — 暂不入 UI（后端专用 / 高级 / 后续迭代）

- **行为因果族** `begin_capture_run` `end_capture_run` `get_run_status` `trace_protocol_flow`：属于独立的「AI Skill 链路」工作流，建议后续独立「行为 / Runs」Tab，本轮不纳入。
- `query_capture_table`：只读逃生口，可放进「设置 / 高级」做表浏览器，优先级低。
- `get_capture_schema`：已通过 `useCaptureSchema` 支撑筛选字段提示，可加「schema 浏览器」抽屉，低优先。
- `get_plugin_contract` `get_plugin_dev_guide` `get_registry_addr`：文档/内部态，作为插件 Tab 开发面板里的链接/只读信息呈现即可，无需独立 UI。

---

## 3. 推荐的导航结构调整

```
现状：  协议数据 | 插件 | 原始包(debug)
建议：  协议数据 | 分析 | 关系 | 插件 | 原始包(debug)
               └─新增─┘ └─新增─┘
```

- **协议数据**：保留原始事件流（list_decoded_data），作为「明细」层。
- **分析**：聚合统计层（aggregate_query + analyze_protocol_patterns）。
- **关系**：证据图 / 因果链 / 链路规则建议层。
- **插件 / 原始包**：保持现状，并做 Tier 2 增强。

`App.tsx` 改动：`ViewTab` 联合类型增加 `"analytics" | "relationship"`，`TABS` 数组插入两项，`activeTab` 渲染对应面板组件。

---

## 4. 新增视图设计要点

### 4.1 分析（Analytics）Tab
- 顶部一行 **KPI 指标卡**（来自 `aggregate_query`，expr 查询框驱动）：如请求数 / 响应数 / 错误数 / 平均时延。
- `aggregatable_fields` 回显为可点快捷 chip，点击填入查询框。
- 分区卡片（来自 `analyze_protocol_patterns`，每区独立 loading/empty/error）：
  - 流统计（flows：event_count / c2s / s2c）
  - 消息类型分布（event_types count，横向条形）
  - 相关性流（correlated_flows）
  - 状态变更模式（state_change_subjects / state_change_patterns）
  - 证据图结构（evidence_graph_nodes / evidence_graph_edges）
  - 方向分布（direction_distribution）

### 4.2 关系（Relationship / Evidence）Tab
- 左：画布（`query_evidence_graph`）——节点按 kind 着色，边按 type 着色、置信度映射透明度；过滤条含 node_kind / edge_type / min_confidence 滑块 / root+max_depth BFS。
- 右（或下方）：`trace_event_chain` 面板——输入 event_id/node_id，展示上游（谁导致它）与下游（它导致什么）双栏按深度排列。
- 底部：`suggest_link_rules` 表——edge_type / source·target / occurrences / avg_confidence / rule_template，行内「采纳」按钮复制规则模板。

### 4.3 侧边栏增强
- 每个会话行：状态点（running 翠绿脉冲 / stopped 灰）+ 名称 + 端口 + 插件徽标 + 计数（events / packets）+ 删除操作（二次确认）。

---

## 5. 接入实施清单（落到代码）

新增 `src/hooks/use-mcp.ts` hooks：
`useAggregateQuery` `useAnalyzePatterns` `useEvidenceGraph` `useTraceEventChain` `useSuggestLinkRules`
`useSessionStatus` `useDeleteSession` `useListInterfaces`
`useCreatePlugin` `useBuildPlugin` `usePluginStatus` `useExplainPlugin` `usePluginManifest`

新增 `src/types/`：`analytics.ts` `evidence.ts` `session-status.ts` `plugin-dev.ts`

新增组件：
`src/components/analytics-panel.tsx` `src/components/relationship-panel.tsx`
`src/components/evidence-graph.tsx` `src/components/trace-panel.tsx` `src/components/link-rule-table.tsx`
增强：`session-sidebar.tsx`（状态+删除）、`start-capture-dialog.tsx`（网卡选择）、`plugin-panel.tsx`（脚手架/编译/详情）

---

## 6. 优先级与建议落地顺序

1. **P0**：分析 Tab（aggregate_query + analyze_protocol_patterns）——用户最关心的「聚合统计」。
2. **P0**：关系 Tab（query_evidence_graph + trace_event_chain + suggest_link_rules）——「关系补足」。
3. **P1**：侧边栏增强（get_session_status + delete_session）+ 开始抓包网卡选择（list_interfaces）。
4. **P1**：插件开发面板（create_plugin / build_plugin / status_plugin / explain_plugin / manifest）。
5. **P2（后续）**：行为/Runs Tab、schema 浏览器、表浏览器。

---

_下一步：确认上述 Tier 1/2 范围后，可直接基于本方案在 `web/` 落地组件与 hook，完全复用既有设计令牌与 `use-mcp` 模式。_
