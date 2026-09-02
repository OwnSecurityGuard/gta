# 成员上手指南

一页速览：如何把本机接进团队 GTA。推荐走 **启动码一键接入**（无需手填 token / 回连地址 / 会话）；高级用户可继续用自定义下载与手动参数。服务器侧部署由管理员完成（见 [团队部署指南](team-deployment.md)）。

## 你需要拿到一样东西

**一个启动码**：形如 `GTA-XXXX-XXXX`。在网页打开「我的接入」（顶部工具栏点下载图标），选目标操作系统与抓包端口（可选绑定项目），点「生成启动码」，复制生成的命令即可。

## 1. 启动码一键接入（推荐）

在目标电脑上打开终端，执行网页「我的接入」里复制到的命令：

- **Linux / macOS**：`curl -fsSL "<服务器>/setup.sh?code=GTA-XXXX-XXXX&platform=linux/amd64" | bash`；
- **Windows**：在 **PowerShell** 粘贴复制到的命令（脚本会自动先领配置、再下载并启动 agent）。

脚本会自动完成：领取配置（token / 会话 / 抓包端口 BPF）→ 下载对应平台的 agent → 写入配置 → 启动。**全程无需手动填 token、回连地址或会话 ID**，agent 会自动注册到你的项目（若绑定）并开始向服务端回推该端口的抓包数据。抓包前本机需：

- **Windows**：安装 [Npcap](https://npcap.com/)（勾选 "WinPcap API-compatible Mode"）；
- **Linux**：`sudo apt install libpcap-dev`（或发行版等价包）。

> 启动码 **一次性 + 24 小时有效**，过期需在「我的接入」重新生成。抓包会话在 agent 首次领取配置时由服务端自动创建，后续可在网页「连接 / 时间线」查看分析。

## 2. 只托管本机插件（不抓包）

把编译好的插件二进制放进 gta-agent 同目录的 `plugins/` 下，然后：

```bash
gta-agent --token gta_tok_xxxx --server <服务器IP>:9091
```

gta-agent 会自动发现 `plugins/` 下的插件进程并拉起，以**隧道模式**注册到服务器。插件（官方脚手架 `create_plugin` 生成的模板原生支持）经 gRPC 连到 `:9091`，崩溃会被自动按退避重启。此后服务器侧 `list_registered_plugins` 就能看到你的插件。

## 3. 高级：自定义下载 / 手动抓包

在「我的接入」切到 **高级下载**，可手动选端口、解码插件与回连地址，下载 zip（通用二进制 + sidecar 配置），解压双击运行即可。或在启动码之外，按需手动开会话并推流：

```bash
start_capture(source="agent", plugin=<可选>)   # 记下 session_id
gta-agent --token gta_tok_xxxx --server <服务器IP>:9091 \
  --session <session_id> --iface <网卡> --filter "port 8984"
```

- `--iface`：抓包网卡名（Windows 如 `以太网`，Linux 如 `eth0`）；
- `--filter`：BPF 过滤表达式（建议加，控制上行带宽）；
- `--session` 留空 = 只托管插件、不抓包。

开发者想写/构建/验证插件，可在网页「更多 → 开发者工具」中完成（脚手架、编译、归因）。

## 4. 常见问题

| 现象 | 排查 |
|---|---|
| 启动码提示无效或已过期 | 重新在「我的接入」生成；确认复制的命令里 code 完整 |
| 请求返回 401 unauthorized | 高级手填场景：token 错了或没带（`Authorization: Bearer gta_tok_xxxx`） |
| 能连上但看不到数据 | 确认抓包端口来源的会话与「我的接入」生成时一致；确认防火墙放行 9091/9092 |
| 抓包启动失败 "capture unavailable" | 未装 Npcap/libpcap，或二进制没用 `-tags pcap` 构建 |
| 插件没注册上 | 插件需支持隧道模式（官方脚手架模板原生支持）；看 `plugins/` 下 agent 的插件日志 |
| 想换身份 | 换 token 即换 owner；别人的会话/插件你看不到也管不了（admin 除外） |