package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

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
// resolves in this order: explicit arg → GTA_REGISTRY_ADDR env → the pipeline's
// actual registry address (via GetRegistryAddr). The last fallback means the
// caller never has to know the address — gta-mcp reads it from the runtime.
//
// 接入是否完成不以「进程启动 / activate 返回 ok（仅拿到 pid）」为准：启动后必须
// 联合校验 list_registered_plugins（registered）、status_plugin.online（online）、
// get_plugin_manifest（manifest_present）三项，全部满足才视为集成完成。
func (m *mcpCapture) handleActivatePlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	registryAddr := req.GetString("registry_addr", "")
	if registryAddr == "" {
		registryAddr = os.Getenv("GTA_REGISTRY_ADDR")
	}
	if registryAddr == "" && m.pipelineClient != nil {
		// 回退到 pipeline 实际监听的 registry 地址，避免「不知道该连哪里」。
		if resp, err := m.pipelineClient.GetRegistryAddr(ctx, &pb.GetRegistryAddrRequest{}); err == nil && resp.GetRegistryAddr() != "" {
			registryAddr = resp.GetRegistryAddr()
		}
	}
	if registryAddr == "" {
		return errorResult(fmt.Errorf("registry_addr is required (pass it, set GTA_REGISTRY_ADDR, or ensure the pipeline is reachable so gta-mcp can read it from the runtime)")), nil
	}
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	resp, err := m.pdClient.Activate(ctx, name, registryAddr)
	if err != nil {
		return errorResult(err), nil
	}

	out := map[string]any{
		"name":             name,
		"registry_addr":    registryAddr,
		"process_launched": resp.GetOk(),
		"instance_id":      resp.GetInstanceId(),
		"message":          resp.GetMessage(),
	}

	// 联合校验：仅看到进程 pid 或 activate 成功不算接入完成。
	if m.pipelineClient != nil && resp.GetOk() {
		registered, online, manifestPresent, detail := m.verifyPluginIntegration(ctx, name)
		integrated := registered && online && manifestPresent
		out["registered"] = registered
		out["online"] = online
		out["manifest_present"] = manifestPresent
		out["integrated"] = integrated
		out["verification_detail"] = detail
		if !integrated {
			out["ok"] = false
			out["message"] = "plugin process launched but not fully integrated: " + detail
		} else {
			out["ok"] = true
		}
	} else {
		// 无 pipeline 连接时只能确认进程已启动，无法验证注册。
		out["ok"] = resp.GetOk()
		out["integrated"] = false
		out["verification_detail"] = "pipeline unreachable: registered/online/manifest not verified"
	}
	return successResult(out), nil
}

// verifyPluginIntegration 轮询确认插件真正接入运行时：同时检查
// list_registered_plugins（registered）、status_plugin.online（online）、
// get_plugin_manifest（manifest_present）。三者皆满足才视为集成完成。仅进程
// 启动成功 / activate 返回 ok 不足以证明接入完成。
func (m *mcpCapture) verifyPluginIntegration(ctx context.Context, name string) (registered, online, manifestPresent bool, detail string) {
	deadline := time.Now().Add(15 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		registered, online, manifestPresent = false, false, false

		listResp, err := m.pipelineClient.ListPlugins(ctx, &pb.ListPluginsRequest{})
		if err == nil {
			for _, p := range listResp.GetPlugins() {
				if p.GetName() == name {
					registered = true
					online = p.GetOnline()
					break
				}
			}
		}
		manResp, merr := m.pipelineClient.GetPluginManifest(ctx, &pb.GetPluginManifestRequest{Name: name})
		manifestPresent = merr == nil && len(manResp.GetManifest()) > 0

		if registered && online && manifestPresent {
			return true, true, true, "registered + online + manifest present"
		}
		if time.Now().After(deadline) {
			var parts []string
			if !registered {
				parts = append(parts, "not in list_registered_plugins")
			} else if !online {
				parts = append(parts, "registered but not online (no heartbeat yet?)")
			}
			if !manifestPresent {
				parts = append(parts, "get_plugin_manifest returned empty/nil")
			}
			return registered, online, manifestPresent, strings.Join(parts, "; ")
		}
		select {
		case <-ctx.Done():
			return registered, online, manifestPresent, "context cancelled before integration confirmed"
		case <-ticker.C:
		}
	}
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

// handleGetRegistryAddr 返回当前 pipeline 的 registry 地址，插件启动时需将其写入
// GTA_REGISTRY_ADDR。此前只能从 pipeline 启动日志人工获取，现由 pipeline 通过
// GetRegistryAddr RPC 直接暴露，gta-mcp 原样转发。
func (m *mcpCapture) handleGetRegistryAddr(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	resp, err := m.pipelineClient.GetRegistryAddr(ctx, &pb.GetRegistryAddrRequest{})
	if err != nil {
		return errorResult(fmt.Errorf("get registry addr: %w", err)), nil
	}
	addr := resp.GetRegistryAddr()
	out := map[string]any{
		"registry_addr": addr,
	}
	if addr == "" {
		out["message"] = "pipeline returned an empty registry address (registry not configured via -registry-addr)"
	} else {
		out["message"] = "set GTA_REGISTRY_ADDR=" + addr + " when launching plugins"
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
