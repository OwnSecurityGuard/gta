package capture

import (
	"context"
	"errors"
	"testing"
	"time"

	"gametrace/pkg/event"
)

type fakeSource struct {
	out   chan event.Packet
	state State
	err   error
}

func (s *fakeSource) Start(ctx context.Context) error {
	if s.state == StateRunning {
		return ErrAlreadyStarted
	}
	s.state = StateRunning
	return nil
}

func (s *fakeSource) Packets() <-chan event.Packet { return s.out }
func (s *fakeSource) Err() error                   { return s.err }
func (s *fakeSource) Close() error {
	s.state = StateClosed
	return nil
}
func (s *fakeSource) Stats() Stats { return Stats{} }
func (s *fakeSource) State() State { return s.state }

func TestRegistryUnknownSource(t *testing.T) {
	_, err := New("unknown", struct{}{})
	if err == nil {
		t.Fatal("expected error for unknown source")
	}
}

func TestRegistryValidate(t *testing.T) {
	Register("fake-test", FactoryFunc{
		ValidateFunc: func(cfg any) error {
			if cfg.(string) == "" {
				return errors.New("cfg required")
			}
			return nil
		},
		NewFunc: func(cfg any) (Source, error) {
			return &fakeSource{out: make(chan event.Packet)}, nil
		},
	})

	_, err := New("fake-test", "")
	if err == nil {
		t.Fatal("expected validation error")
	}

	src, err := New("fake-test", "ok")
	if err != nil {
		t.Fatal(err)
	}
	if src.State() != StateCreated {
		t.Fatalf("expected state created, got %s", src.State())
	}
}

func TestOpen(t *testing.T) {
	Register("fake-open", FactoryFunc{
		ValidateFunc: func(cfg any) error { return nil },
		NewFunc: func(cfg any) (Source, error) {
			return &fakeSource{out: make(chan event.Packet)}, nil
		},
	})

	src, err := Open(context.Background(), "fake-open", nil)
	if err != nil {
		t.Fatal(err)
	}
	if src.State() != StateRunning {
		t.Fatalf("expected state running, got %s", src.State())
	}
	_ = src.Close()
	if src.State() != StateClosed {
		t.Fatalf("expected state closed, got %s", src.State())
	}
}

func TestOpenStartFailure(t *testing.T) {
	Register("fake-open-fail", FactoryFunc{
		ValidateFunc: func(cfg any) error { return nil },
		NewFunc: func(cfg any) (Source, error) {
			return &fakeSource{out: make(chan event.Packet)}, nil
		},
	})

	startCtx, cancel := context.WithCancel(context.Background())
	cancel()
	src, err := Open(startCtx, "fake-open-fail", nil)
	if err != nil {
		t.Fatal(err)
	}
	// fakeSource.Start 不检查 ctx，这里主要验证 Open 流程不 panic。
	_ = src.Close()
}

func TestRegisteredNames(t *testing.T) {
	names := RegisteredNames()
	if len(names) == 0 {
		t.Fatal("expected at least one registered source")
	}
}

func TestStatsCumulative(t *testing.T) {
	Register("fake-stats", FactoryFunc{
		ValidateFunc: func(cfg any) error { return nil },
		NewFunc: func(cfg any) (Source, error) {
			return &fakeSource{out: make(chan event.Packet)}, nil
		},
	})

	src, err := Open(context.Background(), "fake-stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	// Stats 在 Close 后仍应可读取。
	stats := src.Stats()
	_ = stats
}

func TestSourceStartTwice(t *testing.T) {
	Register("fake-twice", FactoryFunc{
		ValidateFunc: func(cfg any) error { return nil },
		NewFunc: func(cfg any) (Source, error) {
			return &fakeSource{out: make(chan event.Packet)}, nil
		},
	})

	src, err := New("fake-twice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := src.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("expected ErrAlreadyStarted, got %v", err)
	}
}

func TestSourcePacketsChannelClose(t *testing.T) {
	out := make(chan event.Packet)
	src := &fakeSource{out: out, state: StateRunning}
	close(out)

	select {
	case <-src.Packets():
	case <-time.After(time.Second):
		t.Fatal("expected closed channel")
	}
}
