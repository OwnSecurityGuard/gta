package analyze

import (
	"fmt"
	"regexp"
	"time"

	"gta/pkg/event"
	"gta/pkg/schema"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// RawRule 是 YAML 中的规则。
type RawRule struct {
	Name      string            `yaml:"name"`
	Filter    string            `yaml:"filter"`
	Enrich    map[string]string `yaml:"enrich"`
	Schema    string            `yaml:"schema"`
	Aggregate struct {
		Type    string   `yaml:"type"`
		Window  string   `yaml:"window"`
		GroupBy []string `yaml:"group_by"`
		Value   string   `yaml:"value"`
		Output  string   `yaml:"output"`
	} `yaml:"aggregate"`
}

// CompiledRule 是编译后的规则。
type CompiledRule struct {
	Name         string
	Filter       *vm.Program
	Enrich       map[string]*vm.Program
	GroupBy      []*vm.Program
	GroupByNames []string
	Value        *vm.Program
	Aggregate    event.Aggregator
}

// ruleEnv 用于 expr 编译期类型推断。
var ruleEnv = map[string]any{
	"event": (*event.Event)(nil),
	"data":  map[string]any(nil),
}

var dataPathRe = regexp.MustCompile(`\bdata\.([a-zA-Z_][a-zA-Z0-9_]*(?:\.[a-zA-Z_][a-zA-Z0-9_]*)*)\b`)

// CompileRule 编译规则。若传入 schema，会校验规则中引用的 data.* 字段路径是否存在。
func CompileRule(r RawRule, s *schema.Schema) (*CompiledRule, error) {
	cr := &CompiledRule{Name: r.Name, Enrich: map[string]*vm.Program{}}
	if err := validateRuleSchema(s, r); err != nil {
		return nil, err
	}
	if r.Filter != "" {
		prog, err := expr.Compile(r.Filter, expr.Env(ruleEnv))
		if err != nil {
			return nil, fmt.Errorf("filter compile: %w", err)
		}
		cr.Filter = prog
	}
	for k, v := range r.Enrich {
		prog, err := expr.Compile(v, expr.Env(ruleEnv))
		if err != nil {
			return nil, fmt.Errorf("enrich %s compile: %w", k, err)
		}
		cr.Enrich[k] = prog
	}
	cr.GroupByNames = make([]string, len(r.Aggregate.GroupBy))
	for i, gb := range r.Aggregate.GroupBy {
		cr.GroupByNames[i] = gb
		prog, err := expr.Compile(gb, expr.Env(ruleEnv))
		if err != nil {
			return nil, fmt.Errorf("group_by compile: %w", err)
		}
		cr.GroupBy = append(cr.GroupBy, prog)
	}
	if r.Aggregate.Value != "" {
		prog, err := expr.Compile(r.Aggregate.Value, expr.Env(ruleEnv))
		if err != nil {
			return nil, fmt.Errorf("value compile: %w", err)
		}
		cr.Value = prog
	}
	window, err := time.ParseDuration(r.Aggregate.Window)
	if err != nil {
		return nil, fmt.Errorf("window parse: %w", err)
	}
	switch r.Aggregate.Type {
	case "count":
		cr.Aggregate = NewCountAgg(window, r.Aggregate.Output).WithGroupBy(cr.GroupBy).WithGroupByNames(cr.GroupByNames)
	case "sum":
		cr.Aggregate = NewSumAgg(window, r.Aggregate.Output).WithGroupBy(cr.GroupBy).WithGroupByNames(cr.GroupByNames).WithValue(cr.Value)
	case "rate":
		cr.Aggregate = NewRateAgg(window, r.Aggregate.Output).WithGroupBy(cr.GroupBy).WithGroupByNames(cr.GroupByNames)
	default:
		return nil, fmt.Errorf("unknown aggregator %s", r.Aggregate.Type)
	}
	return cr, nil
}

func validateRuleSchema(s *schema.Schema, r RawRule) error {
	if s == nil {
		return nil
	}
	exprs := []string{r.Filter, r.Aggregate.Value}
	for _, v := range r.Enrich {
		exprs = append(exprs, v)
	}
	exprs = append(exprs, r.Aggregate.GroupBy...)
	for _, e := range exprs {
		if e == "" {
			continue
		}
		for _, m := range dataPathRe.FindAllStringSubmatch(e, -1) {
			path := m[1]
			if _, err := s.Lookup(path); err != nil {
				return fmt.Errorf("rule %s schema error: %w", r.Name, err)
			}
		}
	}
	return nil
}
