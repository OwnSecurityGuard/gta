package main

import (
	"testing"

	"gta/pkg/capture/agent/proto"
)

func TestBatchAccumulatorSizeThreshold(t *testing.T) {
	b := newBatchAccumulator(3)
	for i := 0; i < 2; i++ {
		if got := b.Push(&proto.RawPacket{Id: "x"}); got != nil {
			t.Fatalf("batch flushed early at %d items", i+1)
		}
	}
	got := b.Push(&proto.RawPacket{Id: "y"})
	if len(got) != 3 {
		t.Fatalf("want batch of 3, got %d", len(got))
	}
	if b.Len() != 0 {
		t.Fatalf("accumulator not empty after flush")
	}
}

func TestBatchAccumulatorFlushPartial(t *testing.T) {
	b := newBatchAccumulator(128)
	b.Push(&proto.RawPacket{Id: "a"})
	b.Push(&proto.RawPacket{Id: "b"})
	got := b.Flush()
	if len(got) != 2 {
		t.Fatalf("want partial batch of 2, got %d", len(got))
	}
	if b.Flush() != nil {
		t.Fatalf("second flush should be empty")
	}
	if b.Push(nil) != nil {
		t.Fatalf("nil push should not flush")
	}
}
