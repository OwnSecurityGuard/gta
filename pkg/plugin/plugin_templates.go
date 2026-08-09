package plugin

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"text/template"
)

// createPluginTemplates embeds the create_plugin skeleton templates.
// They are rendered by RenderCreatePluginTemplates to scaffold a new decoder
// plugin project from plugin.yaml + main.go + go.mod templates.
//
//go:embed templates/create_plugin/*.tmpl
var createPluginTemplates embed.FS

// sourceDir returns the directory of this source file (.../pkg/plugin).
func sourceDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine source file path")
	}
	return filepath.Dir(file), nil
}

// ModuleRoot returns the repository root (the directory containing go.mod for
// module gta). It walks up from this source file. Retained for callers that
// need the repo root (e.g. the create_plugin smoke test).
func ModuleRoot() (string, error) {
	dir, err := sourceDir()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from %s", dir)
		}
		dir = parent
	}
}

// RenderCreatePluginTemplates renders the create_plugin skeleton templates with
// the provided data. Expects a map with keys: Name, Protocol, ProtocolVersion
// (optional), Hints (optional []string). It produces a standalone plugin
// project that depends only on the published github.com/OwnSecurityGuard/gta-plugin-sdk module — no
// source-relative replace directives, so the generated project can be built
// anywhere the SDK module is available (e.g. via a Go module proxy) without any
// access to the gta source tree. Returns template filename -> rendered content.
func RenderCreatePluginTemplates(_ string, data map[string]any) (map[string]string, error) {
	out := make(map[string]string)
	files := []string{"go.mod.tmpl", "main.go.tmpl", "plugin.yaml.tmpl"}
	for _, f := range files {
		raw, err := createPluginTemplates.ReadFile("templates/create_plugin/" + f)
		if err != nil {
			return nil, fmt.Errorf("read template %s: %w", f, err)
		}
		t, err := template.New(f).Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", f, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("render template %s: %w", f, err)
		}
		out[f] = buf.String()
	}
	return out, nil
}
