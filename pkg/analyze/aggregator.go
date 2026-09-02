package analyze

import (
	"fmt"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

func evalString(prog *vm.Program, env map[string]any) (string, error) {
	out, err := expr.Run(prog, env)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", out), nil
}

func evalFloat(prog *vm.Program, env map[string]any) (float64, error) {
	out, err := expr.Run(prog, env)
	if err != nil {
		return 0, err
	}
	switch v := out.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("value is not numeric: %T", out)
	}
}

func windowStart(t time.Time, d time.Duration) time.Time {
	return t.Truncate(d)
}

// groupResult 包含状态机用的字符串 key 和带字段名的分组标签。
type groupResult struct {
	key   string
	group map[string]string
}

func groupKey(progs []*vm.Program, names []string, ctx map[string]any) (groupResult, error) {
	if len(progs) == 0 {
		return groupResult{key: "_", group: map[string]string{"group": "_"}}, nil
	}
	parts := make([]string, len(progs))
	group := make(map[string]string, len(progs))
	for i, p := range progs {
		s, err := evalString(p, ctx)
		if err != nil {
			return groupResult{}, err
		}
		parts[i] = s
		name := names[i]
		if name == "" {
			name = fmt.Sprintf("group_%d", i)
		}
		group[name] = s
	}
	return groupResult{key: strings.Join(parts, "|"), group: group}, nil
}
