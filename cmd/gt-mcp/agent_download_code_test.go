package main

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestServePrebuiltAgentByPlatform drives serveAgentBinaryByPlatform (the "code mode"
// download used by setup.sh / PowerShell onboarding) against a temp bin dir.
func TestServePrebuiltAgentByPlatform(t *testing.T) {
	dir := t.TempDir()
	// 构造 linux/amd64 的假预置产物，供 availableAgentPlatforms 识别为 available。
	bin := filepath.Join(dir, "gt-agent-linux-amd64")
	if err := os.WriteFile(bin, []byte("fake-agent-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_AGENT_BIN_DIR", dir)

	m := &mcpCapture{}
	w := httptest.NewRecorder()

	if served := m.serveAgentBinaryByPlatform(w, "linux/amd64"); !served {
		t.Fatal("expected handler to write a response")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("expected application/zip, got %q", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("response is not a valid zip: %v", err)
	}
	var sawBinary, sawConfig bool
	for _, f := range zr.File {
		if f.Name == "gt-agent-linux-amd64" {
			sawBinary = true
		}
		if f.Name == "config.embedded.json" {
			sawConfig = true
		}
	}
	if !sawBinary || !sawConfig {
		t.Fatalf("zip must contain binary + placeholder config, got %v", zr.File)
	}
}
