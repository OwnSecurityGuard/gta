# GameTrace 分析规则编写指南（rules.yaml）

**目的**：让 AI（或人工）能够为 `analyze` 流式分析引擎扩展规则，而不必阅读 `pkg/analyze/rule.go` 源码。
**来源（SSOT）**：`pkg/analyze/rule.go`（`RawRule` / `CompileRule`）与 `pkg/analyze/engine.go`（`Process` 的 expr 运行环境）。本文件是对它们的可读转述，若与代码冲突以代码为准。

---

## 1. 规则存在哪里、如何生效

- 规则写在仓库根目录的 `rules.yaml`，结构为 `rules:` 列表。
- 由 `gt-pipeline` 在**启动时加载并编译**（`CompileRule`）。编译失败会在启动日志报错，对应规则不生效。
- 修改 `rules.yaml` 后**需要重启 `gt-pipeline`** 才能生效。
- 验证：重启后用 MCP 工具 `aggregate_query` 查询对应 `output` 指标，确认有数据返回。

当前仓库内的真实示例（`rules.yaml`）：

```yaml
rules:
  - name: http_req_count
    filter: 'data.type == "request"'
    aggregate:
      type: count
      window: 10s
      group_by: [data.method]
      output: http_req_count
  - name: http_req_rate
    filter: 'data.type == "request"'
    aggregate:
      type: rate
      window: 10s
      group_by: []
      output: http_req_rate
```

---

## 2. 单条规则的字段结构（RawRule）

```yaml
- name: <string, 必填, 规则名>
  filter: <string, expr, 必须返回 bool>      # 哪些事件进入本规则
  enrich: <map[string]string, 可选>          # 派生字段：key=字段名, value=expr
  schema: <string, 可选>                     # schema id；设置后校验 data.* 路径
  aggregate:
    type: <count | sum | rate>               # 聚合类型
    window: <Go duration 字符串, 如 10s/1m>  # 滑动窗口
    group_by: <[expr...]>                    # 分组键列表
    value: <expr, 仅 sum 需要, 数值>         # 求和表达式
    output: <string, 指标名>                 # 产出指标名（aggregate_query 查询用）
```

字段语义：

- **`filter`**：对每个事件求值，返回 `true` 的事件才进入该规则的 `enrich` + `aggregate`。**必须返回 bool**，否则 `CompileRule` 报错。
- **`enrich`**：一组 `key: expr`，求值后把结果作为派生字段并入该规则的求值上下文（可被 `group_by` / `value` 引用，也会体现在 `aggregate_query` 的结果上下文中）。
- **`aggregate.type`**：`count`（计数）、`sum`（对 `value` 求和）、`rate`（按窗口求速率）。未知类型在编译期报错。
- **`aggregate.window`**：Go `time.ParseDuration` 能解析的字符串（`10s`、`1m`、`5m`）。
- **`aggregate.group_by`**：分组键表达式列表；空列表 `[]` 表示不分组建单一序列。
- **`aggregate.value`**：仅 `sum` 需要，必须是数值表达式。
- **`aggregate.output`**：产出指标名，`aggregate_query(expression=<output>)` 用它查。

---

## 3. 表达式可用的变量

运行时每条事件代入以下环境（`engine.go` `Process`）：

| 变量 | 类型 | 含义 |
|---|---|---|
| `event` | `event.Event` | 完整事件对象（`Identity` / `Relation` / `Context` / `Payload` / `Timestamp` 等） |
| `data` | `any`（实际为 `map[string]any`） | 解码后的业务载荷，`event.Payload.Value.ToAny()` 的结果；即 `data.method`、`data.type` 这种字段 |

> **重要**：规则**编译器**（`CompileRule`）只对 `{event, data}` 做类型推断，因此写在 `filter` / `enrich` / `group_by` / `value` 里的表达式应只引用 `event.*` 或 `data.*`。`data.*` 是解码器实际 emit 出来的字段（如 `data.type`、`data.method`）。
>
> 虽然运行环境还额外注入了 `identity` / `relation` / `context` / `payload`，但编译期不认这些名字——直接在规则里写它们会编译失败。需要这些信息时请用 `event.Identity.*` / `event.Relation.*` / `event.Context.*` / `event.Payload.*`。

`data.*` 路径校验：当规则设置了 `schema` 且该 schema 可解析时，`filter` / `enrich` / `value` / `group_by` 里出现的 `data.<path>` 会对照该 schema 声明的字段做校验（`validateRuleSchema`）。所以 `data.*` 要用解码器实际 emit 的**精确字段名**。

---

## 4. 编写自己的规则（AI 自检步骤）

1. 确定要统计什么（如"每种 HTTP 方法的请求数"）。
2. 选 `aggregate.type`：`count` / `sum` / `rate`，以及 `window`（如 `10s`）。
3. 写 `filter` 限定事件范围，必须返回 bool。例：`data.type == "request"`。
4. 写 `group_by`（如 `[data.method]`）或留空。
5. `sum` 时写 `value`（如 `data.size`）。
6. 取一个唯一的 `output` 名。
7. 加进 `rules.yaml`，**重启 `gt-pipeline`**。
8. 用 `aggregate_query(expression=<output>)` 断言返回 `{name, window, value, group}`，确认规则生效。

常见错误：

- `filter` 写成非 bool（如 `data.type`，缺比较）→ 运行期报错 "filter did not return bool"。
- `data.xxx` 字段名与解码器 emit 的不一致 → 设了 `schema` 时编译期报 "schema error"，未设时静默取不到值。
- `sum` 漏写 `value` → 编译期报 "unknown aggregator" 或 value 缺失。

---

## 5. 与文档体系的边界

- 本文件只讲 `rules.yaml` 的编写。解码器如何 emit `data.*` 字段，见 SDK `Agents.md` §8.1 / `decoder-development.md`。
- 指标如何被查询/可视化，见 `aggregate_query` 的 MCP 工具描述。
- 不要在本文件里写捕获/插件注册/framing 等内容——那些归 SDK 文档与 `troubleshooting.md`。
