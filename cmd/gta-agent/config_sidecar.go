// config_sidecar.go — 预置（generic）二进制的运行时 sidecar 配置加载。
//
// 「多平台下载」只按 GOOS/GOARCH 预编译一份通用的 gta-agent（不带 embedded
// 构建标签，见 Makefile build-agents target）。这份通用产物不能把每个下载者的
// 回连地址 / token / 会话 / 端口 BPF 烧进二进制（那属于 build 期变量），因此
// 服务端在下载时把它包装成 zip：{ gta-agent(.exe) + config.embedded.json }。
// 用户解压后双击运行，本文件让该产物在运行时从「可执行文件同目录」读取
// config.embedded.json，行为与 embedded 固化形态完全一致（免命令行参数）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// loadSidecarConfig 从可执行文件所在目录读取 config.embedded.json。
// 返回 (nil, false) 表示不存在或解析失败——此时回退到正常命令行模式。
func loadSidecarConfig() (*embeddedAgentConfig, bool) {
	exe, err := os.Executable()
	if err != nil {
		return nil, false
	}
	sidecarPath := filepath.Join(filepath.Dir(exe), "config.embedded.json")
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil, false
	}
	var cfg embeddedAgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}