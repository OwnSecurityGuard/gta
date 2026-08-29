# 成员上手指南

一页速览：拿到服务器地址和你的 token 后，如何把本机接进团队 GTA。服务器侧的部署由管理员完成（见 [团队部署指南](team-deployment.md)）。

## 你需要拿到两样东西

1. **服务器地址**：`<服务器IP>`（管理员会告知，插件注册走 `9091`、抓包推流走 `9092`、MCP 走 `8781`）；
2. **你的 token**：形如 `gta_tok_xxxx`，人手一份。它就是你的身份（owner），会话与插件都归属它，**不要共享**。

## 1. 安装 gta-agent

- **Windows**：安装 [Npcap](https://npcap.com/)（勾选 "WinPcap API-compatible Mode"）；
- **Linux**：`sudo apt install libpcap-dev`（或发行版等价包）。

然后从项目 Release 页下载对应平台的 `gta-agent`，或自行构建：

```bash
go build -tags pcap -o gta-agent ./cmd/gta-agent   # 本机抓包需要 -tags pcap
```

> 只想**托管插件**、不参与抓包的话，任何构建都可以——不加 `--session/--iface` 即可，无需 pcap。

## 2. 只托管本机插件（最短路径）

把编译好的插件二进制放进 gta-agent 同目录的 `plugins/` 下，然后：

```bash
gta-agent --token gta_tok_xxxx --server <服务器IP>:9091
```

gta-agent 会自动发现 `plugins/` 下的插件进程并拉起，以**隧道模式**注册到服务器：它给插件注入 `GTA_TUNNEL=1`、`GTA_REGISTRY_ADDR`、`GTA_AUTH_TOKEN` 环境变量，插件（用官方脚手架 `create_plugin` 生成的模板即原生支持，读取 `GTA_TUNNEL` 决定走隧道）经 gRPC 连到 `:9091`。插件崩溃会被自动按退避重启。

此后团队在服务器侧 `list_registered_plugins` 就能看到你的插件（命名空间是你的 owner）。

## 3. 参与一次抓包（owner 发起、成员推流）

1. **owner（或你自己的 MCP 客户端）**在服务器上开一个 agent 来源的会话：

   ```
   start_capture(source="agent", plugin=<可选>)   # 记下返回的 session_id
   ```

2. **你**在本机运行（`--session` 用上一步的 session_id）：

   ```bash
   gta-agent --token gta_tok_xxxx --server <服务器IP>:9091 \
     --session <session_id> --iface 以太网 --filter "port 8984"
   ```

   - `--iface`：抓包网卡名（Windows 如 `以太网`，Linux 如 `eth0`）；
   - `--filter`：BPF 过滤表达式，建议加上，控制上行带宽；
   - `--session` 留空 = 只托管插件、不抓包。

3. owner 侧 `get_session_status` 确认 `packets_in` 增长，`list_decoded_data` 看解码事件。

推流按批聚合（默认 128 包 / 200ms 兜底，可用 `--batch-size`、`--batch-interval` 调整）。**断网重连**：agent 与插件进程都会自动按指数退避重连/重启，恢复网络后继续入库，无需人工干预。

## 4. 常见问题

| 现象 | 排查 |
|---|---|
| 请求返回 401 unauthorized | token 错了或没带（MCP 请求需 `Authorization: Bearer gta_tok_xxxx`） |
| 能连上但看不到数据 | 确认 `--session` 与 owner 的 start_capture 返回值一致；确认防火墙放行 9091/9092 |
| 抓包启动失败 "capture unavailable" | 未装 Npcap/libpcap，或二进制没用 `-tags pcap` 构建（只托管插件就去掉 `--session/--iface`） |
| 插件没注册上 | 插件需支持隧道模式（官方脚手架模板原生支持）；看 `plugins/` 下 agent 打印的插件日志 |
| 想换身份 | 换 token 即换 owner；别人的会话/插件你看不到也管不了（admin 除外） |
