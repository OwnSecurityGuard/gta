//go:build !embedded

// 非固化构建的配置加载桩：直接返回空配置（无内嵌字段可叠加）。
package main

// loadEmbeddedConfig 在普通命令行构建下恒返回空（无固化配置）。
func loadEmbeddedConfig() (*embeddedAgentConfig, bool) {
	return nil, false
}
