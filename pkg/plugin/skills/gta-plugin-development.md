# GTA Plugin 开发指南

> 基于 [contract.yaml](./contract.yaml) 派生的插件开发规范，供开发者快速上手。

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
│  │ (http插件)   │   unix socket                   │
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
| `api_version` | 契约版本，如 `gta.decoder/v2` |
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
├── go.mod               # 模块定义
└── README.md            # 说明文档
```

### 步骤 2：实现 Decode 函数

插件的核心是 `decodePacketV2` 函数（DecodeV2 流式子接口），接收原始字节并通过 stream 回传解码事件：

```go
package main

import (
    "fmt"

    "gta-plugin-sdk"
    pb "gta/pkg/plugin/proto"
)

func decodePacketV2(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
    // TODO: 实现你的协议解码逻辑
    // 输入：req.Payload 是原始网络包字节
    // 通过 stream.Send 回传一个或多个 DecodeResponseV2，顶层只允许 "data" 和 "_fields"
    // 全部结果发完后，发送 Done: true 标记结束

    if err := stream.Send(&pb.DecodeResponseV2{
        InputId:   req.InputId,
        EventType: "my.protocol",
        SchemaId:  "my.protocol.v1",
        // PayloadMsgpack 由 event.Value 的 MsgPack 编码得到，示例用空占位
        PayloadMsgpack: []byte{},
    }); err != nil {
        return err
    }
    return stream.Send(&pb.DecodeResponseV2{InputId: req.InputId, Done: true})
}

func main() {
    sdk.RunRegisterLoop(decodePacketV2)
}
```

### 步骤 3：构建并运行

```bash
cd plugins/my-http
go build -o my-http-plugin .
# 插件会自动：
# 1. 读取 plugin.yaml 中的 manifest
# 2. 连接到 gta-pipeline 的注册中心
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

## 4. Decode RPC 契约

### 通信方式

- **传输层**：Unix Domain Socket（Unix）或 Named Pipe（Windows）
- **RPC 框架**：gRPC 双向流
- **端点发现**：环境变量 `GTA_REGISTRY_ADDR` > `--registry` flag（**两者均未设置则报错，必须显式指定**）

### DecodeRequest

```protobuf
message DecodeRequest {
    string session_id    = 1;  // 会话 ID
    string protocol_hint = 2;  // 协议提示（如 "http"）
    bytes  payload       = 3;  // 原始网络包字节
}
```

### DecodeResponse

```protobuf
message DecodeResponse {
    string session_id  = 1;
    bytes  json        = 2;  // 解码后的 JSON（可选）
    string error       = 3;  // 错误信息（可选）
}
```

### JSON 输出约定

解码后的 JSON 必须遵循以下约定（见 [contract.yaml](./contract.yaml)）：

```json
{
  "data": {
    "type": "http_request",
    "method": "GET",
    "path": "/",
    "headers": { ... }
  },
  "_fields": {
    "session_id": "abc123"
  }
}
```

**限制**：顶层只允许 `data` 和 `_fields` 两个键。

---

## 5. 注册与生命周期

### 插件启动流程

```
1. 读取当前目录 plugin.yaml → manifest bytes
2. 端点发现：GTA_REGISTRY_ADDR > --registry（未设置则直接报错，必须显式指定）
3. 建立 gRPC 连接到 RegistryServer
4. 调用 Register RPC（携带 manifest）
5. 注册成功 → 启动心跳循环（每 5s）
6. Pipeline 发送 Decode RPC → 插件处理
7. 插件异常退出 → 心跳超时 → Registry 自动清理
```

### 环境变量

| 变量 | 说明 | 示例 |
|------|------|------|
| `GTA_REGISTRY_ADDR` | 注册中心地址（**必须显式设置**，无默认兜底） | 见下方说明 |
| `GTA_DECODER_ADDR` | 插件 Decode 服务监听地址（跨机器设 TCP `host:port`；留空=本地 Unix socket） | `0.0.0.0:9092` |
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

三个进程（gta-pipeline / 插件 / gta-mcp）**不再要求共享 workDir**，全部通过显式网络地址互联：

- **插件 → registry（注册/心跳）**：`GTA_REGISTRY_ADDR=host:port`（pipeline 用 `-registry-addr :9091` 监听 TCP）。
- **pipeline → 插件 Decode（实际解码）**：插件用 `GTA_DECODER_ADDR=0.0.0.0:9092` 监听 TCP，
  并用 `GTA_DECODER_PUBLIC_ADDR=10.12.34.57:9092` 上报 pipeline 实际可达地址。
- **gta-mcp → pipeline（控制面）**：gta-pipeline 用 `-control-addr :8088` 监听 TCP，
  gta-mcp 用 `-pipeline-addr host:port` 连接。

地址支持形式：`host:port`（TCP）、`unix:/path`、`npipe:\\.\pipe\name`、裸路径（按 Unix socket 处理）。


### 退出处理

- **正常退出**：插件进程 exit → Registry 心跳超时（默认 15s）→ 自动注销
- **异常退出**：同理，无需手动 deregister
- **优雅关闭**：建议捕获 SIGTERM，停止接收新包后退出

---

## 6. 最佳实践

### 6.1 始终 defer recover

```go
func decodePacket(req *pb.DecodeRequest) ([]byte, error) {
    defer func() {
        if r := recover(); r != nil {
            log.Printf("panic recovered: %v", r)
        }
    }()
    // 你的解码逻辑
}
```

**原因**：单个 malformed 包不应导致整个插件进程崩溃。

### 6.2 不要关闭 pipeline 的流

```go
// ❌ 错误：不要主动关闭 gRPC 流
stream.CloseSend()  // 禁止！

// ✅ 正确：让 pipeline 控制流的关闭
func decodePacket(req *pb.DecodeRequest) ([]byte, error) {
    // 只做解码，不关闭任何连接
}
```

**原因**：流由 pipeline 管理，插件不应干涉。

### 6.3 检测 HTTP body

```go
func decodePacket(req *pb.DecodeRequest) ([]byte, error) {
    // 检查是否是 HTTP 请求（基于 hints）
    if !isHTTPPayload(req.Payload) {
        return nil, nil  // 返回 nil 表示"这不是我的协议"
    }
    // 解码并返回 JSON
    return jsonBytes, nil
}

func isHTTPPayload(payload []byte) bool {
    return bytes.HasPrefix(payload, []byte("GET ")) ||
           bytes.HasPrefix(payload, []byte("POST ")) ||
           bytes.HasPrefix(payload, []byte("PUT ")) ||
           bytes.HasPrefix(payload, []byte("DELETE "))
}
```

### 6.4 使用 SDK 提供的工具

```go
import "gta-plugin-sdk"

// 读取 manifest（自动校验）
manifestBytes, err := sdk.ReadManifest()

// 解析 registry 地址（env > flag > 默认）
addr := sdk.ResolveRegistryAddr()
```

---

## 7. 完整示例：HTTP 插件

参考 [examples/plugins/http-sdk/](../../../examples/plugins/http-sdk/)：

```go
package main

import (
    "encoding/json"
    "net/http"
    "strings"

    "gta-plugin-sdk"
    pb "gta/pkg/plugin/proto"
)

func decodePacket(req *pb.DecodeRequest) ([]byte, error) {
    payload := string(req.Payload)
    
    // 检测 HTTP 请求
    if !strings.HasPrefix(payload, "HTTP/") &&
       !strings.HasPrefix(payload, "GET ") &&
       !strings.HasPrefix(payload, "POST ") {
        return nil, nil
    }
    
    // 解析 HTTP 请求
    r, err := http.ReadRequest(bufio.NewStringReader(payload))
    if err != nil {
        return nil, err
    }
    
    // 构建输出 JSON
    result := map[string]any{
        "data": map[string]any{
            "type":    "http_request",
            "method":  r.Method,
            "path":    r.URL.Path,
            "headers": flattenHeaders(r.Header),
        },
        "_fields": map[string]string{
            "session_id": req.SessionId,
        },
    }
    
    return json.Marshal(result)
}

func flattenHeaders(h http.Header) map[string]string {
    m := make(map[string]string)
    for k, v := range h {
        m[k] = strings.Join(v, "; ")
    }
    return m
}

func main() {
    sdk.RunRegisterLoop(decodePacket)
}
```

---

## 8. MCP 工具参考

以下工具可通过 gta-mcp 调用：

| 工具 | 功能 |
|------|------|
| `get_plugin_contract` | 获取 contract.yaml 全文 |
| `get_plugin_dev_guide` | 获取本开发指南 |
| `create_plugin` | 生成插件项目骨架 |
| `list_registered_plugins` | 列出已注册插件 |
| `get_plugin_manifest` | 获取插件 manifest |
| `deregister_plugin` | 注销插件 |
