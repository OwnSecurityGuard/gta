package main

import (
	"net/netip"
	"time"

	"gta/pkg/capture"
	"gta/pkg/capture/agent/proto"

	"github.com/google/gopacket/layers"
	"github.com/google/uuid"
)

// toRawPacket 把一个抓到的原始帧转成 proto.RawPacket：
// 保留完整帧与 link_type，UUIDv7 生成 id，Unix 纳秒时间戳，
// src/dst/protocol 复用 pkg/capture 的 ParsePacketLayers 解析
// （解析失败不阻塞，留空由服务端解码器自行判断）。
func toRawPacket(raw []byte, linkType layers.LinkType, ts time.Time) *proto.RawPacket {
	id, err := uuid.NewV7()
	if err != nil {
		// UUIDv7 失败几乎不可能；退化为随机 UUID，保证 id 非空。
		id = uuid.New()
	}
	if ts.IsZero() {
		ts = timeNow()
	}
	p := &proto.RawPacket{
		Id:          id.String(),
		Raw:         raw,
		LinkType:    uint32(linkType),
		TimestampNs: ts.UnixNano(),
	}
	src, dst, proto_, _ := capture.ParsePacketLayers(raw, linkType)
	p.Src = formatAddrPort(src)
	p.Dst = formatAddrPort(dst)
	p.Protocol = proto_
	return p
}

// timeNow 便于测试替换时钟。
var timeNow = time.Now

func formatAddrPort(ap netip.AddrPort) string {
	if !ap.IsValid() || ap.Addr().IsUnspecified() {
		return ""
	}
	return ap.String()
}
