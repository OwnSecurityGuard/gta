package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gta/pkg/store"
)

func newTestAccessStore(t *testing.T) *accessCodeStore {
	t.Helper()
	cs, err := store.NewControlStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	store := newAccessCodeStore(cs.DB())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAccessCodeRoundTrip(t *testing.T) {
	s := newTestAccessStore(t)
	c := &accessCode{
		Code: "GTA-A1B2-C3D4", Owner: "alice", Port: 8080,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := s.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), c.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "alice" || got.Port != 8080 || got.Claimed {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if err := s.MarkClaimed(context.Background(), c.Code, "sess-1"); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Get(context.Background(), c.Code)
	if !got2.Claimed || got2.SessionID != "sess-1" {
		t.Fatalf("claim not persisted: %+v", got2)
	}
}

func TestNewAccessCodeFormat(t *testing.T) {
	c := newAccessCode()
	if len(c) != 13 { // "GTA-XXXX-XXXX"
		t.Fatalf("expected GTA-XXXX-XXXX, got %q", c)
	}
	if !strings.HasPrefix(c, "GTA-") {
		t.Fatalf("expected GTA- prefix, got %q", c)
	}
}
