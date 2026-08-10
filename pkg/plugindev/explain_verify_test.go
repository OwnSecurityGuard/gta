package plugindev

import (
	"context"
	"strings"
	"testing"
)

// hasFinding reports whether findings contains an entry with the given category.
func hasFinding(findings []*ExplainFinding, category string) bool {
	for _, f := range findings {
		if f.Category == category {
			return true
		}
	}
	return false
}

// findFinding returns the first finding with the given category, or nil.
func findFinding(findings []*ExplainFinding, category string) *ExplainFinding {
	for _, f := range findings {
		if f.Category == category {
			return f
		}
	}
	return nil
}

// TestExplainVerifyAllUnknown locks in the "全 unknown" attribution: a corpus
// where every input produced no event must surface an all-unknown finding
// referencing inspect-bytes-first (and must NOT flag reassembly, since nothing
// was decoded to correlate).
func TestExplainVerifyAllUnknown(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{
		Name:   "all-unknown",
		Action: "verify",
		Verify: &VerifyResult{
			Verdict: "fail",
			Quality: &QualityStats{TotalInputs: 10, UnknownInputs: 10},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "verify" {
		t.Fatalf("expected action=verify, got %q", res.Action)
	}
	if !hasFinding(res.Findings, "all-unknown") {
		t.Fatalf("expected all-unknown finding, got %+v", res.Findings)
	}
	f := findFinding(res.Findings, "all-unknown")
	if f.RuleID != "inspect-bytes-first" {
		t.Fatalf("expected rule_id inspect-bytes-first, got %q", f.RuleID)
	}
	if hasFinding(res.Findings, "suspected-reassembly") {
		t.Fatalf("all-unknown must not also flag reassembly (nothing decoded to correlate)")
	}
}

// TestExplainVerifyWrongFraming locks in the "错 framing" attribution: an SDK
// payload-framing-by-link-type violation must surface a wrong-framing finding
// pointing back at the same rule_id so the AI can cross-reference contract.yaml.
// The violation means the decoder treated a full link-layer frame as L7 (did NOT
// strip), so the advice must be to add framing — the opposite of the old
// payload-framing-by-link-type guidance.
func TestExplainVerifyWrongFraming(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{
		Name:   "wrong-framing",
		Action: "verify",
		Verify: &VerifyResult{
			Verdict: "fail",
			Violations: []*Violation{
				{RuleID: "payload-framing-by-link-type", Topic: "framing", Severity: "error", Statement: "payload is full frame, strip by link_type"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := findFinding(res.Findings, "wrong-framing")
	if f == nil {
		t.Fatalf("expected wrong-framing finding, got %+v", res.Findings)
	}
	if f.RuleID != "payload-framing-by-link-type" {
		t.Fatalf("expected rule_id payload-framing-by-link-type, got %q", f.RuleID)
	}
	if f.Fix == "" || !strings.Contains(f.Fix, "ExtractL7") {
		t.Fatalf("wrong-framing fix must tell the dev to strip via framing.ExtractL7, got %q", f.Fix)
	}
}

// TestExplainVerifySuspectedEncryption locks in the "疑似加密" attribution:
// high payload entropy combined with a majority of undecodable inputs must
// surface a suspected-encryption finding referencing inspect-bytes-first.
func TestExplainVerifySuspectedEncryption(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{
		Name:   "enc",
		Action: "verify",
		Verify: &VerifyResult{
			Verdict: "fail",
			Quality: &QualityStats{
				TotalInputs:     10,
				UnknownInputs:   8,
				UnknownRatio:    0.8,
				EntropyEstimate: 7.8,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := findFinding(res.Findings, "suspected-encryption")
	if f == nil {
		t.Fatalf("expected suspected-encryption finding, got %+v", res.Findings)
	}
	if f.RuleID != "inspect-bytes-first" {
		t.Fatalf("expected rule_id inspect-bytes-first, got %q", f.RuleID)
	}
}

// TestExplainVerifySuspectedReassembly locks in the "疑似缺流重组" attribution:
// many inputs that produced events but were never correlated must surface a
// suspected-reassembly finding referencing tcp-reassembly-required.
func TestExplainVerifySuspectedReassembly(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{
		Name:   "reasm",
		Action: "verify",
		Verify: &VerifyResult{
			Verdict: "warn",
			Quality: &QualityStats{
				TotalInputs:      5,
				UnknownInputs:    0,
				CorrelatedInputs: 0,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := findFinding(res.Findings, "suspected-reassembly")
	if f == nil {
		t.Fatalf("expected suspected-reassembly finding, got %+v", res.Findings)
	}
	if f.RuleID != "tcp-reassembly-required" {
		t.Fatalf("expected rule_id tcp-reassembly-required, got %q", f.RuleID)
	}
}

// TestExplainVerifyPass verifies a passing verdict yields nothing to fix.
func TestExplainVerifyPass(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{
		Name:   "ok",
		Action: "verify",
		Verify: &VerifyResult{Verdict: "pass", Quality: &QualityStats{TotalInputs: 3, UnknownInputs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "verify passed; nothing to explain" {
		t.Fatalf("expected pass summary, got %q", res.Summary)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected no findings on pass, got %+v", res.Findings)
	}
}

// TestExplainVerifyNoResult verifies a verify request with no recorded result
// (and no inline result) is a graceful no-op rather than an error.
func TestExplainVerifyNoResult(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{Name: "no-result", Action: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "no verify result recorded for no-result" {
		t.Fatalf("expected no-result summary, got %q", res.Summary)
	}
}

// TestExplainVerifyRecorded verifies the attribution falls back to the result
// recorded by plugin.verify (P4) via RecordVerify when none is passed inline.
func TestExplainVerifyRecorded(t *testing.T) {
	name := "recorded"
	RecordVerify(name, &VerifyResult{
		Verdict: "fail",
		Quality: &QualityStats{TotalInputs: 4, UnknownInputs: 4},
	})
	res, err := Explain(context.Background(), &ExplainRequest{Name: name, Action: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(res.Findings, "all-unknown") {
		t.Fatalf("expected all-unknown from recorded result, got %+v", res.Findings)
	}
}

// TestExplainVerifyFailNoPattern verifies a failing verdict that matches no
// decode pattern still returns a pointer to the violations list.
func TestExplainVerifyFailNoPattern(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{
		Name:   "other",
		Action: "verify",
		Verify: &VerifyResult{
			Verdict: "fail",
			Violations: []*Violation{
				{RuleID: "schema-id-versioned", Topic: "schema", Severity: "warn"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(res.Findings, "verify-other") {
		t.Fatalf("expected verify-other fallback finding, got %+v", res.Findings)
	}
}

// TestRecordVerifyClearedOnBuild verifies the validated-invalidation rule: a
// successful build clears the last verify result, so a subsequent explain can no
// longer fall back to it (design §2.2).
func TestRecordVerifyClearedOnBuild(t *testing.T) {
	name := "cleared"
	RecordVerify(name, &VerifyResult{Verdict: "fail", Quality: &QualityStats{TotalInputs: 2, UnknownInputs: 2}})
	if DefaultTracker().LastVerify(name) == nil {
		t.Fatal("expected verify result recorded")
	}
	DefaultTracker().RecordBuild(name, 0, &BuildResponse{OK: true})
	if DefaultTracker().LastVerify(name) != nil {
		t.Fatal("successful build must clear last verify result")
	}
}
