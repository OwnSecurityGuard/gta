# Dockerfile（T15）——服务端镜像：gt-pipeline + gt-mcp。
#
# 多阶段构建：
#   webui   : node:22-bookworm-slim 构建 web/ 前端（Vite 产物 dist）；
#   builder : golang:1.25-bookworm + libpcap-dev，以 cgo（pcap）编译两个服务端二进制；
#             前端产物先 COPY 进 cmd/gt-mcp/webui/，由 //go:embed 嵌入 gt-mcp；
#   runtime : debian:bookworm-slim + libpcap0.8（gopacket/pcap 运行时需要的共享库）。
#
# Web UI：浏览器直接访问 http://<host>:8781（静态资源免鉴权，API 语义不变）。
#
# 远程 Agent 预置二进制：gt-mcp 的 /download/agent 只下发预置产物（见
#   cmd/gt-mcp/agent_download.go 的 agentBinDir/availableAgentPlatforms），不再现场
#   编译。全平台 cgo/pcap 产物无法在单一 builder 内交叉编译，因此镜像默认不烧入，
#   由 docker-compose 把宿主 `./build/agents`（make build-agents 产出）只读挂载到
#   GT_AGENT_BIN_DIR 指向的 /opt/gametrace/agents。缺产物的下载平台会被如实标为不可用。
#   runtime 保留 Go 工具链是为 gt-mcp 内嵌 Developer Plane 现场编译插件
#   （pkg/plugindev/build.go 的 build_plugin），与 agent 无关。
#
# pcap 说明：pipeline 仍可在服务端本地开 pcap 源（实时网卡抓包 / pcap 文件源），
# 因此镜像带 pcap（cgo）编译；agent（gt-agent）推流入口是纯 Go gRPC，服务端
# 不依赖 agent 端的 pcap。
#
# 构建（版本注入与 Makefile 的 -X ldflags 同源，见 pkg/version/version.go）：
#   docker build --build-arg VERSION=v0.5.0 --build-arg GIT_COMMIT=abc1234 -t gt-server .

# ============================================================================
# 阶段 0：webui（前端构建）
# ============================================================================
# node:22 构建 web/（React 19 + Vite），产物经 builder 阶段 COPY 进
# cmd/gt-mcp/webui/ 后由 //go:embed 嵌入 gt-mcp——runtime 阶段零新增文件。
#
# npm registry 默认走 npmmirror（与 GOPROXY 默认 goproxy.cn 的境内网络取向
# 一致）；境外构建可覆盖：
#   docker build --build-arg NPM_REGISTRY=https://registry.npmjs.org .
# VITE_ENABLE_RAW_DEBUG=1 可开启前端「原始包」调试页（构建期静态替换，默认关闭）。
FROM node:22-bookworm-slim AS webui

ARG NPM_REGISTRY=https://registry.npmmirror.com
ARG VITE_ENABLE_RAW_DEBUG=0
ENV VITE_ENABLE_RAW_DEBUG=${VITE_ENABLE_RAW_DEBUG}

WORKDIR /src

# 先只拷 package.json/package-lock.json 做 npm ci，充分利用层缓存。
COPY web/package.json web/package-lock.json ./
RUN npm config set registry ${NPM_REGISTRY} && npm ci

COPY web/ ./
RUN npm run build

# ============================================================================
# 阶段 1：builder
# ============================================================================
FROM golang:1.25-bookworm AS builder

ARG VERSION=dev
ARG GIT_COMMIT=unknown

# 模块代理：镜像默认 GOPROXY 是 proxy.golang.org（国内网络直连不通，表现为
# `dial tcp ...:443: connect: connection refused`）。默认改用 goproxy.cn；
# 境外环境可覆盖：docker build --build-arg GOPROXY=https://proxy.golang.org,direct
# GOSUMDB 用 goproxy.cn 提供的校验和数据库镜像，避免 sum.golang.org 同样不可达。
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}

# pcap 采集层是 cgo 依赖，编译期需要 libpcap 头文件。
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libpcap-dev \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /src

# 先只拷贝 go.mod/go.sum 做 go mod download，充分利用层缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 前端产物嵌入 gt-mcp（//go:embed cmd/gt-mcp/webui）。.dockerignore 已把
# 本地 webui 产物挡在上下文外（目录里只有 .gitkeep），node 阶段产物是唯一
# 来源——镜像内前端永远与本次构建的源码同步，不残留陈旧 hash 产物。
COPY --from=webui /src/dist ./cmd/gt-mcp/webui/

RUN CGO_ENABLED=1 \
	go build -tags pcap -trimpath \
	-ldflags "-s -w -X gametrace/pkg/version.Version=${VERSION} -X gametrace/pkg/version.Commit=${GIT_COMMIT}" \
	-o /out/gt-pipeline ./cmd/gt-pipeline \
	&& CGO_ENABLED=1 \
	go build -tags pcap -trimpath \
	-ldflags "-s -w -X gametrace/pkg/version.Version=${VERSION} -X gametrace/pkg/version.Commit=${GIT_COMMIT}" \
	-o /out/gt-mcp ./cmd/gt-mcp \
	&& CGO_ENABLED=0 \
	go build -trimpath \
	-ldflags "-s -w -X gametrace/pkg/version.Version=${VERSION} -X gametrace/pkg/version.Commit=${GIT_COMMIT}" \
	-o /out/gt-singbox-agent ./cmd/gt-singbox-agent

# ============================================================================
# 阶段 2：runtime
# ============================================================================
FROM debian:bookworm-slim

# libpcap0.8：gopacket/pcap（cgo）的运行时共享库；ca-certificates：出站 TLS。
# 不再安装 gcc/libpcap-dev：原为远程 agent 现场编译（-tags pcap 走 cgo）所需，
# 现 agent 改走预置二进制；build_plugin 编译插件是纯 Go，亦无需 gcc。
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libpcap0.8 ca-certificates \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --system --create-home --home-dir /data gametrace \
	# 预建插件目录：进程只在 scaffold 时懒创建它，首次 list/build 前不存在会让
	# 插件根目录状态依赖调用顺序。注意已存在的命名卷不会回填镜像内容，
	# 升级部署时需自行 mkdir 并 chown gametrace。
	&& mkdir -p /data/plugins \
	&& chown -R gametrace:gametrace /data

COPY --from=builder /out/gt-pipeline /usr/local/bin/gt-pipeline
COPY --from=builder /out/gt-mcp /usr/local/bin/gt-mcp
COPY --from=builder /out/gt-singbox-agent /usr/local/bin/gt-singbox-agent
RUN chmod +x /usr/local/bin/gt-singbox-agent

# == Developer Plane 插件编译：gt-mcp 内嵌的 PluginDev 会现场 `go build` 插件
#    （pkg/plugindev/build.go），故 runtime 保留 Go 工具链 + 模块缓存；构建缓存落
#    /data（gametrace 可写 HOME）。远程 Agent 已改「预置二进制」，不再现场编译，因此不
#    携带 gt-agent 源码、也不装 gcc/libpcap-dev（见上方 apt 安装）。
COPY --from=builder /usr/local/go /usr/local/go
COPY --from=builder /go/pkg/mod /go/pkg/mod

ENV PATH=/usr/local/go/bin:${PATH} \
	# 本地模块缓存：复用镜像内置缓存，避免插件编译时再访问网络
	GOPATH=/go \
	GOMODCACHE=/go/pkg/mod \
	# Go 构建缓存落数据卷（gametrace 可写 HOME）；GOTOOLCHAIN=local 防止自动下载更高版本 Go
	GOCACHE=/data/.cache/go-build \
	GOTOOLCHAIN=local \
	GOPROXY=https://goproxy.cn,direct \
	GOSUMDB=sum.golang.google.cn \
	# 远程 agent 预置产物目录（见下方 mkdir）：agentBinDir() 优先读此变量，否则会
	# 回退到 WORKDIR(/data) 下的 ./build/agents —— 与 docker-compose 挂载点不一致。
	GT_AGENT_BIN_DIR=/opt/gametrace/agents

# 远程 agent 预置产物目录：镜像默认不烧入（全平台 cgo/pcap 产物无法在单一 builder
# 交叉编译），由 docker-compose 把宿主 `./build/agents` 只读挂载到这里。该路径即
# cmd/gt-mcp/agentBinDir 的 GT_AGENT_BIN_DIR 来源，agent 下载按此目录扫可用平台。
RUN mkdir -p /opt/gametrace/agents && chown gametrace:gametrace /opt/gametrace/agents

RUN chown -R gametrace:gametrace /go

# 统一配置（T10）：工作目录落 /data（卷挂载点），所有地址可用 GT_* 环境变量覆盖。
# WORKDIR 一并设为 /data：任何相对路径（含 flag 默认值 "." / "plugins"）都锚在数据卷上，
# 不会掉进容器根目录 / 导致非 root 用户写文件失败（unable to open database file）。
ENV GT_HOME=/data
WORKDIR /data
VOLUME ["/data"]

# 9888 CaptureControl | 9091 PluginRegistry | 9092 AgentIngest | 8781 MCP HTTP/SSE + Web UI
EXPOSE 9888 9091 9092 8781

# 默认跑 pipeline；gt-mcp 用法见 docker-compose.yml（覆盖 entrypoint）。
# 非 root 运行：useradd --create-home 已建 /data 并归 gametrace 所有（T15 评审修复）。
USER gametrace
ENTRYPOINT ["/usr/local/bin/gt-pipeline"]
