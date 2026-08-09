package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/plugin"
)

var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// handleCreatePlugin scaffolds a new decoder plugin project from the embedded
// templates. It writes plugin.yaml + main.go + go.mod into output_dir (default
// <plugins_dir>/<name>). The generated go.mod already contains correct relative
// replace path to github.com/OwnSecurityGuard/gta-plugin-sdk (sibling
// gta-plugin-sdk dir) so it compiles as-is.
func (m *mcpCapture) handleCreatePlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	if !pluginNameRe.MatchString(name) {
		return errorResult(fmt.Errorf("name must be kebab-case (lowercase letters, digits, hyphens; must start with a letter), got %q", name)), nil
	}
	protocol := req.GetString("protocol", "")
	if protocol == "" {
		return errorResult(fmt.Errorf("protocol is required")), nil
	}
	protocolVersion := req.GetString("protocol_version", "")

	var hints []string
	if raw := req.GetString("hints", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &hints); err != nil {
			// fall back to comma-separated
			for _, h := range strings.Split(raw, ",") {
				if h = strings.TrimSpace(h); h != "" {
					hints = append(hints, h)
				}
			}
		}
	}

	outputDir := req.GetString("output_dir", "")
	if outputDir == "" {
		outputDir = filepath.Join(m.pluginsDir, name)
	}
	if abs, err := filepath.Abs(outputDir); err == nil {
		outputDir = abs
	}

	rendered, err := plugin.RenderCreatePluginTemplates(outputDir, map[string]any{
		"Name":            name,
		"Protocol":        protocol,
		"ProtocolVersion": protocolVersion,
		"Hints":           hints,
	})
	if err != nil {
		return errorResult(err), nil
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return errorResult(fmt.Errorf("create output dir: %w", err)), nil
	}

	outputs := map[string]string{
		"go.mod.tmpl":     "go.mod",
		"main.go.tmpl":    "main.go",
		"plugin.yaml.tmpl": "plugin.yaml",
	}
	created := make([]string, 0, len(outputs))
	for tmpl, fname := range outputs {
		path := filepath.Join(outputDir, fname)
		if err := os.WriteFile(path, []byte(rendered[tmpl]), 0o644); err != nil {
			return errorResult(fmt.Errorf("write %s: %w", path, err)), nil
		}
		created = append(created, path)
	}

	slog.Info("create_plugin completed", "name", name, "output_dir", outputDir, "files", created)
	return successResult(map[string]any{
		"name":       name,
		"output_dir": outputDir,
		"created":    created,
	}), nil
}
