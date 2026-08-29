//go:build pcap

// pcap_live_pcap.go — pcap 构建下的实时抓包能力（T14 评审修复）。
//
// 与 cmd/gta-agent 的 capture_pcap.go 同一模式：把 cgo 依赖（gopacket/pcap、
// pcaplive）隔离在 -tags pcap 之后，使无标签 / CGO_ENABLED=0 的交叉编译
// （make release-matrix）也能通过；无 pcap 构建的替代实现见
// pcap_live_nopcap.go。服务端"能本机抓包"的产物是 Docker 镜像（带 libpcap）。
package main

import (
	"context"
	"fmt"

	"github.com/google/gopacket/pcap"

	"gta/pkg/capture"
	"gta/pkg/capture/pcaplive"
)

// listInterfaces 列出可用于实时抓包的网卡名。
func listInterfaces() ([]string, error) {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	names := make([]string, 0, len(devs))
	for _, dev := range devs {
		names = append(names, dev.Name)
	}
	return names, nil
}

// openLiveSource 打开实时网卡抓包 source。
// bpf 为空时默认 "tcp port <port>"；snapLen 为 0 时默认 1600。
func openLiveSource(ctx context.Context, iface string, port int, bpf string, snapLen int32, promisc bool) (capture.Source, error) {
	if bpf == "" {
		bpf = fmt.Sprintf("tcp port %d", port)
	}
	if snapLen == 0 {
		snapLen = 1600
	}
	return capture.Open(ctx, "pcap-live", pcaplive.PcapLiveConfig{
		Device:  iface,
		BPF:     bpf,
		SnapLen: snapLen,
		Promisc: promisc,
	})
}
