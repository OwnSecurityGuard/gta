package skills

import _ "embed"

//go:embed gta-plugin-development.md
var pluginDevGuideMD []byte

// DevGuide 返回 gta-plugin-development.md 的原始内容。
func DevGuide() []byte {
	return pluginDevGuideMD
}
