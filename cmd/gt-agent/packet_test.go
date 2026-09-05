package main

import (
	"net"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/uuid"
)

// makeEthTCPFrame 构造一个 Ethernet/IPv4/TCP 合成帧用于转换单测。
func makeEthTCPFrame(t *testing.T) []byte {
	t.Helper()
	eth := layers.Ethernet{
		SrcMAC:       net.HardwareAddr{1, 2, 3, 4, 5, 6},
		DstMAC:       net.HardwareAddr{6, 5, 4, 3, 2, 1},
		EthernetType: layers.EthernetTypeIPv4,
	}
	ip4 := layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    net.IP{10, 0, 0, 1},
		DstIP:    net.IP{10, 0, 0, 2},
	}
	tcp := layers.TCP{SrcPort: 12345, DstPort: 80}
	_ = tcp.SetNetworkLayerForChecksum(&ip4)
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{ComputeChecksums: false, FixLengths: true}
	if err := gopacket.SerializeLayers(buf, opts, &eth, &ip4, &tcp, gopacket.Payload([]byte("hello"))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestToRawPacket(t *testing.T) {
	frame := makeEthTCPFrame(t)
	ts := time.Unix(1700000000, 123456789)

	// 固定时钟路径：ts 非零时直接使用。
	p := toRawPacket(frame, layers.LinkTypeEthernet, ts)
	if p.Id == "" {
		t.Fatal("id should be generated (UUIDv7)")
	}
	if u, err := uuid.Parse(p.Id); err != nil || u.Version() != 7 {
		t.Fatalf("id is not a UUIDv7: %q err=%v", p.Id, err)
	}
	if p.TimestampNs != ts.UnixNano() {
		t.Fatalf("timestamp mismatch: %d", p.TimestampNs)
	}
	if p.LinkType != uint32(layers.LinkTypeEthernet) {
		t.Fatalf("link_type mismatch: %d", p.LinkType)
	}
	if string(p.Raw) != string(frame) {
		t.Fatal("raw frame must be preserved byte-for-byte")
	}
	if p.Src != "10.0.0.1:12345" {
		t.Fatalf("src mismatch: %q", p.Src)
	}
	if p.Dst != "10.0.0.2:80" {
		t.Fatalf("dst mismatch: %q", p.Dst)
	}
	if p.Protocol != "tcp" {
		t.Fatalf("protocol mismatch: %q", p.Protocol)
	}
}

func TestToRawPacketZeroTimeUsesClock(t *testing.T) {
	frame := makeEthTCPFrame(t)
	orig := timeNow
	defer func() { timeNow = orig }()
	fake := time.Unix(1234, 5678)
	timeNow = func() time.Time { return fake }

	p := toRawPacket(frame, layers.LinkTypeEthernet, time.Time{})
	if p.TimestampNs != fake.UnixNano() {
		t.Fatalf("expected server-now fallback %d, got %d", fake.UnixNano(), p.TimestampNs)
	}
}

func TestToRawPacketGarbageDoesNotBlock(t *testing.T) {
	// 非法帧：解析失败不阻塞，src/dst/protocol 留空。
	p := toRawPacket([]byte{0xde, 0xad, 0xbe, 0xef}, layers.LinkTypeEthernet, time.Unix(1, 0))
	if p.Src != "" || p.Dst != "" || p.Protocol != "" {
		t.Fatalf("garbage frame should leave src/dst/protocol empty, got %q/%q/%q", p.Src, p.Dst, p.Protocol)
	}
}
