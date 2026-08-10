# MCP Plugin Domain 设计 v2：三平面 + 双状态空间

> 状态：设计稿（未实现）
> 涉及仓库：`E:\gta`（MCP + Pipeline + Event Store + Plugin 管理）、`E:\ai_workspace\gta-plugin-sdk`（契约定义 + SDK Runtime）
> v2 变更：按评审意见重构。核心变化是引入 Developer Plane，把 build/activate 移出 MCP 进程；状态从单一状态机拆为 Artifact / Runtime 双状态空间。

---

## 0. 评审裁决

| # | 评审意见 | 裁决 | 说明 |
|---|---|---|---|
| 1 | 拆 Artifact / Runtime 双状态空间 | **采纳** | 见 §2。补一条 v1 漏掉的失效规则 |
| 1b | build/activate 不该由 MCP 做 | **采纳诊断，改药方** | 见 §1。直接砍会打断闭环，正解是移出 MCP **进程**而非移出 MCP **表面** |
| 2 | scaffold 保持纯生成，拆 `analyze_sample` | **采纳** | 见 §4.2。补一条：两者都不得写代码 |
| 3 | brief 返回结构化而非 Markdown | **采纳并加强** | 见 §3。rule ID 应成为 brief/verify/explain 三者的共享词汇 |
| 4 | contract.yaml 迁 SDK | **采纳**（v1 已同此判断） | — |
| 5 | checker 按静态/动态拆到两仓库 | **部分不采纳** | 见 §5。分界线应是"能否离线自测"，不是"静态/动态" |
| 6 | diagnose 改名 explain 并提前 | **采纳** | 见 §7 分期，拆两阶段落地 |
| 7 | sample_bytes 加审计 | **采纳** | 见 §6，含落点选择 |
| 8 | plugin.list 分 local/runtime | **采纳** | 与双平面天然一致 |
| 9 | 增加 failed / 进行中状态 | **采纳需求，改建模** | 见 §2.3。`failed` 不该建成状态 |
| 10 | 工具集约 13 个 | **采纳** | 见 §4，最终 13 个 |

---

## 1. 三平面：MCP 退回协议适配器

### 1.1 分歧点

评审说"MCP 不应该成为 IDE Build Server"——这个诊断我完全同意。但如果据此把 `build` / `activate` 从工具表面拿掉，AI 就必须跳出 MCP 执行 shell，而"不离开 MCP 完成迭代"正是这个设计存在的理由。闭环一断，Gateway 就退化回文档工具。

真正的问题不是**"MCP 要不要暴露 build"**，而是**"谁来实现 build"**。

### 1.2 现状：MCP 本来就是薄代理，是 create_plugin 起的头

查代码可以确认：`cmd/gta-mcp` 里绝大多数 handler 都只是 gRPC 转发（`handleTestPlugin` → `pipelineClient.TestPlugin`、`handleSetSessionPlugin`、`handleDeregisterPlugin` 等），本地只做参数校验。

唯二直接摸本地文件系统的是：

- `handleCreatePlugin`（`create_plugin.go:69-85`）—— 直接 `os.MkdirAll` + `os.WriteFile`
- `handleListPlugins`（`main.go:546`）—— 直接 `os.ReadDir`

也就是说，**"MCP 变成 IDE"这个漂移是从 `create_plugin` 开始的**。如果再往里塞 `exec.Command`，只是把已有的错误放大。

### 1.3 结论：三平面

```
Developer Plane   ← 拥有文件系统与子进程：scaffold / build / activate
Runtime Plane     ← 拥有流量、session、registry：verify 执行 / bind / decode
MCP               ← 只做协议适配与路由，零 exec、零 os.WriteFile
```

具体落法：新增 `pkg/plugindev` 提供 `PluginDev` gRPC 服务，可内嵌于 `gta-pipeline` 也可独立成 `gta-plugin-dev` 二进制。MCP 侧全部改为转发，`create_plugin.go` 里的文件写入下沉过去。

这样做的三个收益：

1. `cmd/gta-mcp` 里 `exec.Command` 数量保持为 0（当前全仓库唯一一处在 `pkg/script/executor.go:69`，跑 Python，与此无关）
2. 生产部署只要不启动 Developer Plane，全部开发态能力零暴露——比一个 `--enable-plugin-dev` 布尔开关强，因为那是**物理隔离**而非条件分支
3. Developer Plane 与 Runtime Plane 的边界，正好对上 §2 的双状态空间

### 1.4 为什么 activate 属于 Developer Plane

`activate` 是"从本地目录拉起一个本地二进制并注入 `GTA_REGISTRY_ADDR`"。生产环境里插件由 systemd / k8s 拉起，gta 从不 spawn（`Manager.Restart()` 至今是占位，`pkg/plugin/manager.go:534`）。所以 activate 只服务于开发回路，归 Developer Plane。

Runtime Plane 对插件进程只做**观测**（registry 注册 + 心跳），不做**管理**。这条线一划，`Manager` 保持被动的现有设计就不必改了。

---

## 2. 双状态空间

### 2.1 两个正交平面

**Artifact State**（Developer Plane 拥有，描述代码）

```
unknown → scaffolded → compiled → validated
```

**Runtime State**（Runtime Plane 拥有，描述运行）

```
offline → registered → active → bound
```

`plugin.status` 返回：

```json
{
  "name": "my-game",
  "artifact": {
    "state": "compiled",
    "source_dir": "...", "binary_path": "...",
    "binary_stale": false,
    "last_attempt": { "action": "build", "ok": true, "at": "...", "duration_ms": 1840 }
  },
  "runtime": {
    "state": "registered",
    "instance_id": "...", "last_heartbeat": "...",
    "bound_sessions": []
  },
  "next_action": { "tool": "plugin.verify", "args": {...}, "why": "已注册但尚未验证过契约合规性" }
}
```

### 2.2 v1 漏掉的：validated 是跨平面产物，必须有失效规则

`validated` 挂在 Artifact 平面，但它的取得依赖 Runtime——要有已注册的实例和真实 session 才能验。这是唯一一处跨平面耦合，必须显式建模，否则会出现"代码改了但状态还显示 validated"的假阳性：

- **失效规则**：任何一次 `build` 成功，Artifact 立即从 `validated` 降级回 `compiled`
- **溯源**：`validated` 必须携带 `proof: { verify_run_id, session_id, verdict, at }`，不能是一个裸 bool
- **陈旧判定**：`binary_stale` 由 `main.go` 与 `*.exe` 的 mtime 比较得出（`tools/hotreload.go` 的实践已经在用这个信号），stale 时 `next_action` 强制指向 `build`

### 2.3 失败不该建成状态

评审第 9 条要加 `failed` / `building` / `activating`。需求真实，但建成状态会有两个问题：

**问题一：`failed` 是二义的。** build 失败后 artifact 是什么状态？没有可用二进制，它**仍然是 `scaffolded`**。如果引入 `build_failed`，就得回答"`build_failed` 和 `scaffolded` 有什么行为差异"——答案是没有，允许的下一步动作完全相同（改代码、重新 build）。这是纯粹的状态膨胀，而且每个状态都要配一个 failed 变体，组合爆炸。

**问题二：状态回答不了"为什么"。** AI 真正需要的是归因，`failed` 这个词零信息量。

建模改为**状态不变 + 附着最后一次尝试**：

```json
"last_attempt": {
  "action": "build",
  "ok": false,
  "at": "2026-08-09T20:31:02+08:00",
  "errors": [{ "file": "main.go", "line": 42, "col": 9, "message": "undefined: event.ValueInt32" }],
  "explain_ref": "expl_01J..."
}
```

`explain_ref` 直接指向一次 `plugin.explain` 的结论，AI 拿到 status 就知道卡在哪、为什么、下一步做什么——比一个 `failed` 状态强得多。

**进行中状态**同理不建成状态。build / activate 做成同步且有界（build 默认 120s，activate 默认等注册 10s），并发观察者用 `in_flight: { action, started_at }` 字段感知。顺带一提，`/events/plugins` 这个 SSE 端点（`main.go:1897`）已经在推插件事件，进行中状态可以复用它推送，不必污染状态机。

---

## 3. 知识供给：rule ID 作为共享词汇

评审第 3 条说 brief 不要返回 Markdown——同意，AI 的 context 很贵，20KB 的 `Agents.md` 灌进去是纯浪费。但我想再推一步：

**如果 brief 的规则是手写第二份，它一定会和 `Agents.md` 漂移**——这就是 P1 契约漂移问题换个地方复发。

所以规则必须来自 SSOT。在 SDK 的 `contract.yaml` 增加机器可读的 `rules:` 段：

```yaml
rules:
  - id: payload-framing-by-link-type
    topic: framing
    severity: error
    statement: "DecodeRequest.payload is a complete link-layer frame for pcap sources; strip by link_type (framing.ExtractL7) before parsing L7"
    doc_ref: "Agents.md#8.1"
  - id: done-required
    topic: lifecycle
    severity: error
    statement: "every input_id must eventually receive a response with done=true"
    doc_ref: "Agents.md#10"
  - id: value-accessor-ok
    topic: value
    severity: warn
    statement: "event.Value accessors return (value, ok); never ignore ok"
    doc_ref: "Agents.md#11"
```

然后**同一套 rule ID 贯穿三个工具**：

| 工具 | 用法 |
|---|---|
| `plugin.brief` | 按 topic / severity 筛选返回规则集 |
| `plugin.verify` | violation 直接标 `rule_id: "done-required"` |
| `plugin.explain` | 归因结论引用 `rule_id`，AI 可回查 |

这才是让三个工具真正能组合的关键——它们说同一种语言。Markdown 只留给人读，且由 rule 表反向生成或引用，不再是独立真源。

`plugin.brief` 与 `plugin.contract` 的分工：

- `brief` = 写代码要遵守的**规则**（rules 段，可按 topic 切片，默认只回 severity=error 的约 12 条）
- `contract` = 机器可读的**规格**（RPC 字段形状、link_types 枚举、manifest schema）

两者都来自同一个 `contract.yaml`，不重复。

---

## 4. 工具集（13 个）

### 4.1 全表

| 工具 | 平面 | 状态迁移 |
|---|---|---|
| `plugin.status` | 聚合两面 | — （每轮入口） |
| `plugin.list` | 聚合两面 | — |
| `plugin.brief` | 静态 | — |
| `plugin.contract` | 静态 | — |
| `plugin.scaffold` | Developer | unknown → scaffolded |
| `plugin.build` | Developer | scaffolded → compiled |
| `plugin.activate` | Developer | offline → registered |
| `plugin.deactivate` | Developer | registered/active → offline |
| `plugin.analyze_sample` | Runtime | — （产出假设） |
| `plugin.sample_bytes` | Runtime | — （产出证据） |
| `plugin.verify` | 跨面 | compiled → validated |
| `plugin.bind` | Runtime | registered → bound |
| `plugin.explain` | 跨面 | — （失败归因） |

`plugin.list` 返回按平面分组：

```json
{
  "local":   [{ "name": "my-game", "artifact_state": "compiled", "binary_stale": false }],
  "runtime": [{ "name": "http", "runtime_state": "active", "instance_id": "..." }]
}
```

顺带修掉 P7：现有 `list_plugins` 在 `main.go:552-555` 显式 `if e.IsDir() { continue }`，只扫顶层文件，而插件实际布局是 `plugins/<name>/<name>.exe`——刚 scaffold 出来的插件根本列不出来。

被吸收掉的旧工具：`get_plugin_manifest` 折进 `plugin.status`（manifest 内联返回）；`deregister_plugin` 折进 `plugin.deactivate`（自己拉起的进程就 kill，外部拉起的就走强制注销）。

### 4.2 scaffold 与 analyze_sample 的边界

同意评审：scaffold 必须是纯生成，v1 里那个 `sample_session_id` 参数是职责爆炸，删掉。

三者的边界要按**输出的可信度**来划，这比按功能划更不容易再糊回去：

| 工具 | 输出性质 | 硬约束 |
|---|---|---|
| `plugin.sample_bytes` | **事实**（hexdump、长度直方图、首字节分布、熵估计） | 不做任何解释 |
| `plugin.analyze_sample` | **假设**（framing 猜测、字段边界猜测），每条带 confidence | 不做任何写入 |
| `plugin.scaffold` | **模板**（plugin.yaml + main.go + go.mod + main_test.go + testdata/） | 不读 session |

**统一铁律：三者都不写解码逻辑。** 证据和假设交给 AI，代码由 AI 写。工具一旦开始"帮忙填 TODO"，出错时的归因链就断了。

流程回到评审建议的形状：`scaffold` → `sample_bytes` / `analyze_sample` → AI 写 decoder → `build` → `activate` → `verify`。

---

## 5. checker 归属：分界线不是静态/动态

评审第 5 条建议把 `CheckDecodeResponse`（input_id 一致、done 生命周期、payload 非空）移到 gta，理由是"SDK 不应该知道 pipeline runtime"。

**这条我不采纳**，原因是它会破坏一个已经建立的重要性质。

### 5.1 反对理由

`CheckDecodeResponse` 校验的是**插件与宿主之间的线上协议**，不是 pipeline 的运行时知识——它是 `DecodeResponseV2` 这个 message 的自洽性，纯函数，不需要知道 session、不需要知道 registry、不需要知道 SQLite。

更关键的是：**插件作者需要在自己的单元测试里跑它**。SDK 的 `Agents.md` §17 明确要求验证 "every decode input eventually emits done=true"。而插件模块的既定性质是 `plugins/<name>/go.mod` **只 require SDK**，不依赖 gta 根模块。把 checker 移到 gta，等于插件作者无法离线自测响应合规性，只能跑起整条 pipeline 才知道对错——这是明显退步。

### 5.2 更好的分界线

评审的直觉没错，确实存在两类校验，只是切分维度选错了。正确的分界线是**"插件作者能否只依赖 SDK、离线跑通"**：

| | SDK `contract/` | gta `pkg/plugin/quality/` |
|---|---|---|
| 判据 | 单条消息的协议自洽性 | 一批消息的统计质量 |
| 输入 | 一个 request/response 对 | 一次 verify 的完整语料 |
| 例子 | manifest 合法、input_id 回显、done 存在、payload 非空、msgpack 可解 | unknown 占比 3%、无任何 correlation、decode_errors 集中在长包、schema_id 未版本化 |
| 能否离线自测 | 能 | 不能（需要真实流量） |
| 性质 | 对错判定 | 好坏判定 |

一句话检验：**能不能在插件项目里 `go test` 跑通？能 → SDK；不能 → gta。**

`plugin.verify` 的结果就是这两层的合并：`violations`（引 SDK checker，带 rule_id）+ `quality`（gta 侧统计）+ `verdict`。

---

## 6. sample_bytes 审计

采纳。补充落点判断：

- **表**：`plugin_debug_access`，建在 `control.sqlite`（`pkg/store/session_store.go` 现在只有 `sessions` 一张表）
- **写入方唯一**：`control.sqlite` 被 MCP（`main.go:331`）和 pipeline（`pipeline/main.go:57`）两个进程同时打开，审计写入必须只由 Runtime Plane 一方执行，避免加剧 SQLite 锁竞争——这个坑 SDK 的 `troubleshooting.md` §8 已经记过
- **记实际值不记请求值**：`returned_packets` / `returned_bytes` 必须记真实返回量，而不是入参里请求的量，否则截断后审计数据是假的
- **仅追加**：无 UPDATE / DELETE 路径

字段：`{ id, at, actor, tool, plugin, session_id, requested_packets, returned_packets, returned_bytes, truncated }`。

硬上限（20 包 / 64 字节）不可通过参数突破，这一条 v1 已有，保留。

---

## 7. 分期

| 阶段 | 内容 | 为什么在这个位置 |
|---|---|---|
| **P0 契约统一** | contract.yaml 迁 SDK 并修正 v2 `output_contract`（现 output_contract 已统一为 msgpack，`payload_msgpack` 为唯一产出，v1 JSON 约定已废止，见 SDK 仓库 contract/contract.yaml）；新增 `rules:` 段；gta 删重复 Manifest 定义改依赖 `sdk.Manifest`；修 `list_plugins` 目录 bug | 不先做，后面每个工具都在放大同一份错误契约 |
| **P1 平面拆分** | 建 `pkg/plugindev` + `PluginDev` gRPC；`create_plugin` 的文件写入下沉；MCP 全面转发化 | 结构先立住，之后加工具是填空而非改架构 |
| **P2 闭环** | `plugin.build`（结构化 file:line:col）+ `plugin.activate/deactivate` + `plugin.status`（双状态 + last_attempt） | AI 第一次能不离开 MCP 跑通 |
| **P3a explain 一期** | 只归因 build 失败与注册失败 | 这两类不需要流量语料，成本低、见效早，正好接住 P2 的失败面 |
| **P4 验证** | `plugin.verify`（SDK violations + gta quality + verdict）；`plugin.sample_bytes` + 审计 | — |
| **P3b explain 二期** | 加解码类归因（全 unknown、错 framing、疑似加密、疑似缺流重组） | 依赖 P4 产出的语料才有判据 |
| **P5 可选** | `plugin.analyze_sample` | 锦上添花 |

`explain` 拆两期，是为了兑现评审第 6 条"应该提前"——它整体依赖 verify 语料，但**构建期失败的归因不依赖任何流量**，可以先落地，AI 在最容易卡住的第一道坎上就能拿到归因。

---

## 8. 验收标准

给 AI 一个正在抓包的 session 和一句"给这个协议写个解码器"，它能在**不打开任何 SDK 文件、不执行任何 shell 命令**的前提下，产出一个 `artifact.state=validated` 且 `runtime.state=bound` 的插件。

- [ ] 全程零 shell 调用
- [ ] 全程未读取 SDK 仓库任何文件
- [ ] `cmd/gta-mcp` 中 `exec.Command` 与 `os.WriteFile` 计数为 0
- [ ] 编译失败时，AI 凭 `last_attempt.errors` 的 file:line:col 定点修复
- [ ] 解码全 unknown 时，AI 凭 `plugin.explain` 定位到 framing 或加密，且结论引用了可回查的 rule_id
- [ ] 每次 `build` 成功后 `artifact.state` 正确从 `validated` 降级回 `compiled`
- [ ] 每次 `sample_bytes` 在 `plugin_debug_access` 留下与实际返回量一致的记录
