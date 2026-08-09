package pcapfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gta/pkg/capture"
	"gta/pkg/capture/internal/base"
	"gta/pkg/event"

	"github.com/google/gopacket/pcapgo"
)

func init() {
	capture.Register("pcap-file", capture.FactoryFunc{
		ValidateFunc: validateConfig,
		NewFunc:      newSource,
	})
}

// PcapFileConfig 是 pcap 文件回放配置。
type PcapFileConfig struct {
	Path        string
	BPF         string
	ReplaySpeed float64 // 0 = burst, 1 = 原速，2 = 2x，0.5 = 半速
}

func validateConfig(cfg any) error {
	c, ok := cfg.(PcapFileConfig)
	if !ok {
		return fmt.Errorf("invalid config type %T, expected PcapFileConfig", cfg)
	}
	if c.Path == "" {
		return errors.New("path is required")
	}
	if c.ReplaySpeed < 0 {
		return errors.New("replay_speed must be non-negative")
	}
	return nil
}

func newSource(cfg any) (capture.Source, error) {
	c := cfg.(PcapFileConfig)
	s := &pcapFileSource{
		cfg: c,
		out: make(chan event.Packet, 64),
	}
	s.StatTracker.Init()
	return s, nil
}

type pcapFileSource struct {
	cfg    PcapFileConfig
	f      *os.File
	reader *pcapgo.Reader
	out    chan event.Packet
	base.Lifecycle
	base.StatTracker
}

func (s *pcapFileSource) Start(ctx context.Context) error {
	return s.Lifecycle.Start(ctx, s.setup, s.loop)
}

func (s *pcapFileSource) setup() error {
	slog.Info("opening pcap file", "path", s.cfg.Path)
	f, err := os.Open(s.cfg.Path)
	if err != nil {
		return err
	}
	reader, err := pcapgo.NewReader(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	s.f = f
	s.reader = reader
	return nil
}

func (s *pcapFileSource) loop(ctx context.Context) {
	defer close(s.out)
	defer func() {
		if s.f != nil {
			_ = s.f.Close()
		}
		slog.Info("pcap file source closed")
	}()

	var lastTS time.Time
	var first bool
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		data, ci, err := s.reader.ReadPacketData()
		if err == io.EOF {
			slog.Info("pcap file reached EOF")
			return
		}
		if err != nil {
			slog.Error("read pcap packet", "error", err)
			s.SetErr(err)
			return
		}

		linkType := s.reader.LinkType()
		pkt := event.Packet{
			Timestamp: ci.Timestamp,
			Raw:       data,
			LinkType:  event.LinkTypeFromLayers(linkType),
			Metadata:  make(map[string]any),
		}
		pkt.Metadata[capture.MetaSource] = "pcap-file"
		pkt.Metadata[capture.MetaCaptureName] = s.cfg.Path
		pkt.Src, pkt.Dst, pkt.Protocol, pkt.TCPFlags = capture.ParsePacketLayers(data, linkType)
		capture.EnrichFromMetadata(&pkt)

		// 截断标记：CaptureLength < Length 表示原始包在抓取时被 SnapLen 截断。
		if ci.CaptureLength < ci.Length {
			pkt.Metadata[capture.MetaTruncated] = true
		}

		s.StatTracker.AddIn(len(data))

		// ReplaySpeed 控制：按原始时间戳间隔发送。
		if !first {
			first = true
		} else if s.cfg.ReplaySpeed > 0 && !lastTS.IsZero() {
			delay := ci.Timestamp.Sub(lastTS)
			if delay > 0 {
				time.Sleep(time.Duration(float64(delay) / s.cfg.ReplaySpeed))
			}
		}
		lastTS = ci.Timestamp

		// 背压告警：channel 满时计数（不丢包，仅记录阻塞次数后阻塞等待）。
		select {
		case s.out <- pkt:
		default:
			s.StatTracker.AddBlocked()
			select {
			case s.out <- pkt:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (s *pcapFileSource) Packets() <-chan event.Packet { return s.out }

func (s *pcapFileSource) Err() error { return s.Lifecycle.Err() }

func (s *pcapFileSource) Close() error { return s.Lifecycle.Close() }

func (s *pcapFileSource) Stats() capture.Stats { return s.StatTracker.Stats() }
