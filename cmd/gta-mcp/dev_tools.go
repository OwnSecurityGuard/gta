package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	pb "gta/pkg/internalipc/proto"
	plugindevpb "gta/pkg/plugindev/proto"
)

// handleBuildPlugin compiles a scaffolded plugin project via the Developer
// Plane and returns structured file:line:col diagnostics on failure. gta-mcp
// never runs the compiler itself — it forwards to pdClient.Build.
func (m *mcpCapture) handleBuildPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	timeoutSec := int(req.GetInt("timeout_sec", 0))
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	resp, err := m.pdClient.Build(ctx, name, timeoutSec)
	if err != nil {
		return errorResult(err), nil
	}
	out := map[string]any{
		"name":   name,
		"ok":     resp.GetOk(),
		"output": resp.GetOutput(),
	}
	var errs []map[string]any
	for _, e := range resp.GetErrors() {
		errs = append(errs, map[string]any{
			"file":    e.GetFile(),
			"line":    e.GetLine(),
			"col":     e.GetCol(),
			"message": e.GetMessage(),
		})
	}
	out["errors"] = errs
	return successResult(out), nil
}

// handleActivatePlugin launches the local plugin binary (Developer Plane) and
// injects GTA_REGISTRY_ADDR so it registers with the runtime. registry_addr
// defaults to the GTA_REGISTRY_ADDR env when not supplied.
func (m *mcpCapture) handleActivatePlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	registryAddr := req.GetString("registry_addr", "")
	if registryAddr == "" {
		registryAddr = os.Getenv("GTA_REGISTRY_ADDR")
	}
	if registryAddr == "" {
		return errorResult(fmt.Errorf("registry_addr is required (pass it, or set GTA_REGISTRY_ADDR to the pipeline's registry address)")), nil
	}
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	resp, err := m.pdClient.Activate(ctx, name, registryAddr)
	if err != nil {
		return errorResult(err), nil
	}
	return successResult(map[string]any{
		"name":        name,
		"ok":          resp.GetOk(),
		"instance_id": resp.GetInstanceId(),
		"message":     resp.GetMessage(),
	}), nil
}

// handleDeactivatePlugin stops the process the Developer Plane launched for the
// plugin. It also best-effort force-deregisters the plugin from the runtime
// registry, covering the case where the process was started externally
// (design: deregister_plugin folds into deactivate — kill if we launched it,
// otherwise force-deregister).
func (m *mcpCapture) handleDeactivatePlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	resp, err := m.pdClient.Deactivate(ctx, name)
	if err != nil {
		return errorResult(err), nil
	}
	out := map[string]any{
		"name":    name,
		"ok":      resp.GetOk(),
		"message": resp.GetMessage(),
	}
	// Best-effort: also remove from the runtime registry if present.
	if m.pipelineClient != nil {
		dresp, derr := m.pipelineClient.DeregisterPlugin(ctx, &pb.DeregisterPluginRequest{Name: name})
		if derr != nil {
			out["registry_deregister"] = map[string]any{"ok": false, "error": derr.Error()}
		} else {
			out["registry_deregister"] = map[string]any{"ok": dresp.GetOk(), "instance_id": dresp.GetInstanceId()}
		}
	}
	return successResult(out), nil
}

// handleStatusPlugin returns the dual-state view (design §2): the artifact
// (Developer Plane, from disk) merged with the runtime state (Runtime Plane,
// from the registry via pipelineClient.ListPlugins), plus the last attempt for
// failure attribution and a suggested next_action.
func (m *mcpCapture) handleStatusPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	ps, err := m.pdClient.Status(ctx, name)
	if err != nil {
		return errorResult(err), nil
	}

	artifact := map[string]any{
		"state":        "unknown",
		"binary_stale": false,
	}
	if a := ps.GetArtifact(); a != nil {
		artifact["state"] = a.GetState()
		artifact["source_dir"] = a.GetSourceDir()
		artifact["binary_path"] = a.GetBinaryPath()
		artifact["binary_stale"] = a.GetBinaryStale()
	}

	devProcess := map[string]any{"launched": false}
	if d := ps.GetDevProcess(); d != nil {
		devProcess = map[string]any{
			"launched":    d.GetLaunched(),
			"pid":         d.GetPid(),
			"instance_id": d.GetInstanceId(),
			"alive":       d.GetAlive(),
			"launched_at": d.GetLaunchedAtUnix(),
		}
	}

	lastAttempt := map[string]any{}
	if la := ps.GetLastAttempt(); la != nil {
		lastAttempt = map[string]any{
			"action":      la.GetAction(),
			"ok":          la.GetOk(),
			"at_unix":     la.GetAtUnix(),
			"duration_ms": la.GetDurationMs(),
			"message":     la.GetMessage(),
			"explain_ref": la.GetExplainRef(),
		}
		var errs []map[string]any
		for _, e := range la.GetErrors() {
			errs = append(errs, map[string]any{
				"file":    e.GetFile(),
				"line":    e.GetLine(),
				"col":     e.GetCol(),
				"message": e.GetMessage(),
			})
		}
		lastAttempt["errors"] = errs
	}

	// Runtime state comes from the Runtime Plane (the registry).
	runtimeState, runtime := m.runtimeState(ctx, name, devProcess)
	next := nextAction(artifact["state"].(string), runtimeState)

	return successResult(map[string]any{
		"name":         name,
		"artifact":     artifact,
		"runtime":      runtime,
		"dev_process":  devProcess,
		"last_attempt": lastAttempt,
		"next_action":  next,
	}), nil
}

// runtimeState queries the registry for the named plugin and derives a runtime
// state string. When the pipeline is unavailable it falls back to the
// Developer Plane's own view of the launched process.
func (m *mcpCapture) runtimeState(ctx context.Context, name string, devProcess map[string]any) (string, map[string]any) {
	runtime := map[string]any{
		"state":          "offline",
		"instance_id":    "",
		"online":         false,
		"last_heartbeat": int64(0),
		"bound_sessions": []any{},
	}
	if m.pipelineClient == nil {
		// No runtime link; infer from the dev-launched process only.
		if launched, _ := devProcess["launched"].(bool); launched {
			if alive, _ := devProcess["alive"].(bool); alive {
				runtime["state"] = "registered"
			}
		}
		return runtime["state"].(string), runtime
	}
	resp, err := m.pipelineClient.ListPlugins(ctx, &pb.ListPluginsRequest{})
	if err != nil {
		// Registry unreachable: degrade to the dev-side view.
		if launched, _ := devProcess["launched"].(bool); launched {
			if alive, _ := devProcess["alive"].(bool); alive {
				runtime["state"] = "registered"
			}
		}
		return runtime["state"].(string), runtime
	}
	for _, p := range resp.GetPlugins() {
		if p.GetName() != name {
			continue
		}
		online := p.GetOnline()
		state := "registered"
		if online {
			state = "active"
		}
		runtime = map[string]any{
			"state":          state,
			"instance_id":    p.GetInstanceId(),
			"online":         online,
			"last_heartbeat": p.GetLastHeartbeatUnix(),
			"bound_sessions": []any{},
		}
		return state, runtime
	}
	// Not in registry: if we launched it and it's alive, it's mid-registration.
	if launched, _ := devProcess["launched"].(bool); launched {
		if alive, _ := devProcess["alive"].(bool); alive {
			runtime["state"] = "registered"
		}
	}
	return runtime["state"].(string), runtime
}

// nextAction suggests the next tool based on the dual-state, per design §2.2.
func nextAction(artifactState, runtimeState string) map[string]any {
	compiled := artifactState == "compiled"
	offlineOrRegistered := runtimeState == "offline" || runtimeState == "registered"
	switch {
	case artifactState == "unknown" || artifactState == "scaffolded":
		return map[string]any{"tool": "build_plugin", "why": "plugin not compiled yet"}
	case compiled && offlineOrRegistered:
		return map[string]any{"tool": "activate_plugin", "why": "compiled but not active; launch it and register with the runtime"}
	case compiled && runtimeState == "active":
		return map[string]any{"tool": "verify (P4)", "why": "active but not validated; run plugin.verify to reach artifact.state=validated"}
	default:
		return nil
	}
}

// handleExplainPlugin attributes the most recent failure of a plugin via the
// Developer Plane (design §2.3 / P3a / P3b). It is a pure forwarder — gta-mcp
// owns no attribution logic — and its ref is what status_plugin's
// last_attempt.explain_ref points back to.
//
// For decode-class attribution (P3b) the caller may pass a `verify` object
// (the plugin.verify result: {violations, quality, verdict}); it is mapped onto
// the gRPC VerifyResult and forwarded verbatim. When omitted, the Developer
// Plane attributes the most recent result recorded by plugin.verify (P4).
func (m *mcpCapture) handleExplainPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	action := req.GetString("action", "")
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}

	var pbVerify *plugindevpb.VerifyResult
	if m, ok := req.Params.Arguments.(map[string]any); ok {
		if raw, ok := m["verify"]; ok && raw != nil {
			pbVerify = verifyResultFromArg(raw)
		}
	}

	resp, err := m.pdClient.Explain(ctx, name, action, pbVerify)
	if err != nil {
		return errorResult(err), nil
	}
	out := map[string]any{
		"ref":         resp.GetRef(),
		"name":        resp.GetName(),
		"action":      resp.GetAction(),
		"at_unix":     resp.GetAtUnix(),
		"summary":     resp.GetSummary(),
		"next_action": resp.GetNextAction(),
	}
	var findings []map[string]any
	for _, f := range resp.GetFindings() {
		fm := map[string]any{
			"category": f.GetCategory(),
			"rule_id":  f.GetRuleId(),
			"why":      f.GetWhy(),
			"fix":      f.GetFix(),
		}
		if e := f.GetError(); e != nil {
			fm["error"] = map[string]any{
				"file":    e.GetFile(),
				"line":    e.GetLine(),
				"col":     e.GetCol(),
				"message": e.GetMessage(),
			}
		}
		findings = append(findings, fm)
	}
	out["findings"] = findings
	return successResult(out), nil
}

// verifyResultFromArg maps the `verify` tool argument (an opaque JSON object
// produced by plugin.verify: {violations, quality, verdict}) onto the gRPC
// VerifyResult. It is strict only about shape, never about attribution — all
// interpretation stays in the Developer Plane. An unparseable payload yields a
// nil result, which makes the Developer Plane attribute its last recorded
// verify instead.
func verifyResultFromArg(raw any) *plugindevpb.VerifyResult {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var v struct {
		Violations []struct {
			RuleID    string `json:"rule_id"`
			Topic     string `json:"topic"`
			Severity  string `json:"severity"`
			Statement string `json:"statement"`
			DocRef    string `json:"doc_ref"`
			Count     int    `json:"count"`
			Sample    string `json:"sample"`
		} `json:"violations"`
		Quality struct {
			TotalInputs          int     `json:"total_inputs"`
			UnknownInputs        int     `json:"unknown_inputs"`
			UnknownRatio         float64 `json:"unknown_ratio"`
			CorrelatedInputs     int     `json:"correlated_inputs"`
			LongPacketErrors     int     `json:"long_packet_errors"`
			EntropyEstimate      float64 `json:"entropy_estimate"`
			SchemaVersionedRatio float64 `json:"schema_versioned_ratio"`
			DecodeErrors         int     `json:"decode_errors"`
		} `json:"quality"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	out := &plugindevpb.VerifyResult{Verdict: v.Verdict}
	for _, vv := range v.Violations {
		out.Violations = append(out.Violations, &plugindevpb.Violation{
			RuleId:    vv.RuleID,
			Topic:     vv.Topic,
			Severity:  vv.Severity,
			Statement: vv.Statement,
			DocRef:    vv.DocRef,
			Count:     int32(vv.Count),
			Sample:    vv.Sample,
		})
	}
	out.Quality = &plugindevpb.QualityStats{
		TotalInputs:          int32(v.Quality.TotalInputs),
		UnknownInputs:        int32(v.Quality.UnknownInputs),
		UnknownRatio:         v.Quality.UnknownRatio,
		CorrelatedInputs:     int32(v.Quality.CorrelatedInputs),
		LongPacketErrors:     int32(v.Quality.LongPacketErrors),
		EntropyEstimate:      v.Quality.EntropyEstimate,
		SchemaVersionedRatio: v.Quality.SchemaVersionedRatio,
		DecodeErrors:         int32(v.Quality.DecodeErrors),
	}
	return out
}
