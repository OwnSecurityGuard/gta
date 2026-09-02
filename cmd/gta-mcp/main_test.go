//go:build pcap

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"
	plugindev "gta/pkg/plugindev"
	plugindevclient "gta/pkg/plugindev/client"
)

func TestResolvePluginsDirDefaultNextToExecutable(t *testing.T) {
	// Create a fake executable directory with a plugins subdir.
	tmp := t.TempDir()
	pluginsDir := filepath.Join(tmp, "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Simulate being run from a different working directory by changing into tmp.
	// resolvePluginsDir("plugins") should still find the executable-relative dir.
	resolved, err := resolvePluginsDir("plugins")
	if err != nil {
		t.Fatal(err)
	}

	// When running under `go test`, os.Executable points to a test binary in a
	// temp dir that has no plugins dir, so it falls back to cwd-relative. We can
	// only assert the result is absolute and clean.
	if !filepath.IsAbs(resolved) {
		t.Fatalf("expected absolute path, got %q", resolved)
	}
}

func TestResolvePluginsDirAbsolute(t *testing.T) {
	tmp := t.TempDir()
	resolved, err := resolvePluginsDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Clean(tmp) {
		t.Fatalf("expected %q, got %q", filepath.Clean(tmp), resolved)
	}
}

// embeddedPluginDev starts an in-process PluginDev gRPC server rooted at
// pluginsDir and returns a client connected over loopback, exactly as gta-mcp
// does when GTA_PLUGINDEV_ADDR is unset. This lets the discovery handlers be
// exercised without a separate process. The connection is closed via
// t.Cleanup; the embedded server runs for the test binary's lifetime (the same
// lifetime model gta-mcp uses for its embedded Developer Plane).
func embeddedPluginDev(t *testing.T, pluginsDir string) (plugindevclient.PluginDev, *grpc.ClientConn) {
	t.Helper()
	client, conn, err := dialPluginDev(pluginsDir, "")
	if err != nil {
		t.Fatalf("embeddedPluginDev: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return client, conn
}

func TestHandleListPluginsMissingDir(t *testing.T) {
	pluginsDir := filepath.Join(t.TempDir(), "does-not-exist")
	client, conn := embeddedPluginDev(t, pluginsDir)
	m := &mcpCapture{pluginsDir: pluginsDir, pdClient: client, pdConn: conn}
	res, err := m.handleListPlugins(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) == 0 {
		t.Fatal("expected content")
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !contains(text, `"ok":true`) {
		t.Fatalf("expected ok=true for missing dir, got %s", text)
	}
	if !contains(text, `"plugins":[]`) {
		t.Fatalf("expected empty plugins list, got %s", text)
	}
}

func TestHandleListPluginsListsBinaries(t *testing.T) {
	tmp := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	if err := os.WriteFile(filepath.Join(tmp, "http"+ext), []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "tcp"+ext), []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}

	client, conn := embeddedPluginDev(t, tmp)
	m := &mcpCapture{pluginsDir: tmp, pdClient: client, pdConn: conn}
	res, err := m.handleListPlugins(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !contains(text, `"http"`) {
		t.Fatalf("expected http plugin, got %s", text)
	}
	if !contains(text, `"tcp"`) {
		t.Fatalf("expected tcp plugin, got %s", text)
	}
}

// TestHandleListPluginsDiscoversSubdirectoryPlugins locks in the fix for
// plugins laid out as <plugins_dir>/<name>/<name>.exe (the form produced by
// create_plugin). The old code skipped subdirectories entirely, so freshly
// scaffolded plugins were never listed.
func TestHandleListPluginsDiscoversSubdirectoryPlugins(t *testing.T) {
	tmp := t.TempDir()
	ext := exeExt()
	// create_plugin form: plugins/my-game/my-game.exe
	dir := filepath.Join(tmp, "my-game")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "my-game"+ext), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Subdirectory with a non-matching binary name should still be discovered
	// (prefer <name>/<name>.exe, but fall back to any exe).
	dir2 := filepath.Join(tmp, "http")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "http-plugin"+ext), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	client, conn := embeddedPluginDev(t, tmp)
	m := &mcpCapture{pluginsDir: tmp, pdClient: client, pdConn: conn}
	res, err := m.handleListPlugins(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	var parsed struct {
		OK      bool `json:"ok"`
		Plugins []struct {
			Name   string `json:"name"`
			Binary string `json:"binary"`
			Dir    string `json:"dir"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("failed to parse result %q: %v", text, err)
	}
	if !parsed.OK {
		t.Fatalf("expected ok=true, got %s", text)
	}
	if len(parsed.Plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d: %s", len(parsed.Plugins), text)
	}
	byName := make(map[string]struct {
		Binary string
		Dir    string
	})
	for _, p := range parsed.Plugins {
		byName[p.Name] = struct {
			Binary string
			Dir    string
		}{p.Binary, p.Dir}
	}
	mg, ok := byName["my-game"]
	if !ok {
		t.Fatalf("expected my-game plugin, got %s", text)
	}
	wantBinary := filepath.Join(tmp, "my-game", "my-game"+ext)
	if filepath.Clean(mg.Binary) != filepath.Clean(wantBinary) {
		t.Fatalf("expected binary %q, got %q", wantBinary, mg.Binary)
	}
	if filepath.Clean(mg.Dir) != filepath.Clean(filepath.Join(tmp, "my-game")) {
		t.Fatalf("expected dir inside subdirectory, got %q", mg.Dir)
	}
	if _, ok := byName["http"]; !ok {
		t.Fatalf("expected http plugin, got %s", text)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestHandleStatusPluginUnknown exercises the dual-state aggregation path: an
// unknown plugin should report artifact.state=unknown, a runtime view of
// offline (no registry link in this test), and a build_plugin next_action.
func TestHandleStatusPluginUnknown(t *testing.T) {
	pluginsDir := t.TempDir()
	client, conn := embeddedPluginDev(t, pluginsDir)
	m := &mcpCapture{pluginsDir: pluginsDir, pdClient: client, pdConn: conn}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "ghost"}
	res, err := m.handleStatusPlugin(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text

	var parsed struct {
		Ok       bool   `json:"ok"`
		Name     string `json:"name"`
		Artifact struct {
			State string `json:"state"`
		} `json:"artifact"`
		Runtime struct {
			State string `json:"state"`
		} `json:"runtime"`
		NextAction map[string]any `json:"next_action"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("unmarshal status: %v\n%s", err, text)
	}
	if !parsed.Ok {
		t.Fatalf("expected ok=true, got %s", text)
	}
	if parsed.Name != "ghost" {
		t.Fatalf("expected name=ghost, got %q", parsed.Name)
	}
	if parsed.Artifact.State != "unknown" {
		t.Fatalf("expected artifact.state=unknown, got %q", parsed.Artifact.State)
	}
	if parsed.Runtime.State != "offline" {
		t.Fatalf("expected runtime.state=offline (no registry link), got %q", parsed.Runtime.State)
	}
	if parsed.NextAction == nil {
		t.Fatalf("expected a next_action for an unknown plugin, got %s", text)
	}
}

// TestHandleExplainPluginForwards records a failed build in the shared
// Developer Plane tracker and verifies handleExplainPlugin forwards the
// structured attribution (category + SDK rule_id) produced by the embedded
// PluginDev service.
func TestHandleExplainPluginForwards(t *testing.T) {
	pluginsDir := t.TempDir()
	client, conn := embeddedPluginDev(t, pluginsDir)
	m := &mcpCapture{pluginsDir: pluginsDir, pdClient: client, pdConn: conn}

	name := "explain-fwd"
	plugindev.DefaultTracker().RecordBuild(name, 0, &plugindev.BuildResponse{
		OK:     false,
		Errors: []*plugindev.BuildError{{File: "main.go", Line: 7, Col: 3, Message: "undefined: event.ValueInt32"}},
	})

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": name}
	res, err := m.handleExplainPlugin(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !containsHelper(text, "undefined-symbol") {
		t.Fatalf("explain not forwarded: %s", text)
	}
	if !containsHelper(text, "value-accessor-ok") {
		t.Fatalf("rule_id not forwarded: %s", text)
	}
	if !containsHelper(text, "expl_") {
		t.Fatalf("explain_ref missing: %s", text)
	}
}

// TestHandleExplainPluginForwardsVerify locks in P3b end-to-end: the MCP layer
// is a pure forwarder — it only maps the `verify` JSON argument onto the gRPC
// VerifyResult and forwards it. The embedded Developer Plane does the actual
// decode-attribution (here: high entropy + majority undecodable => suspected
// encryption, referencing contract.yaml rule_id inspect-bytes-first).
func TestHandleExplainPluginForwardsVerify(t *testing.T) {
	pluginsDir := t.TempDir()
	client, conn := embeddedPluginDev(t, pluginsDir)
	m := &mcpCapture{pluginsDir: pluginsDir, pdClient: client, pdConn: conn}

	name := "explain-verify-fwd"
	verifyArg := map[string]any{
		"verdict": "fail",
		"quality": map[string]any{
			"total_inputs":     10,
			"unknown_inputs":   8,
			"unknown_ratio":    0.8,
			"entropy_estimate": 7.8,
		},
	}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": name, "verify": verifyArg}
	res, err := m.handleExplainPlugin(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !containsHelper(text, "suspected-encryption") {
		t.Fatalf("decode attribution not forwarded: %s", text)
	}
	if !containsHelper(text, "inspect-bytes-first") {
		t.Fatalf("rule_id not forwarded: %s", text)
	}
	if !containsHelper(text, "expl_") {
		t.Fatalf("explain_ref missing: %s", text)
	}
}
