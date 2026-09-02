package fake

import (
	"context"
	"fmt"

	"gta/pkg/capture"
	"gta/pkg/capture/internal/base"
	"gta/pkg/event"
)

func init() {
	capture.Register("fake", capture.FactoryFunc{
		ValidateFunc: validateConfig,
		NewFunc:      newSource,
	})
}

// Config 是 Fake Source 配置。
type Config struct {
	Packets []event.Packet
	Err     error
}

func validateConfig(cfg any) error {
	_, ok := cfg.(Config)
	if !ok {
		return fmt.Errorf("invalid config type %T, expected fake.Config", cfg)
	}
	return nil
}

func newSource(cfg any) (capture.Source, error) {
	c := cfg.(Config)
	s := &fakeSource{
		packets: c.Packets,
		err:     c.Err,
		out:     make(chan event.Packet, 16),
	}
	s.StatTracker.Init()
	return s, nil
}

type fakeSource struct {
	packets []event.Packet
	err     error
	out     chan event.Packet
	base.Lifecycle
	base.StatTracker
}

func (s *fakeSource) Start(ctx context.Context) error {
	return s.Lifecycle.Start(ctx, func() error { return nil }, s.loop)
}

func (s *fakeSource) loop(ctx context.Context) {
	defer close(s.out)
	for _, pkt := range s.packets {
		if pkt.Metadata == nil {
			pkt.Metadata = make(map[string]any)
		}
		pkt.Metadata[capture.MetaSource] = "fake"

		s.StatTracker.AddIn(len(pkt.Raw))

		select {
		case s.out <- pkt:
		case <-ctx.Done():
			return
		}
	}
}

func (s *fakeSource) Packets() <-chan event.Packet { return s.out }

func (s *fakeSource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.Lifecycle.Err()
}

func (s *fakeSource) Close() error { return s.Lifecycle.Close() }

func (s *fakeSource) Stats() capture.Stats { return s.StatTracker.Stats() }

// MustRegister 在测试中使用，确保 fake source 已注册。
// 正常程序入口应通过 import _ "gta/pkg/capture/fake" 触发 init。
func MustRegister() {
	// init 已经注册，这里仅做占位，方便测试代码显式表达依赖。
}
