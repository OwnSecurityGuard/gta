package main

import (
	"testing"
	"time"

	"gta/pkg/event"
)

// makeEv 构造一个带显式 ID 与 TraceContext 的事件，便于断言因果树。
func makeEv(id string, causation, correlation string, ts time.Time) *event.Event {
	ev := event.NewEventWithTrace("sess1", event.EventType("msg"), "game.demo.v1", "decoder",
		event.ValueObject(map[string]event.Value{
			"_meta": event.ValueObject(map[string]event.Value{
				"msg_name": event.ValueString(id + "_msg"),
				"direction": event.ValueString("client_to_server"),
			}),
		}),
		event.TraceContext{}, event.EventContext{})
	ev.Identity.ID = event.EventID(id)
	if causation != "" {
		ev.Trace.CausationID = event.EventID(causation)
	}
	if correlation != "" {
		ev.Trace.CorrelationID = correlation
	}
	ev.Identity.Timestamp = ts
	return ev
}

func TestBuildTimeline_TreeAndConversations(t *testing.T) {
	base := time.Now()
	// e1 -> e2 -> e4  (correlation A)；e3 独立根 (correlation B)；e5 悬空 causation
	events := []*event.Event{
		makeEv("e1", "", "A", base.Add(1*time.Millisecond)),
		makeEv("e2", "e1", "A", base.Add(2*time.Millisecond)),
		makeEv("e3", "", "B", base.Add(3*time.Millisecond)),
		makeEv("e4", "e2", "A", base.Add(4*time.Millisecond)),
		makeEv("e5", "missing", "", base.Add(5*time.Millisecond)),
	}

	tl := buildTimeline(events, "demo-plugin", "stopped")

	if tl.EventCount != 5 {
		t.Fatalf("EventCount = %d, want 5", tl.EventCount)
	}
	if tl.RootCount != 3 {
		t.Fatalf("RootCount = %d, want 3 (e1, e3, e5)", tl.RootCount)
	}

	// 找到 e1 根节点
	var e1 *TimelineNode
	for i := range tl.Roots {
		if tl.Roots[i].ID == "e1" {
			e1 = &tl.Roots[i]
		}
	}
	if e1 == nil {
		t.Fatal("root e1 not found")
	}
	if len(e1.Children) != 1 || e1.Children[0].ID != "e2" {
		t.Fatalf("e1 children = %v, want [e2]", childIDs(e1))
	}
	if len(e1.Children[0].Children) != 1 || e1.Children[0].Children[0].ID != "e4" {
		t.Fatalf("e2 children = %v, want [e4]", childIDs(e1.Children[0]))
	}

	// 时间戳排序：e1 < e2 < e4
	if !e1.Timestamp.Before(e1.Children[0].Timestamp) {
		t.Fatal("children not sorted by timestamp")
	}

	// 对话聚合：A=3, B=1
	convByID := map[string]int{}
	for _, c := range tl.Conversations {
		convByID[c.CorrelationID] = c.EventCount
	}
	if convByID["A"] != 3 || convByID["B"] != 1 {
		t.Fatalf("conversations = %v, want A=3 B=1", convByID)
	}

	// 上下文标注
	if tl.Plugin != "demo-plugin" || tl.Status != "stopped" {
		t.Fatalf("plugin/status not propagated: %q / %q", tl.Plugin, tl.Status)
	}
}

func TestBuildTimeline_SortingStable(t *testing.T) {
	base := time.Now()
	events := []*event.Event{
		makeEv("r", "", "X", base),
		makeEv("c2", "r", "X", base.Add(20*time.Millisecond)),
		makeEv("c1", "r", "X", base.Add(10*time.Millisecond)),
	}
	tl := buildTimeline(events, "", "")
	var r *TimelineNode
	for i := range tl.Roots {
		if tl.Roots[i].ID == "r" {
			r = &tl.Roots[i]
		}
	}
	if len(r.Children) != 2 || r.Children[0].ID != "c1" || r.Children[1].ID != "c2" {
		t.Fatalf("children order = %v, want [c1 c2] (timestamp sorted)", childIDs(r))
	}
}

func childIDs(n *TimelineNode) []string {
	ids := make([]string, 0, len(n.Children))
	for i := range n.Children {
		ids = append(ids, n.Children[i].ID)
	}
	return ids
}
