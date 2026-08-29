# Dockerfile（T15）——服务端镜像：gta-pipeline + gta-mcp。
#
# 多阶段构建：
#   builder : golang:1.25-bookworm + libpcap-dev，以 cgo（pcap）编译两个服务端二进制；
#   runtime : debian:bookworm-slim + libpcap0.8（gopacket/pcap 运行时需要的共享库）。
#
# pcap 说明：pipeline 仍可在服务端本地开 pcap 源（实时网卡抓包 / pcap 文件源），
# 因此镜像带 pcap（cgo）编译；agent（gta-agent）推流入口是纯 Go gRPC，服务端
# 不依赖 agent 端的 pcap。
#
# 构建（版本注入与 Makefile 的 -X ldflags 同源，见 pkg/version/version.go）：
#   docker build --build-arg VERSION=v0.5.0 --build-arg GIT_COMMIT=abc1234 -t gta-server .
#
# 注意：构建需要网络访问 GitHub 以拉取 gta-plugin-sdk（见下方 INTERIM 注释）。

# ============================================================================
# 阶段 1：builder
# ============================================================================
FROM golang:1.25-bookworm AS builder

ARG VERSION=dev
ARG GIT_COMMIT=unknown

# pcap 采集层是 cgo 依赖，编译期需要 libpcap 头文件。
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libpcap-dev \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /src

# ============================================================================
# 临时措施（INTERIM，与 .github/workflows/ci.yml 的 build-test 相同）——
# 等待 gta-plugin-sdk 发布 v0.5.0 后删除：
#   1. 升级 go.mod 中 gta-plugin-sdk 版本；
#   2. 删除下面的 git clone 与 go.work 生成；
#   3. 删除本注释。
# ============================================================================
RUN git clone --depth 1 https://github.com/OwnSecurityGuard/gta-plugin-sdk /src/gta-plugin-sdk

# 先只拷贝 go.mod/go.sum 做 go mod download，充分利用层缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# go.work 指向本地 SDK checkout（与 ci.yml 中生成的内容一致）。
RUN printf 'go 1.25.5\n\nuse (\n\t./\n\t./gta-plugin-sdk\n)\n' > go.work

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
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libpcap0.8 ca-certificates \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --system --create-home --home-dir /data gta

COPY --from=builder /out/gta-pipeline /usr/local/bin/gta-pipeline
COPY --from=builder /out/gta-mcp /usr/local/bin/gta-mcp

# 统一配置（T10）：工作目录落 /data（卷挂载点），所有地址可用 GTA_* 环境变量覆盖。
ENV GTA_HOME=/data
VOLUME ["/data"]

# 9888 CaptureControl | 9091 PluginRegistry | 9092 AgentIngest | 8781 MCP HTTP/SSE
EXPOSE 9888 9091 9092 8781

# 默认跑 pipeline；gta-mcp 用法见 docker-compose.yml（覆盖 entrypoint）。
# -spawn-agent=false：容器内不拉起 gta-singbox-agent（手机代理流量不经容器）。
# 非 root 运行：useradd --create-home 已建 /data 并归 gta 所有（T15 评审修复）。
USER gta
ENTRYPOINT ["/usr/local/bin/gta-pipeline"]
CMD ["-spawn-agent=false"]
