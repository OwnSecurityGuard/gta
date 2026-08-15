// Command verify_four_layer exercises the GTA decoder semantic-contract
// validation end-to-end against the four-layer-demo plugin manifest.
//
// It runs the SAME SDK PluginChecker that gta-pipeline uses at registration
// (declaration phase: runtime/schema/state/evidence/rule) and at decode time
// (CheckEvent: event + payload conformance + state changes + evidence), and
// prints a per-layer verdict. Negative scenarios prove the chain is live (not a
// no-op) by feeding tampered events that each trip a specific layer's rule.
package main

import (
	"fmt"
	"os"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
)

func main() {
	manifestPath := "plugins/four-layer-demo/plugin.yaml"
	if len(os.Args) > 1 {
		manifestPath = os.Args[1]
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fatal("read manifest %s: %v", manifestPath, err)
	}
	m, err := sdk.ParseManifest(data)
	if err != nil {
		fatal("parse manifest: %v", err)
	}
	if err := sdk.ValidateManifest(m); err != nil {
		fatal("validate manifest: %v", err)
	}
	fmt.Printf("manifest loaded: name=%s protocol=%s capabilities=", m.Name, m.Protocol)
	for c := range m.Capabilities {
		fmt.Printf("%s ", c)
	}
	fmt.Println()

	checker := contract.NewPluginChecker()

	// ── Phase 1: declaration-time, all four layers ────────────────────────────
	decl := checker.Check(m)
	printReport("PHASE 1 · DECLARATION (runtime/schema/state/evidence/rule)", decl)

	// ── Phase 2: decode-time, valid four-layer event ─────────────────────────
	valid := validLoginDraft("p1", "alice")
	dec := checker.CheckEvent(m, valid)
	printReport("PHASE 2 · DECODE-TIME valid event (event/schema/state/evidence)", dec)

	// ── Phase 3: negative scenarios, one per layer, to prove liveness ────────
	fmt.Println("\n=== PHASE 3 · NEGATIVE SCENARIOS (each should trip a specific layer) ===")
	negatives := []struct {
		name string
		d    *event.Draft
	}{
		{"undeclared schema_id (event/schema layer)", undeclaredSchemaDraft("p1")},
		{"reserved gta. event_type prefix (event layer)", reservedPrefixDraft()},
		{"missing required field under strict schema (schema layer)", missingFieldDraft("p1")},
		{"state change with undeclared subject (state layer)", unknownSubjectDraft("p1")},
		{"evidence with undeclared semantic (evidence layer)", unknownEvidenceDraft("p1")},
	}
	for _, n := range negatives {
		r := checker.CheckEvent(m, n.d)
		status := "CLEAN (unexpected)"
		if r.HasErrors() {
			status = "TRIPPED"
		}
		ids := ruleIDs(r)
		fmt.Printf("\n  • %-55s => %s  [%s]\n", n.name, status, ids)
		for _, v := range r.Violations {
			fmt.Printf("      [%s/%s] %s :: %s\n", v.Layer, v.Severity, v.RuleID, v.Message)
		}
	}

	fmt.Println("\n=== SUMMARY ===")
	fmt.Printf("registration-phase (declaration) errors : %d\n", countErr(decl))
	fmt.Printf("decode-time valid-event errors         : %d\n", countErr(dec))
	fmt.Println("negative scenarios should each report >=1 error (chain is live).")
}

// ── draft builders ───────────────────────────────────────────────────────────

func validLoginDraft(pid, name string) *event.Draft {
	return &event.Draft{
		Type:      "demo.login",
		SchemaRef: "demo.player.v1",
		Value: event.ValueFromMap(map[string]any{
			"player_id": pid,
			"name":      name,
			"hp":        int64(100),
			"_state_changes": []any{
				map[string]any{
					"subject_type": "player", "subject_id": pid, "op": "set", "path": "hp",
					"before": int64(0), "after": int64(100), "version": int64(1),
				},
			},
			"_evidence": []any{
				map[string]any{
					"kind": "observation", "semantic": "demo.observation.login",
					"statement": "player " + pid + " logged in", "strength": float64(1.0),
					"method": "decode",
					"sources": []any{map[string]any{"kind": "event", "local": "self.event"}},
					"rule_id": "demo.auth.login-success",
				},
			},
			"_meta": map[string]any{"direction": "client_to_server"},
		}),
		CorrelationKey: pid,
	}
}

func undeclaredSchemaDraft(pid string) *event.Draft {
	d := validLoginDraft(pid, "bob")
	d.SchemaRef = "demo.unknown.v1" // not declared in manifest
	return d
}

func reservedPrefixDraft() *event.Draft {
	d := validLoginDraft("p1", "carol")
	d.Type = "gta.foo" // reserved platform namespace
	return d
}

func missingFieldDraft(pid string) *event.Draft {
	d := validLoginDraft(pid, "dave")
	// drop "name" (required, strict schema) by rebuilding without it
	d.Value = event.ValueFromMap(map[string]any{
		"player_id": pid,
		"hp":        int64(100),
		"_meta":     map[string]any{"direction": "client_to_server"},
	})
	return d
}

func unknownSubjectDraft(pid string) *event.Draft {
	d := validLoginDraft(pid, "eve")
	d.Value = event.ValueFromMap(map[string]any{
		"player_id": pid,
		"name":      "eve",
		"hp":        int64(100),
		"_state_changes": []any{
			map[string]any{
				"subject_type": "monster", "subject_id": "m1", "op": "set", "path": "hp",
				"before": int64(0), "after": int64(50), "version": int64(1),
			},
		},
	})
	return d
}

func unknownEvidenceDraft(pid string) *event.Draft {
	d := validLoginDraft(pid, "frank")
	d.Value = event.ValueFromMap(map[string]any{
		"player_id": pid,
		"name":      "frank",
		"hp":        int64(100),
		"_evidence": []any{
			map[string]any{
				"kind": "observation", "semantic": "demo.observation.hax", // not declared
				"statement": "suspicious", "strength": float64(1.0),
				"method": "decode",
				"sources": []any{map[string]any{"kind": "event", "local": "self.event"}},
			},
		},
	})
	return d
}

// ── reporting helpers ────────────────────────────────────────────────────────

func printReport(title string, r *contract.Report) {
	fmt.Printf("\n=== %s ===\n", title)
	if len(r.Violations) == 0 {
		fmt.Println("  PASS · 0 violations across all layers")
	}
	for _, st := range r.Stats() {
		if st.Errors == 0 && st.Warns == 0 {
			continue
		}
		fmt.Printf("  layer %-9s errors=%d warns=%d\n", st.Layer, st.Errors, st.Warns)
	}
	for _, v := range r.Violations {
		fmt.Printf("  [%s/%s] %s :: %s\n", v.Layer, v.Severity, v.RuleID, v.Message)
	}
}

func countErr(r *contract.Report) int {
	n := 0
	for _, v := range r.Violations {
		if v.Severity == contract.SevError {
			n++
		}
	}
	return n
}

func ruleIDs(r *contract.Report) string {
	seen := map[string]bool{}
	out := ""
	for _, v := range r.Violations {
		if seen[v.RuleID] {
			continue
		}
		seen[v.RuleID] = true
		if out != "" {
			out += ", "
		}
		out += v.RuleID
	}
	if out == "" {
		return "no rule tripped"
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}
