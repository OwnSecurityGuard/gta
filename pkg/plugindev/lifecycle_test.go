package plugindev_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/plugindev"
)

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestArtifactStateTransitions(t *testing.T) {
	root := t.TempDir()
	name := "demo"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	recent := time.Now()

	// Dir exists, no binary yet → scaffolded.
	if got := plugindev.ArtifactStateOf(root, name); got.State != "scaffolded" {
		t.Fatalf("expected scaffolded, got %q", got.State)
	}

	// Binary newer than source → compiled, not stale.
	writeFile(t, filepath.Join(dir, "main.go"), "package main", past)
	writeFile(t, filepath.Join(dir, name+".exe"), "ELF", recent)
	got := plugindev.ArtifactStateOf(root, name)
	if got.State != "compiled" {
		t.Fatalf("expected compiled, got %q", got.State)
	}
	if got.BinaryStale {
		t.Fatalf("expected not stale when binary is newer than source")
	}

	// Source newer than binary → compiled but stale.
	writeFile(t, filepath.Join(dir, "main.go"), "package main // changed", recent.Add(2*time.Second))
	got = plugindev.ArtifactStateOf(root, name)
	if got.State != "compiled" {
		t.Fatalf("expected compiled, got %q", got.State)
	}
	if !got.BinaryStale {
		t.Fatalf("expected stale when source is newer than binary")
	}
}

func TestArtifactStateUnknownForMissingDir(t *testing.T) {
	root := t.TempDir()
	got := plugindev.ArtifactStateOf(root, "ghost")
	if got.State != "unknown" {
		t.Fatalf("expected unknown for missing dir, got %q", got.State)
	}
}

func TestActivateMissingBinaryRecordsAttempt(t *testing.T) {
	root := t.TempDir()
	_, err := plugindev.Activate(context.Background(), &plugindev.ActivateRequest{
		Root:         root,
		Name:         "ghost",
		RegistryAddr: ":9091",
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}

	ps, err := plugindev.Status(context.Background(), &plugindev.StatusRequest{Root: root, Name: "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if ps.LastAttempt == nil {
		t.Fatal("expected a last_attempt to be recorded")
	}
	if ps.LastAttempt.Action != "activate" {
		t.Fatalf("expected action=activate, got %q", ps.LastAttempt.Action)
	}
	if ps.LastAttempt.OK {
		t.Fatalf("expected ok=false for failed activate")
	}
}

func TestDeactivateNoProcessIsSafe(t *testing.T) {
	root := t.TempDir()
	resp, err := plugindev.Deactivate(context.Background(), &plugindev.DeactivateRequest{Root: root, Name: "ghost"})
	if err != nil {
		t.Fatalf("deactivate with no process should not error: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected ok=false when no dev-launched process exists")
	}
}
