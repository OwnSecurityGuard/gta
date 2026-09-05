package main

// agentConfig 是探针的持久化配置（probe.json）+ 首启引导（命令行 flag / 启动码）。
//
// 优先级（v2 探针优化，docs/plans/2026-09-05 §4.1）：
//   1. 命令行 flag 非空 → 覆盖并写回 probe.json（首启引导一次性生效）；
//   2. probe.json 已有值 → 直接用（此后一切改参走本地控制面 / 远端指令）；
//   3. embedded / sidecar / 启动码 → 首启引导的默认值来源。
//
// probe.json 只存"身份与回连"，抓包参数（iface/ports/bpf）是会话级配置，
// 由平台指派或本地控制面临时给定，不落 probe.json。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// archiveConfig 是本地留存的保留策略（可在运行中经控制面 / 远端指令调整）。
type archiveConfig struct {
	Enabled   bool  `json:"enabled"`
	MaxAgeHrs int   `json:"max_age_hours"` // 0 = 默认 24
	MaxBytes  int64 `json:"max_bytes"`     // 0 = 默认 4GB
}

// agentConfig 是 probe.json 的内存形态。
type agentConfig struct {
	ProbeID    string `json:"probe_id,omitempty"`
	ProbeToken string `json:"probe_token,omitempty"` // 长期凭证；明文落盘（0600），丢失可重接
	UserToken  string `json:"user_token,omitempty"`  // 用户 token（注册/重接用）
	Server     string `json:"server,omitempty"`      // host[:registryPort]
	IngestAddr string `json:"ingest_addr,omitempty"` // 显式覆盖（默认由 Server 推导）
	RegistryAddr string `json:"registry_addr,omitempty"`
	Name       string `json:"name,omitempty"` // 机器业务名；空 = 注册时默认 hostname

	Archive archiveConfig `json:"archive"`
}

// configDir 返回配置目录（UserConfigDir/gta-agent；失败退回可执行文件同目录）。
func configDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		exe, e := os.Executable()
		if e == nil {
			return filepath.Dir(exe)
		}
		return "."
	}
	return filepath.Join(base, "gta-agent")
}

func configPath() string { return filepath.Join(configDir(), "probe.json") }

// loadAgentConfig 读 probe.json；不存在返回零值配置与 false。
func loadAgentConfig() (*agentConfig, bool) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return &agentConfig{}, false
	}
	cfg := &agentConfig{}
	if err := json.Unmarshal(b, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probe.json 解析失败（忽略该文件）: %v\n", err)
		return &agentConfig{}, false
	}
	return cfg, true
}

// saveAgentConfig 原子写 probe.json（0600：里面存着探针长期凭证）。
func saveAgentConfig(cfg *agentConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}

// mergeFlag 首启引导合并：flag 非空时覆盖并标记 dirty（由调用方写回）。
func mergeFlag(dst *string, flag string, dirty *bool) {
	if flag != "" && flag != *dst {
		*dst = flag
		*dirty = true
	}
}
