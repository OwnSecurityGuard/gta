package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"gametrace/pkg/capture"
	"gametrace/pkg/event"
)

func TestOpen(t *testing.T) {
	cfg := Config{
		Packets: []event.Packet{
			{Timestamp: time.Now(), Raw: []byte("hello"), Metadata: make(map[string]any)},
			{Timestamp: time.Now(), Raw: []byte("world")},
		},
	}
	src, err := capture.Open(context.Background(), "fake", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	count := 0
	for range src.Packets() {
		count++
	}
	if count != 2 {
		t.Fatalf("expected 2 packets, got %d", count)
	}
	if src.Err() != nil {
		t.Fatalf("unexpected error: %v", src.Err())
	}
}

func TestOpenWithError(t *testing.T) {
	expectedErr := errors.New("injected error")
	cfg := Config{
		Packets: []event.Packet{{Timestamp: time.Now(), Raw: []byte("x"), Metadata: make(map[string]any)}},
		Err:     expectedErr,
	}
	src, err := capture.Open(context.Background(), "fake", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	<-src.Packets()
	if !errors.Is(src.Err(), expectedErr) {
		t.Fatalf("expected %v, got %v", expectedErr, src.Err())
	}
}

func TestStartTwice(t *testing.T) {
	src, err := capture.New("fake", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := src.Start(context.Background()); !errors.Is(err, capture.ErrAlreadyStarted) {
		t.Fatalf("expected ErrAlreadyStarted, got %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	src, err := capture.New("fake", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if src.State() != capture.StateClosed {
		t.Fatalf("expected state closed, got %s", src.State())
	}
}

func TestStats(t *testing.T) {
	cfg := Config{
		Packets: []event.Packet{
			{Timestamp: time.Now(), Raw: []byte("abcd"), Metadata: make(map[string]any)},
		},
	}
	src, err := capture.Open(context.Background(), "fake", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	<-src.Packets()
	stats := src.Stats()
	if stats.PacketsIn != 1 {
		t.Fatalf("expected 1 packet in, got %d", stats.PacketsIn)
	}
	if stats.BytesIn != 4 {
		t.Fatalf("expected 4 bytes in, got %d", stats.BytesIn)
	}
}
