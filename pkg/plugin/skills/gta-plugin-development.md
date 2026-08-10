# GTA 插件开发指南 (v2)

> 本文档基于 SDK 中的 `contract/contract.yaml`（**SSOT，单一事实来源**）派生，面向插件开发者。
> 阅读顺序建议：**先看契约 → 再看本指南 → 复用 `framing` 包**。
> 契约中与剥头/重组相关的规则为 `payload-framing-by-link-type`(error)、`link-type-selects-framing`(error)、`inspect-bytes-first`(error)、`tcp-reassembly-required`(warn)，下文均围绕它们展开。

---

## 1. 概念模型

```
┌─────────────────────────────────────────────────────┐
│                  gta-pipeline                       │
│                                                     │
│  ┌──────────────┐    Register RPC    ┌───────────┐ │
│  │ gta-mcp      │ ────────────────>  │ Registry  │ │
│  │ (Agent 入口) │ <───────────────   │ Server    │ │
│  └──────────────┘    List/Deregister └─────┬─────┘ │
│                                            │       │
│  ┌──────────────┐    Decode RPC           │       │
│  │ 插件进程 A   │ ◄───────────────────────┘       │
│  │ (http插件)   │   unix/npipe/tcp socket         │
│  └──────────────┘                                 │
│  ┌──────────────┐    Decode RPC                   │
│  │ 插件进程 B   │ ◄────────────────────────────── │
│  │ (dhcp插件)   │                                  │
│  └──────────────┘                                  │
└─────────────────────────────────────────────────────┘
```

**关键术语：**

| 术语 | 说明 |
|------|------|
| `api_version` | 契约版本，固定为 `gta.decoder/v2` |
| `protocol` | 协议名称（slug），如 `http`、`dhcp` |
| `protocol_version` | 协议版本，如 `1` |
| `type` | 插件类型：`decoder`（数据解码） |
| `hints` | 协议识别提示，逗号分隔的字符串数组 |
| `event` | 输出事件名，与 contract.yaml 的 `registered_events[].event` 对齐 |
| `registry_addr` | 插件与注册中心通信的端点 |

---

## 2. 三步开发流程

### 步骤 1：创建项目骨架

使用 MCP 工具 `create_plugin` 自动生成：

```json
{
  "name": "my-http",
  "protocol": "http",
  "protocol_version": "1",
  "hints": "tcp,dns",
  "output_dir": "./plugins/my-http"
}
```

生成的目录结构：

```
plugins/my-http/
├── plugin.yaml          # Manifest 配置文件
├── main.go              # 插件入口
├── go.mod               # 模块定义（仅 require gta-plugin-sdk）
└── README.md            # 说明文档
```

### 步骤 2：实现 Decode 函数（这是最容易写错的一步）

插件的核心是 `DecodeFuncV2` 回调（DecodeV2 双向流接口），接收原始字节并通过 stream 回传解码事件：

```go
package main

import (
	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// ra 在插件进程内只创建一次，跨所有会话/包复用。
var ra = framing.NewReassembler()

func decode(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	// ⚠️ req.GetPayload() 是【完整链路层帧】，不是 L7！
	//    必须按 link_type 剥头，再按流做 TCP 重组，才能拿到业务字节。
	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		// 非 IP 流量 / 纯 ACK / 握手 / FIN：无业务数据，直接 done。
		return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
	}

	s := ra.Push(seg)
	for {
		raw := s.Bytes()
		msg, n := parseOneHTTPMessage(raw) // 你的协议解析逻辑
		if n == 0 {
			break // 不完整：等下一个 segment
		}
		s.Consume(n)
		if err := emitEvent(stream, req.GetInputId(), msg); err != nil {
			return err
		}
	}
	return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
}

func main() {
	sdk.RunRegisterLoop(decode)
}
```

> **千万不要**把 `req.GetPayload()` 当 HTTP/TCP 报文正文直接解析。pcap 类来源（回环、以太网、RawIP 等）交付的是**带链路层头的完整帧**，直接按 L7 解析会得到 0 事件（详见 §7 排查手册）。只有 `ProxyPayload`(1001) / `TLSPlaintext`(1002) 两种 link_type 才是已剥好的纯 L7。

### 步骤 3：构建并运行

```bash
cd plugins/my-http
go build -o my-http-plugin .
# 插件会自动：
# 1. 读取 plugin.yaml 中的 manifest
# 2. 连接到 gta-pipeline 的注册中心（GTA_REGISTRY_ADDR）
# 3. 注册为 Decoder 插件
# 4. 启动心跳循环
```

---

## 3. plugin.yaml Manifest 详解

### 必填字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `api_version` | string | 固定为 `gta.decoder/v2` |
| `name` | string | 插件唯一标识，建议格式 `{author}-{protocol}` |
| `protocol` | string | 协议名称，slug 格式（小写字母/数字/连字符） |
| `type` | string | 固定为 `decoder` |

### 可选字段

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `protocol_version` | int | `1` | 协议版本 |
| `hints` | string | 空 | 逗号分隔的协议提示词，如 `tcp,dns` |
| `event` | string | 同 `protocol` | 输出事件名 |
| `meta` | map | 空 | 附加元数据 |

### 示例

```yaml
api_version: gta.decoder/v2
name: gta-http
protocol: http
protocol_version: 1
type: decoder
hints: tcp,dns
event: http
meta:
  author: gta-team
  description: HTTP protocol decoder
```

---

## 4. Decode RPC 契约 (v2)

### 通信方式

- **传输层**：Unix Domain Socket（Unix）、Named Pipe（Windows）或 TCP（跨机器）
- **RPC 框架**：gRPC 双向流（DecodeV2）
- **端点发现**：环境变量 `GTA_REGISTRY_ADDR` > `--registry` flag（**两者均未设置则 fatal，必须显式指定**）

### DecodeRequest

```protobuf
message DecodeRequest {
  string session_id   = 1;  // 会话 ID
  string protocol_hint = 2; // 协议提示（如 "http"）
  bytes  payload      = 3;  // 【完整链路层帧】，含 L2/L3/L4 头，未剥头！
  int32  link_type    = 4;  // DLT，决定如何剥头（见 §5）
  string input_id     = 5;  // 本次解码输入唯一标识，复用 packet_id
  string packet_id    = 6;  // 原始抓包 ID
  string flow_id      = 7;  // 五元组流 ID
  string src          = 8;  // "ip:port"
  string dst          = 9;  // "ip:port"
  string direction    = 10; // 方向（pipeline 推断，可经 _meta.direction 覆盖）
  int64  timestamp_ns = 11; // 包时间戳（Unix ns）
}
```

### DecodeResponseV2

```protobuf
message DecodeResponseV2 {
  string input_id          = 1; // 对应 DecodeRequest.input_id
  bool   done              = 2; // true = 该 input_id 结果已全部发完
  string event_type        = 3; // 事件类型，done=true 时为空
  string schema_id         = 4; // Schema 版本，如 "http.request.v1"
  bytes  payload_msgpack   = 5; // MsgPack 编码的 event.Value
  string error             = 6; // 错误信息（设置后代表解码失败）
  string correlation_key   = 7; // 业务关联键
  string causation_input_id = 8;// 指向导致本结果的 input_id
}
```

### 事件编码约定（v2：MsgPack，不再是 JSON `data`/`_fields`）

v2 **不再使用** `data`/`_fields` 顶层 JSON。每个事件是一个 `event.Value`（MsgPack 编码）写入 `payload_msgpack`。系统保留字段以 `_` 开头：`_meta`（方向、flow_id、msg_name 等）、`_state_changes`（状态变更声明）。

```go
import "github.com/OwnSecurityGuard/gta-plugin-sdk/event"

func emitEvent(stream pb.Decoder_DecodeV2Server, inputID string, m httpMsg) error {
	val := event.ValueFromAny(map[string]any{
		"type":   "http_request",
		"method": m.Method,
		"path":   m.Path,
		"headers": map[string]any{"host": m.Host},
		// 系统保留字段以下划线开头：
		"_meta": map[string]any{"direction": "client->server"},
	})
	mp, err := val.MarshalMsgpack()
	if err != nil {
		return err
	}
	return stream.Send(&pb.DecodeResponseV2{
		InputId:        inputID,
		EventType:      "http.request",
		SchemaId:       "http.request.v1",
		PayloadMsgpack: mp,
	})
}
```

**限制**：每次 `DecodeRequest` 至少回传一个 `{input_id, done:true}` 消息；无业务事件时只回传该空结果（不要静默不回传，否则 pipeline 会一直等待）。

---

## 5. 链路层剥头与 TCP 重组（framing 包）

> 这是 v1 指南最大的坑：旧文档说"payload 已是 L7，不要再剥头"。**那是错的**。下面是正确的做法。

### 5.1 为什么需要 framing

对于**每一个 pcap 类来源**，pipeline 交给插件的 `payload` 都是**完整链路层帧**：

```
DecodeRequest.payload = 链路层头 + IP + TCP/UDP + 应用字节
```

pipeline 解析 L2/L3/L4 只是为了填充 `link_type`、`src`、`dst`、`protocol_hint` 等上下文字段，**从不削减 payload 本身**。只有两个代理类 link_type —— `ProxyPayload`(1001) 和 `TLSPlaintext`(1002) —— 才是已经剥好的纯 L7。

因此每个解码器在开始解析业务字段前都需要两件与协议无关、且极易写错的事：
1. **按 `link_type` 剥封装**（链路/网络/传输头）。
2. **按流做 TCP 重组**，让跨多个报文的应用消息完整可用。

这两件事全部由 SDK 的 `framing` 包代劳，插件**不要**自己手写 `payload[14:]` 之类的偏移。

### 5.2 `framing.ExtractL7` —— 按 link_type 剥头

```go
seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
// ok == false: 非 IP 流量（ARP/ICMP）、截断帧或无法识别的 link_type。
//               属正常情况，回 done=true 即可，不要当作错误。
// ok == true 且 len(seg.Payload) == 0: 纯 ACK / 握手 / FIN，仍 Push 给重组器以维护流状态。
// seg.Flow / seg.Seq / seg.Flags / seg.IsTCP 由 ExtractL7 一并填好。
```

各 link_type 的剥头方式（contract.yaml `payload_framing` 段为权威定义）：

| link_type | 值 | 剥头方式 |
|-----------|----|----------|
| `Null`（BSD/Npcap 回环） | 0 | 4 字节 AF_* 头（主机字节序；gopacket 自动探测大小端） |
| `Ethernet` | 1 | 14 字节以太网头 |
| `Loop`（OpenBSD 回环） | 108 | 4 字节 AF_* 头（网络字节序） |
| `LinuxSLL` | 113 | 16 字节 cooked 头 |
| `Raw` / `RawIP` | 101 / 1000 | 无链路头，按首字节版本选 IPv4/IPv6 |
| `IEEE80211` | 105 | 802.11 头 |
| `ProxyPayload` | 1001 | 已是 L7，无需剥头 |
| `TLSPlaintext` | 1002 | 已是 L7，无需剥头 |

> **回环特别说明**：Npcap 在 Windows 上把本机(127.0.0.1)流量走回环接口，交付的帧带着 4 字节 AF_INET 头（主机字节序 `02 00 00 00`）。手写"跳过 4 字节"在 x86 上能跑，但换架构/OpenBSD 回环（`Loop`，大端）就会出错。`framing.ExtractL7` 用 gopacket 统一处理两种字节序，跨平台安全。

### 5.3 `framing.Reassembler` —— 按流 TCP 重组

```go
var ra = framing.NewReassembler() // 进程级单例，跨会话复用

func decode(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
	}
	s := ra.Push(seg) // 按 seg.Flow 维护 per-direction 重组缓冲
	for {
		raw := s.Bytes()              // 当前已重组好的连续字节
		msg, n := parseOneMessage(raw) // 业务解析：尽量多解一条完整消息
		if n == 0 {
			break                    // 不完整，等下一个 segment
		}
		s.Consume(n)                  // 推进重组前沿
		emitEvent(stream, req.GetInputId(), msg)
	}
	return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
}
```

`Reassembler` 处理：乱序到达（缓存于 oob 待缺口补齐）、重传去重、序列号回绕（uint32 模运算）、`FIN`/`RST`（FIN 排干后注销该流，RST 立即清空）。UDP 与代理类输入走"透传"路径（每个报文自包含，不拼接）。

并发安全：Push / Bytes / Consume / Forget / Reset 均 goroutine-safe，但**单条流的解析循环必须顺序执行**——在同一 goroutine 上调用 Bytes→解析→Consume→重复，不要并发读写同一流。

---

## 6. 完整示例：HTTP 流式解码器（v2，正确姿势）

```go
package main

import (
	"bufio"
	"bytes"
	"net/http"

	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

var ra = framing.NewReassembler()

func decode(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
	}
	s := ra.Push(seg)
	for {
		raw := s.Bytes()
		if len(raw) == 0 {
			break
		}
		// 尝试从 TCP 字节流中解析出一条完整 HTTP 消息。
		msg, n := parseHTTP(raw)
		if n == 0 {
			break // 头部/body 还不完整，等下一 segment
		}
		s.Consume(n)
		if err := emitHTTP(stream, req.GetInputId(), msg); err != nil {
			return err
		}
	}
	return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
}

func parseHTTP(raw []byte) (*http.Request, int) {
	r := bufio.NewReader(bytes.NewReader(raw))
	req, err := http.ReadRequest(r)
	if err != nil {
		return nil, 0 // 不完整
	}
	consumed := len(raw) - r.Buffered()
	return req, consumed
}

func emitHTTP(stream pb.Decoder_DecodeV2Server, inputID string, r *http.Request) error {
	val := event.ValueFromAny(map[string]any{
		"type":      "http_request",
		"method":    r.Method,
		"path":      r.URL.Path,
		"host":      r.Host,
		"_meta":     map[string]any{"direction": "client->server"},
	})
	mp, err := val.MarshalMsgpack()
	if err != nil {
		return err
	}
	return stream.Send(&pb.DecodeResponseV2{
		InputId:        inputID,
		EventType:      "http.request",
		SchemaId:       "http.request.v1",
		PayloadMsgpack: mp,
	})
}

func main() {
	sdk.RunRegisterLoop(decode)
}
```

---

## 7. 0 事件排查手册（当你解析出 0 个事件时）

0 事件几乎总是同一个根因：**把完整帧当成了 L7**。按以下顺序排查，不要先怀疑自己的协议解析。

1. **先 `sample_bytes_plugin` 看首字节。** 这是契约 `inspect-bytes-first`(error) 要求的首要动作——在任何剥头假设之前，先确认线上到底交付了什么。
   - 首字节为 `0x45`/`0x46`…（IPv4 版本号 4）：链路头已被剥，是 RawIP 类来源。
   - 首字节 `0x02 0x00 0x00 0x00`（回环 AF_INET）：带 4 字节回环头的完整帧。
   - 首字节 `0x45` 但来自以太网：其实前面还应有 14 字节以太头（说明链路头没剥）。
2. **确认是否用了 `framing.ExtractL7`。** 任何 pcap 来源都必须先 `ExtractL7`；不要手写 `payload[14:]`。只有 `ProxyPayload`/`TLSPlaintext` 才无需剥头。
3. **TCP 类协议必须接 `Reassembler`。** 典型症状：HTTP body 恒为空、只解到第一条不完整消息、或 `Bytes()` 始终为空（乱序/缺口）。这是 `tcp-reassembly-required`(warn) 直接对应的坑。
4. **回环流量**最容易踩：127.0.0.1 通信在 Npcap 上走回环接口，帧带 4 字节头。纯 L7 解码器会逐字节错位，整条流解析失败 → 0 事件。
5. **`verify_plugin`** 会把离线会话的原始包喂给你的解码器，并对照 contract.yaml 规则（含 `payload-framing-by-link-type`/`link-type-selects-framing`）给出 `pass|warn|fail` 判定与证据。0 事件时优先跑它。
6. **`explain_plugin`** 对 0 事件给出归因与修复建议（已修正：不再误导"payload 已是 L7"）。

> 踩坑实录：曾有人坚信"payload 已是 L7"（这条经证伪的旧规则原叫 `payload-is-l7`，现行 SSOT 规则为 `payload-framing-by-link-type`(error)：必须按 link_type 剥头），写出纯 L7 解码器，回环帧导致 0 事件，转而用 Python 手剥字节定位，再补 TCP 重组——耗费大量调试 token。正确做法是开发前先读契约，0 事件先 `sample_bytes_plugin`，回环解码器直接内置 `ExtractL7` + `Reassembler`。

---

## 8. 离线 pcap 回放循环（不抓包也能开发/验证）

无需真实网卡，用 `start_capture(pcap_file=...)` 把离线 pcap 灌入 pipeline，再用 `verify_plugin` 跑解码。推荐开发闭环：

```
1. 准备一份真实抓包帧（含链路层头），覆盖回环与以太网两种 link_type。
2. MCP: start_capture(pcap_file="fixtures/http_loop.pcap")   # 离线回放
3. MCP: verify_plugin(name="my-http")                        # 解码 + 契约校验
4. 看 unknown 比例 / 0 事件 / 重组缺口提示，回到代码修 framing。
5. 反复 2-4，直到 verify 通过、list_decoded_data 能看到业务事件。
```

要点：
- fixture 必须**保留链路层头**（契约 `real-fixture-required`）。用 `tcpdump -w` / Wireshark 导出的原始帧即可，不要用已"Follow TCP Stream"导出的纯文本（那已经是 L7，掩盖了剥头问题）。
- `sample_bytes_plugin` 有上限（默认 20 包 / 64 字节），仅用于"看首字节"，不足以替代完整回放。

---

## 9. 注册与生命周期

### 插件启动流程

```
1. 读取当前目录 plugin.yaml → manifest bytes
2. 端点发现：GTA_REGISTRY_ADDR > --registry（未设置则直接 fatal，必须显式指定）
3. 建立 gRPC 连接到 RegistryServer
4. 调用 Register RPC（携带 manifest）
5. 注册成功 → 启动心跳循环（默认 5s）
6. Pipeline 发送 Decode RPC → 插件处理
7. 插件异常退出 → 心跳超时 → Registry 自动清理
```

### 环境变量

| 变量 | 说明 | 示例 |
|------|------|------|
| `GTA_REGISTRY_ADDR` | 注册中心地址（**必须显式设置**，无默认兜底） | 见下方说明 |
| `GTA_DECODER_ADDR` | 插件 Decode 服务监听地址（跨机器设 TCP `host:port`；留空=本地 socket） | `0.0.0.0:9092` |
| `GTA_DECODER_PUBLIC_ADDR` | pipeline 回拨插件的可达地址（跨机器必填，填插件真实 IP） | `10.12.34.57:9092` |

> **`GTA_REGISTRY_ADDR` 的真实取值**：由 gta-pipeline 启动时打印在日志 `GTA_REGISTRY_ADDR` 字段中。
> - Windows（命名管道）：`npipe:\\.\pipe\gta-registry`
> - Linux/macOS（Unix socket）：`unix:<workdir>/run/registry.sock`
> - 跨机器（TCP）：`host:port`（gta-pipeline 用 `-registry-addr` 开启）
>
> SDK **不再提供默认地址**——未设置 `GTA_REGISTRY_ADDR` 会直接 fatal，避免连到错误端点。必须显式设置（或 `--registry=`）。
> 插件进程运行时的工作目录需包含 `plugin.yaml`。
| `GTA_DECODER_UNIX_SOCKET_DIR` | Unix socket 目录（pipeline 侧） | `./work` |

### 跨机器部署（进程不在同一台机器）

三个进程（gta-pipeline / 插件 / gta-mcp）**不要求共享 workDir**，全部通过显式网络地址互联：

- **插件 → registry（注册/心跳）**：`GTA_REGISTRY_ADDR=host:port`（pipeline 用 `-registry-addr :9091` 监听 TCP）。
- **pipeline → 插件 Decode（实际解码）**：插件用 `GTA_DECODER_ADDR=0.0.0.0:9092` 监听 TCP，并用 `GTA_DECODER_PUBLIC_ADDR=10.12.34.57:9092` 上报 pipeline 实际可达地址。
- **gta-mcp → pipeline（控制面）**：gta-pipeline 用 `-control-addr :9888` 监听 TCP，gta-mcp 用 `-pipeline-addr host:port` 连接。

地址支持形式：`host:port`（TCP）、`unix:/path`、`npipe:\\.\pipe\name`、裸路径（按 Unix socket 处理）。

### 退出处理

- **正常退出**：插件进程 exit → Registry 心跳超时（默认 15s）→ 自动注销
- **异常退出**：同理，无需手动 deregister
- **优雅关闭**：建议捕获 SIGTERM，停止接收新包后退出

---

## 10. 最佳实践

### 10.1 始终 defer recover

`DecodeFuncV2` 外层已由 SDK 的 `DecodeV2` 捕获 panic 并回传 `{error, done:true}`，但业务解析内部仍建议自保：

```go
func parseOneMessage(raw []byte) (msg any, n int) {
	defer func() {
		if r := recover(); r != nil {
			// 单个 malformed 包不应导致整个插件崩溃
			msg, n = nil, 0
		}
	}()
	// 你的解析逻辑
}
```

### 10.2 不要关闭 pipeline 的流

```go
// ❌ 错误：不要主动关闭 gRPC 流
stream.CloseSend()  // 禁止！

// ✅ 正确：只发结果，由 pipeline 控制流生命周期
```

### 10.3 用 framing 而非手写偏移

```go
// ❌ 错误：假设永远是以太网 + 假设已剥头
body := req.Payload[14:]

// ✅ 正确：按 link_type 剥头 + 重组
seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
if !ok || len(seg.Payload) == 0 {
	return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
}
```

### 10.4 事件编码用 event.Value + MsgPack

```go
import "github.com/OwnSecurityGuard/gta-plugin-sdk/event"

val := event.ValueFromAny(map[string]any{
	"type": "game.login",
	"uid":  12345,
	"_meta": map[string]any{"flow_id": req.GetFlowId()},
})
mp, _ := val.MarshalMsgpack()
stream.Send(&pb.DecodeResponseV2{
	InputId:        req.GetInputId(),
	EventType:      "game.login",
	SchemaId:       "game.login.v1",
	PayloadMsgpack: mp,
})
```

### 10.5 复用 SDK 提供的工具

```go
import "github.com/OwnSecurityGuard/gta-plugin-sdk"

// 读取 manifest（自动校验）
manifestBytes, err := sdk.ReadManifest()

// 解析 registry 地址（env > flag，无默认）
addr := sdk.ResolveRegistryAddr()
```

---

## 11. MCP 工具参考

通过 gta-mcp 可调用以下工具（开发/验证插件时高频使用）：

| 工具 | 功能 |
|------|------|
| `get_plugin_contract` | 获取 SDK `contract/contract.yaml` 全文（**SSOT**，写/审插件代码前必读） |
| `get_plugin_dev_guide` | 获取本开发指南 |
| `create_plugin` | 生成插件项目骨架 |
| `sample_bytes_plugin` | **看首个包的字节**，确认 link_type 与帧结构（0 事件排查第一步） |
| `verify_plugin` | 离线回放原始包 + 契约校验，给出 `pass\|warn\|fail` 判定与证据 |
| `explain_plugin` | 对解码结果（含 0 事件）做归因与修复建议 |
| `list_registered_plugins` | 列出已注册插件 |
| `get_plugin_manifest` | 获取插件 manifest |
| `deregister_plugin` | 注销插件 |
| `set_session_plugin` | 运行时热切换会话绑定的解码插件（无需停抓） |
| `start_capture` / `stop_capture` | 启动/停止抓包（`pcap_file=` 可离线回放） |
| `list_decoded_data` | 查询已解码事件 |

---

> 约定速记：
> - **payload 是完整帧**，先 `ExtractL7` 再 `Reassembler`。
> - 只有 `ProxyPayload`(1001) / `TLSPlaintext`(1002) 是纯 L7。
> - 0 事件先 `sample_bytes_plugin`，再 `verify_plugin`。
> - v2 事件用 `event.Value` + MsgPack，顶层不再有 `data`/`_fields`。
