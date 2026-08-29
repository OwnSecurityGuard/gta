.PHONY: proto test build build-mcp build-pipeline build-plugin-dev build-agent build-examples run-mcp run-pipeline run-plugin-dev release release-matrix docs

TAGS := pcap

# ============================================================================
# 版本信息注入（T14）
#
# VERSION 优先取 git tag（tag 推送触发 CI 时），否则取 `git describe` 的
# 可读近似值，再退回 dev。GIT_COMMIT 是短哈希。CI 的 release job 通过
# make 的命令行变量覆盖，例如：
#   make release-matrix VERSION=v0.5.0 GIT_COMMIT=abc1234
# ============================================================================
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# 注入 pkg/version 的包级变量（见 pkg/version/version.go）。
LDFLAGS := -s -w \
	-X gta/pkg/version.Version=$(VERSION) \
	-X gta/pkg/version.Commit=$(GIT_COMMIT)

# ============================================================================
# 交叉编译矩阵（T14 release 产物）
#
# 说明：pcap 采集层是 cgo 依赖（github.com/google/gopacket/pcap），交叉编译
# 无法携带目标平台的 libpcap，因此 release 矩阵统一 CGO_ENABLED=0：
#   - 二进制可编译、可启动，pcap 文件源（pcapgo，纯 Go）仍可用；
#   - 实时网卡抓包（pcaplive）在无 cgo 二进制上不可用；
#   - 需要"能本机抓包"的二进制时用对应平台的原生工具链加 -tags pcap 编译
#     （Linux 服务端请用 Docker 镜像，见 Dockerfile，内含 libpcap）。
# windows/amd64 产物带 .exe 后缀，其余不带。
# ============================================================================
RELEASE_PLATFORMS := windows/amd64 linux/amd64 linux/arm64 darwin/arm64
RELEASE_CMDS := gta-pipeline gta-mcp gta-agent


# 只生成 gta 自有的进程间控制面 proto。
# 插件线上契约（plugin.proto）已迁入 SDK 仓库，在那边 make proto 生成——
# 两侧各生成一份会在 protobuf 全局注册表里撞同一个文件路径并 panic。
# 这里覆盖两个 gta 自有协议：internalipc（抓包控制面）与 plugindev（开发平面
# 控制面，P1 平面拆分引入）。
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		pkg/internalipc/proto/internal.proto
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		pkg/plugindev/proto/plugindev.proto
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		pkg/capture/mobile/proto/mobile.proto
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		pkg/capture/agent/proto/agent.proto

test:
	go test -tags $(TAGS) ./...

build-mcp:
	go build -tags $(TAGS) -ldflags '$(LDFLAGS)' -o bin/gta-mcp.exe ./cmd/gta-mcp

build-pipeline:
	go build -tags $(TAGS) -ldflags '$(LDFLAGS)' -o bin/gta-pipeline.exe ./cmd/gta-pipeline

# Developer Plane 独立二进制。默认由 gta-mcp 内嵌（dialPluginDev 在
# GTA_PLUGINDEV_ADDR 为空时起进程内实例），只有需要物理隔离开发平面时才用它。
build-plugin-dev:
	go build -tags $(TAGS) -o bin/gta-plugin-dev.exe ./cmd/gta-plugin-dev

# 移动端流量入口（sing-box 侧 → GTA）：TCP 中继 + gRPC 推送连接级数据。
build-agent:
	go build -tags $(TAGS) -o bin/gta-singbox-agent.exe ./cmd/gta-singbox-agent

build: build-mcp build-pipeline build-plugin-dev build-agent

build-examples:
	go build -tags $(TAGS) -o bin/http-server.exe ./examples/http/server
	go build -tags $(TAGS) -o bin/http-client.exe ./examples/http/client

run-mcp:
	go run -tags $(TAGS) ./cmd/gta-mcp

run-pipeline:
	go run -tags $(TAGS) ./cmd/gta-pipeline

run-plugin-dev:
	go run -tags $(TAGS) ./cmd/gta-plugin-dev

# 重新生成 README 中的 MCP 工具目录（与 cmd/gta-mcp/main.go 对齐）。
docs:
	go run ./scripts/gen_tool_table

# release-matrix：交叉编译全平台 release 产物到 bin/release/（T14）。
# CI release job 在 tag push（v*）时调用；本地可用 make release-matrix 验证。
# 版本注入见文件头部说明：make release-matrix VERSION=v0.5.0 GIT_COMMIT=abc1234
release-matrix:
	set -e; \
	mkdir -p bin/release; \
	for platform in $(RELEASE_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		for cmd in $(RELEASE_CMDS); do \
			echo "==> CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build ./cmd/$$cmd"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				go build -tags $(TAGS) -ldflags '$(LDFLAGS)' \
				-o bin/release/$$cmd-$$os-$$arch$$ext ./cmd/$$cmd; \
		done; \
	done; \
	ls -la bin/release/
