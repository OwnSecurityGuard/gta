# 探针（Probe）优化设计方案

> 状态：v2，已并入 review 意见（UI 以会话为中心 / 状态三维度 / 权限 creator-only）· 2026-09-05
> 范围：gta-agent（网卡探针）/ gta-singbox-agent（移动代理探针）/ gta-pipeline / gta-mcp / WebUI

---

## 0. 术语

| 术语 | 含义 |
|---|---|
| **探针 probe** | 跑在被观测机器上的 agent 进程实例，有全局唯一 `probe_id`。当前形态：`gta-agent`（pcap）、`gta-singbox-agent`（mobile） |
| **机器名** | 探针在 UI 上的显示名，默认 hostname，可在管理页改（如 `game-server-01`） |
| **capability** | 探针能力标签：`pcap` / `mobile` / `plugin_host` |
| **本地控制面** | 探针在自己机器上暴露的回环 HTTP 接口，给"坐在那台机器前的人"用 |
| **远端控制面** | 平台下发指令给探针的通道，给"在 Web 页面上的人"用 |
| **archive** | 探针在本机的长期留存数据（区别于上行缓冲 spool） |

**一句话定位**：探针是基础设施，会话是用户任务。用户来平台是为了"抓 game-server-01 的 8080"，不是为了"操作一个 probe"。

---

## 1. 现状与痛点

### 1.1 三条数据上行链路

```
① pipeline 本机 pcap   : 网卡在 pipeline 启动时固定（-iface），start_capture(source=nic) 直接用
② gta-agent → AgentIngest(:9092) → Hub → session
                        : 命令行 --server/--token/--session/--iface/--filter，人工去成员机执行
③ gta-singbox-agent → mobile source : 已有本地回环 HTTP 控制面，pipeline 同机直连
```

### 1.2 三个痛点对应到代码

| 需求 | 现状 | 病灶位置 |
|---|---|---|
| ① 探针暴露控制接口，别用命令行改参数 | `cmd/gta-agent/main.go` 用 12 个 flag 驱动；`--session/--iface/--filter` 一旦启动不可变，改参数必须杀进程重跑 | 参数与生命周期绑死 |
| ② 页面选机器、启停、改端口 | `StartCaptureDialog` 的"抓包探针"只是**生成一条命令让人去抄**（`buildAgentCommand`）；机器不可见、不可选、不可控 | 探针不是资源，没有身份、没有注册表、没有控制通道 |
| ③ 数据在探针机器上持久化，可按时间离线加载 | `pkg/spool` 是**上行缓冲**：`AckN` 后 segment 即删；服务端没接住的数据（会话未建、断线超配额）永久丢失，无法事后补救 | 只有"未确认缓冲"，没有"留存" |

### 1.3 一个必须正视的约束

**成员机在 NAT 后，平台无法主动连它。** 现存唯一可达路径是探针**主动外连** pipeline 的 gRPC（AgentIngest :9092）。
这决定了远端控制面**必须走反向通道**（探针 outbound 建流，服务端在流上下发指令），而不是"服务端去调探针的本地 HTTP"。
（singbox-agent 现在能被直连，只是因为它和 pipeline 同机；这个前提对成员机不成立。）

---

## 2. 目标 / 非目标

**目标**
1. 探针启动后所有运行参数可经接口读写，命令行只剩首启引导；改 BPF/端口热生效，不重启进程。
2. 平台上一句话发起抓包：**选机器 + 填端口 + 开始**；可随时停、随时改端口。
3. 探针状态分三个维度呈现（连接 / 抓包 / 数据），能一眼区分"没连上""没开始抓""抓了没数据"。
4. 探针本机长期留存原始帧；平台可查询某机器某时间窗的归档，并按需导入成会话做离线分析。

**非目标（本次不做，明确划线）**
- 一个探针同时服务多个抓包会话（保持 1 探针 1 会话，UI 上占用即置灰）。
- 探针共享给别人 / 归属项目 —— **探针是个人资源，只有注册者本人能用**（见 §6.3）。
- 归档数据自动上传/集中存储（导入是**按需拉取**，不是同步）。
- 探针自动升级、远程安装插件。
- 移动端代理探针的二维码/VPN 模型改造（它已能常驻，只做注册表纳管 + 控制语义对齐）。

---

## 3. 总体架构

```
                       ┌──────────────── 平台侧 (pipeline + mcp + webui) ────────────────┐
                       │                                                                 │
   WebUI ───MCP──▶ 选机器 + 端口 → 开始抓包                                              │
                       │            │                                                    │
                       │      ┌─────▼──────┐    ┌──────────────┐   ┌─────────────────┐   │
                       │      │ ProbeRegistry│   │ ControlChannel│   │  ControlStore   │   │
                       │      │ (身份/鉴权)  │   │ (指令/状态)   │   │ probes 表       │   │
                       │      └─────┬──────┘    └──────┬───────┘   │ archive_index 表 │   │
                       └────────────┼──────────────────┼───────────┴─────────────────┘   │
                                    │                  │ ① Command (下行)                  │
        ┌───────────────────────────┼──────────────────┼──────────────────────────────┐  │
        │ 成员机                     │ ② Event/Ack(上行) │                              │  │
        │   ┌───────────────────────▼──────────────────▼──────────┐                    │  │
        │   │ gta-agent (常驻)                                      │                    │  │
        │   │  ┌────────────┐  ┌──────────┐  ┌──────────────────┐  │                    │  │
        │   │  │ControlAgent│  │ pcap loop│  │ ingest client    │──┼──③ Push(:9092)────▶│  │
        │   │  └─────┬──────┘  └────┬─────┘  └────────┬─────────┘  │                    │  │
        │   │        │              │                 │             │                    │  │
        │   │  ┌─────▼──────────────▼─────────────────▼─────────┐   │                    │  │
        │   │  │ Archive（一份数据 / 两个游标）                    │   │                    │  │
        │   │  │   send-cursor（上行未确认）  retention（留存窗口） │   │                    │  │
        │   │  └─────┬──────────────────────────────────────────┘   │                    │  │
        │   │  ┌─────▼─────────────┐                                │                    │  │
        │   │  │ 本地控制面 :19500  │◀── 坐在这台机器前的人/脚本      │                    │  │
        │   │  └───────────────────┘                                │                    │  │
        │   └───────────────────────────────────────────────────────┘                    │  │
        └───────────────────────────────────────────────────────────────────────────────┘  │
                                                                                            │
     ④ 离线加载：ArchiveUpload 流（新建，不占控制流）──▶ Hub ──▶ 新 session（解码+落库）────┘
```

三条通道职责分离，互不挤占：

| 通道 | 方向 | 承载 | 生命周期 |
|---|---|---|---|
| ① Control Stream（**新增**） | 探针 outbound，双向流 | 指令下行 / 状态与指令结果上行 | 探针存活期间常连 |
| ② Push（**现有**） | 探针 outbound，客户端流 | 实时抓包数据 | 抓包期间 |
| ③ ArchiveUpload（**新增**） | 探针 outbound，客户端流 | 历史归档回放 | 按需，一次性 |

---

## 4. 需求①：探针本地控制面

### 4.1 命令行参数 → 配置资源

现有 12 个 flag 分三类处理：

| 参数 | 处置 | 说明 |
|---|---|---|
| `--server` / `--registry-addr` / `--ingest-addr` / `--token` / `--code` / `--mcp` | **降为首启引导**，写进 `probe.json`，之后由控制面 `PUT /v1/config` 改 | 一次性，正常不改 |
| `--session` / `--iface` / `--filter` / `--snaplen` / `--promisc` | **升级为抓包配置**，走 `/v1/capture/*` | 本次需求核心 |
| `--plugin-dir` / `--spool-dir` / `--batch-size` / `--batch-interval` | 归入 `/v1/config`（热生效或标记 need_restart） | 次要 |

**热生效边界（必须说清楚，不能含糊）**

| 变更 | 是否重启抓包 | 实现 |
|---|---|---|
| ports / hosts / bpf（BPF 表达式重编译） | **热生效，不中断** | 在现有 `*pcap.Handle` 上调 `SetBPFFilter` |
| snaplen / promisc | **需重开 handle**（进程不重启，抓包中断 ~100ms） | 关 handle → `openLiveWithFallback` 重开 |
| iface | **需重开 handle**（同上） | 同上 |
| session（换推流目标） | **热生效** | 只换 ingest client 的 `session_id`，重新开流；归档不动 |

### 4.2 接口（回环 HTTP，与 singbox-agent 现有 `/v1` 语义对齐）

```
GET  /v1/status                 三维度状态（见 §5）
GET  /v1/health                 存活
GET  /v1/config                 当前生效配置（脱敏：token 只回显前 6 位）
PUT  /v1/config                 部分更新；返回 {applied, need_restart:[]}
GET  /v1/interfaces             本机 pcap 设备清单（设备名 + 友好名 + IPv4）——页面选网卡用
POST /v1/capture/start          {session_id, iface?, ports?[], hosts?[], bpf?, snaplen?, promisc?}
POST /v1/capture/stop           停上报 + 关 handle，进程不退出
POST /v1/capture/filter         {ports?[], hosts?[], bpf?}  热更新，不中断
GET  /v1/archive?from=&to=      归档段摘要 [{seg_id, first_ts, last_ts, packets, bytes, link_type}]
POST /v1/archive/export         {from, to, format:"pcapng"|"native"} → 本地文件路径（P1）
```

**鉴权**：首次启动生成 `control.token`（0600），写操作需 `Authorization: Bearer <token>`。
（singbox-agent 现在的控制面是裸奔的，顺手补上——"信任边界 = 能登录本机的进程"这句话在有 token 之后才成立。）
**不监听非回环地址。** 需要跨机控制走远端控制面（§6），不要放监听。
端口默认 `127.0.0.1:19500`，被占时自动 +1 并写 `<datadir>/control.port`，避免"不知道去哪找"。

---

## 5. 探针状态模型（三维度）

**不要一个 `probe.status`。** 三件事彼此独立，必须分开看——否则"探针在线但没抓到包"和"探针根本没连上"在 UI 上长得一样，用户只能干瞪眼。

### 5.1 维度一：连接（connection_state）

| 值 | 含义 | 判定 |
|---|---|---|
| `online` | 控制流在 | 流存活 |
| `offline` | 控制流断了 | 断流 > 30s |

附：`connected_since`、`last_heartbeat_at`（心跳 10s）。

### 5.2 维度二：抓包（capture.state）

状态机：

```
        assign                    opened+推流就绪
  idle ────────▶ starting ──────────────────────▶ running
                    │                                │
                    │ 开 handle/BPF/推流失败           │ stop
                    ▼                                ▼
                 failed  ◀──── 运行期错误 ───────  stopped
                    │                                │
                    └──── 收到新 assign / 重试 ────────┘
                              → starting
```

| 值 | 含义 | 何时 |
|---|---|---|
| `idle` | 从未被指派 | 刚注册、尚未抓过 |
| `starting` | 正在开网卡/设 BPF/建推流 | 收到 `AssignCapture`；10s 未进 running → `failed` |
| `running` | 抓包中 | pcap 循环在跑 |
| `stopped` | 正常停止（终态直到新指令） | 收到 `StopCapture`；保留 last session 信息供 UI 展示 |
| `failed` | 失败，带 `error` + `failed_at` | 开 handle / BPF 编译 / 推流连续失败；**进程不自杀**，常驻等待重试 |

`idle` 与 `stopped` 的区别：`stopped` 带"上次抓了什么"（session_id、包数、停止时间），`idle` 没有。

### 5.3 维度三：数据（data）

| 字段 | 含义 |
|---|---|
| `last_packet_time` | 最后一次**抓到帧**的时刻（探针本地，抓包侧） |
| `last_upload_time` | 最后一次**成功推流并被确认**的时刻（上行侧，`AckN` 后更新） |
| `packets_captured` / `packets_acked` | 抓到 / 已确认推走（差值 = 在途积压） |
| `spool_depth` / `dropped` | 磁盘积压 / 丢弃计数 |

两个时间戳缺一不可：`last_packet` 新但 `last_upload` 旧 = 抓到了但推不上去（上行链路断了）；反之是时钟/探针异常。

### 5.4 Status 结构（本地 `/v1/status` 与心跳共用）

```json
{
  "probe_id": "prb_7f3a",
  "name": "game-server-01",
  "hostname": "WIN-A1B2",
  "version": "1.4.0",
  "connection": { "state": "online", "since": "2026-09-05T16:20:00Z", "last_heartbeat": "..." },
  "capture": {
    "state": "running",
    "session_id": "20260905_164955.419_3340",
    "iface": "\\Device\\NPF_{...}",
    "ports": [8080],
    "hosts": [],
    "snaplen": 1600,
    "error": "",
    "updated_at": "..."
  },
  "data": {
    "last_packet_time": "2026-09-05T16:49:58Z",
    "last_upload_time": "2026-09-05T16:49:58Z",
    "packets_captured": 128304,
    "packets_acked": 128304,
    "spool_depth": 0,
    "dropped": 0
  },
  "archive": { "bytes": 2147483648, "segments": 9, "oldest": "...", "newest": "..." }
}
```

### 5.5 三维度 → 用户看得懂的话（前端直接用这张表）

| connection | capture | data | 含义 | 页面提示 |
|---|---|---|---|---|
| online | running | `last_packet` 新 | 一切正常 | 抓包中 · 2 秒前有数据 |
| online | running | 长时间无包 | BPF/端口不对，或这台机器真没流量 | 抓包中，但 5 分钟没抓到包 —— 检查端口是不是 8080 |
| online | running | 有包但没上传 | 上行链路断了 | 抓到 1.2 万包未上传 —— 检查这台机器到服务端的网络 |
| online | idle / stopped | — | 没在抓 | 空闲 · 点「开始抓包」 |
| online | failed | — | 网卡/BPF 出错 | 显示 `error` + [重试] |
| online | starting | — | 正在起 | 正在启动抓包… |
| offline | 任意 | — | 探针没运行或机器网络断了 | 离线 · 上次在线 12 分钟前 |

**UI 呈现**：三个独立 chip（连接 / 抓包 / 数据），**不要合并成"一个状态灯"**。

---

## 6. 需求②：探针注册与远端控制

### 6.1 身份：从"启动码一次性"到"长期凭证"

现有 `create_access_code` → agent `claim` 只领取 `server/token/session`，agent 借**用户 token**说话。
问题：用户 token 轮换/过期 → 探针掉线；且无法区分"哪台机器"。

**改造**：claim 之后追加一次注册，换发长期凭证：

```
claim(code) → {server, token, …}
      ↓
POST /v1/probes/register  {hostname, os, arch, capabilities, name?}
      ↓
{ probe_id, probe_token }        ← 落盘 <datadir>/probe.json
      ↓
此后：Push / Control 一律用 probe_token（Bearer），不再用用户 token
```

- `probe_token` 服务端只存哈希；丢失可凭新启动码重新注册（覆盖旧 token）。
- 老 agent（只有用户 token）继续可用：`authorize` 解析不出 probe 时退化为现有 owner 语义。**不破坏现有链路。**
- `name` 默认 hostname，用户可在管理页改成 `game-server-01` 这类业务名。

### 6.2 数据模型（ControlStore 新表）

```sql
CREATE TABLE probes (
  probe_id     TEXT PRIMARY KEY,
  name         TEXT NOT NULL,          -- 机器名，默认 hostname，可改
  owner        TEXT NOT NULL,          -- 注册者（唯一使用者，见 6.3）
  tenant_id    TEXT NOT NULL DEFAULT 'default',
  capabilities TEXT NOT NULL,          -- csv: pcap,mobile,plugin_host
  token_hash   TEXT NOT NULL,
  version      TEXT, hostname TEXT, os TEXT, arch TEXT,
  -- 三维度状态快照（由心跳刷新，供离线时展示）
  connection_state TEXT,
  capture_state    TEXT,
  last_packet_time INTEGER,
  last_upload_time INTEGER,
  last_session_id  TEXT,
  status_error     TEXT,
  created_at TEXT, last_seen_at TEXT
);

-- 服务端缓存的归档摘要：探针离线时也能看到"它机器上有哪些数据"
CREATE TABLE probe_archive_segments (
  probe_id TEXT, seg_id TEXT,
  first_ts INTEGER, last_ts INTEGER,
  packets INTEGER, bytes INTEGER, link_type INTEGER,
  updated_at TEXT,
  PRIMARY KEY (probe_id, seg_id)
);
```

**没有 `project_id`。** 探针是个人资源（§6.3），不挂项目。会话仍可归属项目——那是会话的事，与探针无关。

### 6.3 权限：creator-only

**探针默认只有注册者本人能用，别人用不了。** 不做项目共享、不做角色矩阵。

| Action | 规则 |
|---|---|
| `probe:read` | owner 本人 或 global admin |
| `probe:use`（选它抓包） | owner 本人 或 global admin |
| `probe:control`（启停/改参数/导归档） | owner 本人 或 global admin |
| `probe:manage`（改名/吊销/删除） | owner 本人 或 global admin |

- 接入 `pkg/authz`：新增 `KindProbe` + 四个 Action，`Decide` 里走 creator 轴（与"个人会话"同构，见 `decideSession` 的非项目分支）。
- **不做** project 轴、`RoleMember/Admin` 矩阵、共享探针。将来真要共享，再加 `project_id` + 角色表；现在加就是给不存在的未来写代码。
- `list_probes` 直接按 `owner = current_user` 过滤（admin 可见全部）。

### 6.4 控制通道：反向 gRPC 长流

成员机在 NAT 后，**服务端直连探针本地 HTTP 不可行**（仅同机/局域网成立）；探针轮询指令延迟大、离线判定粗糙。只做反向长流。

```proto
service AgentControl {
  // 探针发起：上行 Hello/Heartbeat(带三维度 Status)/CommandResult，下行 Command
  rpc Connect(stream ControlEvent) returns (stream Command);
  // 按需：归档回放上传（独立流，避免大数据挤占控制流）
  rpc UploadArchive(stream ArchiveChunk) returns (UploadAck);
}

message Command {
  string id = 1;
  oneof payload {
    AssignCapture assign = 2;    // {session_id, iface?, ports[], hosts[], bpf?, snaplen?, promisc?}
    StopCapture   stop   = 3;
    UpdateFilter  filter = 4;    // {ports[], hosts[], bpf?}
    SetConfig     config = 5;    // 白名单键
    ArchiveQuery  aq     = 6;    // {from_unix, to_unix}
    ArchiveUpload au     = 7;    // {target_session_id, from_unix, to_unix}
    Retry         retry  = 8;    // failed → 重试上一次 assign
  }
}
message ControlEvent {
  oneof payload {
    Hello hello = 1;             // probe_id + capabilities + version（开流首包）
    Heartbeat hb = 2;            // 10s，带 §5.4 的完整 Status 快照
    CommandResult result = 3;    // {id, ok, error}
    ArchiveSegments segs = 4;
  }
}
```

**指令语义约定（重要）**
- 指令至少一次投递，靠 `id` 幂等；探针侧维护已处理 id 集合（LRU 256 条）。
- 断线期间**不补偿堆积指令**：重连后服务端重新评估"期望状态"并下发，探针以平台期望为准，而不是补执行一条过期指令。
- 在线判定：控制流断开 > 30s → `connection_state=offline`；三维度快照保留最后值，UI 显示"上次在线 X 前"。

### 6.5 会话绑定模型

- 建会话 = 选机器 + 端口 → 服务端建 session + 下发 `AssignCapture` → 探针本地起抓包推流。**不再有"贴命令让人去跑"这一步。**
- 1 探针 1 会话：正在 `running` 的机器在下拉里置灰，标注"占用中 · session xxx"。
- `probe_stop_capture` → `StopCapture` → 关 handle、停上报、进程常驻（`capture.state=stopped`）。
- **会话 metadata 存快照**：`probe_id` + `probe_name`（改名不影响历史会话显示）+ `source=probe`。
- **AgentIngest 鉴权补一条**：探针用 `probe_token` 推流时，除现有"session owner 匹配"外，校验"该 session 的 assigned_probe 就是本探针"，防止 A 机器往 B 的会话灌数据。

---

## 7. UI：以会话为中心，探针退到「管理」

### 7.1 为什么这么改

用户的心智是"我要抓 game-server-01 的 8080"，不是"我要操作一个 probe"。
探针是 exporter，会话才是 dashboard —— 没人天天盯 exporter 页面。

```
一级导航：  会话 Sessions  ·  创建抓包
二级（管理）：管理 > 探针      ← 基础设施，日常不点
```

### 7.2 创建抓包（一级，改造现有 StartCaptureDialog）

```
抓包源   [ 服务器网卡 | 远程机器 ]        ← "抓包探针"改叫"远程机器"

机器     ┌─────────────────────────────┐
         │ game-server-01        ● 在线 │   ← 只列我有权限的机器
         │ WIN-A1B2 · v1.4.0           │      三态 chip 直接显示在卡片上
         └─────────────────────────────┘
         ┌─────────────────────────────┐
         │ test-box-02      ○ 离线 · 2h │   ← 置灰不可选
         └─────────────────────────────┘
         ┌─────────────────────────────┐
         │ db-01          ◐ 占用中 · 抓包 │   ← 置灰不可选
         └─────────────────────────────┘

端口     8080                            ← 单个输入框，支持 "8080,9090"

解析器   [ Godot ][ HTTP ][ ... ]        ← 现有插件卡片，不变

项目     [ 选择项目 ▾ ]                   ← 现有 initialProjectId

▸ 高级设置
   网卡   [ 自动 ▾ ]（来自 /v1/interfaces）
   主机过滤  空
   自定义 BPF  空（填了就覆盖端口）

                              [ 取消 ]  [ 开始 ]
```

- 机器卡片上的三态 chip：**连接（在线/离线）· 抓包（空闲/运行中/失败）· 数据（N 秒前有数据）**。
- 全离线时给出降级入口："没有在线的机器 → 去「管理 > 探针」看接入方式"。
- 老的"复制启动命令"从主流程移除，折叠在管理页做应急入口。

### 7.3 会话详情（一级）

来源机器显示为业务名，不是 probe_id：

```
来源   game-server-01  ·  端口 8080
       连接 ● 在线      抓包 ● 运行中      数据 ● 2 秒前
                              [ 停止抓包 ] [ 改端口 ] [ 导入历史 ]
```

三态在会话页常驻——抓包没数据时，用户第一眼看的是"到底哪一段断了"。

### 7.4 管理 > 探针（二级，基础设施视图）

| 机器名 | 主机 | 版本 | 连接 | 抓包 | 数据 | 归档 | 操作 |
|---|---|---|---|---|---|---|---|
| game-server-01 | WIN-A1B2 | 1.4.0 | 在线 3h | 运行中 | 2s 前 | 1.8GB / 9 段 | 改名 · 吊销 · 导入历史 · 删除 |
| test-box-02 | ubuntu-2 | 1.4.0 | 离线 2h | 已停止 | 2h 前 | 512MB / 3 段 | 改名 · 吊销 · 导入历史 · 删除 |

- 顶部 [+ 接入新机器]：生成启动码 → 复制命令（唯一需要人去机器上执行的时刻，一次性）。
- 点行展开：网卡清单、当前 BPF、spool 深度、dropped、最近错误、最近指令历史。
- 吊销 = 作废 `probe_token`，该机器下次启动需重新接入。

### 7.5 平台侧能力

**MCP 工具**

```
list_probes              按 owner 过滤，带三维度状态（含离线快照）
get_probe                详情 + 实时三态 + 归档用量
probe_start_capture      {probe_id, ports, hosts?, iface?, plugin?, project_id?} → session_id
probe_stop_capture       {probe_id}
probe_update_filter      {probe_id, ports?, hosts?}     热更新，不停抓包
probe_retry_capture      {probe_id}                     failed → 重试
probe_list_archive       {probe_id, from, to} → 段摘要
probe_import_archive     {probe_id, from, to, project_id?} → session_id
（P2）probe_rename / probe_revoke
```

**WebUI**：新增「管理 > 探针」页；改 `StartCaptureDialog`；会话详情加来源机器与三态。

---

## 8. 需求③：本地持久化与离线加载

### 8.1 现状问题

`pkg/spool.Queue` 是**上行缓冲**：`Append → Next → AckN`，确认后 segment 删除。它保证"不丢上行"，但**不保证"机器上有留存"**。
所以：会话没建、服务端挂了、断线超过 512MB 配额 → 数据永久消失，事后无法补救。

### 8.2 设计：一份数据，两个游标

不新增第二份写入（双写 = 双倍磁盘 + 双写不一致风险），而是把 spool 从"发后即焚队列"升级为"**带留存策略的分段日志**"：

```
写入：抓到的帧只写一次 → 按时间/大小滚动的 segment 文件
游标1 send-cursor    ：上行未确认位置（= 现在的 cursor.json），确认后仅推进，不删数据
游标2 retention      ：保留窗口（默认 24h / 4GB），独立清理
```

- 清理约束：**只能删"已全部确认 且 超出保留窗口"的段**。未确认数据永不被 retention 删除。
- 触顶行为：保留窗口到了但未确认数据占满配额 → **停止抓包并告警**（与现有 `ErrFull` 一致：宁可停抓，也不静默覆盖历史）。
- 对外 API 兼容：`Append/Next/AckN/Requeue/Depth` 语义不变（381 行 `queue_test.go` 继续是护栏），新增：

```go
func (q *Queue) SetRetention(cfg Retention) error          // {MaxAge, MaxBytes}
func (q *Queue) Segments(from, to time.Time) ([]SegmentInfo, error)
func (q *Queue) Read(segID string, from, to time.Time, fn func(*agentproto.RawPacket) error) error
func (q *Queue) ExportPcapNG(from, to time.Time, w io.Writer) error   // P1
```

**格式选择**

| 方案 | 评价 |
|---|---|
| 沿用现有 `[u32 len][protobuf RawPacket]` segment + 段索引 | ✅ 零转换，回放直接反序列化喂 Hub（RawPacket 已带 link_type/时间戳/UUID）；复用现有代码与测试 |
| pcapng | ❌ 要写 SHB/IDB/EPB，回放还得解析；仅有"Wireshark 能直接看"一个好处 |

**选前者**，把"Wireshark 友好"降级为 `/v1/archive/export?format=pcapng`（P1，低频操作）。

**段布局**

```
<datadir>/archive/
  seg-20260905-1600.gta          帧数据（[u32 len][RawPacket]…）
  seg-20260905-1600.idx          {seg_id, first_ts, last_ts, packets, bytes, link_type}
  ...
  send.cursor                    上行未确认游标（原 cursor.json）
```
滚动：1 小时 或 256MB，先到先滚。时间范围查询 → 只读 `*.idx`（文件数极少，毫秒级）。

### 8.3 离线加载流程

```
① 用户：会话页/探针页 → [导入历史] → 选时间窗
② 平台 probe_list_archive(probe_id, from, to)
     → 命中 probe_archive_segments 缓存；探针在线则顺带刷新，离线则用缓存（标注可能过期）
③ 确认 → probe_import_archive
     → 平台建 session（source=probe-archive，metadata 记 probe_id + probe_name + 时间窗）
     → 下发 ArchiveUpload{target_session_id, from, to}
④ 探针开 UploadArchive 流，按段顺序读、按原始时间戳推送（背压由 Hub 现有机制传导）
⑤ 服务端收到 → Hub.Deliver(target_session_id) → 走正常解码 + 落库
⑥ 会话出现在会话列表，标记为「离线导入 · game-server-01 · 16:00–17:00」
```

**关键决策：导入结果落新 session，不回填老 session。**
理由：老 session 可能已关闭或只有部分数据，混进去会污染统计与时间线，且无法区分"实时抓到的"和"事后补的"。新 session 的 metadata 明确写 `source=probe-archive` + 机器名 + 时间窗，可追溯。

**回放节流**：默认尽快推送（离线补数据，没人想等），由 Hub 背压（现有 `DefaultDeliverTimeout`）自然限速；预留 `speed` 参数但**本次不实现**。

### 8.4 默认配置

| 项 | 默认 | 说明 |
|---|---|---|
| 留存时长 | 24h | 覆盖"今天抓的忘了建会话，晚上想起来补"的绝大多数场景 |
| 留存容量 | 4GB | 约 250 万帧 @1600B |
| 段滚动 | 1h / 256MB | 先到先滚 |
| 归档开关 | 开 | 可在 `/v1/config` 关闭（隐私场景） |

---

## 9. 隐私与安全护栏（P0，不是可选项）

探针在**别人的机器**上长期留存**含明文 payload 的原始流量**，这是本方案最大的新增风险面。

1. **留存默认有界**：24h / 4GB 双上限，触顶停抓告警而非静默覆盖。
2. **目录权限**：`<datadir>/archive` 创建为 0700（Windows：仅当前用户 ACL），`probe.json` / `control.token` 0600。
3. **导出审计**：谁、在什么时候、从哪台机器、导出了哪个时间窗 —— 落审计表，页面可见。
4. **本地控制面鉴权**：`control.token`，不放宽监听地址。
5. **远端只能拉不能推**：指令集不含任意命令执行；`SetConfig` 走白名单键。
6. **归档元数据最小化**：服务端只缓存时间/包数/字节数，不含 payload、不含 host 名单。

---

## 10. 分期

| 阶段 | 内容 | 交付后可解决 |
|---|---|---|
| **P0** | 探针注册（probe_id/token）+ probes 表 + authz（creator-only）+ 本地控制面 + 三维度状态 + 反向控制通道 + `list/get/start/stop/update_filter/retry` + 「创建抓包」改造（选机器+端口）+ 「管理 > 探针」页 | 需求①、需求② |
| **P1** | spool 升级为双游标分段日志 + 段索引 + 保留策略 + `probe_list_archive` / `probe_import_archive` + 平台侧归档摘要缓存 | 需求③ |
| **P2** | pcapng 导出、探针改名/吊销、singbox-agent 纳入注册表统一纳管、`speed` 限流、归档自动上传 | 锦上添花 |

P0 与 P1 边界清晰：P0 只动控制面，P1 才动 `pkg/spool`（有测试护栏的核心件）。

---

## 11. 待拍板（已收下 review 意见后的剩余项）

| # | 决策 | 我的推荐 | 备选 |
|---|---|---|---|
| 1 | 持久化形态 | 改 spool 为双游标分段日志（写一次） | spool + archive 双写（简单但双倍磁盘） |
| 2 | 归档格式 | 沿用 protobuf segment + 段索引，pcapng 只做导出 | 直接落 pcapng |
| 3 | 移动端代理探针 | 纳入同一注册表（capability=mobile），仅统一控制语义与三态，不改其 VPN/二维码模型 | 暂不纳管，两套列表并存 |

已按 review 定稿的：UI 以会话为中心（§7）、三维度状态（§5）、权限 creator-only（§6.3）、1 探针 1 会话、离线导入落新 session。

---

## 12. 已知风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| spool 改造（P1）触碰不丢包核心件 | 数据丢失 | 保留 `queue_test.go` 全部用例；新增"retention 绝不删未确认段"单测；灰度先开单机器 |
| 三维度状态来源分散（控制流/抓包循环/推流各报一份） | UI 显示自相矛盾 | 单一 `Status` 结构由探针侧聚合后上报，服务端不做推断；心跳 10s |
| 指令重放导致重复启停 | 抓包抖动 | 指令 id 幂等 + 重连后以"平台期望状态"校正，而非补发指令队列 |
| 归档留存引发隐私顾虑 | 成员机抵触 | 第 9 节护栏；留存默认 24h；页面明示"哪些数据存在你机器上" |
| 老 agent 与新注册流程并存 | 两套凭证逻辑 | `authorize` 解析不出 probe 时退化现有 owner 语义，不破坏老链路 |
