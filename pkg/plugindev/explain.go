package plugindev

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ExplainRefPrefix prefixes every explain_ref (design §2.3). A ref uniquely
// identifies one plugin.explain conclusion so last_attempt can point back to
// it.
const ExplainRefPrefix = "expl_"

var explainSeq int64

// newExplainRef returns a process-unique explain_ref derived from the nanosecond
// clock plus a monotonic counter, so two conclusions generated in the same
// instant never collide.
func newExplainRef() string {
	n := atomic.AddInt64(&explainSeq, 1)
	return ExplainRefPrefix + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatInt(n, 36)
}

// ExplainRequest asks for the attribution of a plugin's most recent failure.
type ExplainRequest struct {
	Name   string
	Action string // optional: build | activate | deactivate; empty = latest
}

// ExplainFinding is one attributed cause of a failure.
type ExplainFinding struct {
	Error    *BuildError // set for build failures; nil for process failures
	Category string      // machine category (see contract.yaml-adjacent vocabulary)
	RuleID   string      // optional SDK contract rule_id (SSOT: contract.yaml rules:)
	Why      string      // human explanation
	Fix      string      // actionable corrective step
}

// ExplainResult is the conclusion of a plugin.explain invocation. Its Ref is
// what last_attempt.explain_ref points back to.
type ExplainResult struct {
	Ref        string
	Name       string
	Action     string
	At         time.Time
	Summary    string
	Findings   []*ExplainFinding
	NextAction string
}

// Explain attributes the most recent build/activate/deactivate failure for a
// plugin. On a failed build or activate the Developer Plane also calls this
// automatically and stores the result so Status can surface explain_ref
// immediately (design §2.3 / P3a). When the last attempt succeeded, Explain
// returns a no-failure conclusion so the AI knows there is nothing to fix.
func Explain(ctx context.Context, req *ExplainRequest) (*ExplainResult, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	last := defaultTracker.LastAttempt(req.Name)
	if last == nil {
		return &ExplainResult{
			Ref:        newExplainRef(),
			Name:       req.Name,
			At:         time.Now(),
			Summary:    "no recent attempt recorded for " + req.Name,
			NextAction: "run build_plugin or activate_plugin to produce a result",
		}, nil
	}
	// Honour an explicit action filter.
	if req.Action != "" && req.Action != last.Action {
		return &ExplainResult{
			Ref:        newExplainRef(),
			Name:       req.Name,
			Action:     req.Action,
			At:         time.Now(),
			Summary:    fmt.Sprintf("last attempt was %q, not %q", last.Action, req.Action),
			NextAction: "explain without an action filter to see the latest attempt",
		}, nil
	}

	res := &ExplainResult{
		Ref:    newExplainRef(),
		Name:   req.Name,
		Action: last.Action,
		At:     last.At,
	}

	if last.OK {
		res.Summary = fmt.Sprintf("last %s succeeded; nothing to explain", last.Action)
		res.NextAction = "continue to the next step (activate / status / verify)"
		defaultTracker.RecordExplain(req.Name, res)
		return res, nil
	}

	// Failure attribution.
	switch last.Action {
	case "build":
		res.Findings = explainBuild(last)
		res.Summary = fmt.Sprintf("build failed with %d error(s); fix and re-run build_plugin", len(res.Findings))
		res.NextAction = "edit the flagged file:line, then re-run build_plugin"
	case "activate":
		res.Findings = explainActivate(last)
		res.NextAction = activateNextAction(res.Findings)
	default:
		res.Summary = fmt.Sprintf("%s failed; see message", last.Action)
		res.Findings = []*ExplainFinding{{
			Category: "other",
			Why:      last.Message,
			Fix:      "inspect the reported message and retry",
		}}
		res.NextAction = "inspect the reported message and retry"
	}

	// Wire the ref back into last_attempt so Status can surface it.
	last.ExplainRef = res.Ref
	defaultTracker.RecordExplain(req.Name, res)
	return res, nil
}

// explainBuild classifies each structured compiler diagnostic.
func explainBuild(last *LastAttempt) []*ExplainFinding {
	var findings []*ExplainFinding
	if len(last.Errors) == 0 {
		findings = append(findings, &ExplainFinding{
			Category: "build-no-errors",
			Why:      "build reported a failure but produced no structured diagnostics; the raw compiler/linker output is in the attempt message",
			Fix:      "re-run build_plugin and read the output under message; common causes are linker errors or a missing go.sum entry",
		})
		return findings
	}
	for _, e := range last.Errors {
		findings = append(findings, classifyBuildError(e))
	}
	return findings
}

// classifyBuildError maps a single Go compiler diagnostic to a category, an
// optional SDK contract rule_id, and a corrective step.
func classifyBuildError(e *BuildError) *ExplainFinding {
	msg := e.Message

	// Undefined symbol — the most common and most actionable.
	if i := strings.Index(msg, "undefined: "); i >= 0 {
		sym := strings.TrimSpace(msg[i+len("undefined: "):])
		f := &ExplainFinding{Error: e, Category: "undefined-symbol", Why: "a referenced symbol is not in scope", Fix: "define or import the missing symbol"}
		if strings.HasPrefix(sym, "event.Value") {
			// The Value accessors are fixed-name; a typo or wrong accessor
			// surfaces as undefined and the ok-return is easy to drop.
			f.RuleID = "value-accessor-ok"
			f.Why = "event.Value 访问器返回 (value, ok)，名字写错即 undefined；忽略 ok 也会编译失败或行为错误"
			f.Fix = "用 v, ok := event.ValueXxx(...) 并检查 ok；确认访问器名与 SDK 一致"
		} else if isPackageName(sym) {
			f.Why = "包未引入或 go.mod 未 tidy，导致符号不可见"
			f.Fix = "在 plugins/<name> 下 go mod tidy，并确认 import 路径正确"
		}
		return f
	}

	// Module / go.sum issues.
	if strings.Contains(msg, "missing go.sum entry") ||
		strings.Contains(msg, "updates to go.mod needed") ||
		strings.Contains(msg, "no required module provides") {
		return &ExplainFinding{Error: e, Category: "module", Why: "依赖未解析（go.mod / go.sum 缺失或过期）", Fix: "在 plugins/<name> 下运行 go mod tidy 后重新 build_plugin"}
	}

	// Type mismatches.
	if strings.Contains(msg, "cannot use") ||
		strings.Contains(msg, "cannot assign") ||
		strings.Contains(msg, "mismatched types") ||
		strings.Contains(msg, "is not") && strings.Contains(msg, "type") {
		return &ExplainFinding{Error: e, Category: "type-mismatch", Why: "类型不兼容：函数入参/返回值形状与调用处不符", Fix: "检查报错行的类型，对照 SDK 的类型签名修正"}
	}

	// The ok-return dropped: "declared and not used" for the ok variable.
	if strings.Contains(msg, "declared and not used") {
		return &ExplainFinding{Error: e, Category: "unused-var", RuleID: "value-accessor-ok", Why: "Value 访问器的 ok 返回值被丢弃（声明但未使用）", Fix: "使用 v, ok := event.ValueXxx(...) 并检查 ok，不要丢弃 ok"}
	}

	// Import issues.
	if strings.Contains(msg, "imported and not used") {
		return &ExplainFinding{Error: e, Category: "import", Why: "有 import 但未使用", Fix: "删除未使用的 import"}
	}

	// Syntax.
	if strings.Contains(msg, "syntax error") ||
		strings.Contains(msg, "expected '") ||
		strings.Contains(msg, "unexpected ") {
		return &ExplainFinding{Error: e, Category: "syntax", Why: "语法错误", Fix: "检查报错行的括号/分号/关键字"}
	}

	return &ExplainFinding{Error: e, Category: "other", Why: "未能自动归类的编译错误", Fix: "阅读 message 并对照 SDK 文档修正"}
}

// isPackageName reports whether sym looks like a package reference (pkg.Something),
// which suggests a missing import / go.mod entry rather than a local typo.
func isPackageName(sym string) bool {
	i := strings.Index(sym, ".")
	return i > 0 && i < len(sym)-1
}

// explainActivate classifies an activation (runtime registration) failure from
// the recorded message.
func explainActivate(last *LastAttempt) []*ExplainFinding {
	m := last.Message
	switch {
	case strings.Contains(m, "binary not found"):
		return []*ExplainFinding{{
			Category: "binary-missing",
			Why:      "插件尚未构建（artifact 仍是 scaffolded），没有可执行的二进制",
			Fix:      "先运行 build_plugin 生成二进制，再 activate_plugin",
		}}
	case strings.Contains(m, "exited immediately") || strings.Contains(m, "failed to stay up") || strings.Contains(m, "during startup"):
		return []*ExplainFinding{{
			Category: "process-crash",
			RuleID:   "error-not-panic",
			Why:      "进程在注册前就崩溃，常见原因是 main 发生 panic（如 GTA_REGISTRY_ADDR 解析失败、SDK 初始化错误）或未 defer recover()",
			Fix:      "读取 <name>.dev.log 看 panic 栈；在 main 加 defer recover()；确认 GTA_REGISTRY_ADDR 可达且格式正确；重新 build_plugin 后再 activate_plugin",
		}}
	case strings.Contains(m, "already active"):
		return []*ExplainFinding{{
			Category: "already-active",
			Why:      "该插件已在运行，不能重复拉起",
			Fix:      "先 deactivate_plugin 再 activate_plugin",
		}}
	case strings.Contains(m, "start failed"):
		return []*ExplainFinding{{
			Category: "start-failed",
			Why:      "进程无法启动（权限不足或缺失运行依赖，如 DLL）",
			Fix:      "检查二进制权限与运行依赖，再重新 activate_plugin",
		}}
	default:
		return []*ExplainFinding{{
			Category: "other",
			Why:      m,
			Fix:      "检查 message 中的错误并修正后重试",
		}}
	}
}

// activateNextAction derives a single human next step from the activation
// findings.
func activateNextAction(findings []*ExplainFinding) string {
	if len(findings) == 0 {
		return "inspect the reported message and retry"
	}
	switch findings[0].Category {
	case "binary-missing":
		return "run build_plugin, then activate_plugin"
	case "process-crash":
		return "fix the startup panic (see <name>.dev.log), re-run build_plugin, then activate_plugin"
	case "already-active":
		return "run deactivate_plugin, then activate_plugin"
	case "start-failed":
		return "fix the binary/permission issue, then re-run activate_plugin"
	default:
		return "inspect the reported message and retry"
	}
}
