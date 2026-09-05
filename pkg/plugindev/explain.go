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
	Action string // optional: build | activate | deactivate | verify; empty = latest

	// Verify is an inline verify result to attribute (decode-class failures,
	// P3b). When set, Action is treated as "verify" and the result is
	// classified directly. When omitted on a verify request, the most recent
	// result recorded by plugin.verify (P4) via RecordVerify is used.
	Verify *VerifyResult
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
	// Decode-class attribution (P3b): attribute a verify result rather than a
	// build/activate attempt. Either an inline result was supplied, or the AI
	// asked for action=verify and we fall back to the last recorded result.
	if req.Action == "verify" || req.Verify != nil {
		return explainVerifyRequest(req)
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

// explainVerifyRequest attributes a verify result for a plugin, either the one
// passed inline or the most recent recorded by plugin.verify (P4). It mirrors
// the build/activate path: a pass has nothing to fix, a failure is classified
// and the conclusion is stored so Status can surface explain_ref.
func explainVerifyRequest(req *ExplainRequest) (*ExplainResult, error) {
	res := &ExplainResult{
		Ref:    newExplainRef(),
		Name:   req.Name,
		Action: "verify",
		At:     time.Now(),
	}
	result := req.Verify
	if result == nil {
		result = defaultTracker.LastVerify(req.Name)
	}
	if result == nil {
		res.Summary = "no verify result recorded for " + req.Name
		res.NextAction = "run plugin.verify to produce a verdict, then explain with action=verify"
		defaultTracker.RecordExplain(req.Name, res)
		return res, nil
	}
	if result.Verdict == "pass" {
		res.Summary = "verify passed; nothing to explain"
		res.NextAction = "continue to bind (plugin.bind) to reach runtime.state=bound"
		defaultTracker.RecordExplain(req.Name, res)
		return res, nil
	}

	findings := explainVerify(result)
	if len(findings) == 0 {
		// Verify failed/warn but no decode-class pattern matched: still surface
		// the raw verdict so the AI has a pointer into the violations list.
		findings = []*ExplainFinding{{
			Category: "verify-other",
			Why:      "verify 未通过（verdict=" + result.Verdict + "）但未命中任何已知解码类模式；见 violations 与 quality 细节",
			Fix:      "检查 verify 返回的 violations 列表，按 rule_id 逐条修正后重新 verify",
		}}
	}
	res.Findings = findings
	res.Summary = fmt.Sprintf("verify=%s with %d decode finding(s)", result.Verdict, len(findings))
	res.NextAction = verifyNextAction(findings)
	defaultTracker.RecordExplain(req.Name, res)
	return res, nil
}

// explainVerify classifies a verify result into the four decode-class findings
// (design §7 / P3b). Each finding references a contract.yaml rule_id so the AI
// can cross-reference the same vocabulary used by brief/verify.
//
// The patterns are intentionally independent and may co-occur (e.g. a decoder
// that both strips headers AND sees all-unknown), so multiple findings can be
// returned.
func explainVerify(result *VerifyResult) []*ExplainFinding {
	var findings []*ExplainFinding

	// 1) Wrong framing — driven by SDK violations on the framing rules.
	// These fire when a decoder treats a full link-layer frame as L7 (did NOT
	// strip by link_type / reassemble). This is the highest-signal finding
	// because it comes straight from the contract checker, not a heuristic.
	for _, v := range result.Violations {
		switch v.RuleID {
		case "payload-framing-by-link-type", "link-type-selects-framing":
			findings = append(findings, &ExplainFinding{
				Category: "wrong-framing",
				RuleID:   v.RuleID,
				Why:      "SDK 校验（" + v.RuleID + "）判定解码器把完整链路层帧当成了 L7：pcap 类来源交付的是带链路/网络/传输头的完整帧，需要先按 link_type 剥头、再按流重组，而不是直接当应用层字节解析",
				Fix:      "用 framing.ExtractL7(payload, link_type) 按 link_type 剥头，再用 framing.NewReassembler 做 TCP 重组；只有 ProxyPayload(1001)/TLSPlaintext(1002) 才是纯 L7（见 contract.yaml payload-framing-by-link-type）",
			})
		}
	}

	q := result.Quality
	if q == nil {
		// No quality stats: only SDK violations are attributable.
		return findings
	}

	// 2) All-unknown — every input produced no event.
	unknownRatio := q.UnknownRatio
	if unknownRatio <= 0 && q.TotalInputs > 0 {
		unknownRatio = float64(q.UnknownInputs) / float64(q.TotalInputs)
	}
	if q.TotalInputs > 0 && q.UnknownInputs >= q.TotalInputs {
		findings = append(findings, &ExplainFinding{
			Category: "all-unknown",
			RuleID:   "inspect-bytes-first",
			Why:      "解码器对全部 " + strconv.Itoa(q.TotalInputs) + " 个输入都未产出任何事件（全 unknown）。典型根因是剥头/重组缺失：把带链路层头的完整帧直接当 L7 解析，业务字节整体错位导致 0 命中",
			Fix:      "①先用 sample_bytes_plugin 看真实首字节确认 link_type 与帧结构；②用 framing.ExtractL7 按 link_type 剥头；③TCP 类协议接 framing.Reassembler 重组。不要假设 payload 已是 L7（只有 ProxyPayload/TLSPlaintext 才是）",
		})
	} else if unknownRatio >= AllUnknownRatioThreshold {
		findings = append(findings, &ExplainFinding{
			Category: "all-unknown",
			RuleID:   "inspect-bytes-first",
			Why:      fmt.Sprintf("%.0f%% 的输入未能解码（全 unknown），大概率解码器没按 link_type 剥头/重组，把完整帧当 L7 解析", unknownRatio*100),
			Fix:      "先用 sample_bytes_plugin 看真实首字节；用 framing.ExtractL7 剥头 + framing.Reassembler 重组；TCP body 恒空往往是缺重组的信号",
		})
	}

	// 3) Suspected encryption/compression — high entropy + majority undecodable.
	if q.EntropyEstimate >= HighEntropyThreshold && unknownRatio >= EncryptionUnknownRatioThreshold {
		findings = append(findings, &ExplainFinding{
			Category: "suspected-encryption",
			RuleID:   "inspect-bytes-first",
			Why: fmt.Sprintf("首字节分布接近均匀、熵估计约 %.1f bit/byte（接近 8 上限），且 %.0f%% 输入未能解码，疑似加密或压缩流，没有明文 framing 可识别",
				q.EntropyEstimate, unknownRatio*100),
			Fix: "确认该协议是否真有明文层；若确为加密/压缩，先解密再解码，或显式将 schema 标注为 opaque bytes 而非强行解析",
		})
	}

	// 4) Suspected missing stream reassembly — many inputs, none correlated,
	// but the decoder DID produce some events (otherwise there is nothing to
	// correlate and the all-unknown finding already covers it).
	if q.TotalInputs > 1 && q.CorrelatedInputs == 0 && (q.TotalInputs-q.UnknownInputs) > 0 {
		findings = append(findings, &ExplainFinding{
			Category: "suspected-reassembly",
			RuleID:   "tcp-reassembly-required",
			Why:      fmt.Sprintf("同一会话被切成 %d 个 input，但没有任何 correlation_key 串联，疑似缺流重组或粘包未切分", q.TotalInputs),
			Fix:      "按流重组后再按长度前缀/分隔符切分消息（用 framing.NewReassembler）；只有在确实可推断时才填 correlation_key（correlation-only-when-known）",
		})
	}

	return findings
}

// verifyNextAction derives a single human next step from the decode findings,
// ordered by how directly it unblocks the decoder.
func verifyNextAction(findings []*ExplainFinding) string {
	if len(findings) == 0 {
		return "review the verify verdict and retest"
	}
	switch findings[0].Category {
	case "wrong-framing":
		return "add framing.ExtractL7 + Reassembler to strip by link_type (payload is a full frame, NOT L7), then re-verify"
	case "all-unknown":
		return "run sample_bytes_plugin to inspect real bytes, then add framing.ExtractL7 + Reassembler"
	case "suspected-encryption":
		return "confirm whether the stream is encrypted/compressed; decrypt first or mark schema opaque"
	case "suspected-reassembly":
		return "reassembly the stream and split messages by length-prefix/delimiter before decoding"
	default:
		return "review the verify violations and retest"
	}
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
			Why:      "进程在注册前就崩溃，常见原因是 main 发生 panic（如 GT_REGISTRY_ADDR 解析失败、SDK 初始化错误）或未 defer recover()",
			Fix:      "读取 <name>.dev.log 看 panic 栈；在 main 加 defer recover()；确认 GT_REGISTRY_ADDR 可达且格式正确；重新 build_plugin 后再 activate_plugin",
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
