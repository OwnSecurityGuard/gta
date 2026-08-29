//go:build pcap

package main

import (
	"context"
	"fmt"
	"log/slog"

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
func runCapture(ctx context.Context, cfg captureConfig, out chan<- *proto.RawPacket) error {
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
					slog.Info("capture handle closed", "iface", cfg.Iface)
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
