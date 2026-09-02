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

// Scaffold renders the create_plugin skeleton and writes it to the resolved
// output directory. Resolution order for the output dir (strictly honors the
// caller's output_dir):
//   - req.OutputDir 非空 → 直接使用该目录（MCP create_plugin 的 output_dir）；
//   - 否则回退到 req.Root/req.Name（服务端配置的 plugins 目录）。
//
// 返回的 OutputDir 是文件实际写入的绝对路径；SDKVersion / FramingAvailable 透传
// 自请求，便于调用方（MCP）如实返回「实际 SDK 版本」与「framing 是否可用」。
func Scaffold(_ context.Context, req *ScaffoldRequest) (*ScaffoldResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.Protocol == "" {
		return nil, fmt.Errorf("protocol is required")
	}

	// 解析输出目录：优先使用显式 output_dir，否则回退 Root/Name。
	var outputDir string
	if req.OutputDir != "" {
		outputDir = req.OutputDir
	} else {
		if req.Root == "" {
			return nil, fmt.Errorf("root (plugins dir) is required when output_dir is not set")
		}
		outputDir = filepath.Join(req.Root, req.Name)
	}
	if abs, err := filepath.Abs(outputDir); err == nil {
		outputDir = abs
	}

	// 版本/framing 默认值：调用方未显式声明时回退到本包常量，保证单一事实来源。
	sdkVersion := req.SDKVersion
	if sdkVersion == "" {
		sdkVersion = SDKVersion
	}
	framingAvailable := req.FramingAvailable
	if !framingAvailable {
		// 未显式声明（如直接调用/测试）时回退到本包常量；当 SDK 确实不含
		// framing 时，调用方必须显式置 false 以生成「framing 不可用」分支。
		framingAvailable = FramingAvailable
	}

	rendered, err := RenderCreatePluginTemplates(map[string]any{
		"Name":             req.Name,
		"Protocol":         req.Protocol,
		"ProtocolVersion":  req.ProtocolVersion,
		"Hints":            req.Hints,
		"SDKVersion":       sdkVersion,
		"FramingAvailable": framingAvailable,
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
		Name:             req.Name,
		OutputDir:        outputDir,
		Created:          created,
		SDKVersion:       sdkVersion,
		FramingAvailable: framingAvailable,
	}, nil
}
