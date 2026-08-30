# Dockerfile（T15）——服务端镜像：gta-pipeline + gta-mcp。
#
# 多阶段构建：
#   builder : golang:1.25-bookworm + libpcap-dev，以 cgo（pcap）编译两个服务端二进制；
#   runtime : debian:bookworm-slim + libpcap0.8（gopacket/pcap 运行时需要的共享库）。
#
# 远程 Agent 即时编译（方案一）：gta-mcp 的 /download/agent 会在服务端用
#   go build -tags "embedded pcap" 现场编译 gta-agent 下载产物。为此 runtime 镜像
#   额外携带了 Go 工具链、Go 模块缓存、gta-agent 源码（GTA_AGENT_SRC_DIR），以及
#   cgo 编译所需的 gcc + libpcap-dev。源码目录对运行用户可写，以便写入/清理
#   config.embedded.json（go:embed 配置）。
#
# pcap 说明：pipeline 仍可在服务端本地开 pcap 源（实时网卡抓包 / pcap 文件源），
# 因此镜像带 pcap（cgo）编译；agent（gta-agent）推流入口是纯 Go gRPC，服务端
# 不依赖 agent 端的 pcap。
#
# 构建（版本注入与 Makefile 的 -X ldflags 同源，见 pkg/version/version.go）：
#   docker build --build-arg VERSION=v0.5.0 --build-arg GIT_COMMIT=abc1234 -t gta-server .

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

RUN CGO_ENABLED=1 \
	go build -tags pcap -trimpath \
	-ldflags "-s -w -X gta/pkg/version.Version=${VERSION} -X gta/pkg/version.Commit=${GIT_COMMIT}" \
	-o /out/gta-pipeline ./cmd/gta-pipeline \
	&& CGO_ENABLED=1 \
	go build -tags pcap -trimpath \
	-ldflags "-s -w -X gta/pkg/version.Version=${VERSION} -X gta/pkg/version.Commit=${GIT_COMMIT}" \
	-o /out/gta-mcp ./cmd/gta-mcp

# ============================================================================
# 阶段 2：runtime
# ============================================================================
FROM debian:bookworm-slim

# libpcap0.8：gopacket/pcap（cgo）的运行时共享库；ca-certificates：出站 TLS。
# gcc + libc6-dev + libpcap-dev：gta-mcp 服务端即时编译 agent 下载产物（-tags pcap 走 cgo）。
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libpcap0.8 ca-certificates gcc libc6-dev libpcap-dev \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --system --create-home --home-dir /data gta \
	# 预建插件目录：进程只在 scaffold 时懒创建它，首次 list/build 前不存在会让
	# 插件根目录状态依赖调用顺序。注意已存在的命名卷不会回填镜像内容，
	# 升级部署时需自行 mkdir 并 chown gta。
	&& mkdir -p /data/plugins \
	&& chown -R gta:gta /data

COPY --from=builder /out/gta-pipeline /usr/local/bin/gta-pipeline
COPY --from=builder /out/gta-mcp /usr/local/bin/gta-mcp

# == 远程 Agent 即时编译（方案一）：把 Go 工具链 + 模块缓存 + gta-agent 源码带进 runtime ==
# Go 工具链与模块缓存复用 builder（golang 镜像默认 GOPATH=/go，mod 缓存位于 /go/pkg/mod）。
COPY --from=builder /usr/local/go /usr/local/go
COPY --from=builder /go/pkg/mod /go/pkg/mod
# gta-agent 源码（repo 根含 go.mod）；命令以 <src>/cmd/gta-agent 为 srcDir，
# 需要在 srcDir 内写/删 config.embedded.json，故整个源码树归 gta 所有。
COPY --from=builder /src /opt/gta-agent/src

ENV GTA_AGENT_SRC_DIR=/opt/gta-agent/src/cmd/gta-agent \
	PATH=/usr/local/go/bin:${PATH} \
	# 本地模块缓存：复用镜像内置缓存，避免下载即编译时再访问网络
	GOPATH=/go \
	GOMODCACHE=/go/pkg/mod \
	# 构建缓存落在数据卷内（gta 可写 HOME）；GOTOOLCHAIN=local 防止自动下载更高版本 Go
	GOCACHE=/data/.cache/go-build \
	GOTOOLCHAIN=local \
	GOPROXY=https://goproxy.cn,direct \
	GOSUMDB=sum.golang.google.cn

RUN chown -R gta:gta /go /opt/gta-agent/src

# 统一配置（T10）：工作目录落 /data（卷挂载点），所有地址可用 GTA_* 环境变量覆盖。
# WORKDIR 一并设为 /data：任何相对路径（含 flag 默认值 "." / "plugins"）都锚在数据卷上，
# 不会掉进容器根目录 / 导致非 root 用户写文件失败（unable to open database file）。
ENV GTA_HOME=/data
WORKDIR /data
VOLUME ["/data"]

# 9888 CaptureControl | 9091 PluginRegistry | 9092 AgentIngest | 8781 MCP HTTP/SSE
EXPOSE 9888 9091 9092 8781

# 默认跑 pipeline；gta-mcp 用法见 docker-compose.yml（覆盖 entrypoint）。
# -spawn-agent=false：容器内不拉起 gta-singbox-agent（手机代理流量不经容器）。
# 非 root 运行：useradd --create-home 已建 /data 并归 gta 所有（T15 评审修复）。
USER gta
ENTRYPOINT ["/usr/local/bin/gta-pipeline"]
CMD ["-spawn-agent=false"]
