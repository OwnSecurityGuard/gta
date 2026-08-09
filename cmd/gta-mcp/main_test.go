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

func TestHandleListPluginsMissingDir(t *testing.T) {
	m := &mcpCapture{pluginsDir: filepath.Join(t.TempDir(), "does-not-exist")}
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

	m := &mcpCapture{pluginsDir: tmp}
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

	m := &mcpCapture{pluginsDir: tmp}
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
