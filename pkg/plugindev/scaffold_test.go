package plugindev_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gametrace/pkg/plugindev"
)

// TestScaffoldSmokeBuild verifies that Scaffold renders a buildable plugin
// skeleton and that the rendered go.mod depends only on the published SDK
// module (no baked-in local paths) — the core invariant of the P1 plane
// split: scaffolding must not couple to the gametrace source tree.
//
// The build step is best-effort: it is skipped when the local gta-plugin-sdk
// checkout is unavailable (e.g. CI without the sibling repo) so the unit
// suite stays hermetic.
func TestScaffoldSmokeBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold smoke build in -short mode")
	}

	root := t.TempDir()
	name := "smokeplugin"
	resp, err := plugindev.Scaffold(context.Background(), &plugindev.ScaffoldRequest{
		Name:     name,
		Protocol: "sample",
		Root:     root,
	})
	if err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}
	if len(resp.Created) == 0 {
		t.Fatalf("Scaffold created no files")
	}

	dir := filepath.Join(root, name)
	goMod := readFile(t, filepath.Join(dir, "go.mod"))
	if containsHardcodedPath(goMod) {
		t.Fatalf("generated go.mod must not contain a local path:\n%s", goMod)
	}
	mainGo := readFile(t, filepath.Join(dir, "main.go"))
	if !contains(mainGo, "sdk.RunRegisterLoop") {
		t.Fatalf("generated main.go missing RunRegisterLoop entrypoint:\n%s", mainGo)
	}

	// Best-effort build: wire a local replace to the sibling SDK repo, tidy
	// (so the SDK's transitive requirements land in go.mod), then compile.
	// Skipped when the toolchain can't resolve deps (CI without the sibling
	// repo or an offline module cache) so the unit suite stays hermetic.
	sdk := findLocalSDK(t)
	if sdk == "" {
		t.Skip("local gta-plugin-sdk not found (tried canonical + sibling); skipping build")
	}
	edit := exec.Command("go", "mod", "edit",
		"-replace", "github.com/OwnSecurityGuard/gta-plugin-sdk="+sdk)
	edit.Dir = dir
	if out, e := edit.CombinedOutput(); e != nil {
		t.Fatalf("go mod edit -replace: %v\n%s", e, out)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, e := tidy.CombinedOutput(); e != nil {
		t.Skipf("go mod tidy failed (deps unavailable?): %v\n%s", e, out)
	}
	build := exec.Command("go", "build", "-o", name+".exe", ".")
	build.Dir = dir
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("go build failed: %v\n%s", e, out)
	}
}

// repoRoot returns the gametrace repository root (two levels up from this package
// directory, pkg/plugindev).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	return root
}

// findLocalSDK locates the gta-plugin-sdk checkout so the smoke build can
// resolve it without network access. It prefers the canonical
// E:\ai_workspace\gta-plugin-sdk (per project convention) and falls back to a
// sibling E:\gta-plugin-sdk of the gametrace repo root. Returns "" if neither exists.
func findLocalSDK(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	candidates := []string{
		filepath.Join(root, "..", "ai_workspace", "gta-plugin-sdk"),
		filepath.Join(root, "..", "gta-plugin-sdk"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// containsHardcodedPath reports whether s bakes in a filesystem path (e.g. a
// replace directive or an absolute/relative module path), which the published
// scaffold template must never do. A plain `require` line with a module path
// is fine; only a `=>` rewrite or a `replace` pointing at a path trips this.
func containsHardcodedPath(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "=>") {
			return true
		}
		if strings.HasPrefix(trimmed, "replace") && (strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\")) {
			return true
		}
	}
	return false
}
