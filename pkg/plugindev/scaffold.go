package plugindev

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed templates/create_plugin/*.tmpl
var createPluginTemplates embed.FS

// RenderCreatePluginTemplates renders the create_plugin skeleton templates with
// the provided data. Expects a map with keys: Name, Protocol, ProtocolVersion
// (optional), Hints (optional []string). The generated project depends only on
// the published github.com/OwnSecurityGuard/gta-plugin-sdk module — no
// source-relative replace directives — so it builds anywhere the SDK module is
// reachable. Returns template filename -> rendered content.
func RenderCreatePluginTemplates(data map[string]any) (map[string]string, error) {
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

// Scaffold renders the create_plugin skeleton and writes it to Root/Name.
// It returns the absolute output dir and the list of created file paths.
func Scaffold(_ context.Context, req *ScaffoldRequest) (*ScaffoldResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.Protocol == "" {
		return nil, fmt.Errorf("protocol is required")
	}
	if req.Root == "" {
		return nil, fmt.Errorf("root (plugins dir) is required")
	}

	outputDir := filepath.Join(req.Root, req.Name)
	if abs, err := filepath.Abs(outputDir); err == nil {
		outputDir = abs
	}

	rendered, err := RenderCreatePluginTemplates(map[string]any{
		"Name":            req.Name,
		"Protocol":        req.Protocol,
		"ProtocolVersion": req.ProtocolVersion,
		"Hints":           req.Hints,
	})
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	outputs := map[string]string{
		"go.mod.tmpl":      "go.mod",
		"main.go.tmpl":     "main.go",
		"plugin.yaml.tmpl": "plugin.yaml",
	}
	created := make([]string, 0, len(outputs))
	for tmpl, fname := range outputs {
		path := filepath.Join(outputDir, fname)
		if err := os.WriteFile(path, []byte(rendered[tmpl]), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		created = append(created, path)
	}
	return &ScaffoldResponse{
		Name:      req.Name,
		OutputDir: outputDir,
		Created:   created,
	}, nil
}
