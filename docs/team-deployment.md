# 团队部署指南

本文面向**服务器管理员（owner）**：在一台 Linux 服务器上用 Docker Compose 部署共享的 GTA 服务端，供团队成员用 `gta-agent` 接入。成员本机的上手步骤见 [成员上手指南](member-onboarding.md)。

## 架构总览

```
成员 A 本机                          服务器（Docker Compose）                成员的 AI Agent / 浏览器
┌──────────────────────┐            ┌──────────────────────────────────┐   ┌──────────────────────┐
│ gta-agent            │            │ gta-pipeline 容器                 │   │ gta-mcp 客户端        │
│  ├─ 抓包推流 ─────────┼── :9092 ──►│  AgentIngest gRPC                │   │  Authorization:      │
│  └─ 托管插件 ─────────┼── :9091 ──►│  PluginRegistry gRPC（隧道注册）  │   │   Bearer gta_xxx ────┼──► :8781
└──────────────────────┘            │  CaptureControl gRPC :9888       │   └──────────────────────┘
                                    │  └─ control.sqlite / sessions/   │
                                    │ gta-mcp 容器 ── HTTP/SSE :8781 ──┼──► 前端 / MCP 工具
                                    └──────────────────────────────────┘
```

| 端口 | 用途 | 谁连进来 |
|------|------|----------|
| 9888 | CaptureControl gRPC | gta-mcp（compose 内已互连，一般不对外） |
| 9091 | PluginRegistry gRPC | 成员本机插件（经 gta-agent 隧道注册） |
| 9092 | AgentIngest gRPC | gta-agent 抓包推流 |
| 8781 | gta-mcp HTTP/SSE（`/sse`+`/message`、`/mcp`、`/events/plugins`） | 团队成员的 AI Agent / 浏览器 |

数据落在共享卷 `/data`（control.sqlite + sessions/，SQLite WAL 模式，pipeline 与 mcp 跨进程并发安全）。

## 1. 准备

服务器要求：Linux + Docker（含 compose 插件）。

```bash
git clone https://github.com/OwnSecurityGuard/gta
cd gta
cp .env.example .env
```

## 2. 配置 token（`.env`）

编辑 `.env`，必填 `GTA_AUTH_TOKENS`，格式为逗号分隔的 `owner=token` 列表：

```dotenv
# 基本格式：GTA_AUTH_TOKENS=alice=gta_tok_xxx,bob=gta_tok_yyy
# 需要 admin 权限（看到所有人的会话/插件、管理他人资源）时加 :admin 后缀：
GTA_AUTH_TOKENS=alice=gta_tok_aaa:admin,bob=gta_tok_bbb
```

规则（实现见 `pkg/auth/token.go`）：

- 每个 owner 只能有一个 token；token 不能重复；缺 `=`、空 token 等格式错误会导致**启动失败**（不会静默降级为匿名模式）。
- `:admin` 后缀写在 token 之后（`gta_tok_aaa:admin`），实际 token 是 `gta_tok_aaa`。
- 变量留空 = 匿名模式（所有请求放行，owner 统一为 `local`）。**团队部署务必配置**。
- token 是敏感信息，只走环境变量，不进 gta.yaml。

生成随机 token：

```bash
openssl rand -hex 24   # 输出前加 gta_tok_ 前缀
```

其余 `.env` 可选项（端口映射 `GTA_*_PORT`、容器内监听地址 `GTA_*_ADDR`、CORS `GTA_MCP_ALLOWED_ORIGINS`、版本注入 `GTA_VERSION`/`GTA_GIT_COMMIT`）见 `.env.example` 内注释。

## 3. 启动与验证

```bash
docker compose up -d --build
docker compose ps          # pipeline、mcp 两个服务应为 running
docker compose logs pipeline | tail
```

验证（假设宿主机 IP 为 `10.0.0.5`）：

```bash
# 无 token → 401 unauthorized（鉴权已生效）
curl -s -o /dev/null -w '%{http_code}\n' http://10.0.0.5:8781/mcp
# 正确 token → 非 401（405/400 等取决于 HTTP 方法，不再是 unauthorized）
curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer gta_tok_bbb' http://10.0.0.5:8781/mcp
```

HTTP 路由：`/sse`、`/message`、`/mcp`、`/events/plugins` 均在鉴权链内（Bearer 校验，未带 token 返回 401）；`/singbox/profile` 是唯一豁免端点（手机客户端无法自定义请求头，且只输出代理地址信息）。

**owner 隔离**：每个 token 对应一个 owner，会话与插件按 owner 作用域——bob 看不到/管不了 alice 的会话与插件；带 `:admin` 的 owner 可以跨 owner 操作。MCP 工具（如 `start_capture`）的调用身份取自 HTTP 请求的 `Authorization: Bearer`，无需额外配置。

### 2.1 项目（Project）组织单元

「项目 Project」是一等组织单元，在 owner 之上提供一层**团队级归组**：项目持有 Game / 成员（Members）/ 解码插件（Decoder Plugins）/ 规则（Rules）/ 会话（Sessions），会话可归属到某个项目，避免多用户各自裸奔一堆 `session_xxx`。

项目相关接口：

| MCP 工具 | 作用 |
|---|---|
| `create_project` / `list_projects` / `get_project` / `update_project` / `delete_project` | 项目 CRUD，跨 owner 可见（见下） |
| `add_project_member` / `remove_project_member` | 项目成员增删；角色仅 `admin`/`member` 两档，不做复杂 RBAC |
| `set_project_plugins` / `set_project_rules` | 项目持有的插件/规则条目（关联数据，非独立管理后台） |
| `set_session_project` | 把既有会话绑定到项目（或空串解除），一键 `start_capture` 也可自动带 `project_id` |

可见性与权限：

- **可见性**：项目对 `created_by`、成员列表内的人、以及全局 `:admin` 可见（持久化在 `control.sqlite` 的 `projects` 表，跨 owner 共享）。
- **管理权**：仅项目 `created_by` 或全局 `:admin` 可改/删项目、管理成员与插件/规则（`member` 只读可看、可抓包）。
- **一手体验**：Web 首页「我的项目」展示项目在线/离线状态与最近会话；从项目发起抓包自动携带 `project_id`，会话持久化到 `sessions.project_id`。

```bash
# 通过 MCP 创建项目并加成员
PROJECT_ID=$(mcp call create_project '{"name":"王者荣耀测试服","game":"王者荣耀"}')
mcp call add_project_member "{\"project_id\":\"$PROJECT_ID\",\"user\":\"bob\",\"role\":\"member\"}"
mcp call start_capture '{"source":"agent","project_id":"'$PROJECT_ID'"}'   # 自动归属
# 校验归属：select session_id, project_id from sessions; （example）
```

**断线重连**：gta-agent 推流断线后按指数退避自动重连；插件进程崩溃由 gta-agent 按退避策略重启。服务器重启后 `restart: unless-stopped` 会自动拉起容器。

**监听地址回写**：各 gRPC/HTTP 服务实际监听地址会回写到 `<workdir>/addr.<name>.json`（如 `addr.mcp.json`、`addr.control.json`），方便多实例/动态端口场景读取。

### 启动排障

| 现象 | 原因 | 处理 |
|---|---|---|
| 构建卡在 `go mod download`，报 `proxy.golang.org ... connection refused` | builder 镜像默认 `GOPROXY` 是 proxy.golang.org，国内网络不可达 | 镜像已默认改用 `goproxy.cn`；如仍失败显式换源：`docker compose build --build-arg GOPROXY=https://goproxy.cn,direct` |
| `mcp` 容器反复重启，日志 `flag provided but not defined: -spawn-agent` | compose 只覆盖了 `entrypoint` 未写 `command`，镜像 `CMD ["-spawn-agent=false"]` 被追加给了 `gta-mcp`（该 flag 属于 pipeline） | mcp 服务必须显式写 `command`（见 `docker-compose.yml`），不要留空 |
| `mcp` 报 `open control store: unable to open database file (14)`，路径是 `/control.sqlite` | 工作目录没落到 `/data`，锚到了容器根目录，而进程以非 root 的 `gta` 用户运行 | 确认 `GTA_HOME=/data` 已传入且镜像 `WORKDIR /data` 生效；数据目录应是 `/data/control.sqlite` |

排查第一步建议先分清是**构建失败**还是**容器 restart loop**：前者看 build 输出，后者直接
`docker compose logs <服务名>`；`docker inspect <容器> --format '{{.Config.Cmd}}'` 能确认
entrypoint 被覆盖后镜像 CMD 是否被意外继承。

## 4. 日志（可选）

两个容器默认把日志同时写文件（`/data/logs/`，按大小轮转）和 stderr（docker logs）。容器内通常只留一路即可，用环境变量关闭另一路（T17）：

| 环境变量 | 效果 |
|---|---|
| `GTA_LOG_FILE_DISABLED=1` | 不落盘，只写 stderr（docker logs 可见） |
| `GTA_LOG_STDERR_DISABLED=1` | 只写文件，不写 stderr |

在 `docker-compose.yml` 对应服务的 `environment:` 段加一行即可，如：

```yaml
    environment:
      GTA_LOG_FILE_DISABLED: "1"
```

## 5. 成员接入

把服务器地址和每人自己的 token 发给成员，成员按 [成员上手指南](member-onboarding.md) 操作。核心命令：

```bash
gta-agent --token gta_tok_bbb --server 10.0.0.5:9091 --session <session_id> --iface 以太网 --filter "port 8984"
```

（`--session` 的值来自 owner 侧 MCP `start_capture(source="agent")` 返回的 session_id，见上手指南。）

## 6. 验收清单（新机器演练）

- [ ] 全新 Linux 机器 `docker compose up -d --build` 一次成功（需 GitHub 可达，见临时依赖）；
- [ ] 正确 token 的 MCP 请求 200；伪造 token 被拒绝（401/403）；
- [ ] bob 的 MCP 客户端看不到 alice 的会话与插件（owner 隔离）；admin 可以；
- [ ] 成员 `gta-agent` 推流后，owner 侧 `get_session_status` 的 `packets_in` 增长；
- [ ] 拔掉成员网线/杀掉 agent 进程再恢复，推流自动重连继续入库；
- [ ] 服务器重启后容器自愈（`restart: unless-stopped`），历史会话仍可查询。

## 7. 非 Docker 部署（简述）

裸机/裸容器自建时直接跑二进制即可，注意：

- 服务端实时网卡抓包源需要 libpcap（cgo，`-tags pcap` 构建）；
- 两个进程共用同一 `GTA_HOME` 工作目录（与 compose 的 /data 卷等价）；
- `gta-mcp` 用 `GTA_CONTROL_ADDR` 指向 pipeline 的 9888；
- 其余环境变量与上文一致（见 `pkg/config/app.go`）。

更多配置细节：`examples/gta.yaml`（统一配置文件示例）、`.env.example`。
