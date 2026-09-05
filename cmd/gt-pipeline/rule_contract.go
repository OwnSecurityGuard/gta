package main

import (
	"fmt"
	"strings"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
	sdkcontract "github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	sdkschema "github.com/OwnSecurityGuard/gta-plugin-sdk/schema"

	"gametrace/pkg/analyze"
	"gametrace/pkg/plugin"
)

// ruleContractWarning 是一条 rules.yaml 聚合引用与插件 manifest 声明不匹配的告警。
type ruleContractWarning struct {
	Rule   string // rules.yaml 规则名
	Kind   string // "aggregate"（value 求和源）| "group"（group_by 分组键）
	Path   string // data.* 字段路径（不含 data. 前缀）
	Schema string // 声明该字段的 schema wire id
}

func (w ruleContractWarning) String() string {
	capName := "aggregatable"
	if w.Kind == "group" {
		capName = "groupable"
	}
	return fmt.Sprintf("rule %s: data.%s is declared in schema %s but not %s (Semantic Contract v1: add `%s: true` in plugin.yaml, or aggregate a declared field)",
		w.Rule, w.Path, w.Schema, capName, capName)
}

// checkRulesAgainstManifest 把 rules.yaml 聚合规则引用的 data.* 字段对齐到插件
// manifest 的 aggregatable/groupable 声明（Semantic Contract v1 §13 aggregate_query 放行位）。
//
// 语义：
//   - 字段被至少一个 schema 声明且任一声明带对应能力位 → 放行；
//   - 字段被声明但所有声明都缺能力位 → 告警（声明了字段却没开聚合位，属"声明不全"）；
//   - 字段未在任何 schema 声明 → 不告警（legacy manifest 无 schema 声明，保持兼容）。
//
// 返回告警列表（可能为空）。
func checkRulesAgainstManifest(rules []*analyze.CompiledRule, m *sdk.Manifest) []ruleContractWarning {
	if m == nil || len(rules) == 0 {
		return nil
	}
	idx := sdkcontract.ManifestSchemaIndex(m)
	var warns []ruleContractWarning
	for _, r := range rules {
		for _, path := range analyze.DataPaths(r.ValueName) {
			if w := checkFieldCapability(idx, r.Name, path, "aggregate"); w != nil {
				warns = append(warns, *w)
			}
		}
		for _, gb := range r.GroupByNames {
			for _, path := range analyze.DataPaths(gb) {
				if w := checkFieldCapability(idx, r.Name, path, "group"); w != nil {
					warns = append(warns, *w)
				}
			}
		}
	}
	return warns
}

// checkFieldCapability 检查单个 data.* 路径的聚合/分组能力位。
// 返回 nil 表示放行（含"未声明"的兼容放行）。
func checkFieldCapability(idx map[string]*sdkschema.Schema, rule, path, kind string) *ruleContractWarning {
	declared := ""
	for ref, sch := range idx {
		f := lookupSchemaPath(sch, path)
		if f == nil {
			continue
		}
		ok := f.Aggregatable
		if kind == "group" {
			ok = f.Groupable
		}
		if ok {
			return nil // 任一 schema 放行即合法
		}
		if declared == "" {
			declared = ref
		}
	}
	if declared == "" {
		return nil
	}
	return &ruleContractWarning{Rule: rule, Kind: kind, Path: path, Schema: declared}
}

// lookupSchemaPath 按 "a.b.c" 点分路径在 schema 字段树中下钻（仅静态 object 子字段；
// array/dynamic object 的键运行期才能确定，视为未声明）。
func lookupSchemaPath(s *sdkschema.Schema, path string) *sdkschema.Field {
	segs := strings.Split(path, ".")
	fields := s.Fields
	var f *sdkschema.Field
	for i, seg := range segs {
		if fields == nil {
			return nil
		}
		next := fields[seg]
		if next == nil {
			return nil
		}
		f = next
		if i < len(segs)-1 {
			if f.Type != sdkschema.TypeObject || f.Dynamic {
				return nil
			}
			fields = f.Fields
		}
	}
	return f
}

// checkAggregationContract 在解码器挂载（首次或热切换）后，把本会话的聚合规则
// 与该插件 manifest 的 aggregatable/groupable 声明对齐（Semantic Contract v1 §13）。
// 违规只告警不拦截：rules.yaml 是全局的，manifest 是 per-plugin 的，
// 由插件作者按告警补声明（或在规则侧改用已声明字段）。
func (t *captureTask) checkAggregationContract(client pb.DecoderClient) {
	if len(t.rules) == 0 || client == nil {
		return
	}
	name, ok := t.registry.NameByClient(client)
	if !ok {
		return
	}
	raw, err := t.registry.GetPluginManifest(name)
	if err != nil {
		return
	}
	m, err := plugin.ParseManifest(raw)
	if err != nil {
		return
	}
	for _, w := range checkRulesAgainstManifest(t.rules, m) {
		t.logger.Warn("aggregation rule contract mismatch", "plugin", name, "warning", w.String())
	}
}
