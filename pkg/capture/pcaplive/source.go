package pcaplive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gta/pkg/capture"
	"gta/pkg/capture/internal/base"
	"gta/pkg/event"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

func init() {
	capture.Register("pcap-live", capture.FactoryFunc{
		ValidateFunc: validateConfig,
		NewFunc:      newSource,
	})
}

// PcapLiveConfig 是实时网卡抓包配置。
//
// 设备选择优先级：
//  1. Device 非空：仅抓该设备（向后兼容）
//  2. Devices 非空：抓列表中所有设备
//  3. 两者都空：监听本机所有可用设备（pcap.FindAllDevs）
type PcapLiveConfig struct {
	Device  string   // 单设备（向后兼容；非空时优先于 Devices）
	Devices []string // 多设备；Device 与 Devices 均为空时监听所有设备
	BPF     string
	SnapLen int32
	Promisc bool
}

func validateConfig(cfg any) error {
	c, ok := cfg.(PcapLiveConfig)
	if !ok {
		return fmt.Errorf("invalid config type %T, expected PcapLiveConfig", cfg)
	}
	if c.SnapLen < 0 {
		return errors.New("snap_len must be non-negative")
	}
	// Device 与 Devices 均空合法：表示监听所有设备。
	// 设备存在性在 setup 阶段（OpenLive）校验。
	return nil
}

func newSource(cfg any) (capture.Source, error) {
	c := cfg.(PcapLiveConfig)
	if c.SnapLen == 0 {
		c.SnapLen = 1600
	}
	s := &pcapLiveSource{
		cfg: c,
		out: make(chan event.Packet, 256),
	}
	s.StatTracker.Init()
	return s, nil
}

type pcapLiveSource struct {
	cfg         PcapLiveConfig
	handles     []*pcap.Handle
	deviceNames []string
	out         chan event.Packet
	base.Lifecycle
	base.StatTracker
}

func (s *pcapLiveSource) Start(ctx context.Context) error {
	return s.Lifecycle.Start(ctx, s.setup, s.loop)
}

// resolveDevices 解析最终要抓取的设备名列表。
// 优先级：Device > Devices > FindAllDevs（所有设备）。
func (s *pcapLiveSource) resolveDevices() ([]string, error) {
	if s.cfg.Device != "" {
		return []string{s.cfg.Device}, nil
	}
	if len(s.cfg.Devices) > 0 {
		return s.cfg.Devices, nil
	}
	// 未指定设备：监听所有可用设备。
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	if len(devs) == 0 {
		return nil, errors.New("no devices found; specify device explicitly")
	}
	names := make([]string, 0, len(devs))
	for _, d := range devs {
		names = append(names, d.Name)
	}
	slog.Info("no device specified, capturing all interfaces", "count", len(names), "devices", names)
	return names, nil
}

func (s *pcapLiveSource) setup() error {
	devices, err := s.resolveDevices()
	if err != nil {
		return err
	}

	slog.Info("opening live capture", "devices", devices, "snaplen", s.cfg.SnapLen, "promisc", s.cfg.Promisc, "bpf", s.cfg.BPF)

	s.handles = make([]*pcap.Handle, 0, len(devices))
	s.deviceNames = make([]string, 0, len(devices))
	for _, dev := range devices {
		h, err := s.openDevice(dev)
		if err != nil {
			// 任一设备打开失败则整体失败，关闭已打开的 handle。
			for _, opened := range s.handles {
				opened.Close()
			}
			s.handles = nil
			s.deviceNames = nil
			return fmt.Errorf("open device %q: %w", dev, err)
		}
		s.handles = append(s.handles, h)
		s.deviceNames = append(s.deviceNames, dev)
	}
	return nil
}

// openDevice 打开单个网卡并设置 BPF。失败时返回错误，调用方负责清理已打开的 handle。
func (s *pcapLiveSource) openDevice(dev string) (*pcap.Handle, error) {
	h, err := pcap.OpenLive(dev, s.cfg.SnapLen, s.cfg.Promisc, pcap.BlockForever)
	if err != nil {
		return nil, err
	}
	if s.cfg.BPF != "" {
		if err := h.SetBPFFilter(s.cfg.BPF); err != nil {
			h.Close()
			return nil, fmt.Errorf("set bpf: %w", err)
		}
	}
	return h, nil
}

func (s *pcapLiveSource) loop(ctx context.Context) {
	defer close(s.out)
	defer func() {
		for _, h := range s.handles {
			h.Close()
		}
		slog.Info("live capture closed", "devices", s.deviceNames)
	}()

	var wg sync.WaitGroup

	// 丢包统计 goroutine：每秒聚合所有 handle 的 Drops，退出前读一次最终值。
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runDropStats(ctx)
	}()

	// 每个 handle 独立 goroutine 抓包，共享 out channel。
	for i, h := range s.handles {
		wg.Add(1)
		go func(h *pcap.Handle, devName string) {
			defer wg.Done()
			s.captureFromHandle(ctx, h, devName)
		}(h, s.deviceNames[i])
	}

	wg.Wait()
}

// runDropStats 周期性聚合所有 handle 的内核丢包数，写入 Stats.Drops。
// 退出前读取最终值，保证关闭后能查到累计 Drops。
func (s *pcapLiveSource) runDropStats(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.refreshDropStats()
			return
		case <-ticker.C:
			s.refreshDropStats()
		}
	}
}

func (s *pcapLiveSource) refreshDropStats() {
	var totalDrops uint64
	for _, h := range s.handles {
		if hs, err := h.Stats(); err == nil {
			totalDrops += uint64(hs.PacketsDropped)
		}
	}
	s.StatTracker.SetDrops(totalDrops)
}

// captureFromHandle 从单个 handle 抓包并发往 out channel。
func (s *pcapLiveSource) captureFromHandle(ctx context.Context, h *pcap.Handle, devName string) {
	src := gopacket.NewPacketSource(h, h.LinkType())
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-src.Packets():
			if !ok {
				slog.Info("capture handle closed", "device", devName)
				return
			}
			data := pkt.Data()
			linkType := h.LinkType()
			ep := event.Packet{
				Timestamp: pkt.Metadata().Timestamp,
				Raw:       data,
				LinkType:  event.LinkTypeFromLayers(linkType),
				Metadata:  make(map[string]any),
			}
			ep.Metadata[capture.MetaSource] = "pcap-live"
			ep.Metadata[capture.MetaDevice] = devName
			ep.Src, ep.Dst, ep.Protocol, ep.TCPFlags = capture.ParsePacketLayers(data, linkType)
			capture.EnrichFromMetadata(&ep)

			// 截断标记：CaptureLength < Length 表示包被 SnapLen 截断，payload 尾部丢失。
			meta := pkt.Metadata()
			if meta.CaptureLength < meta.Length {
				ep.Metadata[capture.MetaTruncated] = true
			}

			s.StatTracker.AddIn(len(data))

			// 背压告警：channel 满时计数（不丢包，仅记录阻塞次数后阻塞等待）。
			// 持续背压会导致内核缓冲溢出，体现为 Drops 增长。
			select {
			case s.out <- ep:
			default:
				s.StatTracker.AddBlocked()
				select {
				case s.out <- ep:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (s *pcapLiveSource) Packets() <-chan event.Packet { return s.out }

func (s *pcapLiveSource) Err() error { return s.Lifecycle.Err() }

func (s *pcapLiveSource) Close() error { return s.Lifecycle.Close() }

func (s *pcapLiveSource) Stats() capture.Stats { return s.StatTracker.Stats() }
