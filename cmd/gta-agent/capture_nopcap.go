//go:build !pcap

package main

import (
	"context"
	"errors"

	"gta/pkg/capture/agent/proto"
)

// captureConfig 在无 pcap 构建下仍需存在，供 main 组装参数。
type captureConfig struct {
	Iface   string
	BPF     string
	SnapLen int32
	Promisc bool
}

// runCapture 在未带 -tags pcap 编译时不可用：返回明确错误。
// 插件托管不受影响。
func runCapture(ctx context.Context, cfg captureConfig, out chan<- *proto.RawPacket) error {
	return errors.New("capture unavailable: this binary was built without -tags pcap; rebuild with `go build -tags pcap ./cmd/gta-agent`")
}
