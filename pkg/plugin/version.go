package plugin

import (
	"fmt"
	"strings"
)

// ProtocolVersion 是当前主程序实现的插件协议版本。
// 插件通过 manifest 的 api_version 字段声明兼容版本（gta.decoder/v2）。
//
// 版本协商规则（基于 CheckManifestVersion）：
//   - manifest 声明的 api_version major 必须与 ProtocolVersion 一致
//   - minor 递增代表向后兼容增量，major 递增代表破坏性变更
//
// 版本号约定：major.minor，如 v2、v2.1。
//
// 历史：v2 起 Decoder 仅保留 DecodeV2 流式子接口（v1 解码回调与 Decode v1 RPC 已移除，
// 见 commit "sdk!: remove v1 decode handler, keep only DecodeV2" / "proto!: remove Decode v1 RPC"）。
const ProtocolVersion = "v2"

// majorVersion 提取版本号的 major 部分（"v1.2" → "v1"）。
// 无分隔符时原样返回（"v1" → "v1"）。
func majorVersion(v string) string {
	if v == "" {
		return ""
	}
	// 支持 "v1"、"v1.2"、"v1.2.3" 格式
	for i := 0; i < len(v); i++ {
		if v[i] == '.' {
			return v[:i]
		}
	}
	return v
}

// apiVersionPrefix 是 manifest api_version 字段的前缀。
// 完整格式：gta.decoder/v<数字>，如 gta.decoder/v2。
const apiVersionPrefix = "gta.decoder/"

// ParseManifestAPIVersion 从 manifest 的 api_version 字段提取版本号部分。
// "gta.decoder/v2" → "v2"
// "gta.decoder/v2.3" → "v2.3"
// 非法格式（无前缀或无版本号）返回空串。
func ParseManifestAPIVersion(apiVersion string) string {
	if !strings.HasPrefix(apiVersion, apiVersionPrefix) {
		return ""
	}
	v := strings.TrimPrefix(apiVersion, apiVersionPrefix)
	if v == "" {
		return ""
	}
	return v
}

// CheckManifestVersion 校验 manifest 声明的 api_version 是否与主程序兼容。
// 规则：major 版本必须匹配 ProtocolVersion；api_version 格式必须是 gta.decoder/v<数字>。
// 返回 nil 表示兼容，返回 error 描述不兼容原因。
func CheckManifestVersion(m *Manifest) error {
	if m == nil || m.APIVersion == "" {
		return fmt.Errorf("manifest api_version is empty")
	}
	declared := ParseManifestAPIVersion(m.APIVersion)
	if declared == "" {
		return fmt.Errorf("manifest api_version %q must match gta.decoder/v<digit>", m.APIVersion)
	}
	if majorVersion(declared) != majorVersion(ProtocolVersion) {
		return fmt.Errorf("plugin protocol version mismatch: manifest declared %s, manager requires %s (major must match)",
			declared, ProtocolVersion)
	}
	return nil
}
