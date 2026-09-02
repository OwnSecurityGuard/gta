package capture

import (
	"net/netip"

	"gta/pkg/event"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// ParsePacketLayers 从原始链路层数据包中解析出源/目的地址、传输层协议与 TCP 标志位。
// 该函数被所有基于 pcap 的 Source 实现复用，避免在每个子包中重复实现
// decodeLayers / ipAddrPair。
// 返回的 event.TCPFlags 仅在协议为 "tcp" 时有效，其他协议为零值。
//
// 支持的 LinkType：
//   - Ethernet (1): 标准以太网
//   - Null/Loopback (0/108): BSD loopback
//   - RawIP (DLT_RAW=101): VPN/TUN 设备输出，无链路层头，数据直接是 IP 包
//
// 不支持的 LinkType（如 ProxyPayload/TLSPlaintext 等自定义值 1000+）会返回空五元组，
// 由 Source 层通过 Metadata fallback 补充（见 EnrichFromMetadata）。
func ParsePacketLayers(data []byte, linkType layers.LinkType) (netip.AddrPort, netip.AddrPort, string, event.TCPFlags) {
	var eth layers.Ethernet
	var loop layers.Loopback
	var ip4 layers.IPv4
	var ip6 layers.IPv6
	var tcp layers.TCP
	var udp layers.UDP

	var parser *gopacket.DecodingLayerParser
	switch linkType {
	case layers.LinkTypeNull, layers.LinkTypeLoop:
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeLoopback, &loop, &ip4, &ip6, &tcp, &udp)
	case layers.LinkTypeRaw:
		// DLT_RAW: VPN/TUN 输出，无链路层封装，数据直接是 IP 包。
		// 根据 IP 头首字节的 version 字段（高 4 位）选择 v4/v6 parser。
		if len(data) == 0 {
			return netip.AddrPort{}, netip.AddrPort{}, "", event.TCPFlags{}
		}
		if data[0]>>4 == 6 {
			parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv6, &ip6, &tcp, &udp)
		} else {
			parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, &ip4, &tcp, &udp)
		}
	default:
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip4, &ip6, &tcp, &udp)
	}

	decoded := []gopacket.LayerType{}
	_ = parser.DecodeLayers(data, &decoded)
	for _, t := range decoded {
		switch t {
		case layers.LayerTypeTCP:
			srcIP, dstIP := ipAddrPair(&ip4, &ip6)
			flags := event.TCPFlags{
				FIN: tcp.FIN,
				SYN: tcp.SYN,
				RST: tcp.RST,
				ACK: tcp.ACK,
				PSH: tcp.PSH,
				URG: tcp.URG,
			}
			return netip.AddrPortFrom(srcIP, uint16(tcp.SrcPort)),
				netip.AddrPortFrom(dstIP, uint16(tcp.DstPort)),
				"tcp",
				flags
		case layers.LayerTypeUDP:
			srcIP, dstIP := ipAddrPair(&ip4, &ip6)
			return netip.AddrPortFrom(srcIP, uint16(udp.SrcPort)),
				netip.AddrPortFrom(dstIP, uint16(udp.DstPort)),
				"udp",
				event.TCPFlags{}
		}
	}
	return netip.AddrPort{}, netip.AddrPort{}, "", event.TCPFlags{}
}

func ipAddrPair(ip4 *layers.IPv4, ip6 *layers.IPv6) (netip.Addr, netip.Addr) {
	if ip4.Version == 4 {
		return netip.AddrFrom4([4]byte(ip4.SrcIP)), netip.AddrFrom4([4]byte(ip4.DstIP))
	}
	return netip.AddrFrom16([16]byte(ip6.SrcIP)), netip.AddrFrom16([16]byte(ip6.DstIP))
}

// EnrichFromMetadata 在 ParsePacketLayers 无法解析五元组时（Protocol 为空），
// 从 Packet.Metadata 中的 client_addr/server_addr 回填 Src/Dst。
//
// 适用场景：
//   - 移动代理（LinkType=ProxyPayload）：只有应用层 payload，无 IP/TCP 头，
//     真实五元组由代理层通过 Metadata[client_addr/server_addr] 提供。
//   - TLS 解密明文（LinkType=TLSPlaintext）：同上。
//
// 链路层抓包（pcap-live/pcap-file）的 Protocol 通常非空，调用此函数为 no-op。
func EnrichFromMetadata(pkt *event.Packet) {
	if pkt.Protocol != "" {
		return
	}
	// 根据 LinkType 推断协议类型，供下游区分处理。
	switch pkt.LinkType {
	case event.LinkTypeProxyPayload:
		pkt.Protocol = "proxy"
	case event.LinkTypeTLSPlaintext:
		pkt.Protocol = "tls"
	}
	// 从 Metadata 回填五元组（格式 "ip:port"）。
	if v, ok := pkt.Metadata[MetaClientAddr].(string); ok && v != "" {
		if ap, err := netip.ParseAddrPort(v); err == nil {
			pkt.Src = ap
		}
	}
	if v, ok := pkt.Metadata[MetaServerAddr].(string); ok && v != "" {
		if ap, err := netip.ParseAddrPort(v); err == nil {
			pkt.Dst = ap
		}
	}
}
