package analyze

import (
	"context"
	"testing"
	"time"

	"gametrace/pkg/event"
	"gametrace/pkg/schema"
)

func makeAgg() struct {
	Type    string   `yaml:"type"`
	Window  string   `yaml:"window"`
	GroupBy []string `yaml:"group_by"`
	Value   string   `yaml:"value"`
	Output  string   `yaml:"output"`
} {
	return struct {
		Type    string   `yaml:"type"`
		Window  string   `yaml:"window"`
		GroupBy []string `yaml:"group_by"`
		Value   string   `yaml:"value"`
		Output  string   `yaml:"output"`
	}{}
}

func TestEngineFilterAndSum(t *testing.T) {
	raw := RawRule{
		Name:   "dmg",
		Filter: "data.name == \"attack\" && data.payload.damage > 0",
		Aggregate: func() struct {
			Type    string   `yaml:"type"`
			Window  string   `yaml:"window"`
			GroupBy []string `yaml:"group_by"`
			Value   string   `yaml:"value"`
			Output  string   `yaml:"output"`
		} {
			a := makeAgg()
			a.Type = "sum"
			a.Window = "1s"
			a.GroupBy = []string{`data.attacker.id`}
			a.Value = "data.payload.damage"
			a.Output = "dmg_sum"
			return a
		}(),
	}
	cr, err := CompileRule(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine([]*CompiledRule{cr}, nil)
	ev := &event.Event{
		Payload: event.Payload{
			Value: event.ValueObject(map[string]event.Value{
				"name": event.ValueString("attack"),
				"attacker": event.ValueObject(map[string]event.Value{
					"id": event.ValueString("p1"),
				}),
				"payload": event.ValueObject(map[string]event.Value{
					"damage": event.ValueInt(10),
				}),
			}),
		},
	}
	if _, err := eng.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	// 窗口未到期，FlushAll 强制输出当前窗口
	time.Sleep(1100 * time.Millisecond)
	metrics := eng.FlushAll()
	if len(metrics) == 0 {
		t.Fatal("expected metrics")
	}
	if metrics[0].Value != 10 {
		t.Fatalf("expected 10, got %v", metrics[0].Value)
	}
}

func TestGroupByNames(t *testing.T) {
	raw := RawRule{
		Name:   "pkts",
		Filter: "data.attacker.id != \"\"",
		Aggregate: func() struct {
			Type    string   `yaml:"type"`
			Window  string   `yaml:"window"`
			GroupBy []string `yaml:"group_by"`
			Value   string   `yaml:"value"`
			Output  string   `yaml:"output"`
		} {
			a := makeAgg()
			a.Type = "count"
			a.Window = "1s"
			a.GroupBy = []string{`data.attacker.id`, `data.target.id`}
			a.Output = "pkt_count"
			return a
		}(),
	}
	cr, err := CompileRule(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine([]*CompiledRule{cr}, nil)
	type testEvent struct {
		attackerID string
		targetID   string
	}
	for _, te := range []testEvent{
		{attackerID: "a1", targetID: "t1"},
		{attackerID: "a1", targetID: "t2"},
		{attackerID: "a1", targetID: "t1"},
	} {
		ev := &event.Event{
			Payload: event.Payload{
				Value: event.ValueObject(map[string]event.Value{
					"attacker": event.ValueObject(map[string]event.Value{
						"id": event.ValueString(te.attackerID),
					}),
					"target": event.ValueObject(map[string]event.Value{
						"id": event.ValueString(te.targetID),
					}),
				}),
			},
		}
		if _, err := eng.Process(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	metrics := eng.FlushAll()
	if len(metrics) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(metrics))
	}
	for _, m := range metrics {
		if _, ok := m.Group["data.attacker.id"]; !ok {
			t.Fatalf("missing attacker key in group: %+v", m.Group)
		}
		if _, ok := m.Group["data.target.id"]; !ok {
			t.Fatalf("missing target key in group: %+v", m.Group)
		}
	}
}

func TestSchemaValidation(t *testing.T) {
	s := &schema.Schema{Fields: map[string]*schema.Field{
		"name": {Type: "string"},
		"attacker": {Type: "object", Fields: map[string]*schema.Field{
			"id": {Type: "string"},
		}},
		"payload": {Type: "object", Fields: map[string]*schema.Field{
			"damage": {Type: "number"},
		}},
	}}
	valid := RawRule{
		Name:   "valid",
		Filter: "data.name == \"attack\" && data.payload.damage > 0",
		Aggregate: func() struct {
			Type    string   `yaml:"type"`
			Window  string   `yaml:"window"`
			GroupBy []string `yaml:"group_by"`
			Value   string   `yaml:"value"`
			Output  string   `yaml:"output"`
		} {
			a := makeAgg()
			a.Type = "sum"
			a.Window = "1s"
			a.GroupBy = []string{`data.attacker.id`}
			a.Value = "data.payload.damage"
			a.Output = "dmg"
			return a
		}(),
	}
	if _, err := CompileRule(valid, s); err != nil {
		t.Fatalf("expected valid rule: %v", err)
	}

	invalid := RawRule{
		Name:   "invalid",
		Filter: "data.notfound == 1",
		Aggregate: func() struct {
			Type    string   `yaml:"type"`
			Window  string   `yaml:"window"`
			GroupBy []string `yaml:"group_by"`
			Value   string   `yaml:"value"`
			Output  string   `yaml:"output"`
		} {
			a := makeAgg()
			a.Type = "count"
			a.Window = "1s"
			a.Output = "cnt"
			return a
		}(),
	}
	if _, err := CompileRule(invalid, s); err == nil {
		t.Fatal("expected schema error for invalid field")
	}
}

func TestFlushSemantics(t *testing.T) {
	raw := RawRule{
		Name:   "cnt",
		Filter: "data.name == \"hit\"",
		Aggregate: func() struct {
			Type    string   `yaml:"type"`
			Window  string   `yaml:"window"`
			GroupBy []string `yaml:"group_by"`
			Value   string   `yaml:"value"`
			Output  string   `yaml:"output"`
		} {
			a := makeAgg()
			a.Type = "count"
			a.Window = "1h"
			a.Output = "hits"
			return a
		}(),
	}
	cr, err := CompileRule(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine([]*CompiledRule{cr}, nil)
	ev := &event.Event{
		Payload: event.Payload{
			Value: event.ValueObject(map[string]event.Value{
				"name": event.ValueString("hit"),
			}),
		},
	}
	if _, err := eng.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if closed := eng.Flush(); len(closed) != 0 {
		t.Fatalf("expected no closed window metrics, got %d", len(closed))
	}
	final := eng.FlushAll()
	if len(final) != 1 || final[0].Value != 1 {
		t.Fatalf("expected final metric value 1, got %+v", final)
	}
	// 再次 FlushAll 应已清空，返回空
	if again := eng.FlushAll(); len(again) != 0 {
		t.Fatalf("expected empty after final flush, got %d", len(again))
	}
}
