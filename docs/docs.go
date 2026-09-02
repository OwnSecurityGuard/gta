// Package docs 通过 go:embed 提供 docs/ 下对外发布的文档原文，
// 供 MCP 工具（get_plugin_dev_guide 等）直接返回，保证文档 SSOT 在 docs/。
package docs

import _ "embed"

//go:embed gta-plugin-development.md
var pluginDevGuideMD []byte

// DevGuide 返回 gta-plugin-development.md 的原始内容。
func DevGuide() []byte {
	return pluginDevGuideMD
}
