package semantic

import (
	"testing"
	"time"

	"gta/pkg/event"
)

func TestBaselineManager_Apply_FirstChange_NoBefore(t *testing.T) {
	bm := NewBaselineManager(nil)
	ev := newEvent("s1", "flow-1", "game.login", map[string]any{
		"_state_changes": []any{
			map[string]any{
				"subject_type": "player",
				"subject_id":   "1001",
				"op":           "set",
				"path":         "gold",
				"after":        100,
				"version":      1,
			},
		},
	})

	changes, err := bm.Apply(ev, "s1")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	sc := changes[0]
	if sc.BeforeResolved {
		t.Errorf("first change should not have resolved before")
	}
	if !sc.AfterResolved {
		t.Errorf("first change should resolve after")
	}
	if sc.EntityVersion != 1 {
		t.Errorf("version = %d, want 1", sc.EntityVersion)
	}
}

func TestBaselineManager_Apply_SecondChange_HasBefore(t *testing.T) {
	bm := NewBaselineManager(nil)

	// 第一次：gold = 100
	first := newEvent("s1", "flow-1", "game.login", map[string]any{
		"_state_changes": []any{
			map[string]any{
				"subject_type": "player",
				"subject_id":   "1001",
				"op":           "set",
				"path":         "gold",
				"after":        100,
				"version":      1,
			},
		},
	})
	if _, err := bm.Apply(first, "s1"); err != nil {
		t.Fatalf("apply first: %v", err)
	}

	// 第二次：gold = 150
	second := newEvent("s1", "flow-1", "game.add_gold", map[string]any{
		"_state_changes": []any{
			map[string]any{
				"subject_type": "player",
				"subject_id":   "1001",
				"op":           "set",
				"path":         "gold",
				"after":        150,
				"version":      2,
			},
		},
	})
	changes, err := bm.Apply(second, "s1")
	if err != nil {
		t.Fatalf("apply second: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	sc := changes[0]
	if !sc.BeforeResolved {
		t.Errorf("second change should resolve before")
	}
	beforeVal, ok := sc.Before.AsInt()
	if !ok || beforeVal != 100 {
		t.Errorf("before = %v, want 100", sc.Before.ToAny())
	}
}

func TestBaselineManager_Isolation_BySessionAndFlow(t *testing.T) {
	bm := NewBaselineManager(nil)

	// 会话 A 的 flow-1：gold = 100
	a := newEvent("sA", "flow-1", "game.login", map[string]any{
		"_state_changes": []any{
			map[string]any{
				"subject_type": "player",
				"subject_id":   "1001",
				"op":           "set",
				"path":         "gold",
				"after":        100,
				"version":      1,
			},
		},
	})
	if _, err := bm.Apply(a, "sA"); err != nil {
		t.Fatalf("apply A: %v", err)
	}

	// 会话 B 的 flow-1：同名实体但不应继承基线
	b := newEvent("sB", "flow-1", "game.login", map[string]any{
		"_state_changes": []any{
			map[string]any{
				"subject_type": "player",
				"subject_id":   "1001",
				"op":           "set",
				"path":         "gold",
				"after":        200,
				"version":      1,
			},
		},
	})
	changes, err := bm.Apply(b, "sB")
	if err != nil {
		t.Fatalf("apply B: %v", err)
	}
	if changes[0].BeforeResolved {
		t.Errorf("cross-session baseline should not be reused")
	}
}

// newEvent 是测试辅助函数，构造一个最小可用 Event。
func newEvent(sessionID, flowID, eventType string, payload map[string]any) *event.Event {
	return event.NewEventWithTime(
		sessionID,
		event.EventType(eventType),
		"test.v1",
		"test-source",
		event.ValueFromAny(payload),
		time.Now(),
		event.EventContext{FlowID: flowID, RawPacketID: "pkt-" + eventType, Direction: "unknown"},
	)
}
