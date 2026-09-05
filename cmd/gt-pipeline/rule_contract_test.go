package main

import (
	"testing"

	"gametrace/pkg/analyze"
	"gametrace/pkg/plugin"
)

const contractTestManifest = `api_version: gta.decoder/v2
name: contract-test-decoder
protocol: test_proto
type: decoder
capabilities:
  decode: true
  schema: true
schemas:
  - id: test.player.v1
    version: 1
    strict: true
    fields:
      hp:      { type: uint32, semantic: health, unit: hp, aggregatable: true }
      mana:    { type: uint32, semantic: mana, unit: mp }
      name:    { type: string }
      method:  { type: string }
      payload:
        type: object
        fields:
          damage: { type: uint32, aggregatable: true }
          note:   { type: string }
`

func mustCompileRule(t *testing.T, name, value string, groupBy ...string) *analyze.CompiledRule {
	t.Helper()
	var r analyze.RawRule
	r.Name = name
	r.Aggregate.Type = "sum"
	r.Aggregate.Window = "10s"
	r.Aggregate.Value = value
	r.Aggregate.GroupBy = groupBy
	cr, err := analyze.CompileRule(r, nil)
	if err != nil {
		t.Fatalf("compile rule %s: %v", name, err)
	}
	return cr
}

// TestCheckRulesAgainstManifest 覆盖四种对齐结果：
// 已声明且开聚合位 → 放行；已声明未开位 → 告警；嵌套路径同规则；未声明 → 兼容放行。
func TestCheckRulesAgainstManifest(t *testing.T) {
	m, err := plugin.ParseManifest([]byte(contractTestManifest))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	warns := checkRulesAgainstManifest([]*analyze.CompiledRule{
		mustCompileRule(t, "ok_sum", "data.hp"),                 // 声明 + aggregatable → 放行
		mustCompileRule(t, "bad_sum", "data.mana"),              // 声明未开 aggregatable → 告警
		mustCompileRule(t, "nested_ok", "data.payload.damage"),  // 嵌套声明 + aggregatable → 放行
		mustCompileRule(t, "undeclared", "data.ghost"),          // 未声明 → 兼容放行
		mustCompileRule(t, "grouped", "data.hp", "data.method"), // group_by 声明未开 groupable → 告警
	}, m)

	if len(warns) != 2 {
		t.Fatalf("expected 2 warnings (bad_sum aggregate + grouped group), got %d: %v", len(warns), warns)
	}
	for _, w := range warns {
		switch {
		case w.Rule == "bad_sum":
			if w.Kind != "aggregate" || w.Path != "mana" {
				t.Errorf("bad_sum warning = %+v, want aggregate/mana", w)
			}
		case w.Rule == "grouped":
			if w.Kind != "group" || w.Path != "method" {
				t.Errorf("grouped warning = %+v, want group/method", w)
			}
		default:
			t.Errorf("unexpected warning: %+v", w)
		}
	}
}

// TestCheckRulesAgainstManifest_Empty 验证 nil manifest / 空规则的短路返回。
func TestCheckRulesAgainstManifest_Empty(t *testing.T) {
	if got := checkRulesAgainstManifest(nil, nil); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
	r := mustCompileRule(t, "r", "data.hp")
	if got := checkRulesAgainstManifest([]*analyze.CompiledRule{r}, nil); got != nil {
		t.Errorf("nil manifest should return nil, got %v", got)
	}
}
