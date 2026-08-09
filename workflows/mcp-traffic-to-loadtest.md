# Workflow: Traffic-To-Loadtest

> 本文档定义 Agent 使用 `game-traffic-analysis` MCP 工具从真实流量提取证据、编写 load-test 代码的标准流程。
>
> 设计目标：让 Agent 消费摘要、文件路径与字段级小输出，而非读取大块原始协议 payload，从而最小化 Token 消耗。
>
> 相关设计文档：
> - 工具差距分析：[docs/mcp-traffic-to-loadtest-tool-gap.md](file:///e:/go-toolkit/game-traffic%20-analysis/docs/mcp-traffic-to-loadtest-tool-gap.md)
> - Run 窗口设计：[docs/mcp-run-window-design.md](file:///e:/go-toolkit/game-traffic%20-analysis/docs/mcp-run-window-design.md)
> - 结构化字段迁移：[docs/mcp-decoded-events-schema-migration.md](file:///e:/go-toolkit/game-traffic%20-analysis/docs/mcp-decoded-events-schema-migration.md)
> - Trace 工具设计：[docs/mcp-trace-protocol-flow-design.md](file:///e:/go-toolkit/game-traffic%20-analysis/docs/mcp-trace-protocol-flow-design.md)

---

## 1. 角色与边界

### Agent 职责
- 按本文档流程调用 MCP 工具
- 消费工具返回的摘要、文件路径、字段级输出
- 基于证据链编写 load-test 代码
- 仅在必要时才请求原始 payload（rare / bounded）

### MCP 层职责
- 聚合、过滤、脱敏、导出证据
- 默认返回简短结构化输出
- 大结果写文件，返回路径 + 计数 + 关键 ID + 不确定性摘要
- 通过 `run_id` 隔离每次用户操作

### 不在边界内
- Agent 不直接读 SQLite 数据库
- Agent 不解析 pcap 文件
- Agent 不手工拼接 request/response（由 `trace_protocol_flow` 完成）

---

## 2. 前置条件

### 必须已落地（P0）
- [x] `decoded_events` 结构化字段（`flow_id` / `direction` / `msg_name` / `msg_id` / `is_push` / `src` / `dst`）
- [x] `begin_capture_run` / `end_capture_run` / `get_capture_run_status` + `RunRegistry`
- [x] `trace_protocol_flow`

### 待落地（P1，本流程支持降级运行）
- [ ] `select_operation_flow` — 缺失时 Agent 用 `list_flows` 手动选择
- [ ] `summarize_operation_messages` — 缺失时 Agent 用 `list_messages` 概览
- [ ] `list_flows` / `list_messages` / `get_message` — 缺失时降级到 `list_decoded_data`
- [ ] `entity_snapshots` 表 + 插件产出 — 缺失时 `trace_protocol_flow` 的 `entity_diffs` 为空 + uncertainty
- [ ] `trace_request_effect` — 缺失时 Agent 用 `trace_protocol_flow` 的 step 内信息

### 现有可用（底层）
- `start_capture` / `stop_capture` / `get_capture_status`
- `list_plugins` / `list_interfaces` / `list_sessions` / `delete_session`
- `aggregate_query` / `list_decoded_data` / `get_capture_schema`
- `save_script` / `list_scripts` / `run_script` / `delete_script`

---

## 3. 标准流程（8 步）

### Step 1: `begin_capture_run` — 标记操作开始

```
Agent → begin_capture_run(feature_name, project_path, plugin_name?, port?, device?, filter?)
MCP   → {run_id, time_from, capture_status, capture_isolation_mode, session_id, uncertainties?}
```

**Agent 行为**：
- 记录 `run_id` 与 `time_from`
- 检查 `capture_status`：若 `not_started` 且 `capture_isolation_mode=time_window_only`，提示用户先启动 capture 或补全 `plugin_name`+`port`
- 检查 `uncertainties`：若有 auto_start 失败等告警，告知用户但继续流程

**Token 预算**：< 200 tokens（仅 run_id + 状态）

### Step 2: 用户执行操作

```
Agent → (提示用户执行目标操作，如 "升级建筑" / "登录" / "战斗")
用户  → (在游戏/应用中执行操作)
```

**Agent 行为**：等待用户确认操作完成

**Token 预算**：0（无 MCP 调用）

### Step 3: `end_capture_run` — 关闭操作窗口

```
Agent → end_capture_run(run_id)
MCP   → {run_id, time_to, duration_ms, captured_flow_count, captured_message_count,
         client_request_count, decode_error_count, uncertainties}
```

**Agent 行为**：
- 记录 `time_to` / `duration_ms`
- 检查 `captured_message_count`：若为 0 或 -1，检查 `uncertainties` 决定是否继续
- 检查 `decode_error_count`：若非 0 且非 -1，告知用户存在解码错误

**幂等性**：重复调用 `end_capture_run` 返回相同 summary（`idempotent: true`），可安全重试

**Token 预算**：< 300 tokens

### Step 4: `get_capture_run_status` — fail-fast 检查

```
Agent → get_capture_run_status(run_id)
MCP   → {run_id, status, flow_count, client_request_count, server_message_count, decode_error_count, uncertainties?}
```

**Agent 行为**：
- 若 `status=not_found`：报错并终止
- 若 `server_message_count=0` 且 `client_request_count=0`：提示用户操作未捕获到流量，终止
- 若 `flow_count=1`（或 -1 但 `server_message_count > 0`）：继续 Step 5
- 若 `flow_count>1`：继续 Step 5（需选择 flow）
- 若计数为 -1（结构化字段未落地）：依赖 `uncertainties` 判断，若 `server_message_count > 0` 则继续

**Token 预算**：< 200 tokens

### Step 5: `select_operation_flow` — 选择操作流

**首选**（P1 已落地）：
```
Agent → select_operation_flow(run_id, feature_name?, require_client_requests=true)
MCP   → {run_id, selected_flow_id, candidate_flows[], selection_reason, confidence}
```

**降级**（P1 未落地，用 `list_flows`）：
```
Agent → list_flows(run_id, limit=50)
MCP   → {count, flows[{flow_id, src, dst, protocol, first_seen, last_seen, message_count, client_request_count}]}
Agent → (启发式选择：client_request_count 最高 + 时间聚集度最高)
```

**再降级**（`list_flows` 也未落地）：
```
Agent → list_decoded_data(session_id, filter="timestamp >= time_from AND timestamp <= time_to", limit=200)
Agent → (应用层聚合 DISTINCT flow_id，启发式选择)
```

**Agent 行为**：
- 若 `confidence=high` 且 `selected_flow_id` 非空：直接用
- 若 `confidence=medium`：用 `selected_flow_id` 但记录候选
- 若 `confidence=low` 或多候选并列：提示用户从 `candidate_flows` 选择
- 记录 `flow_id`

**Token 预算**：< 500 tokens（首选）/ < 2000 tokens（降级，受 limit 影响）

### Step 6: `summarize_operation_messages` — 流量概览

**首选**（P1 已落地）：
```
Agent → summarize_operation_messages(run_id, flow_id, include_background=false)
MCP   → {request_count, response_count, server_push_count,
         grouped_message_names[{name, direction, count}],
         suspicious_or_noise_summary, representative_msg_ids[]}
```

**降级**（P1 未落地，用 `list_messages`）：
```
Agent → list_messages(flow_id, run_id, limit=200, include_json=false)
MCP   → {count, messages[{msg_id, flow_id, timestamp, direction, name, is_push, raw_len}]}
Agent → (应用层聚合：按 name + direction 分组计数)
```

**再降级**（`list_messages` 也未落地）：
```
Agent → list_decoded_data(session_id, filter="flow_id == X", limit=200)
Agent → (从 json 字段手动提取 type/method/name)
```

**Agent 行为**：
- 检查 `request_count` 与 `response_count` 是否匹配（`request_count == response_count` 理想）
- 若 `server_push_count` 高且 `request_count` 低：可能是推送主导协议，提示用户
- 检查 `suspicious_or_noise_summary`：识别心跳等噪声
- 记录 `representative_msg_ids` 供后续深挖

**Token 预算**：< 600 tokens（首选）/ < 3000 tokens（降级）

### Step 7: `trace_protocol_flow` — 提取证据链（主步骤）

```
Agent → trace_protocol_flow(run_id, flow_id, feature_name,
                             noise_filter={drop_heartbeats: true},
                             entity_diff={enabled: true, window_ms: 500})
MCP   → {
  run_id, flow_id, feature_name, time_window,
  steps[{step_id, request_msg_id, request{name, direction, key_fields},
         response{msg_id, name, key_fields}, pushes[], entity_diffs[], why_related}],
  uncertainties[],
  file_path?,  // 大结果时
  // 或完整 steps（小结果时）
}
```

**Agent 行为**：
- 若返回 `file_path`（大结果，steps > 50）：用 `step_count` + `summary` 概览，必要时读文件
- 检查 `uncertainties`：
  - "unpaired request" → 提示用户该 request 无响应，可能是异步或丢失
  - "entity_diffs empty" → 提示用户插件未产出 entity snapshot，entity 证据缺失
  - 其他 → 记录但继续
- 逐 step 分析证据链：request → response → pushes → entity_diffs → why_related
- 若某 step 需深挖 → Step 7a
- 若某 msg 需原始 payload → Step 7b

**Token 预算**：
- 小结果（steps ≤ 50）：随 step 数线性增长，典型 1000–5000 tokens
- 大结果（steps > 50）：< 500 tokens（仅摘要 + file_path）

### Step 7a: `trace_request_effect` — 单 request 深挖（可选）

**场景**：某 step 的 `response.key_fields` 或 `entity_diffs` 需要更详细分析

```
Agent → trace_request_effect(request_msg_id, window_ms=2000, include_raw_ext=false)
MCP   → {request, response, pushes[], entity_diffs[], raw_ext?, uncertainties[]}
```

**降级**（P1 未落地）：直接从 `trace_protocol_flow` 的 step 内信息获取，不深挖

**Agent 行为**：
- 仅在 `trace_protocol_flow` 输出不够时调用
- 限制调用次数（典型 1–3 次），避免 Token 膨胀

**Token 预算**：< 500 tokens / 次

### Step 7b: `get_message` — 取原始 payload（rare / bounded）

**场景**：需要查看某条消息的完整 JSON（如编写 load-test 时需要字段细节）

```
Agent → get_message(flow_id, msg_id, redact=true, max_bytes=8192)
MCP   → {msg_id, flow_id, timestamp, direction, name, is_push,
         json (脱敏后, 截断至 max_bytes), truncated, full_path?}
```

**降级**（P2 未落地，用 `list_decoded_data`）：
```
Agent → list_decoded_data(session_id, filter="msg_id == X AND flow_id == Y", limit=1)
```

**Agent 行为**：
- **仅在编写 load-test 代码时**调用，确认字段结构
- 限制调用次数（典型 3–10 次）
- `redact=true` 默认，避免敏感数据泄漏
- 若 `truncated=true`：读 `full_path` 文件

**Token 预算**：< 2000 tokens / 次（受 max_bytes 限制）

### Step 8: 编写 load-test 代码

```
Agent → (基于 steps + key_fields + entity_diffs + 必要的 raw payload，编写 load-test 代码)
Agent → (保存到 project_path)
```

**Agent 行为**：
- 基于证据链的 `request.name` + `key_fields` 构造请求
- 基于 `response.key_fields` 校验响应
- 基于 `pushes` 模拟服务器推送处理
- 基于 `entity_diffs` 验证状态变更
- 参考 `get_message` 的 raw payload 补充字段细节

**Token 预算**：编写代码本身不耗 MCP Token，仅代码生成

---

## 4. Fallback 链总览

```
高级工具 (P0/P1)           降级 1 (P1 低级)         降级 2 (现有底层)
─────────────────────────────────────────────────────────────────
select_operation_flow  →   list_flows           →   list_decoded_data (应用层聚合 flow_id)
summarize_operation_   →   list_messages        →   list_decoded_data (手动提取字段)
    messages
trace_protocol_flow    →   (无降级，必须落地)
trace_request_effect   →   (用 trace_protocol_flow 的 step 内信息)
get_message            →   list_decoded_data (filter="msg_id == X")
```

**降级触发条件**：
- 工具未实现（MCP 返回 `unknown tool`）
- 工具返回错误（如依赖的表不存在）
- 工具返回的 `uncertainties` 指示降级（如 `flow_count=-1`）

**降级时的 Token 成本**：每次降级约 2–5 倍 Token，应优先推动 P1 工具落地。

---

## 5. Token 预算总览

| 步骤 | 首选工具 | 首选 Token | 降级 Token |
|---|---|---|---|
| 1. begin_capture_run | begin_capture_run | < 200 | — |
| 3. end_capture_run | end_capture_run | < 300 | — |
| 4. get_capture_run_status | get_capture_run_status | < 200 | — |
| 5. select_operation_flow | select_operation_flow | < 500 | < 2000 (list_flows) / < 5000 (list_decoded_data) |
| 6. summarize_operation_messages | summarize_operation_messages | < 600 | < 3000 (list_messages) |
| 7. trace_protocol_flow | trace_protocol_flow | 1000–5000 (小) / < 500 (大+文件) | — |
| 7a. trace_request_effect | trace_request_effect | < 500 / 次 | — |
| 7b. get_message | get_message | < 2000 / 次 | < 5000 / 次 (list_decoded_data) |

**典型单次工作流总 Token**：
- 最优路径（所有 P0/P1 落地）：3000–8000 tokens
- 降级路径（仅 P0 落地，P1 全降级）：15000–30000 tokens
- 最差路径（仅现有底层）：30000+ tokens

---

## 6. `uncertainties` 处理规范

工具返回的 `uncertainties` 是字符串数组，Agent 应：

1. **分类**：
   - `blocking`：阻碍继续（如 "run not found"）→ 终止流程，告知用户
   - `warning`：影响证据完整性（如 "entity_diffs empty"）→ 告知用户但继续
   - `info`：提示信息（如 "auto_start failed, fallback to time_window_only"）→ 记录但继续

2. **传播**：将关键 uncertainties 传播到最终输出，让用户知晓证据链的不完整性

3. **降级触发**：若 uncertainty 指示某个工具的能力缺失（如 `flow_count=-1`），触发对应 fallback

**示例处理**：
```python
for u in uncertainties:
    if "not found" in u or "no messages" in u:
        return blocking_error(u)
    elif "empty" in u or "unavailable" in u:
        warnings.append(u)
    else:
        info.append(u)
```

---

## 7. 隔离模式说明

`begin_capture_run` 返回的 `capture_isolation_mode` 决定数据隔离方式：

| 模式 | 含义 | Agent 行为 |
|---|---|---|
| `reuse_existing` | 复用运行中的 capture，参数匹配 | 直接继续，数据在现有 session |
| `auto_start` | 自动启动新 capture | 继续流程，capture 在后台运行 |
| `time_window_only` | 仅记录时间窗口，无独立 capture | 若 `session_id` 非空，数据在指定 session；若为空，跨 session 查询受限 |

**`time_window_only` 的限制**：
- `captured_message_count` 可能返回 -1（无法定位 db）
- `flow_count` / `client_request_count` 可能返回 -1
- Agent 应提示用户：建议先 `start_capture` 再 `begin_capture_run` 以获得完整数据

---

## 8. 大结果处理

### 触发条件
- `trace_protocol_flow`：steps > 50 → 写 `workDir/runs/{run_id}/trace.json`
- `get_message`：json > max_bytes（默认 8192）→ 写 `workDir/runs/{run_id}/msg_{flow_id}_{msg_id}.json`

### Agent 行为
1. 收到 `file_path` 字段时，**不要立即读文件**
2. 先用返回的摘要（`step_count` / `summary` / `truncated`）判断是否需要详情
3. 仅在编写 load-test 代码确需细节时，用 `Read` 工具读文件
4. 读文件时优先读特定 step / 字段，避免全量加载

### 文件生命周期
- 文件在 `workDir/runs/{run_id}/` 下，随 run 持久化
- 重复调用同 `run_id` + `flow_id` 的 `trace_protocol_flow` 会覆盖 `trace.json`
- `end_capture_run` 不删除文件；run 文件保留供后续分析

---

## 9. 与脚本引擎的协作

`run_script` 工具允许执行 Python 脚本，脚本内通过 `gta_api` 模块访问 `query_events` / `query_metrics`。本工作流与脚本引擎的协作场景：

### 场景 1：复杂数据分析
```
Agent → run_script(name="analyze_flow_distribution", args={"run_id": "...", "flow_id": 12})
脚本  → query_events(filter="flow_id == 12") → 应用层统计 → 输出分布报告
Agent → (基于报告补充 trace_protocol_flow 的分析)
```

### 场景 2：load-test 数据生成
```
Agent → run_script(name="gen_loadtest_data", args={"run_id": "...", "flow_id": 12, "feature": "upgrade"})
脚本  → query_events → 提取 request body → 生成 load-test fixtures (JSON)
Agent → (基于 fixtures 编写 load-test 代码)
```

**协作规范**：
- 脚本访问数据走 `query_events` / `query_metrics`（Python 侧薄封装，转发到 `list_decoded_data` / `aggregate_query`）
- 脚本输出应结构化（JSON），便于 Agent 解析
- 脚本不直接读 db，避免路径依赖

---

## 10. 错误处理与重试

### 可重试错误
- `end_capture_run` 幂等：重复调用返回相同 summary
- `get_capture_run_status` 幂等：纯读查询
- `trace_protocol_flow` 幂等：纯读查询，文件覆盖

### 不可重试错误
- `begin_capture_run` 失败：检查参数后重新调用（生成新 run_id）
- `start_capture` 失败：检查 plugin/port 后重试

### 网络错误
- stateless HTTP 模式下，任何工具调用可能因网络中断失败
- Agent 应重试 1–2 次，仍失败则告知用户

---

## 11. 完整流程示例（伪代码）

```python
# Step 1: 标记操作开始
run = begin_capture_run(
    feature_name="upgrade_building",
    project_path="/path/to/loadtest_project",
    plugin_name="http",
    port=8080
)
run_id = run["run_id"]
if run["capture_status"] == "not_started":
    print("警告：capture 未启动，请先 start_capture")
    # 或提示用户补全 plugin_name + port

# Step 2: 用户执行操作
input("请在游戏中执行'升级建筑'操作，完成后按 Enter")

# Step 3: 关闭操作窗口
summary = end_capture_run(run_id=run_id)
if summary["captured_message_count"] == 0:
    print("未捕获到消息，请检查 capture 配置")
    exit(1)

# Step 4: fail-fast 检查
status = get_capture_run_status(run_id=run_id)
if status["server_message_count"] == 0:
    print("无服务器消息，操作可能未触发")
    exit(1)

# Step 5: 选择 flow
flow = select_operation_flow(run_id=run_id, feature_name="upgrade_building")
flow_id = flow["selected_flow_id"]
if flow_id is None:
    # 降级：手动从 candidate_flows 选择
    flow_id = prompt_user_select(flow["candidate_flows"])

# Step 6: 流量概览
overview = summarize_operation_messages(run_id=run_id, flow_id=flow_id)
print(f"请求 {overview['request_count']} 条，响应 {overview['response_count']} 条，推送 {overview['server_push_count']} 条")

# Step 7: 提取证据链（主步骤）
trace = trace_protocol_flow(
    run_id=run_id,
    flow_id=flow_id,
    feature_name="upgrade_building",
    noise_filter={"drop_heartbeats": True},
    entity_diff={"enabled": True, "window_ms": 500}
)

if "file_path" in trace:
    # 大结果：先看摘要，必要时读文件
    print(f"证据链较大（{trace['step_count']} 步），已写入 {trace['file_path']}")
    steps = read_trace_file(trace["file_path"])
else:
    steps = trace["steps"]

# 处理 uncertainties
for u in trace.get("uncertainties", []):
    if "empty" in u or "unavailable" in u:
        print(f"警告：{u}")

# Step 7b: 必要时取原始 payload（rare）
for step in steps[:3]:  # 仅前 3 步深挖
    raw = get_message(flow_id=flow_id, msg_id=step["request_msg_id"], redact=True, max_bytes=8192)
    # 解析 raw["json"] 提取字段细节
    pass

# Step 8: 编写 load-test 代码
write_loadtest_code(project_path, steps, feature_name="upgrade_building")
print(f"load-test 代码已生成到 {project_path}")
```

---

## 12. 验收清单

工作流落地后，以下场景应可跑通：

- [ ] HTTP 协议：捕获 HTTP 请求/响应，生成 load-test 代码
- [ ] 业务协议（模拟）：捕获 Req/Resp 配对 + push，生成 load-test 代码
- [ ] 大结果：steps > 50 时返回 file_path，Agent 不爆 Token
- [ ] 降级路径：P1 工具未落地时，通过 `list_flows` / `list_messages` / `list_decoded_data` 完成流程
- [ ] 不确定性传播：`uncertainties` 正确传递到最终输出
- [ ] 幂等性：重复 `end_capture_run` / `trace_protocol_flow` 不产生副作用
- [ ] 隔离模式：`reuse_existing` / `auto_start` / `time_window_only` 三种模式均可运行
- [ ] Token 预算：最优路径单次工作流 < 10000 tokens

---

## 13. 未决问题

1. **`select_operation_flow` 未落地时的人工选择 UX**：Agent 如何向用户呈现 `candidate_flows`？建议返回结构化候选清单，用户回复数字选择。

2. **多 flow 操作的处理**：当前流程假设单 flow。若操作跨多 flow（如 HTTP 连接切换），是否支持 `flow_id` 数组？建议首版单 flow，后续扩展。

3. **`trace_protocol_flow` 与 `trace_request_effect` 的调用时机**：Agent 何时应调用 `trace_request_effect` 深挖？建议在 `trace_protocol_flow` 的 step `key_fields` 不足或 `entity_diffs` 需要详细分析时。

4. **脚本引擎的协作边界**：`run_script` 应在 Step 7 之后（补充分析）还是 Step 8 之前（生成 fixtures）？建议两者皆可，按场景选择。

5. **load-test 代码的验证**：Agent 生成的代码是否需要回放验证？建议首版不做，后续加 `validate_loadtest` 工具。

6. **`file_path` 的跨平台兼容**：`workDir/runs/{run_id}/trace.json` 在 Windows 上路径分隔符问题。建议 MCP 返回正斜杠路径，Agent 用 `pathlib` 处理。

7. **并发 run 的隔离**：多个 `begin_capture_run` 并发时，Agent 如何区分？建议 `run_id` 作为唯一隔离键，不依赖 session 状态。
