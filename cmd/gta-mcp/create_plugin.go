package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// handleCreatePlugin validates the request (local parameter checks only) and
// forwards scaffolding to the Developer Plane. All filesystem writes happen in
// pkg/plugindev, reachable via the PluginDev gRPC service — gta-mcp never calls
// os.WriteFile itself.
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

	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	resp, err := m.pdClient.Scaffold(ctx, name, protocol, protocolVersion, hints)
	if err != nil {
		return errorResult(err), nil
	}

	slog.Info("create_plugin completed", "name", resp.Name, "output_dir", resp.OutputDir, "files", resp.Created)
	return successResult(map[string]any{
		"name":       resp.Name,
		"output_dir": resp.OutputDir,
		"created":    resp.Created,
	}), nil
}
