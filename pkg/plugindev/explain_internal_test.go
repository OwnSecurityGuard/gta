package plugindev

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These tests drive the package-level defaultTracker so they can inspect the
// explain_ref wiring directly (Explain / RecordBuild / RecordActivate all
// operate on it). Each test uses a unique plugin name to avoid cross-test
// contamination of the shared tracker.

func TestExplainBuildUndefinedSymbol(t *testing.T) {
	name := "explain-build-undef"
	defaultTracker.RecordBuild(name, time.Second, &BuildResponse{
		OK: false,
		Errors: []*BuildError{
			{File: "main.go", Line: 42, Col: 9, Message: "undefined: event.ValueInt32"},
		},
	})
	res, err := Explain(context.Background(), &ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Action != "build" {
		t.Fatalf("action=%q want build", res.Action)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings=%d want 1", len(res.Findings))
	}
	f := res.Findings[0]
	if f.Category != "undefined-symbol" {
		t.Fatalf("category=%q want undefined-symbol", f.Category)
	}
	if f.RuleID != "value-accessor-ok" {
		t.Fatalf("rule_id=%q want value-accessor-ok", f.RuleID)
	}
	if f.Error == nil || f.Error.Line != 42 {
		t.Fatalf("error not mapped: %+v", f.Error)
	}
	// explain_ref must be written back into last_attempt.
	la := defaultTracker.LastAttempt(name)
	if la == nil || la.ExplainRef != res.Ref {
		t.Fatalf("explain_ref not written back: %+v", la)
	}
	if defaultTracker.ExplainResultOf(name) == nil {
		t.Fatal("explain result not recorded")
	}
}

func TestExplainBuildModuleIssue(t *testing.T) {
	name := "explain-build-module"
	defaultTracker.RecordBuild(name, time.Second, &BuildResponse{
		OK: false,
		Errors: []*BuildError{
			{File: "go.mod", Line: 1, Col: 1, Message: "updates to go.mod needed"},
		},
	})
	res, err := Explain(context.Background(), &ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Category != "module" {
		t.Fatalf("expected module finding, got %+v", res.Findings)
	}
}

func TestExplainActivateBinaryMissing(t *testing.T) {
	name := "explain-activate-missing"
	defaultTracker.RecordActivate(name, 0, false, "binary not found: /x/plugins/foo/foo.exe")
	res, err := Explain(context.Background(), &ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Action != "activate" {
		t.Fatalf("action=%q want activate", res.Action)
	}
	if len(res.Findings) != 1 || res.Findings[0].Category != "binary-missing" {
		t.Fatalf("expected binary-missing finding, got %+v", res.Findings)
	}
	if !strings.Contains(res.NextAction, "build_plugin") {
		t.Fatalf("next_action=%q should mention build_plugin", res.NextAction)
	}
}

func TestExplainActivateProcessCrash(t *testing.T) {
	name := "explain-activate-crash"
	defaultTracker.RecordActivate(name, 0, false, "process exited during startup: panic: runtime error")
	res, err := Explain(context.Background(), &ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Category != "process-crash" {
		t.Fatalf("expected process-crash finding, got %+v", res.Findings)
	}
	if res.Findings[0].RuleID != "error-not-panic" {
		t.Fatalf("expected error-not-panic rule_id, got %q", res.Findings[0].RuleID)
	}
}

func TestExplainNoFailure(t *testing.T) {
	name := "explain-ok"
	defaultTracker.RecordBuild(name, time.Second, &BuildResponse{OK: true})
	res, err := Explain(context.Background(), &ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(res.Summary, "succeeded") {
		t.Fatalf("summary=%q should say succeeded", res.Summary)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected no findings on success, got %+v", res.Findings)
	}
}

func TestExplainNoAttempt(t *testing.T) {
	res, err := Explain(context.Background(), &ExplainRequest{Name: "explain-no-attempt-unique"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Findings != nil {
		t.Fatalf("expected nil findings, got %+v", res.Findings)
	}
}

// TestActivateAutoExplains verifies that a failed Activate automatically runs
// Explain and writes explain_ref into last_attempt (design §2.3 / P3a).
func TestActivateAutoExplains(t *testing.T) {
	name := "auto-explain-activate"
	root := t.TempDir()
	_, err := Activate(context.Background(), &ActivateRequest{
		Root:         root,
		Name:         name,
		RegistryAddr: "127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected Activate to fail (binary not found)")
	}
	la := defaultTracker.LastAttempt(name)
	if la == nil {
		t.Fatal("last attempt not recorded")
	}
	if la.ExplainRef == "" {
		t.Fatal("auto explain_ref not set on last_attempt")
	}
	res := defaultTracker.ExplainResultOf(name)
	if res == nil {
		t.Fatal("explain result not recorded")
	}
	if len(res.Findings) != 1 || res.Findings[0].Category != "binary-missing" {
		t.Fatalf("auto explain wrong category: %+v", res.Findings)
	}
}
