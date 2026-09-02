package correlation

import (
	"testing"
)

func TestRememberLookupForget(t *testing.T) {
	s := New(0) // default limit

	s.Remember("flow|rpc_seq|10", "req-event-1")
	p, ok := s.Lookup("flow|rpc_seq|10")
	if !ok || p.CausationID != "req-event-1" {
		t.Fatalf("Lookup miss or wrong id: %+v, %v", p, ok)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}

	// 同 key 覆盖
	s.Remember("flow|rpc_seq|10", "req-event-2")
	p, _ = s.Lookup("flow|rpc_seq|10")
	if p.CausationID != "req-event-2" {
		t.Fatalf("overwrite failed: %+v", p)
	}

	s.Forget("flow|rpc_seq|10")
	if _, ok := s.Lookup("flow|rpc_seq|10"); ok {
		t.Fatalf("Forget did not remove key")
	}
	if s.Len() != 0 {
		t.Fatalf("Len after Forget = %d, want 0", s.Len())
	}
}

func TestDifferentKeysIsolate(t *testing.T) {
	s := New(0)
	s.Remember("flowA|rpc_seq|10", "a")
	// flow B 相同 seq 应为不同键
	if _, ok := s.Lookup("flowB|rpc_seq|10"); ok {
		t.Fatalf("flow B should not match flow A")
	}
}

func TestLimitEvictsOldest(t *testing.T) {
	s := New(2)
	s.Remember("k1", "e1")
	s.Remember("k2", "e2")
	s.Remember("k3", "e3") // 淘汰 k1
	if _, ok := s.Lookup("k1"); ok {
		t.Fatalf("k1 should be evicted")
	}
	if _, ok := s.Lookup("k2"); !ok {
		t.Fatalf("k2 should remain")
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
}
