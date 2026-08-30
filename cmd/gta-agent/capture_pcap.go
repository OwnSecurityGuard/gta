//go:build pcap

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"gta/pkg/capture/agent/proto"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

// captureConfig 是抓包参数。
type captureConfig struct {
	Iface   string
	BPF     string
	SnapLen int32
	Promisc bool
}

// runCapture 打开网卡、应用 BPF，把抓到的每个完整帧转成 RawPacket
// 发往 out，直到 ctx 取消或网卡关闭。
// 只支持单网卡（agent 一次只服务一个会话）；Iface 为空返回错误。
// 抓包源意外关闭时向 ended 非阻塞发送错误（ctx 正常取消则不发送）。
func runCapture(ctx context.Context, cfg captureConfig, out chan<- *proto.RawPacket, ended chan<- error) error {
	if cfg.Iface == "" {
		return fmt.Errorf("capture requires --iface")
	}
	if cfg.SnapLen <= 0 {
		cfg.SnapLen = 1600
	}
	h, err := pcap.OpenLive(cfg.Iface, cfg.SnapLen, cfg.Promisc, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("open %q: %w", cfg.Iface, err)
	}
	if cfg.BPF != "" {
		if err := h.SetBPFFilter(cfg.BPF); err != nil {
			h.Close()
			return fmt.Errorf("set bpf: %w", err)
		}
	}
	slog.Info("live capture opened", "iface", cfg.Iface, "bpf", cfg.BPF, "snaplen", cfg.SnapLen)

	go func() {
		defer h.Close()
		src := gopacket.NewPacketSource(h, h.LinkType())
		linkType := layers.LinkType(h.LinkType())
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-src.Packets():
				if !ok {
					// 抓包源意外关闭（网卡消失/pcap 错误）：向上通知，
					// 否则 agent 会永远停在阻塞于空 channel 的推流上。
					slog.Warn("capture source closed unexpectedly", "iface", cfg.Iface)
					notifyCaptureEnded(ended, errors.New("capture source closed for iface "+cfg.Iface))
					return
				}
				data := pkt.Data()
				if len(data) == 0 {
					continue
				}
				ts := pkt.Metadata().Timestamp
				if ts.IsZero() {
					ts = timeNow()
				}
				select {
				case out <- toRawPacket(data, linkType, ts):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return nil
}

// notifyCaptureEnded 非阻塞地上报抓包意外结束（ended 容量为 1）。
func notifyCaptureEnded(ended chan<- error, err error) {
	select {
	case ended <- err:
	default:
	}
}

// resolveDefaultIface 挑选默认抓包网卡，供未固化网卡名的下载形态 agent 使用：
//  1. UDP 出口网卡（Dial 探测不真正发包，最可靠）；
//  2. 兜底取首个 Up + 非回环 + 绑定了 IPv4 的网卡。
func resolveDefaultIface() (string, error) {
	if ip := outboundLocalIP(); ip != "" {
		if name := ifaceByIP(ip); name != "" {
			return name, nil
		}
	}
	if name := firstUpIface(); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("no usable network interface found; run with --iface to choose one")
}

// outboundLocalIP 用 UDP 拨号到公网地址探测本机出口网卡 IP（不真正发包）。
func outboundLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && a.IP != nil {
		return a.IP.String()
	}
	return ""
}

// ifaceByIP 返回绑定了指定 IP 的网卡名。
func ifaceByIP(ipStr string) string {
	target := net.ParseIP(ipStr)
	if target == nil {
		return ""
	}
	for _, iface := range allIfaces() {
		for _, a := range iface.Addrs {
			if a.ip != nil && a.ip.Equal(target) {
				return iface.name
			}
		}
	}
	return ""
}

// firstUpIface 返回首个处于 Up 状态、非回环且绑定了 IPv4 的网卡名。
func firstUpIface() string {
	for _, iface := range allIfaces() {
		if iface.name == "" {
			continue
		}
		for _, a := range iface.Addrs {
			if a.ip != nil && a.ip.To4() != nil {
				return iface.name
			}
		}
	}
	return ""
}

// ifaceAddr 是 ifaceByIP / firstUpIface 共用的网卡地址描述。
type ifaceAddr struct {
	ip net.IP
}

// ifaceEntry 描述一个候选网卡。
type ifaceEntry struct {
	name  string
	Addrs []ifaceAddr
}

// allIfaces 遍历本机网卡，过滤掉未启用/回环项。
func allIfaces() []ifaceEntry {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := []ifaceEntry{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		entry := ifaceEntry{name: iface.Name}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip == nil {
				continue
			}
			ip = ip.To4()
			entry.Addrs = append(entry.Addrs, ifaceAddr{ip: ip})
		}
		if len(entry.Addrs) > 0 {
			out = append(out, entry)
		}
	}
	return out
}
