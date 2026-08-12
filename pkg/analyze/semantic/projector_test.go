package semantic

import (
	"testing"
	"time"

	"gta/pkg/event"
)

// mkEvent 构造一个用于投影测试的 Event，payload 支持 _meta 嵌套。
func mkEvent(eventType string, ctx event.EventContext, payload map[string]any) *event.Event {
	return event.NewEventWithTime(
		"sess-1",
		event.EventType(eventType),
		eventType+".v1",
		event.SourceID("tcp"), // 注意：Source 是协议提示，非"plugin"标记
		event.ValueFromAny(payload),
		time.Unix(0, 0),
		ctx,
	)
}

func TestSemanticProjector_PureDeterministic(t *testing.T) {
	p := NewSemanticProjector()
	ev := mkEvent("http.request", event.EventContext{FlowID: "flow-1"},
		map[string]any{"_meta": map[string]any{"msg_name": "GET /", "direction": "client_to_server"}})

	a := p.Project(ev)
	b := p.Project(ev)
	if a != b {
		t.Fatalf("projector is not pure: two calls produced different results\n a=%+v\n b=%+v", a, b)
	}
	// 入参未被修改（不可变性）。
	if _, ok := ev.MetaValue("msg_name"); !ok {
		t.Fatal("projector mutated the input event payload")
	}
}

func TestSemanticProjector_PluginSemantic(t *testing.T) {
	p := NewSemanticProjector()
	ev := mkEvent("http.request", event.EventContext{FlowID: "flow-1"},
		map[string]any{
			"_meta": map[string]any{
				"msg_name":  "GET /api/login",
				"direction": "client_to_server",
			},
		})

	se := p.Project(ev)
	if se.EventID != ev.Identity.ID {
		t.Errorf("EventID mismatch: got %s want %s", se.EventID, ev.Identity.ID)
	}
	if se.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", se.SessionID)
	}
	if se.FlowID != "flow-1" {
		t.Errorf("FlowID = %q, want flow-1", se.FlowID)
	}
	if se.Name != "GET /api/login" {
		t.Errorf("Name = %q, want 'GET /api/login'", se.Name)
	}
	if se.Direction != "client_to_server" {
		t.Errorf("Direction = %q, want client_to_server", se.Direction)
	}
	// 硬约束 4：confidence 必须为 1.0。
	if se.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (Phase 2 forbids inferred values)", se.Confidence)
	}
	// Phase 2 冻结：source 恒为 engine（投影本身是 Engine 的确定性投影），
	// 不因 _meta 存在而误判为 plugin。
	if se.Source != SourceEngine {
		t.Errorf("Source = %q, want engine (Phase 2 freezes Source=engine)", se.Source)
	}
}

func TestSemanticProjector_KindDerivation(t *testing.T) {
	p := NewSemanticProjector()
	cases := []struct {
		name     string
		metaKind string // 空表示不提供 _meta.kind
		evType   string
		want     SemanticKind
	}{
		{"explicit meta kind", "push", "foo.bar", SemanticPush},
		{"type request", "", "http.request", SemanticRequest},
		{"type response", "", "http.response", SemanticResponse},
		{"type push", "", "notify.push", SemanticPush},
		{"type state_change", "", "game.state_change", SemanticStateChange},
		{"type transaction", "", "game.transaction", SemanticTransaction},
		{"unknown type -> neutral message", "", "weird.unknown.thing", SemanticMessage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := map[string]any{}
			if c.metaKind != "" {
				payload["_meta"] = map[string]any{"kind": c.metaKind}
			}
			ev := mkEvent(c.evType, event.EventContext{}, payload)
			se := p.Project(ev)
			if se.Kind != c.want {
				t.Errorf("Kind = %q, want %q", se.Kind, c.want)
			}
		})
	}
}

func TestSemanticProjector_NoGuess(t *testing.T) {
	p := NewSemanticProjector()
	// 即便 msg_name 看似"登录"，Phase 2 也绝不猜测 operation / subject。
	ev := mkEvent("game.message", event.EventContext{},
		map[string]any{"_meta": map[string]any{"msg_name": "C2S_Login"}})

	se := p.Project(ev)
	if se.Operation != "" {
		t.Errorf("Operation = %q, want empty (Phase 2 must not guess)", se.Operation)
	}
	if se.Subject != nil {
		t.Errorf("Subject = %+v, want nil (Phase 2 must not auto-identify entity)", se.Subject)
	}
	if se.Kind != SemanticMessage {
		t.Errorf("Kind = %q, want message (no deterministic rule matched)", se.Kind)
	}
	if se.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", se.Confidence)
	}
}

func TestSemanticProjector_EngineSourceWhenNoMeta(t *testing.T) {
	p := NewSemanticProjector()
	// 无 _meta：引擎仅按类型派生，source=engine，但 confidence 仍为 1。
	ev := mkEvent("game.move", event.EventContext{Direction: "client_to_server"},
		map[string]any{"x": 1, "y": 2})

	se := p.Project(ev)
	if se.Source != SourceEngine {
		t.Errorf("Source = %q, want engine", se.Source)
	}
	if se.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", se.Confidence)
	}
	// Direction 回退到 Context.Direction（纯读取，非推断）。
	if se.Direction != "client_to_server" {
		t.Errorf("Direction = %q, want client_to_server (fallback)", se.Direction)
	}
}

func TestSemanticProjector_NilEvent(t *testing.T) {
	p := NewSemanticProjector()
	se := p.Project(nil)
	if se.Kind != SemanticMessage || se.Confidence != 1.0 || se.Source != SourceEngine {
		t.Errorf("nil event projection = %+v, want safe default", se)
	}
}
