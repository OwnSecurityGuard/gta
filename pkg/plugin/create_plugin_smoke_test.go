package plugin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gta/pkg/plugin"
)

// TestCreatePluginSmokeBuild is the Phase 4 engineering acceptance check: a
// project scaffolded by RenderCreatePluginTemplates must compile out of the box.
// It generates the skeleton inside the repository (so the relative replace paths
// resolve), then runs `go build ./...` and asserts success.
func TestCreatePluginSmokeBuild(t *testing.T) {
	root, err := plugin.ModuleRoot()
	if err != nil {
		t.Fatalf("ModuleRoot: %v", err)
	}

	genRoot := filepath.Join(root, ".phase4_smoke")
	outDir := filepath.Join(genRoot, "smoke-plugin")
	t.Cleanup(func() { os.RemoveAll(genRoot) })

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rendered, err := plugin.RenderCreatePluginTemplates(outDir, map[string]any{
		"Name":            "smoke-plugin",
		"Protocol":        "tcp",
		"ProtocolVersion": "v1",
		"Hints":           []string{"tcp"},
	})
	if err != nil {
		t.Fatalf("RenderCreatePluginTemplates: %v", err)
	}

	outputs := map[string]string{
		"go.mod.tmpl":      "go.mod",
		"main.go.tmpl":     "main.go",
		"plugin.yaml.tmpl": "plugin.yaml",
	}
	for tmpl, fname := range outputs {
		if err := os.WriteFile(filepath.Join(outDir, fname), []byte(rendered[tmpl]), 0o644); err != nil {
			t.Fatalf("write %s: %v", fname, err)
		}
	}

	// Sanity check: go.mod must not contain the old hard-coded relative paths.
	if containsHardcodedPath(rendered["go.mod.tmpl"]) {
		t.Fatalf("go.mod.tmpl still contains hard-coded relative replace paths:\n%s", rendered["go.mod.tmpl"])
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go not on PATH: %v", err)
	}

	// The generated go.mod depends only on the published
	// github.com/OwnSecurityGuard/gta-plugin-sdk (no source coupling). For this
	// in-repo smoke build we point it at the local SDK copy (sibling directory
	// gta-plugin-sdk) so it compiles without a module proxy. The template itself
	// stays coupling-free.
	sdkPath := filepath.Join(root, "..", "gta-plugin-sdk")
	editCmd := exec.Command(goBin, "mod", "edit", "-replace", "github.com/OwnSecurityGuard/gta-plugin-sdk="+sdkPath)
	editCmd.Dir = outDir
	if out, err := editCmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit replace for smoke build failed: %v\n%s", err, out)
	}

	cmd := exec.Command(goBin, "build", "-mod=mod", "./...")
	cmd.Dir = outDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build of generated plugin failed: %v\n%s", err, out)
	}
}

func containsHardcodedPath(s string) bool {
	return len(s) > 0 && (contains(s, "../../../../../../pkg/plugin/sdk") ||
		contains(s, "../../../../../../.."))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
