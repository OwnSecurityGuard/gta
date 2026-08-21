package analyze

import (
	"context"
	"fmt"
	"log/slog"

	"gta/pkg/event"

	"github.com/expr-lang/expr"
)

// Engine 接收 Event，按 CompiledRule 处理并输出 Metric。
type Engine struct {
	rules  []*CompiledRule
	logger *slog.Logger // 带 session_id 等上下文的 logger
}

// NewEngine 创建分析引擎。logger 用于记录规则匹配等调试信息。
func NewEngine(rules []*CompiledRule, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{rules: rules, logger: logger}
}

// Process 处理单个 Event 事件，返回该事件触发的 Metric（当前窗口未到期时可能为空）。
// 错误通过 return 传递，由调用方记录日志（避免重复记录）。
func (e *Engine) Process(ctx context.Context, ev *event.Event) ([]event.Metric, error) {
	data := ev.Payload.Value.ToAny()

	base := map[string]any{
		"event":    ev,
		"data":     data,
		"identity": ev.Identity,
		"trace": ev.Trace,
		"context":  ev.Context,
		"payload":  ev.Payload,
	}

	var all []event.Metric
	for _, r := range e.rules {
		env := make(map[string]any, len(base))
		for k, v := range base {
			env[k] = v
		}
		if r.Filter != nil {
			out, err := expr.Run(r.Filter, env)
			if err != nil {
				return nil, fmt.Errorf("rule %s filter: %w", r.Name, err)
			}
			ok, isBool := out.(bool)
			if !isBool {
				return nil, fmt.Errorf("rule %s filter did not return bool", r.Name)
			}
			if !ok {
				continue
			}
			e.logger.Debug("rule matched", "rule", r.Name, "event", ev.Identity.ID)
		}
		ruleCtx := map[string]any{}
		for k, prog := range r.Enrich {
			out, err := expr.Run(prog, env)
			if err != nil {
				return nil, fmt.Errorf("rule %s enrich %s: %w", r.Name, k, err)
			}
			ruleCtx[k] = out
		}
		aggEnv := make(map[string]any, len(env)+len(ruleCtx))
		for k, v := range env {
			aggEnv[k] = v
		}
		for k, v := range ruleCtx {
			aggEnv[k] = v
		}
		if err := r.Aggregate.Add(ev, aggEnv); err != nil {
			return nil, fmt.Errorf("rule %s aggregate: %w", r.Name, err)
		}
	}
	return all, nil
}

// Flush 输出所有规则已关闭窗口的指标。
func (e *Engine) Flush() []event.Metric {
	var all []event.Metric
	for _, r := range e.rules {
		all = append(all, r.Aggregate.Flush()...)
	}
	return all
}

// FlushAll 输出所有规则已关闭窗口 + 当前窗口的指标，用于 shutdown。
func (e *Engine) FlushAll() []event.Metric {
	var all []event.Metric
	for _, r := range e.rules {
		all = append(all, r.Aggregate.FinalFlush()...)
	}
	return all
}

