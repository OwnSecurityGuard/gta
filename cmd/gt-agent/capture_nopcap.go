//go:build !pcap

package main

import (
	"context"
	"errors"

	"gametrace/pkg/capture/agent/proto"
)

// captureConfig 在无 pcap 构建下仍需存在，供 main 组装参数。
type captureConfig struct {
	Iface   string
	BPF     string
	SnapLen int32
	Promisc bool
}

// liveCapture 在无 pcap 构建下不存在实际实现。
type liveCapture struct{}

// SetFilter 无 pcap 构建下不可用。
func (lc *liveCapture) SetFilter(bpf string) error {
	return errors.New("capture unavailable: no pcap support in this binary")
}

// runCapture 在未带 -tags pcap 编译时不可用：返回明确错误。
// 插件托管不受影响。
func runCapture(ctx context.Context, cfg captureConfig, out chan<- *proto.RawPacket, ended chan<- error) (*liveCapture, error) {
	return nil, errors.New("capture unavailable: this binary was built without -tags pcap; rebuild with `go build -tags pcap ./cmd/gt-agent`")
}

// resolveDefaultIface 在无 pcap 构建下不可用（无法抓包）。
func resolveDefaultIface() (string, error) {
	return "", errors.New("capture unavailable: no pcap support in this binary")
}

// IfaceInfo 是本地控制面 /v1/interfaces 的网卡条目。
type IfaceInfo struct {
	Name        string   `json:"name"`
	Friendly    string   `json:"friendly"`
	Description string   `json:"description"`
	IPs         []string `json:"ips"`
}

// listInterfacesLocal 无 pcap 构建下无法枚举抓包设备。
func listInterfacesLocal() []IfaceInfo { return nil }
