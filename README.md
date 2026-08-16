# GTA — Game Traffic Analysis

面向游戏协议流量分析的 AI Agent 工具链。捕获网络数据包、通过插件解码游戏协议、实时聚合分析、并通过 MCP 协议暴露给 AI Agent，让 Agent 从真实流量中提取协议证据链并自动生成压测代码。

## 架构

系统按**三平面**切分（详见 `docs/plugin-domain-design.md` §1）。MCP 只是协议适配器：它不执行子进程、不写文件、不做失败归因，所有能力向下转发。

```
AI Agent (Claude / DeepSeek / ...)
        │  MCP over HTTP
        │  SSE: :8781/sse (+ /message)   Streamable: :8781/mcp
        ▼
┌──────────────────────────────────────────────────────────────┐
│  gta-mcp  (:8781)                                            │
│  全量 MCP 工具纯转发：零 exec / 零文件写入 / 零归因逻辑      │
└────────┬─────────────────────────────────┬───────────────────┘
         │ gRPC PluginDev                  │ gRPC CaptureControl (:9888)
         ▼                                 ▼
┌────────────────────────────┐   ┌──────────────────────────────┐
│  Developer Plane           │   │  Runtime Plane               │
│  gta-plugin-dev (:8089)    │   │  gta-pipeline                │
│  或由 gta-mcp 内嵌（默认） │   │  ┌────────────────────────┐  │
│                            │   │  │ capture.Source         │  │
│  拥有文件系统与子进程：    │   │  │ → decode.Dispatcher    │  │
│  scaffold / build /        │   │  │ → analyze.Engine       │  │
│  activate / deactivate /   │   │  │ → store.SQLiteStore    │  │
│  status / explain          │   │  └────────────────────────┘  │
│                            │   │  拥有流量 / session / 注册表 │
│                            │   │  verify / sample_bytes /     │
│                            │   │  bind / decode               │
│                            │   │  PluginRegistry (:9091)      │
└────────────┬───────────────┘   └──────────────┬───────────────┘
             │ 拉起本地二进制并注入             │ gRPC 注册 + 心跳
             │ GTA_REGISTRY_ADDR                │（只观测，不管理进程）
             └──────────►  解码插件进程  ◄──────┘
```

**为什么这样切**：生产部署只要不启动 Developer Plane，全部开发态能力零暴露——是物理隔离，而不是一个 `--enable-plugin-dev` 布尔开关。

### 端口

| 服务 | 默认地址 | 说明 |
|------|---------|------|
| gta-mcp HTTP | `:8781` | `/sse` + `/message`（SSE）、`/mcp`（Streamable HTTP）、`/events/plugins`（插件事件流） |
| CaptureControl gRPC | `:9888` | gta-mcp → gta-pipeline 的控制通道 |
| PluginRegistry gRPC | `:9091` | 解码插件注册到 gta-pipeline |
| PluginDev gRPC | `:8089` | 仅独立部署 Developer Plane 时监听；内嵌模式走 `127.0.0.1:0` |

## 组件

| 组件 | 说明 |
|------|------|
| **gta-mcp** | MCP 服务器（工具目录见下文，由脚本生成），纯协议适配 + 路由 |
| **gta-pipeline** | Runtime Plane：抓包 / 解码 / 聚合 / 落库 / 插件注册表一体进程 |
| **gta-plugin-dev** | Developer Plane：插件脚手架 / 编译 / 拉起 / 状态 / 失败归因。**默认由 gta-mcp 内嵌，无需单独启动** |
| **插件系统** | 独立进程解码器，gRPC 通信，支持热加载与崩溃隔离 |
| **语义引擎** | 事件流 → 语义证据图（节点/边/可解释归因），契约见 `docs/semantic-evidence-v1.md` |
| **聚合引擎** | 基于滑动窗口的实时指标聚合 |

插件 SDK 已拆为独立仓库 `github.com/OwnSecurityGuard/gta-plugin-sdk`，插件工程只依赖 SDK，与 gta 源码零耦合。

## 快速开始

### 环境要求

- Go 1.25+
- libpcap / npcap（Windows 需安装 [Npcap](https://npcap.com/)）
- protoc + protoc-gen-go + protoc-gen-go-grpc（仅修改 proto 时需要）

### 构建

```bash
make build              # gta-mcp + gta-pipeline + gta-plugin-dev → bin/

make build-mcp          # 单独构建
make build-pipeline
make build-plugin-dev
```

插件是独立 module，单独构建（以 SDK 参考插件为例，蓝本见 [gta-plugin-sdk/examples/http-stream-decoder](https://github.com/OwnSecurityGuard/gta-plugin-sdk/tree/main/examples/http-stream-decoder)）：

```bash
cd plugins/four-layer-demo && go build -o four-layer-demo.exe .
```

### 运行

启动顺序：**pipeline → mcp → 插件**（插件可后启，注册表支持热注册）。

```bash
# 终端 1 — Runtime Plane（数据面，先启）
./bin/gta-pipeline.exe -workdir . -log-format text

# 终端 2 — 控制面（内嵌 Developer Plane）
./bin/gta-mcp.exe -work-dir . -plugins-dir plugins -log-format text

# 终端 3 — 解码插件（按需；推荐直接走 MCP 工具链 activate_plugin 拉起）
cd plugins/four-layer-demo && GTA_REGISTRY_ADDR=127.0.0.1:9091 ./four-layer-demo.exe
```

启动后在 AI Agent 客户端配置 MCP 连接 `http://127.0.0.1:8781/sse`，或走 Streamable HTTP 的 `http://127.0.0.1:8781/mcp`。

`make run-mcp` / `make run-pipeline` 走 `go run`，适合改代码时快速验证；长跑用 `bin/` 下的二进制。

#### 独立部署 Developer Plane（可选）

`GTA_PLUGINDEV_ADDR` 为空时，gta-mcp 会在 `127.0.0.1:0` 起一个进程内 PluginDev 服务并自连。只有需要跨机器、单独重启开发平面、或生产环境要求彻底不加载开发能力时，才拆成独立进程：

```bash
# 终端 A
./bin/gta-plugin-dev.exe -addr :8089 -plugins-dir /path/to/plugins

# 终端 B
GTA_PLUGINDEV_ADDR=127.0.0.1:8089 ./bin/gta-mcp.exe -work-dir . -plugins-dir plugins
```

生产环境**不启动**该进程即可：所有插件开发工具随之失效，这是物理隔离。

### 主要启动参数

**gta-pipeline**

| 参数 | 默认 | 说明 |
|------|------|------|
| `-workdir` | `.` | 工作目录（会话库、日志的根） |
| `-rules` | 空 | `rules.yaml` 路径 |
| `-control` | `<workdir>/control.sqlite` | 控制库路径 |
| `-control-addr` | `:9888` | CaptureControl gRPC 监听地址 |
| `-registry-addr` | `:9091` | PluginRegistry gRPC 监听地址 |
| `-debug` / `-log-format` / `-log-file` | `false` / `json` / `<workdir>/logs/gta-pipeline.log` | 日志 |

**gta-mcp**

| 参数 | 默认 | 说明 |
|------|------|------|
| `-addr` | `:8781` | HTTP 监听地址 |
| `-work-dir` | `.` | 会话数据库工作目录 |
| `-plugins-dir` | `plugins` | 插件目录 |
| `-pipeline-addr` | `:9888` | gta-pipeline 的 CaptureControl 地址 |
| `-iface` | 空 | 抓包网卡，空表示全部 |
| `-enable-raw-debug` | `false`（或 `GTA_MCP_ENABLE_RAW_DEBUG=1`） | 暴露 `list_raw_packets` / `decode_raw_packets`，插件开发调试用 |
| `-debug` / `-log-format` / `-log-file` | `false` / `json` / `<workdir>/logs/gta-mcp.log` | 日志 |

**gta-plugin-dev**

| 参数 | 默认 | 说明 |
|------|------|------|
| `-addr` | `:8089`（或 `GTA_PLUGINDEV_ADDR`） | PluginDev gRPC 监听地址，支持 `host:port` / `unix:/path` / `npipe:\\.\pipe\name` |
| `-plugins-dir` | `./plugins`（或 `GTA_PLUGINS_DIR`） | 服务被限定的插件根目录，客户端无权指定落盘位置 |

### 运行测试

```bash
make test
```

## 核心功能

<!-- BEGIN TOOL TABLE (generated by scripts/gen_tool_table; do not edit) -->
### MCP 工具（40 个；`list_raw_packets` / `decode_raw_packets` 需 `-enable-raw-debug`，默认不注册）

由 `go run ./scripts/gen_tool_table` 从 `cmd/gta-mcp/main.go` 生成，勿手改。

| 工具 | 功能（描述首句） |
|------|------|
| `start_capture` | Start capturing traffic on a server port, or replay a pcap file |
| `stop_capture` | Stop a running capture session and flush all data |
| `get_session_status` | Get capture status for a specific or current session |
| `list_plugins` | List available decoder plugins |
| `create_plugin` | Scaffold a new decoder plugin project (plugin.yaml + main.go + go.mod) from templates |
| `build_plugin` | Compile a scaffolded plugin project via the Developer Plane |
| `activate_plugin` | Launch the local plugin binary and inject GTA_REGISTRY_ADDR so it registers with the runtime |
| `deactivate_plugin` | Stop the plugin process the Developer Plane launched for name, and best-effort force-deregister it from the ru… |
| `status_plugin` | Return the dual-state view of a plugin (design §2): artifact (unknown→scaffolded→compiled→validated, from disk… |
| `explain_plugin` | Attribute the most recent build or activate failure of a plugin (design §2.3 / P3a) |
| `get_plugin_contract` | Return the full contract.yaml spec for the GTA decoder plugin API |
| `get_plugin_dev_guide` | Return the full plugin development guide (markdown) |
| `get_registry_addr` | Return the registry address the pipeline is currently listening on (its -registry-addr, e.g |
| `get_capabilities` | Return a self-describing catalog of all MCP tools grouped by workflow (capture / query / evidence / behavior /… |
| `list_registered_plugins` | List all plugins currently registered with the pipeline (active via gRPC PluginRegistry) |
| `get_plugin_manifest` | Get the plugin.yaml manifest of a registered plugin by name |
| `deregister_plugin` | Manually deregister a plugin from the pipeline |
| `set_session_plugin` | Hot-swap the decoder plugin bound to a RUNNING capture session |
| `list_interfaces` | List available pcap capture interfaces |
| `list_live_sessions` | List currently active capture sessions from the pipeline |
| `list_decoded_data` | List decoded protocol events from a capture session |
| `list_state_changes` | List state change projections from a capture session, with optional filtering by subject_type, subject_id, op,… |
| `query_capture_table` | Read-only escape hatch for internal projection/audit tables that have no dedicated tool |
| `aggregate_query` | Query aggregated metrics/statistics using an expr expression over {name, window, value, group} |
| `query_evidence_graph` | Query the evidence graph (nodes + edges) built by the semantic analysis engine |
| `trace_event_chain` | Trace the complete upstream and downstream evidence chain for an event |
| `analyze_protocol_patterns` | Analyze protocol patterns in captured traffic to support AI-driven link rule discovery |
| `suggest_link_rules` | Suggest link rules based on evidence graph analysis |
| `begin_capture_run` | Mark the start of a user operation or behavior WITHOUT starting capture |
| `end_capture_run` | Close the current behavior window |
| `get_run_status` | Quickly check whether a behavior run has useful data |
| `trace_protocol_flow` | Build the chronological evidence chain (causation chain) for one behavior |
| `get_capture_schema` | Describe available fields for decoded events, state_changes projections, aggregation metrics and current rules |
| `list_raw_packets` | [PLUGIN DEBUG ONLY] List raw packets from a capture session, with optional protocol/src/dst filtering |
| `decode_raw_packets` | [PLUGIN DEBUG ONLY] Decode raw packets of an offline session using a specified plugin |
| `test_plugin` | Test a plugin by decoding an offline session's raw packets in-process and returning sampled decoded events |
| `verify_plugin` | Verify a plugin by decoding an offline session's raw packets and checking contract violations (SDK checker, ea… |
| `sample_bytes_plugin` | Sample the first bytes of a session's raw packets as FACTS only (hexdump, length histogram, first-byte distrib… |
| `list_all_sessions` | List all capture sessions with their metadata (including stopped/offline sessions) |
| `delete_session` | Delete a capture session and its data |

<!-- END TOOL TABLE -->

### 双状态空间

插件状态拆成两个正交平面（设计 §2），避免"代码改了但状态还显示已验证"的假阳性：

- **Artifact State**（Developer Plane 拥有，描述代码）：`unknown → scaffolded → compiled → validated`
- **Runtime State**（Runtime Plane 拥有，描述运行）：`offline → registered → active → bound`

`validated` 是唯一的跨平面产物——必须有真实 session 跑过 `verify_plugin` 才能取得，且携带证明 `{verify_run_id, session_id, verdict, at}`；任何一次成功的 `build_plugin` 都会使其失效，退回 `compiled`。

### 插件系统

- **热加载**：插件作为独立进程，通过 gRPC 注册到 pipeline，自动发现和切换
- **崩溃隔离**：插件独立进程，崩溃不影响 pipeline
- **多项目隔离**：A 项目会话只认 A 插件，B 项目会话只认 B 插件
- **跨机器部署**：支持 TCP / Unix Socket / Windows Named Pipe
- **只观测不管理**：Runtime Plane 对插件进程只做注册与心跳观测，拉起/停止归 Developer Plane

### 聚合规则

通过 `rules.yaml` 配置实时聚合：

```yaml
rules:
  - name: http_req_count
    filter: 'data.type == "request"'
    aggregate:
      type: count
      window: 10s
      group_by: [data.method]
      output: http_req_count
```

### 语义引擎与证据图

语义引擎将解码后的事件流转化为结构化证据图，供 AI Agent 分析协议逻辑。

**证据节点类型**：`request` / `response` / `push_message` / `state_change` / `transaction`

**证据边类型**：`ResponseTo`（请求-响应）、`DecodedFrom`（解码来源）、`PossibleFollowup`（可能后继）、`Contains`（事务包含）、`PrecededBy`（状态前驱）

**核心能力**：

| 能力 | 说明 |
|------|------|
| 消息命名模式匹配 | 当协议未显式关联请求-响应时，通过命名后缀推断（如 `LoginReq`→`LoginResp`、`CS_*`→`SC_*`），置信度 0.85 |
| 链路规则自动建议 | 聚合证据图边模式，按（source_type, target_type, edge_type）生成结构化规则建议 |
| 事件链双向追踪 | BFS 遍历证据图上下游，输出完整事件调用链及每一步的关联原因 |
| 时间聚类 | 按请求边界将事件归入逻辑事务组（`NodeTransaction`），支持 `MergeGap` 合并快速连续请求，默认 200ms |

## 项目结构

```
cmd/
  gta-mcp/        MCP 服务器入口（控制面）
  gta-pipeline/   Runtime Plane 入口（抓包 / 解码 / 聚合 / 落库 / 注册表）
  gta-plugin-dev/ Developer Plane 独立入口（可选，默认由 gta-mcp 内嵌）
  mcp-test/       MCP 连通性冒烟客户端
pkg/
  capture/        流量来源抽象（pcap-live / pcap-file / fake）
  decode/         解码调度层
  analyze/        语义分析引擎（证据图构建 / 命名模式匹配 / 链路聚类 / 基线管理）
  event/          核心数据模型（Packet / Event / Metric）
  store/          SQLite 持久化层（会话库 + control.sqlite + 调试审计）
  plugin/         插件运行时（注册表 / 契约 / 质量校验 quality）
  plugindev/      Developer Plane 领域逻辑（scaffold / build / activate / status / explain）
  internalipc/    进程间通信（gRPC CaptureControl）
  config/         配置管理（rules.yaml）
  schema/         模式推断
  logging/        统一日志
plugins/          解码插件（独立 module：four-layer-demo / godot-gateway / godot-world）
scripts/          维护脚本（gen_tool_table 等）
examples/         示例流量源（HTTP 客户端 / 服务端，用于造流量）
docs/             设计文档（semantic-evidence-v1.md 为语义契约 SSOT；archive/ 存放过期设计稿）
```

## License

MIT
