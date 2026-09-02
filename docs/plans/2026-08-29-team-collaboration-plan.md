# GTA 团队协作改造 · 实施计划

日期：2026-08-29
设计来源：`docs/plans/2026-08-29-team-collaboration-design.md`（已批准）
范围：**P0 + P1 + P2 全部**
分支：从 `master` 切 `feat/team-collaboration`

## 执行约定

- **TDD 强制**：每个任务先写失败测试 → 看它红 → 实现 → 看它绿 → 提交。
- 每任务一个 commit，信息格式 `feat(team): <简述>`。
- 两个仓库：`E:\gta`（宿主）、`E:\ai_workspace\gta-plugin-sdk`（SDK）。
  SDK 改动必须先落地并被宿主 `replace` 到，宿主才能联调。
- **proto 只准在 SDK 侧生成一份**（`make proto` in SDK）。两侧各生成会在 protobuf
  全局注册表撞同一文件路径并 panic。
- 回归底线：**现有本地单机用法（`tunnel=false`、无 token 时走默认）行为完全不变**。
  每个阶段结束必须跑 `go build -tags pcap ./... && go test -tags pcap ./...`。

## 任务依赖图

```
A1(去replace/修CI) ──┬── B1(SDK proto) ── B2(宿主 TunnelHub) ── B3(SDK 隧道模式) ── B4(manager 分支+owner键)
                     │
A2(pkg/auth) ────────┼── B4
                     ├── D1(SessionMeta.Owner)
                     └── E1..E4 (P1)
C1(AgentIngest proto) ── C2(capture/agent source) ── C3(cmd/gta-agent)
B1 ──────────────────────────────────────────────────────────────────── C3
F1..F5 (P2) 依赖上述全部
```

---

## Phase A — 地基

### A1 · 去掉绝对路径 replace，启用 go.work 本地联调，修复 CI

- **文件**：`go.mod`、`plugins/godot-gateway/go.mod`、`plugins/godot-world/go.mod`、
  `examples/http-decoder/go.mod`、`go.work.example`（新增）、`.gitignore`、
  `.github/workflows/ci.yml`
- **TDD**：
  1. 红：在 CI 配置里加一步 `go build ./...`（ubuntu），本地模拟 `GOFLAGS=` 干净环境
     `go build ./...` → 当前必失败（找不到 `E:/ai_workspace/gta-plugin-sdk`）。
  2. 实现：4 处 `replace` 全部删除，改为 `require github.com/OwnSecurityGuard/gta-plugin-sdk v0.4.0`。
     新增 `go.work.example`（内容：`go 1.25` + `use .` + `use ../ai_workspace/gta-plugin-sdk`），
     **必须加入 `.gitignore`**，README 说明本地联调时 `cp go.work.example go.work`。
  3. 绿：干净环境（无 go.work）`go build ./...` 通过；`cp go.work.example go.work` 后同样通过。
- **验收**：`go build ./...` 与 `go test ./...` 在无本地 SDK 路径的机器上通过。
- **commit**：`feat(team): drop absolute-path SDK replace, add go.work.example`

### A2 · `pkg/auth`：token → owner 映射 + gRPC 拦截器 + MCP HTTP middleware

- **文件**：`pkg/auth/token.go`（新增）、`pkg/auth/interceptor.go`（新增）、
  `pkg/auth/http.go`（新增）、`pkg/auth/auth_test.go`（新增）
- **设计**：
  - `type Principal struct { Owner string; IsAdmin bool }`
  - `type Resolver interface { Resolve(token string) (*Principal, bool) }`
  - `StaticResolver(map[string]Principal)` — 从 `GTA_AUTH_TOKENS="alice=gta_aaa,bob=gta_bbb"`
    或配置文件的 `auth.tokens` 段加载。
  - `GTA_AUTH_TOKENS` 为空 ⇒ **匿名模式**：`Principal{Owner: "local"}`，且
    `Required=false`（放行），保证回归底线。
  - gRPC：`UnaryInterceptor` / `StreamInterceptor` 从 metadata
    `authorization: Bearer <token>` 提取；`ctx` 注入 `Principal`。
  - HTTP：`Middleware(next http.Handler)` 从 `Authorization` header 提取。
  - 辅助：`OwnerFrom(ctx) string`。
- **TDD**：
  1. 红：测试「空 token + required=true → 拒绝」「正确 token → 解析出 owner」
     「错误 token → 拒绝」「匿名模式放行且 owner=local」。
  2. 实现。
  3. 绿。
- **验收**：4 组用例全绿；未接线进 main.go（接线在 B4/E4）。
- **commit**：`feat(team): add pkg/auth with token resolver and interceptors`

---

## Phase B — 反向隧道（核心）

### B1 · SDK：proto 增加 `Connect` 与隧道帧

- **文件（SDK）**：`proto/plugin.proto` → `make proto` 重新生成
- **改动**：
  ```proto
  service PluginRegistry {
    rpc Register(RegisterRequest) returns (RegisterResponse);
    rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
    rpc Deregister(DeregisterRequest) returns (DeregisterResponse);
    rpc Connect(stream TunnelFrame) returns (stream TunnelFrame);
  }
  message RegisterRequest {
    string socket_path = 1;
    bytes  manifest    = 2;
    bool   tunnel      = 3;   // true = 走反向隧道，服务端跳过回拨
  }
  message TunnelFrame {
    uint32 stream_id = 1;
    oneof payload {
      bytes request     = 2;  // server→plugin: marshaled DecodeRequest
      bool  half_close  = 3;  // server→plugin: CloseSend()
      bytes response    = 4;  // plugin→server: marshaled DecodeResponseV2
      StreamEnd end     = 5;  // plugin→server: 流结束（error 空 = 正常 EOF）
      StreamReset reset = 6;  // server→plugin: 取消流
    }
  }
  message StreamEnd   { string error = 1; }
  message StreamReset { string reason = 1; }
  ```
- **TDD**：
  1. 红：`proto_test.go` 断言 `(*pb.RegisterRequest).GetTunnel()` 存在、
     `pb.TunnelFrame` 的 oneof 六个分支可构造、round-trip marshal/unmarshal 保真。
  2. 实现（改 proto + `make proto`）。
  3. 绿。
- **验收**：`make proto` 无警告；`go build ./...` 通过；`go test ./...` 通过。
- **commit**（SDK）：`feat(tunnel): add PluginRegistry.Connect and TunnelFrame to contract`

### B2 · 宿主：`pkg/plugin/tunnel.go` — TunnelHub + tunnelClient

- **文件**：`pkg/plugin/tunnel.go`（新增）、`pkg/plugin/tunnel_test.go`（新增）
- **设计**：
  - `type TunnelHub struct` 持有 `Connect` 的两端：
    - 服务端侧：实现 `pb.PluginRegistryServer` 的 `Connect` 方法（由 `RegistryServer` 转发）。
    - 提供 `DecoderClient(instanceID) (pb.DecoderClient, bool)`。
  - `tunnelClient` 实现 `pb.DecoderClient`：
    - `DecodeV2(ctx, opts...) (pb.Decoder_DecodeV2Client, error)`：分配 `streamID`，
      注册 `chan *pb.DecodeResponseV2`（有界，如 64），返回 `tunnelStream`。
    - `tunnelStream` 实现 `pb.Decoder_DecodeV2Client`：
      `Send(*DecodeRequest)` → 封装成 `TunnelFrame{Request: marshaled}` 写入出向队列；
      `Recv()` → 从本 streamID 的响应 chan 读；`CloseSend()` → 发 `HalfClose` 帧；
      另有 gRPC 标准方法 `Header/Trailer/Context/SendMsg/RecvMsg`。
  - 读写两个 goroutine 分别 `Recv()`/`Send()` 底层 `Connect` 流，避免死锁。
  - 流结束（`end` 帧）→ 关闭响应 chan 并回收 streamID。
  - **背压**：响应 chan 满时记录 warn 并丢弃最旧（或阻塞+超时），明确取舍。
- **TDD**：
  1. 红：`tunnel_test.go` 用 `bufconn` 或内存 pipe 建一对 `Connect` 流，
     断言「单流往返」「多 streamID 并发复用不串」「HalfClose 后 Recv 返回 io.EOF」
     「流结束帧后 Recv 返回 io.EOF」「底层流断开后所有上层 Recv 立即返回错误」。
  2. 实现。
  3. 绿。
- **验收**：5 组用例全绿，含并发 `-race`。
- **commit**：`feat(team): add reverse tunnel hub implementing pb.DecoderClient`

### B3 · SDK：`RunRegisterLoop` 隧道模式

- **文件（SDK）**：`registry.go`、`tunnel.go`（新增）、`tunnel_test.go`（新增）
- **设计**：
  - 新增 `GTA_REGISTRY_TUNNEL=1`（或 `RunTunnelRegisterLoop` 独立入口）启用。
  - 隧道模式下**不起** Decode gRPC server，注册时 `socket_path` 置空、`tunnel=true`。
  - 注册成功后在同一条 `*grpc.ClientConn` 上调用 `Connect`，开双向流。
  - 收到 `request` 帧 → 反序列化 `DecodeRequest` → 交给 `tunnelStreamServer`
    （实现 `pb.Decoder_DecodeV2Server`：`Recv()` 从请求队列读、`Send()` 写响应帧）→
    直接调 `decoder.DecodeV2(fakeStream)`。**用户 `decodeFuncV2` 零改动。**
  - `Recv()` 返回 `io.EOF` 当收到 `half_close` 帧；`Send()` 把响应 marshal 成帧写回。
  - 流断开 → 指数退避重连（复用现有退避逻辑）。
- **TDD**：
  1. 红：`tunnel_test.go` 用一个假 `Connect` 流，断言
     「收到 request 帧 → 调用一次 decodeFuncV2」「用户 Send 的响应变成 response 帧回传」
     「收到 half_close → 用户的 Recv 得到 io.EOF」「done=true 的响应帧正确回传」。
  2. 实现。
  3. 绿。
- **验收**：4 组用例全绿；`examples/http-stream-decoder` 切隧道模式仍能编译。
- **commit**（SDK）：`feat(tunnel): support reverse-tunnel registration mode`

### B4 · 宿主：`RegistryServer.Register` 分支 + `owner/name` 键

- **文件**：`pkg/plugin/manager.go`、`pkg/plugin/manager_test.go`（扩展）
- **改动**：
  1. `Register` 增加 owner 解析：从 `ctx` 取 `Principal`（A2），owner 为空时回退 `"local"`。
  2. `req.Tunnel == true` ⇒ **跳过** `dialDecoder` 回拨；`RegisteredPlugin.SocketPath` 记
     `"tunnel:<instance_id>"`；等待 `Connect` 到达后才 `Online=true`。
  3. `byName` 的键改为 `Owner + "/" + m.Name`。新增 `findKey(owner, name) string`。
  4. `FindByName(name)` 语义：内部按 `owner/name` 查；为兼容现有调用方
     （`capture_task.go:453-466`、`main.go:722`），提供 `FindByNameFor(owner, name)`，
     并让 `FindByName(name)` 在**只有一个 owner 匹配该 name 时**退化命中（避免破坏现有行为）。
  5. `Find(protocolHint)` 按 owner 过滤：只匹配调用方 owner 的插件（owner 为空则全匹配）。
  6. `PluginSummary` 增加 `Owner` 字段。
  7. gRPC server 装配 A2 的拦截器。
- **TDD**：
  1. 红：测试「alice 与 bob 注册同名插件 → 两者并存且各自 FindByNameFor 命中自己的」
     「tunnel=true 时不回拨也能注册成功」「tunnel=false 仍走回拨（现有测试保持绿）」
     「owner 不同时 Find 不串」。
  2. 实现。
  3. 绿。
- **验收**：新增 4 组 + **现有 manager_test.go 全部保持绿**（回归底线）。
- **commit**：`feat(team): namespace plugins by owner and accept tunnel registration`

---

## Phase C — 本地 agent 抓包链路

### C1 · `AgentIngest` proto

- **文件**：`pkg/capture/agent/proto/agent.proto`（新增）+ 生成；`Makefile` 加 proto target
- **改动**：见设计文档 §D3 的 `AgentIngest` / `PacketBatch` / `RawPacket` 定义。
  批量发送（`repeated RawPacket`）降低 gRPC 往返开销。
- **TDD**：
  1. 红：`agent_proto_test.go` 断言 `PacketBatch` round-trip 保真、空批次可 marshal。
  2. 实现（`make proto`）。
  3. 绿。
- **commit**：`feat(team): add AgentIngest proto for remote packet push`

### C2 · `pkg/capture/agent` source

- **文件**：`pkg/capture/agent/source.go`（新增）、`config.go`、`source_test.go`（新增）
- **设计**：仿照 `pkg/capture/mobile/source.go` 的结构（`base.Lifecycle` +
  `base.StatTracker` + `Register(name, FactoryFunc{})`），但：
  - `Push(stream AgentIngest_PushServer)` 接收 `PacketBatch`。
  - **保留完整帧与 `link_type`**（不退化成 `LinkTypeProxyPayload`），
    直接映射为 `event.Packet{ID, Timestamp, Raw, LinkType, Src, Dst, Protocol, Metadata}`。
  - `Metadata` 注入 `capture.MetaSource="agent"`、`iface`、`agent_id`（owner）。
  - 背压：`out` chan 满时 `AddBlocked()` 并等待（与 mobile source 一致）。
- **TDD**：
  1. 红：测试「收到一个含 3 包的批次 → Packets() 产出 3 个 event.Packet，link_type 与 raw 保真」
     「metadata 含 source=agent」「ctx 取消后 Close 立即返回」。
  2. 实现。
  3. 绿。
- **commit**：`feat(team): add agent capture source preserving full frames`

### C3 · `cmd/gta-agent`

- **文件**：`cmd/gta-agent/main.go`（新增）、`cmd/gta-agent/plugin_host.go`（新增）
- **职责**：成员本机单二进制。
  1. 读配置：`--server`、`--token`、`--iface`、`--port`（BPF 过滤）、`--plugin-dir`。
  2. **抓包推流**：复用 `pkg/capture/pcaplive` 抓本机网卡 → 攒批 → `AgentIngest.Push`。
  3. **托管插件**：扫描 `--plugin-dir` 下的插件可执行文件，逐个 spawn，
     为每个插件注入 `GTA_REGISTRY_ADDR=<server:9091>`、`GTA_REGISTRY_TUNNEL=1`、
     `GTA_AUTH_TOKEN=<token>`；插件退出按退避重启。
  4. 断线指数退避重连（首次 1s，上限 30s）。
- **TDD**：
  1. 红：`plugin_host_test.go` 断言「扫描目录得到插件清单」
     「spawn 的环境变量包含隧道与 token」；`main_test.go` 断言「配置解析的默认值与 flag 覆盖」。
  2. 实现。
  3. 绿。
- **验收**：`go build -tags pcap ./cmd/gta-agent` 成功；冒烟：连不上服务端时不崩溃、持续重试。
- **commit**：`feat(team): add gta-agent for local capture push and plugin hosting`

---

## Phase D — owner 落库

### D1 · `SessionMeta.Owner` + 按 owner 过滤 + `current.<owner>.json`

- **文件**：`pkg/store/eventstore.go`、`pkg/store/session_store.go`、
  `cmd/gta-pipeline/pipeline_service.go`、`cmd/gta-mcp/main.go`
- **改动**：
  1. `SessionMeta` 加 `Owner string`（`json:"owner"`），`sessions` 表加 `owner` 列（含迁移）。
  2. `StartSession` 从 ctx 取 owner 写入；`ListSessions` 支持按 owner 过滤（空 = 全部）。
  3. `current.json` → `current.<owner>.json`（`main.go:145-147,182`），
     单 owner 场景退化为 `current.local.json`。
- **TDD**：
  1. 红：测试「两个 owner 各起一个 session → 按 owner 过滤各自只见自己的」
     「current 文件按 owner 分片，A 的当前会话不被 B 覆盖」。
  2. 实现。
  3. 绿。
- **commit**：`feat(team): persist session owner and shard current-session state`

---

## Phase E — P1 体验与正确性

### E1 · 统一配置：`pkg/config.Config` + `gta.yaml` + `GTA_*` + `GTA_HOME`

- **文件**：`pkg/config/config.go`（新增）、`pkg/config/config_test.go`（新增）、
  `cmd/gta-mcp/main.go`、`cmd/gta-pipeline/main.go`、`cmd/gta-agent/main.go`
- **设计**：
  - `Config` 字段：`Server{ControlAddr, RegistryAddr, MCPAddr, AgentAddr}`、
    `WorkDir`、`Auth{Tokens}`、`Log{Level, Format, File}`。
  - `Load(path)` 读 yaml；每字段可被 `GTA_<SECTION>_<FIELD>` 环境变量覆盖；
    优先级 **flag > 环境变量 > 配置文件 > 默认值**。
  - `WorkDir` 默认：环境变量 `GTA_HOME` → `os.UserHomeDir()/.gta` → `.`（保持现状兜底）。
  - 端口支持 `:0`；实际监听后把地址写入 `<workdir>/run/addr.json`。
- **TDD**：
  1. 红：测试「配置文件值被环境变量覆盖」「环境变量被 flag 覆盖」
     「未设任何值时 WorkDir 回落到 ~/.gta」「坏 yaml 返回明确错误而非 panic」。
  2. 实现并接线到三个 main。
  3. 绿。
- **commit**：`feat(team): unify configuration with gta.yaml, GTA_* env and GTA_HOME`

### E2 · `pkg/config/proxy.go` 的 `ServerAddr` 可配置

- **文件**：`pkg/config/proxy.go:50-51`
- **改动**：`ListenAddr` / `ServerAddr` 默认值改为从 `Config` 取；
  `ServerAddr` 缺省仍为 `127.0.0.1:9090`（保持现状），但可被 `GTA_PROXY_SERVER_ADDR` 覆盖。
- **TDD**：环境变量覆盖生效；不设时行为不变。
- **commit**：`fix(team): make proxy server address configurable`

### E3 · 收紧 CORS

- **文件**：`cmd/gta-mcp/main.go:2703`
- **改动**：`Access-Control-Allow-Origin: *` → 由 `GTA_MCP_CORS_ORIGINS` 配置，
  默认**同源**（不输出该 header）；`Allow-Headers` 显式包含 `Authorization`。
- **TDD**：测试「未配置时不输出 ACAO」「配置了白名单时命中才输出」。
- **commit**：`fix(team): restrict CORS to configured origins`

### E4 · MCP 按 owner 过滤 + `start_capture` 支持 `source=agent`

- **文件**：`cmd/gta-mcp/main.go`
- **改动**：
  1. HTTP server 套上 A2 的 middleware；owner 注入请求 `ctx`。
  2. `list_registered_plugins`（`:657`）、`list_plugins`（`:617`）、
     会话列表类工具按 owner 过滤；admin 可看全部。
  3. `start_capture`（`:2375`）新增 `source=agent`：走 C2 的 agent source，
     并接受 `agent_id` 参数指定由哪个成员的 agent 供数。
  - `set_session_plugin`（`:722`）的插件查找改用 `FindByNameFor(owner, name)`。
- **TDD**：
  1. 红：测试「alice 的 list_registered_plugins 看不到 bob 的插件」
     「start_capture source=agent 能建会话」。
  2. 实现。
  3. 绿。
- **commit**：`feat(team): filter MCP tools by owner and add agent capture source`

---

## Phase F — P2 分发与运维

### F1 · Makefile 跨平台矩阵 + version ldflags

- **文件**：`Makefile`
- **改动**：新增 `PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64`；
  `make dist` 输出到 `dist/<os>-<arch>/`（Windows 加 `.exe`）；
  version 通过 `-ldflags "-X main.version=$(VERSION)"` 注入；
  **新增 `build-agent` 产出 `gta-agent`**（成员要拿它去本机跑）。
- **验收**：`make dist` 在 Windows 上产出 5 个平台的二进制。
- **commit**：`build: add cross-platform dist matrix and version ldflags`

### F2 · Docker + compose + `.env.example`

- **文件**：`Dockerfile`（新增，多阶段）、`docker-compose.yml`（新增）、
  `.env.example`（新增）、`.dockerignore`（新增）
- **设计**：
  - Dockerfile：golang 构建（含 `-tags pcap`，Linux 用 libpcap-dev）→ distroless/alpine 运行。
  - compose 两个 service：`gta-pipeline`（9888/9091/9092）、`gta-mcp`（8781），
    共享 volume 存放 `sessions/`、`runs/`、`control.sqlite`；环境变量全部从 `.env` 注入。
  - `.env.example` 含 `GTA_AUTH_TOKENS=alice=gta_xxx,bob=gta_yyy` 示例。
- **验收**：`docker compose up -d` 后 `:8781` 与 `:9091` 可连通（本地无 Docker 时至少通过配置静态检查）。
- **commit**：`build: add Dockerfile, compose stack and .env.example`

### F3 · CI release job

- **文件**：`.github/workflows/ci.yml`（扩展）或新增 `release.yml`
- **改动**：tag 推送时触发 `make dist` 并上传产物；修好已被 A1 破坏的 build job。
- **commit**：`ci: add release job and fix build on clean checkout`

### F4 · 日志治理

- **文件**：`pkg/logging/logging.go`、`cmd/gta-plugin-dev/main.go`
- **改动**：文件+stderr 双写改为可配置（`GTA_LOG_STDERR`，容器默认关）；
  `MaxBackups <= 0` 才补默认（修 `:70-72` 的 `< 0` 判断 bug）；
  `gta-plugin-dev` 接入 `pkg/logging`。
- **commit**：`fix: make log dual-write configurable and wire plugin-dev logging`

### F5 · 文档

- **文件**：`README.md`、`docs/team-deployment.md`（新增）、`docs/team-member-onboarding.md`（新增）
- **改动**：修正 README 过时端口（8087/8088 → 8781/9888）与架构图；
  新增「服务端 10 分钟部署」「成员 3 步上手」两份文档。
- **commit**：`docs: add team deployment and onboarding guides, fix stale ports`

---

## 阶段验收

| 阶段 | 验收命令 | 通过标准 |
|---|---|---|
| A | `go build -tags pcap ./... && go test -tags pcap ./...` | 干净环境可构建；auth 用例全绿 |
| B | 同上 + SDK `go test ./...` | 隧道两端 9 组用例全绿；**现有 manager_test 全绿** |
| C | 同上 | agent 能连上服务端并推包；插件被托管后出现在注册表 |
| D | 同上 | owner 隔离用例全绿 |
| E | 同上 + 手工冒烟 | MCP 按 owner 隔离；`gta.yaml` 生效 |
| F | `make dist` | 5 平台产物；compose 起得来 |

## 最终验收（对应设计文档 §7）

1. 全新 Linux 机器 `docker compose up -d` 就绪，无需 Go 环境。
2. 成员 A 跑 `gta-agent --token <A> --server <host>`：抓本机流量、本地插件自动注册、
   服务端可见 `alice/<plugin>`。
3. 成员 B 同时接入且插件同名：**并存不互相顶替**。
4. 无 token / 错 token 被拒（gRPC 与 MCP 两侧）。
5. A 查不到 B 的会话（admin 除外）。
6. 断网恢复后插件与 agent 自动重连。
7. `git clone` 到新机器 `go build ./...` 与 `go test ./...` 通过。
8. **现有本地单机用法行为完全不变**。
