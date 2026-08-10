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
│  37 个 MCP 工具，纯转发：零 exec / 零文件写入 / 零归因逻辑   │
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
| **gta-mcp** | MCP 服务器，37 个工具，纯协议适配 + 路由 |
| **gta-pipeline** | Runtime Plane：抓包 / 解码 / 聚合 / 落库 / 插件注册表一体进程 |
| **gta-plugin-dev** | Developer Plane：插件脚手架 / 编译 / 拉起 / 状态 / 失败归因。**默认由 gta-mcp 内嵌，无需单独启动** |
| **插件系统** | 独立进程解码器，gRPC 通信，支持热加载与崩溃隔离 |
| **脚本引擎** | Python 运行时，支持自定义分析脚本 |
| **聚合引擎** | 基于滑动窗口的实时指标聚合 |

插件 SDK 已拆为独立仓库 `github.com/OwnSecurityGuard/gta-plugin-sdk`，插件工程只依赖 SDK，与 gta 源码零耦合。

## 快速开始

### 环境要求

- Go 1.25+
- libpcap / npcap（Windows 需安装 [Npcap](https://npcap.com/)）
- Python 3.x（脚本引擎）
- protoc + protoc-gen-go + protoc-gen-go-grpc（仅修改 proto 时需要）

### 构建

```bash
make build              # gta-mcp + gta-pipeline + gta-plugin-dev → bin/

make build-mcp          # 单独构建
make build-pipeline
make build-plugin-dev
```

插件是独立 module，单独构建：

```bash
cd plugins/http && go build -o http-plugin.exe .
```

### 运行

启动顺序：**pipeline → mcp → 插件**（插件可后启，注册表支持热注册）。

```bash
# 终端 1 — Runtime Plane（数据面，先启）
./bin/gta-pipeline.exe -workdir . -log-format text

# 终端 2 — 控制面（内嵌 Developer Plane）
./bin/gta-mcp.exe -work-dir . -plugins-dir plugins -log-format text

# 终端 3 — 解码插件（按需）
cd plugins/http && GTA_REGISTRY_ADDR=127.0.0.1:9091 ./http-plugin.exe
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
| `-python` | `python` | Python 解释器路径 |
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

### MCP 工具（37 个，其中 2 个原始包调试工具默认不注册）

| 类别 | 工具 | 功能 |
|------|------|------|
| 抓包控制 | `start_capture` / `stop_capture` / `get_capture_status` | 启动、停止、查询抓包 |
| | `list_interfaces` / `list_capture_sessions` / `set_session_plugin` | 网卡列表、会话列表、会话换绑插件 |
| 数据查询 | `list_decoded_data` / `list_state_changes` | 解码事件与状态变更（支持 expr 过滤） |
| | `aggregate_query` / `get_capture_schema` | 聚合指标查询、模式推断 |
| | `trace_protocol_flow` | 时序证据链（request/response/push/entity_diff） |
| 原始包调试 | `list_raw_packets` / `decode_raw_packets` | 需 `-enable-raw-debug`，默认关闭 |
| 操作分析 | `begin_capture_run` / `end_capture_run` / `get_capture_run_status` | 标记操作窗口、获取摘要 |
| 插件开发<br>(Developer Plane) | `create_plugin` / `build_plugin` | 脚手架生成、编译（失败返回 file:line:col 诊断） |
| | `activate_plugin` / `deactivate_plugin` | 拉起 / 停止本地插件进程 |
| | `status_plugin` / `explain_plugin` | 双状态视图、失败归因（带 SDK rule_id） |
| 插件验证<br>(Runtime Plane) | `test_plugin` / `verify_plugin` | 冒烟解码、契约违规 + 语料质量 + verdict |
| | `sample_bytes_plugin` | 受限取样（硬上限 20 包 / 64 字节，全量审计） |
| 插件运行时 | `list_plugins` / `list_registered_plugins` | 目录扫描 vs 注册表在线实例 |
| | `get_plugin_manifest` / `deregister_plugin` | 读取 manifest、强制下线 |
| 插件知识 | `get_plugin_contract` / `get_plugin_dev_guide` | 契约 SSOT、开发指南 |
| 脚本引擎 | `save_script` / `run_script` / `list_scripts` / `delete_script` | Python 脚本管理与执行 |
| 会话管理 | `list_sessions` / `delete_session` | 会话生命周期管理 |

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
  analyze/        实时聚合引擎（count / rate / sum）
  event/          核心数据模型（Packet / Event / Metric）
  store/          SQLite 持久化层（会话库 + control.sqlite + 调试审计）
  plugin/         插件运行时（注册表 / 契约 / 质量校验 quality）
  plugindev/      Developer Plane 领域逻辑（scaffold / build / activate / status / explain）
  internalipc/    进程间通信（gRPC CaptureControl）
  config/         配置管理（rules.yaml）
  script/         Python 脚本引擎
  schema/         模式推断
  logging/        统一日志
plugins/
  http/           HTTP 解码插件（独立 module）
examples/
  http/           示例 HTTP 客户端 / 服务端，用于造流量
docs/             设计文档（plugin-domain-design.md 为插件域权威设计）
workflows/        标准工作流定义
```

## License

MIT
