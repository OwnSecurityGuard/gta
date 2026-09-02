//go:build !pcap

// pcap_live_nopcap.go — 无 pcap 构建下的实时抓包占位实现（T14 评审修复）。
//
// make release-matrix 的交叉编译产物（CGO_ENABLED=0，不带 -tags pcap）没有
// libpcap：实时网卡抓包与网卡枚举不可用，给出明确错误而非编译失败；
// pcap 文件源（pcapgo，纯 Go）与 agent 推流（gRPC）不受影响。
// 需要"能本机抓包"的服务端产物请用 Docker 镜像（Dockerfile，带 libpcap）。
package main

import (
	"context"
	"errors"

	"gta/pkg/capture"
)

// errNoPcap 是无 pcap 构建下的统一错误。
var errNoPcap = errors.New("live capture unavailable: this binary was built without -tags pcap (cross-compiled release); use the Docker image or rebuild with `go build -tags pcap ./cmd/gta-pipeline`")

// listInterfaces 无 pcap 构建下返回空列表（不报错）：上层据此给出
// "no capture interfaces available" 的明确错误。
func listInterfaces() ([]string, error) {
	return nil, nil
}

// openLiveSource 无 pcap 构建下不可用：返回明确错误。
func openLiveSource(ctx context.Context, iface string, port int, bpf string, snapLen int32, promisc bool) (capture.Source, error) {
	return nil, errNoPcap
}
