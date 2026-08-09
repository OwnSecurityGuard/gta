.PHONY: proto test build build-mcp build-pipeline build-examples run-mcp run-pipeline

TAGS := pcap

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

test:
	go test -tags $(TAGS) ./...

build-mcp:
	go build -tags $(TAGS) -o bin/gta-mcp.exe ./cmd/gta-mcp

build-pipeline:
	go build -tags $(TAGS) -o bin/gta-pipeline.exe ./cmd/gta-pipeline

build: build-mcp build-pipeline

build-examples:
	go build -tags $(TAGS) -o bin/http-server.exe ./examples/http/server
	go build -tags $(TAGS) -o bin/http-client.exe ./examples/http/client

run-mcp:
	go run -tags $(TAGS) ./cmd/gta-mcp

run-pipeline:
	go run -tags $(TAGS) ./cmd/gta-pipeline
