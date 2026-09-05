//go:build pcap

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"gametrace/pkg/capture/agent/proto"

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

// liveCapture 是一次运行中的抓包会话：暴露 BPF 热更新（不中断抓包）。
type liveCapture struct {
	h   *pcap.Handle
	ctx context.Context
}

// SetFilter 热更新 BPF（在现有 handle 上重编译，不断流）。
// 失败返回错误但**不清除**旧过滤——保留旧规则比裸奔安全。
func (lc *liveCapture) SetFilter(bpf string) error {
	if bpf == "" {
		bpf = ""
	}
	if err := lc.h.SetBPFFilter(bpf); err != nil {
		return fmt.Errorf("set bpf: %w", err)
	}
	slog.Info("bpf filter updated", "bpf", bpf)
	return nil
}

// runCapture 打开网卡、应用 BPF，把抓到的每个完整帧转成 RawPacket
// 发往 out，直到 ctx 取消或网卡关闭。
// 只支持单网卡（agent 一次只服务一个会话）；Iface 为空返回错误。
// 抓包源意外关闭时向 ended 非阻塞发送错误（ctx 正常取消则不发送）。
// 返回的 *liveCapture 供运行期热更新（SetFilter）；启动失败返回错误。
func runCapture(ctx context.Context, cfg captureConfig, out chan<- *proto.RawPacket, ended chan<- error) (*liveCapture, error) {
	if cfg.Iface == "" {
		return nil, fmt.Errorf("capture requires an interface")
	}
	if cfg.SnapLen <= 0 {
		cfg.SnapLen = 1600
	}
	h, err := openLiveWithFallback(cfg.Iface, cfg.SnapLen, cfg.Promisc)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", cfg.Iface, err)
	}
	if cfg.BPF != "" {
		if err := h.SetBPFFilter(cfg.BPF); err != nil {
			h.Close()
			return nil, fmt.Errorf("set bpf: %w", err)
		}
	}
	slog.Info("live capture opened", "iface", cfg.Iface, "bpf", cfg.BPF, "snaplen", cfg.SnapLen)
	lc := &liveCapture{h: h, ctx: ctx}

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
				_ = lc
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
	return lc, nil
}

// notifyCaptureEnded 非阻塞地上报抓包意外结束（ended 容量为 1）。
func notifyCaptureEnded(ended chan<- error, err error) {
	select {
	case ended <- err:
	default:
	}
}

// openLiveWithFallback 打开 pcap 设备；Windows 下用户/配置给的常是友好名
// （如 "WLAN"），npcap 只认 \Device\NPF_{GUID}，直开会报 No such device exists。
// 失败时尝试把友好名翻译成 pcap 设备名（经该网卡的 IP 在设备清单里反查）重开一次。
func openLiveWithFallback(iface string, snaplen int32, promisc bool) (*pcap.Handle, error) {
	h, err := pcap.OpenLive(iface, snaplen, promisc, pcap.BlockForever)
	if err == nil {
		return h, nil
	}
	alt := pcapDeviceByFriendlyName(iface)
	if alt == "" || alt == iface {
		return nil, err
	}
	h2, err2 := pcap.OpenLive(alt, snaplen, promisc, pcap.BlockForever)
	if err2 != nil {
		return nil, err // 保留原始错误，更有指向性
	}
	slog.Info("opened pcap device via friendly-name fallback", "given", iface, "device", alt)
	return h2, nil
}

// pcapDeviceByFriendlyName 把 Windows 网卡友好名（net.Interfaces 的 Name，
// 即 ncpa.cpl 里的连接名）翻译成 pcap 设备名：同名网卡拿 IP，再到 pcap 设备
// 清单里按 IP 反查。找不到返回空串。
func pcapDeviceByFriendlyName(friendly string) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if !strings.EqualFold(iface.Name, friendly) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return ""
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP == nil {
				continue
			}
			if dev := pcapDeviceByIP(ipnet.IP); dev != "" {
				return dev
			}
		}
	}
	return ""
}

// pcapDeviceByIP 在 pcap 设备清单里按绑定 IP 反查设备名。
func pcapDeviceByIP(target net.IP) string {
	if target == nil {
		return ""
	}
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return ""
	}
	for _, d := range devs {
		for _, a := range d.Addresses {
			if a.IP != nil && a.IP.Equal(target) {
				return d.Name
			}
		}
	}
	return ""
}

// resolveDefaultIface 挑选默认抓包网卡，供未固化网卡名的下载形态 agent 使用。
//
// 不能用 net.Interfaces() 的名字直接 OpenLive：Windows 下那是友好名（WLAN/以太网），
// npcap 只认 \Device\NPF_{GUID}（兼容模式下对连接名的支持也不可靠），跨机器部署时
// 会踩 "No such device exists"。因此直接枚举 pcap 设备清单，按
// 「出口 IP 匹配 → 首个有 IPv4 的设备」两级挑选，返回值保证 pcap 能打开。
func resolveDefaultIface() (string, error) {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return "", fmt.Errorf("enumerate pcap devices: %w", err)
	}
	if len(devs) == 0 {
		return "", errors.New(
			"pcap 没有枚举到任何网卡：目标机需要安装 Npcap（https://npcap.com，" +
				"安装时勾选 WinPcap API-compatible Mode）；已安装的话尝试重装并重启")
	}
	// 1) 出口网卡：UDP 拨号探测出口 IP（不真正发包），按 IP 精确匹配设备。
	if ipStr := outboundLocalIP(); ipStr != "" {
		if dev := pcapDeviceByIP(net.ParseIP(ipStr)); dev != "" {
			return dev, nil
		}
	}
	// 2) 兜底：首个绑定了 IPv4 的设备。
	var withIPv4 []string
	for _, d := range devs {
		for _, a := range d.Addresses {
			if a.IP != nil && a.IP.To4() != nil {
				withIPv4 = append(withIPv4, d.Name)
				break
			}
		}
	}
	if len(withIPv4) > 0 {
		return withIPv4[0], nil
	}
	// 3) 全都没有 IPv4：列出设备清单辅助排障。
	var sb strings.Builder
	for _, d := range devs {
		fmt.Fprintf(&sb, "\n  %s (%s)", d.Name, d.Description)
	}
	return "", fmt.Errorf("pcap 设备均未绑定 IPv4，无法自动选择网卡：%s", sb.String())
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

// IfaceInfo 是本地控制面 /v1/interfaces 的网卡条目。
type IfaceInfo struct {
	Name        string   `json:"name"`        // pcap 设备名（可直接用于 start.iface）
	Friendly    string   `json:"friendly"`    // 友好名（net.Interfaces 的 Name）
	Description string   `json:"description"`
	IPs         []string `json:"ips"`
}

// listInterfacesLocal 枚举 pcap 设备清单（跨平台设备名 + 友好名对照）。
func listInterfacesLocal() []IfaceInfo {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return nil
	}
	byIP := map[string]string{}
	if nifs, err := net.Interfaces(); err == nil {
		for _, nif := range nifs {
			addrs, _ := nif.Addrs()
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP != nil {
					byIP[ipn.IP.String()] = nif.Name
				}
			}
		}
	}
	out := make([]IfaceInfo, 0, len(devs))
	for _, d := range devs {
		info := IfaceInfo{Name: d.Name, Description: d.Description}
		for _, a := range d.Addresses {
			if a.IP != nil {
				info.IPs = append(info.IPs, a.IP.String())
				if f, ok := byIP[a.IP.String()]; ok && info.Friendly == "" {
					info.Friendly = f
				}
			}
		}
		out = append(out, info)
	}
	return out
}
