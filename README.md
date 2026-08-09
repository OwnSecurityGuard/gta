# GTA — Game Traffic Analysis

面向游戏协议流量分析的 AI Agent 工具链。捕获网络数据包、通过插件解码游戏协议、实时聚合分析、并通过 MCP 协议暴露给 AI Agent，让 Agent 从真实流量中提取协议证据链并自动生成压测代码。

## 架构

```
AI Agent (Claude / DeepSeek / ...)
        │
        ▼  MCP (SSE / Streamable HTTP)
┌──────────────────────────────────────────┐
│  gta-mcp  (:8087)                        │
│  22+ MCP 工具：抓包控制 / 数据查询 /      │
│  插件管理 / 脚本引擎 / 操作分析           │
└──────────────┬───────────────────────────┘
               │ gRPC CaptureControl (:8088)
               ▼
┌──────────────────────────────────────────┐
│  gta-pipeline                            │
│  ┌──────────────────────────────────┐    │
│  │  capture.Source                  │    │
│  │  → decode.Dispatcher → 解码插件  │    │
│  │  → analyze.Engine (滑动窗口聚合)  │    │
│  │  → store.SQLiteStore (落库)      │    │
│  └──────────────────────────────────┘    │
│  PluginRegistry (:9091)                  │
└──────────────────────────────────────────┘
```

## 组件

| 组件 | 说明 |
|------|------|
| **gta-mcp** | MCP 服务器，提供 22+ 个工具供 AI Agent 调用 |
| **gta-pipeline** | 抓包 / 解码 / 聚合 / 落库一体进程 |
| **插件系统** | 独立进程解码器，gRPC 通信，支持热加载 |
| **脚本引擎** | Python 运行时，支持自定义分析脚本 |
| **聚合引擎** | 基于滑动窗口的实时指标聚合 |

## 快速开始

### 环境要求

- Go 1.25+
- libpcap / npcap（Windows 需安装 [Npcap](https://npcap.com/)）
- Python 3.x（脚本引擎）
- protoc + protoc-gen-go + protoc-gen-go-grpc（仅修改 proto 时需要）

### 构建

```bash
# 构建全部
make build

# 单独构建
make build-mcp       # gta-mcp.exe → bin/
make build-pipeline  # gta-pipeline.exe → bin/
```

### 运行

```bash
# 先启动 pipeline（数据面）
make run-pipeline

# 再启动 MCP 服务（控制面 + 查询面）
make run-mcp
```

MCP 服务启动后，在 AI Agent 客户端配置 MCP 连接 `http://localhost:8087/sse` 即可使用。

### 运行测试

```bash
make test
```

## 核心功能

### MCP 工具

| 类别 | 工具 | 功能 |
|------|------|------|
| 抓包控制 | `start_capture` / `stop_capture` / `get_capture_status` | 启动、停止、查询抓包 |
| | `list_interfaces` / `list_capture_sessions` | 网卡列表、会话列表 |
| 数据查询 | `list_decoded_data` / `list_raw_packets` | 解码事件与原始包查询（支持 expr 过滤） |
| | `aggregate_query` / `get_capture_schema` | 聚合指标查询、模式推断 |
| 操作分析 | `begin_capture_run` / `end_capture_run` | 标记操作窗口、获取摘要 |
| | `trace_protocol_flow` | 时序证据链（request/response/push/entity_diff） |
| 插件管理 | `list_plugins` / `get_plugin_manifest` | 插件发现与信息 |
| | `create_plugin` | 脚手架生成新插件项目 |
| 脚本引擎 | `save_script` / `run_script` | Python 脚本管理与执行 |
| 会话管理 | `list_sessions` / `delete_session` | 会话生命周期管理 |

### 插件系统

- **热加载**：插件作为独立进程，通过 gRPC 注册到 pipeline，自动发现和切换
- **崩溃隔离**：插件独立进程，崩溃不影响 pipeline
- **多项目隔离**：A 项目会话只认 A 插件，B 项目会话只认 B 插件
- **跨机器部署**：支持 TCP / Unix Socket / Windows Named Pipe

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
  gta-mcp/       MCP 服务器入口
  gta-pipeline/  Pipeline 进程入口
pkg/
  capture/       流量来源抽象（pcap-live / pcap-file / fake）
  decode/        解码调度层
  analyze/       实时聚合引擎（count / rate / sum）
  event/         核心数据模型（Packet / Event / Metric）
  store/         SQLite 持久化层
  plugin/        插件系统（注册 / 契约 / SDK / 脚手架）
  internalipc/   进程间通信（gRPC CaptureControl）
  config/        配置管理（rules.yaml）
  script/        Python 脚本引擎
  schema/        模式推断
  logging/       统一日志
examples/
  plugins/       插件示例（echo / http-sdk）
  http/          示例 HTTP 客户端/服务端
docs/            设计文档
workflows/       标准工作流定义
```

## License

MIT
