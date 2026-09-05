# GameTrace 团队协作改造设计

日期：2026-08-29
状态：**待批准**（批准前不写任何实现代码）
分支策略：从 `master` 切 `feat/team-collaboration`

---

## 1. 目标与非目标

### 目标

1. **一套共享服务端**：团队只部署一次 `gt-pipeline` + `gt-mcp`，成员不再各自部署。
2. **本地插件接入**：每个成员在自己机器上写插件、本地运行，直接接入共享服务端，无需把插件二进制或源码交给服务器。
3. **本地抓包接入**：成员能抓自己本机/手机的流量，数据汇入共享服务端统一落库与查询。
4. **多人不打架**：插件不因同名互相顶替；会话、数据按 owner 隔离；接入需要凭证。

### 非目标（明示不做）

- 不做完整账号体系 / RBAC / SSO（本轮采用「每用户 token + owner 隔离」）。
- 不做 Web UI 改造（沿用现有 MCP + 前端）。
- 不改事件模型与存储 schema 的语义（只加 `owner` 列）。
- 不实现插件的多机负载均衡（一个插件实例一对一服务一条隧道）。

---

## 2. 现状诊断：为什么今天只能是个人项目

按「阻塞程度」排序。这是改造清单的来源。

### P0 — 不做就根本无法给第二个人用

| # | 阻塞点 | 证据 | 后果 |
|---|---|---|---|
| B1 | **Decode 拨号方向反了**：pipeline 是 client，插件是 gRPC server | `manager.go:170` 注册时回拨 `req.SocketPath` 验证可达；SDK `registry.go:68` 起 server | 插件在本地、服务端在服务器 ⇒ 服务器必须能反向拨进成员电脑，NAT/办公网下不可行 |
| B2 | **插件名全局唯一，后注册者顶掉前者** | `manager.go:84` `byName map[name]string`，`:191-196` 关旧连接 | 两个人都写 `my-plugin` ⇒ 互相踢线，解码结果随机跳变 |
| B3 | **零鉴权** | 全部 `grpc.NewServer()` 无拦截器、`insecure.NewCredentials()`；`main.go:2703` CORS `*` | 任何人能连 :9091 注册插件、顶替他人、读全部会话 |
| B4 | **4 处 go.mod `replace` 指向本地绝对路径** | `go.mod:44`、`plugins/godot-gateway/go.mod:22`、`plugins/godot-world/go.mod:21`、`examples/http-decoder/go.mod:26` → `E:/ai_workspace/gta-plugin-sdk` | 换机器 `go build` 直接失败；**CI 已因此损坏**（`.github/workflows/ci.yml:22` 在 ubuntu 上跑） |
| B5 | **抓包 100% 发生在 pipeline 所在机器** | `capture_task.go:320` 在进程内 `openCaptureSources`（pcap-live / pcap-file / mobile） | 服务端集中后，成员抓不到自己本机的流量 |

### P1 — 能跑但体验撕裂、数据互串

| # | 阻塞点 | 证据 |
|---|---|---|
| B6 | 所有数据目录以**当前工作目录**为根，无 `GT_HOME` / `~/.gametrace` 概念 | `main.go:32` `-work-dir .`、`main.go:2332`；`sessions/`、`runs/`、`control.sqlite`、`current.json`、`logs/` 全落 CWD |
| B7 | **全局 `current.json`**：多客户端共享「当前会话」 | `main.go:145-147,182`；MCP 是 stateless HTTP，无 per-connection 状态 |
| B8 | 会话无 owner 字段，全员数据互相可见 | `SessionMeta`（`eventstore.go:162-184`）无 owner/tenant |
| B9 | 端口固定且无配置文件，一台机器无法跑两套实例 | `-control-addr :9888`、`-registry-addr :9091`、`-addr :8781`；`pkg/config` 只有 rules.yaml / proxy.json 两个单一用途 loader，无统一配置 |
| B10 | `pkg/config/proxy.go:50-51` `ServerAddr` 写死 `127.0.0.1:9090`，agent 必须与 pipeline 同机 | 手机/远程设备连不上 loopback |

### P2 — 分发与运维

| # | 阻塞点 | 证据 |
|---|---|---|
| B11 | Makefile 只输出 `.exe`，无 GOOS/GOARCH 矩阵、无 version ldflags | `Makefile:25,28,33,37` |
| B12 | 无 Dockerfile / docker-compose / systemd unit / `.env.example` | 根目录已确认不存在 |
| B13 | CI 无 release job、无产物上传 | `.github/workflows/ci.yml` 只有 build/vet/test |
| B14 | 日志双写 file+stderr，容器里噪音翻倍；`gt-plugin-dev` 完全没接日志 | `logging.go:97`；`cmd/gt-plugin-dev/main.go:21` 裸 `slog.Info` |

---

## 3. 目标架构

```
┌─────────────── 共享服务端（Docker Compose，一台机器）───────────────┐
│                                                                    │
│  ┌──────────────┐        ┌──────────────────────────────────────┐  │
│  │  gt-mcp     │ gRPC   │  gt-pipeline                        │  │
│  │  :8781       │───────▶│  ├─ CaptureControl   :9888           │  │
│  │  /mcp /sse   │        │  ├─ PluginRegistry   :9091           │  │
│  │  + Bearer 鉴权│        │  ├─ AgentIngest      :9092 (新)      │  │
│  └──────────────┘        │  ├─ capture_task (会话/落库)          │  │
│         ▲                │  └─ TunnelHub (新，插件反向隧道服务端) │  │
│         │ MCP over HTTP  └──────────────────────────────────────┘  │
│         │                ▲                    ▲                    │
│         │                │ 出向 gRPC          │ 出向 gRPC           │
│         │                │ (Register+Connect) │ (Push packets)     │
└─────────┼────────────────┼────────────────────┼────────────────────┘
          │                │                    │
    ┌─────┴──────┐   ┌─────┴────────────────────┴─────┐
    │ 成员的 AI  │   │ 成员本机 gt-agent（单二进制）  │
    │ 客户端/IDE │   │  ├─ pcap 抓本机网卡             │
    └────────────┘   │  ├─ 托管本地插件（隧道客户端）   │
                     │  └─ 一条出向长连接打通全部      │
                     └────────────────────────────────┘
```

**三条出向通道，全部由成员的机器主动发起**，服务端不反向拨号：

1. **插件隧道**（`PluginRegistry.Register` + `Connect`）— 解决 B1。
2. **抓包推流**（`AgentIngest.Push`）— 解决 B5。
3. **控制/查询**（MCP over HTTP + Bearer token）— 解决 B7/B8。

---

## 4. 关键设计决策

### D1 — 反向隧道：让 Dispatcher 零改动

**核心洞察**：`Dispatcher` 只依赖 `pb.DecoderClient` 接口（`dispatcher.go:63`）。只要隧道服务端产出一个实现了 `pb.DecoderClient` 的对象，`pkg/decode/` 完全不用动。

两端各做一个「流适配器」，隧道中间只传 `DecodeRequest` / `DecodeResponseV2` 的序列化字节：

```
服务端侧                                          插件侧
Dispatcher ──DecodeV2()──▶ tunnelClient            tunnelServer ──▶ 用户的 decodeFuncV2
              (实现 DecoderClient)                    (实现 Decoder_DecodeV2Server)
                    │                                        ▲
                    └── stream_id 复用 ── TunnelFrame ────────┘
                         (一条 gRPC bidi 流)
```

- **插件侧复用现成逻辑**：SDK 已有 `Decoder` 实现 `pb.DecoderServer`。隧道只需构造一个「假 server stream」（实现 `pb.Decoder_DecodeV2Server`，`Recv()` 从隧道帧队列读、`Send()` 写回隧道帧），直接调 `decoder.DecodeV2(fakeStream)`。**用户代码零改动**。
- **服务端侧**：`TunnelHub` 持有 `Connect` 流；`tunnelClient.DecodeV2()` 分配 `stream_id` 并注册响应队列。

**新增 proto**（放在 SDK `plugin.proto`，宿主与 SDK 共享）：

```proto
service PluginRegistry {
  rpc Register(RegisterRequest) returns (RegisterResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
  rpc Deregister(DeregisterRequest) returns (DeregisterResponse);
  rpc Connect(stream TunnelFrame) returns (stream TunnelFrame);  // 新增
}

message RegisterRequest {
  string socket_path = 1;
  bytes  manifest    = 2;
  bool   tunnel      = 3;   // 新增：true = 走反向隧道，跳过回拨
}

message TunnelFrame {
  uint32 stream_id = 1;
  oneof payload {
    bytes request    = 2;  // server→plugin: marshaled DecodeRequest
    bool  half_close = 3;  // server→plugin: CloseSend()
    bytes response   = 4;  // plugin→server: marshaled DecodeResponseV2
    StreamEnd end    = 5;  // plugin→server: 流结束（error 空 = 正常 EOF）
    StreamReset reset= 6;  // server→plugin: 取消流
  }
}
message StreamEnd   { string error = 1; }
message StreamReset { string reason = 1; }
```

- 流的开启是隐式的：某 `stream_id` 的第一个 `request` 帧即开流（不再单独定义 Open 帧，YAGNI）。
- **向后兼容**：`tunnel=false`（缺省）时走原有回拨路径，本地插件行为完全不变。
- 隧道断线 ⇒ 该插件标记 offline（`emit(PluginEventOffline)`），重连后重新 `Register`。

### D2 — owner 隔离与鉴权

- 新增 `pkg/auth`：
  - 凭证源：`GT_AUTH_TOKENS` 环境变量或 `auth.tokens` 配置段，形如 `alice=gt_xxxx,bob=gt_yyyy`。启动时加载进内存 map。
  - gRPC：`UnaryInterceptor` + `StreamInterceptor` 从 metadata `authorization: Bearer <token>` 解析 owner，注入 `context`。
  - MCP HTTP：middleware 从 `Authorization` header 解析 owner；同时收紧 CORS（去掉 `*`）。
- **插件键改为 `owner/name`**（`manager.go:84` 的 `byName`）—— 彻底解决 B2。同名插件按 owner 并存，各自路由。
  - 兼容：`FindByName("name")` 在单 owner 语境下退化为 `FindByName("<caller-owner>/name")`。
- **会话**：`SessionMeta` 加 `Owner` 字段（`Extra` 之外的一等公民，落到 `sessions` 表），查询默认按 owner 过滤；带 `admin` 标记的用户可看全部。
- **`current.json` 按 owner 分片**：`current.<owner>.json`（B7）。

### D3 — 本地 agent

新增 `cmd/gt-agent`，单二进制，成员本机一键启动。承担两件事：

1. **抓包推流**（解决 B5）
   - 复用 `pkg/capture/pcaplive` 的抓包逻辑（agent 在宿主仓库内编译，可带 `-tags pcap`）。
   - 新 source `agent`：pipeline 侧起 `AgentIngest` gRPC server（默认 `:9092`），agent 批量推送原始包。
   - **保留完整帧与 link_type**（不像 mobile source 那样退化成 `LinkTypeProxyPayload`），使插件在本地开发与在团队服务端跑的行为完全一致。

   ```proto
   service AgentIngest {
     rpc Push(stream PacketBatch) returns (PushAck);
   }
   message PacketBatch {
     string session_id = 1;
     string iface = 2;
     repeated RawPacket packets = 3;
   }
   message RawPacket {
     string id = 1;              // agent 侧生成（UUIDv7）
     int64  timestamp_ns = 2;
     uint32 link_type = 3;
     bytes  raw = 4;
     string src = 5; string dst = 6; string protocol = 7;
     map<string, string> metadata = 8;
   }
   ```

2. **托管本地插件**：agent 启动时自动发现并拉起本机插件进程，为每个插件建立隧道连接。这样成员只需 `gt-agent --token gt_xxx --server host:9091`，插件自动接入。

- agent 侧 BPF 过滤 + 批量发送，控制上行带宽。
- 断线指数退避重连（与 SDK `RunRegisterLoop` 同一套策略）。

### D4 — 配置与分发

- `pkg/config` 引入统一 `Config`：`Load(path)` 读 `gametrace.yaml`；每个字段支持 `GT_*` 环境变量兜底；优先级 **flag > 环境变量 > 配置文件 > 默认值**。
- 默认 workdir 改为 `GT_HOME`，缺省 `~/.gametrace`（B6）。
- 端口支持 `:0` 动态分配并回写地址文件，允许同机跑多套（B9）。
- `pkg/config/proxy.go:50-51` 的 `ServerAddr` 改为可配置（B10）。
- **去掉 4 处绝对路径 replace**（B4）：改用 `require gta-plugin-sdk v0.4.0`；本地联调换 `go.work`（提供 `go.work.example`，且 **必须进 .gitignore**，避免再次污染 CI）。

---

## 5. 改造清单

### P0 — 必做（否则团队用不起来）

| ID | 任务 | 主要文件 |
|---|---|---|
| T1 | SDK：`plugin.proto` 加 `Connect` + `TunnelFrame` + `RegisterRequest.tunnel`；重新生成 pb | SDK `proto/plugin.proto` |
| T2 | SDK：`RunRegisterLoop` 支持隧道模式（假 server stream 适配器），需 token metadata | SDK `registry.go` |
| T3 | 宿主：`pkg/plugin/tunnel.go` — `TunnelHub` + `tunnelClient`（实现 `pb.DecoderClient`） | 新增 |
| T4 | 宿主：`manager.go` 注册分支支持 `tunnel=true`（跳过回拨）；`byName` 键改 `owner/name` | `pkg/plugin/manager.go` |
| T5 | `pkg/auth`：token 解析 + gRPC 拦截器 + MCP HTTP middleware | 新增 |
| T6 | 去掉 4 处绝对路径 `replace`，加 `go.work.example`，修 CI | `go.mod`、`plugins/*/go.mod`、`.github/` |
| T7 | `cmd/gt-agent`：抓包 + 推流 + 托管插件 | 新增 |
| T8 | 宿主：`AgentIngest` 服务 + `pkg/capture/agent` source | `pkg/capture/mobile/proto` 同级新增 |
| T9 | `SessionMeta.Owner` + 会话按 owner 过滤 + `current.<owner>.json` | `pkg/store/eventstore.go`、`cmd/gt-mcp/main.go` |

### P1 — 体验与正确性

| ID | 任务 | 主要文件 |
|---|---|---|
| T10 | 统一配置：`pkg/config.Config` + `gametrace.yaml` + `GT_*` 环境变量 + `GT_HOME` 默认 workdir | `pkg/config`、各 `main.go` |
| T11 | `pkg/config/proxy.go` 的 `ServerAddr` 可配置 | `pkg/config/proxy.go:50-51` |
| T12 | 收紧 CORS，移除 `*` | `cmd/gt-mcp/main.go:2703` |
| T13 | MCP：`list_registered_plugins` / `set_session_plugin` 等按 owner 过滤；`start_capture` 支持 `source=agent` | `cmd/gt-mcp/main.go` |

### P2 — 分发与运维

| ID | 任务 | 主要文件 |
|---|---|---|
| T14 | Makefile GOOS/GOARCH 矩阵 + version ldflags | `Makefile` |
| T15 | Dockerfile（多阶段，含 Npcap 变体）+ docker-compose.yml + `.env.example` | 新增 |
| T16 | CI release job + 产物上传 | `.github/workflows/` |
| T17 | `gt-plugin-dev` 接入 `pkg/logging`；日志双写改为可配置 | `cmd/gt-plugin-dev/main.go`、`pkg/logging` |
| T18 | 文档：团队部署指南、成员上手指南（1 页）、README 端口与架构图修正 | `docs/`、`README.md` |

---

## 6. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| 隧道背压：单条 bidi 流承载多逻辑流，慢插件阻塞他人 | 解码延迟 | 每插件独立隧道；`stream_id` 队列有界 + 超时；未来可升级为多物理流 |
| agent 抓包需要管理员权限（Npcap） | 成员上手成本 | 文档明示；提供「仅代理模式」（复用现有 mobile source，无需提权）作为降级 |
| 本地抓包推流占用上行带宽 | 团队网络 | agent 侧 BPF 端口过滤 + 批量 + 可选采样 |
| `owner/name` 改动影响现有 `FindByName` 调用方 | 回归 | 保留单 owner 退化语义；`capture_task.go:453-466` 的解析顺序不变 |
| SDK 与宿主需同步发布（proto 变更） | 版本错配 | SDK 打新 tag 后，宿主 `go.mod` 升版本；`pkg/plugindev/version.go` 的 `SDKVersion` 三者同步 |

---

## 7. 验收标准

1. 一台全新 Linux 机器上 `docker compose up -d` 后，服务端就绪；**不需要任何 Go 环境**。
2. 成员 A 在本机 `gt-agent --token <A> --server <host>` 后：抓本机流量、本地插件自动注册进服务端注册表、服务端能看到 `alice/<plugin>`。
3. 成员 B 同时接入，也写同名 `<plugin>`：**两者并存不互相顶替**，各自解码各自会话。
4. 无 token 或错误 token 的连接被拒绝（gRPC 与 MCP 两侧均验证）。
5. 成员 A 在 MCP 客户端查不到成员 B 的会话（admin 除外）。
6. 断掉成员 A 的网络再恢复，插件与 agent 自动重连，会话数据不丢（重连期间的包允许丢失，明确记录）。
7. `git clone` 到全新机器后 `go build ./...` 与 `go test ./...` 通过（B4 已消除）。
8. 现有本地单机用法（`tunnel=false`）**行为完全不变** —— 这是回归底线。
