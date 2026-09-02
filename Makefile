.PHONY: proto test build build-mcp build-pipeline build-plugin-dev build-agent build-agents build-examples run-mcp run-pipeline run-plugin-dev release release-matrix web-build docs

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
# 无法携带目标平台的 libpcap，因此 release 矩阵统一 CGO_ENABLED=0，且**不带**
# -tags pcap：
#   - cmd/gta-agent 与 cmd/gta-pipeline 的实时抓包（gopacket/pcap、pcaplive）
#     均按 pcap / !pcap 构建标签门控，无标签构建可编译，运行时给出明确错误；
#   - pcap 文件源（pcapgo，纯 Go）与 agent 推流（gRPC）不受影响；
#   - 需要"能本机抓包"的服务端产物用 Docker 镜像（见 Dockerfile，带 libpcap）。
# windows/amd64 产物带 .exe 后缀，其余不带。
# 本 target 只用 POSIX sh 语法（$$ 转义 + for/if），无 GNU make 扩展。
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

# ============================================================================
# 多平台下载 agent 预置矩阵（T-Web First） -> build/agents/
#
# 远程 agent 需要在"用户本机"做实时抓包，因此必须携带目标平台的 libpcap
# （cgo），无法像 release-matrix 那样 CGO_ENABLED=0 纯交叉。这份预置只需
# 在各自平台/具备交叉 CC 的 runner 上构建一次并随镜像/发布带上：
#   - windows/amd64、linux/amd64 为 P0 必选；arm64 为 P1 增量；
#   - 产物是「通用」gta-agent（不带 embedded 标签），下载时由服务端把
#     config.embedded.json 作为 sidecar 打进 zip，运行时从 exe 同目录读取；
#   - 缺平台的产物未提供时，后端 get_agent_download_options 会如实标记
#     该平台不可下载（不会像旧方案那样回落到服务端本机平台）。
# 本机（windows/amd64）可直接跑 make build-agents 验证；linux 产物需在
# linux 上构建或配好 mingw/交叉工具链。
AGENT_PLATFORMS := windows/amd64 linux/amd64 windows/arm64 linux/arm64
build-agents:
	set -e; \
	mkdir -p build/agents; \
	for platform in $(AGENT_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "==> GOOS=$$os GOARCH=$$arch -tags pcap go build ./cmd/gta-agent"; \
		GOOS=$$os GOARCH=$$arch go build -tags pcap \
			-o build/agents/gta-agent-$$os-$$arch$$ext ./cmd/gta-agent; \
	done; \
	ls -la build/agents/

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

# web-build：构建前端并把产物同步进 gta-mcp 的 embed 目录（cmd/gta-mcp/webui/）。
# vite 产物照常出在 web/dist（vite.config.ts 不改，避免 outDir 清空误删 tracked
# 文件）；这里先清空 webui 旧产物再复制（保留 .gitkeep），重复构建不会积累
# 陈旧 hash 产物。此后 go build ./cmd/gta-mcp 即内嵌最新前端。
# 跑过一次后，未重新 web-build 也不会破坏构建：embed 里的旧产物照常可用。
web-build:
	cd web && npm ci && npm run build
	rm -rf cmd/gta-mcp/webui/assets
	rm -f cmd/gta-mcp/webui/index.html
	cp -r web/dist/. cmd/gta-mcp/webui/

# release-matrix：交叉编译全平台 release 产物到 bin/release/（T14）。
# CI release job 在 tag push（v*）时调用；本地可用 make release-matrix 验证。
# 版本注入见文件头部说明：make release-matrix VERSION=v0.5.0 GIT_COMMIT=abc
# 前置依赖 web-build：release 的 gta-mcp 产物内嵌最新前端（需要本机 node）。
release-matrix: web-build
	set -e; \
	mkdir -p bin/release; \
	for platform in $(RELEASE_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		for cmd in $(RELEASE_CMDS); do \
			echo "==> CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build ./cmd/$$cmd"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				go build -ldflags '$(LDFLAGS)' \
				-o bin/release/$$cmd-$$os-$$arch$$ext ./cmd/$$cmd; \
		done; \
	done; \
	ls -la bin/release/
