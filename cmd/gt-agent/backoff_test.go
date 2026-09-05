package main

import (
	"testing"
	"time"
)

func TestBackoffSequence(t *testing.T) {
	b := newBackoff()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Fatalf("step %d: got %v, want %v", i, got, w)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("after reset: got %v, want 1s", got)
	}
}
