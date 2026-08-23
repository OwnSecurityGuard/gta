package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProxyServerConfig 是移动代理抓包服务器的持久化配置。
//
// 架构关系：
//
//	手机代理软件 ── HTTP CONNECT ──▶ gta-singbox-agent ── gRPC ──▶ mobile Source（代理抓包会话）
//	         （ListenAddr）                    （ServerAddr）
//
// 它描述"服务端"应如何监听与分帧、绑定解码插件、以及按目标主机/端口筛选哪些连接
// 需要抓包。配置落盘为 <workdir>/proxy.json，由 gta-pipeline 在启动/热更新时读取并应用。
type ProxyServerConfig struct {
	// ListenAddr 是 gta-singbox-agent 的 HTTP CONNECT 代理监听地址，手机代理软件连这里。
	// 手机通过局域网访问时必须绑定局域网接口，如 "0.0.0.0:12000"。
	ListenAddr string `json:"listen_addr"`
	// ServerAddr 是 mobile Source 的 gRPC 监听地址，agent 推送连接级数据到这里。
	// 必须与代理抓包会话的 mobile source listen_addr 一致。
	ServerAddr string `json:"server_addr"`
	// FrameStyle 代理抓包会话的分帧方式：raw（默认）| length_prefix。
	FrameStyle string `json:"frame_style"`
	// PrefixLen length_prefix 分帧的长度前缀字节数（1|2|4），默认 4。
	PrefixLen int `json:"prefix_len"`
	// LittleEndian 长度前缀字节序，默认大端。
	LittleEndian bool `json:"little_endian"`
	// Plugin 代理抓包会话绑定的解码插件名（空表示仅抓原始包不解码）。
	Plugin string `json:"plugin"`
	// IncludeHosts 连接筛选：仅抓取目标主机（CONNECT 中的 host）在此列表内的连接。
	// 支持 IP 或域名，不区分大小写；为空表示不按主机筛选。
	IncludeHosts []string `json:"include_hosts"`
	// IncludePorts 连接筛选：仅抓取目标端口在此列表内的连接。
	// 为空表示不按端口筛选。
	IncludePorts []int `json:"include_ports"`
}

// DefaultProxyServerConfig 返回与 gta-pipeline 启动参数一致的默认值。
func DefaultProxyServerConfig() ProxyServerConfig {
	return ProxyServerConfig{
		ListenAddr: "0.0.0.0:12000",
		ServerAddr: "127.0.0.1:9090",
		FrameStyle: "raw",
		PrefixLen:  4,
	}
}

// ListenHost 返回 ListenAddr 的主机部分（如 "0.0.0.0"）。
func (c ProxyServerConfig) ListenHost() string {
	h, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return strings.TrimSpace(c.ListenAddr)
	}
	return h
}

// ListenPort 返回 ListenAddr 的端口号（解析失败返回 0）。
func (c ProxyServerConfig) ListenPort() int {
	_, p, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// FilterHostList 返回逗号连接的筛选主机列表（供 agent --filter-hosts 参数）。
func (c ProxyServerConfig) FilterHostList() string {
	return strings.Join(c.IncludeHosts, ",")
}

// FilterPortList 返回逗号连接的筛选端口列表（供 agent --filter-ports 参数）。
func (c ProxyServerConfig) FilterPortList() string {
	parts := make([]string, 0, len(c.IncludePorts))
	for _, p := range c.IncludePorts {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
}

// Normalize 校验并补齐默认值；返回规范化后的副本。
func (c ProxyServerConfig) Normalize() (ProxyServerConfig, error) {
	if strings.TrimSpace(c.ListenAddr) == "" {
		c.ListenAddr = DefaultProxyServerConfig().ListenAddr
	}
	if strings.TrimSpace(c.ServerAddr) == "" {
		c.ServerAddr = DefaultProxyServerConfig().ServerAddr
	}
	switch c.FrameStyle {
	case "", "raw", "length_prefix":
		if c.FrameStyle == "" {
			c.FrameStyle = "raw"
		}
	default:
		return c, fmt.Errorf("unsupported frame_style %q (allowed: raw|length_prefix)", c.FrameStyle)
	}
	if c.FrameStyle == "length_prefix" {
		switch c.PrefixLen {
		case 0:
			c.PrefixLen = 4
		case 1, 2, 4:
		default:
			return c, fmt.Errorf("prefix_len must be 1|2|4, got %d", c.PrefixLen)
		}
	}
	if c.ListenPort() <= 0 {
		return c, errors.New("listen_addr must be host:port")
	}
	// 插件名去空白。
	c.Plugin = strings.TrimSpace(c.Plugin)
	// 筛选主机：去空白、去重。
	c.IncludeHosts = normalizeStringList(c.IncludeHosts)
	// 筛选端口：校验 1-65535、去重。
	seenPorts := make(map[int]struct{}, len(c.IncludePorts))
	ports := make([]int, 0, len(c.IncludePorts))
	for _, p := range c.IncludePorts {
		if p < 1 || p > 65535 {
			return c, fmt.Errorf("include_port must be 1-65535, got %d", p)
		}
		if _, ok := seenPorts[p]; ok {
			continue
		}
		seenPorts[p] = struct{}{}
		ports = append(ports, p)
	}
	if len(ports) == 0 {
		c.IncludePorts = nil
	} else {
		c.IncludePorts = ports
	}
	return c, nil
}

// normalizeStringList 去除空白项并去重（保持原顺序）。
func normalizeStringList(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ProxyConfigPath 返回 <workdir>/proxy.json 的标准路径。
func ProxyConfigPath(workDir string) string {
	return filepath.Join(workDir, "proxy.json")
}

// LoadProxyServerConfig 读取 proxy.json；文件不存在或内容非法时回退默认值。
// 路径为空时同样回退默认值（便于未配置的启动场景）。
func LoadProxyServerConfig(path string) (ProxyServerConfig, error) {
	cfg := DefaultProxyServerConfig()
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Debug("proxy config not found, using defaults", "path", path)
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse proxy config %s: %w", path, err)
	}
	return cfg.Normalize()
}

// SaveProxyServerConfig 将配置原子写入 proxy.json（临时文件 + rename）。
func SaveProxyServerConfig(path string, cfg ProxyServerConfig) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("proxy config path is empty")
	}
	norm, err := cfg.Normalize()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(norm, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal proxy config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create proxy config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write proxy config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit proxy config: %w", err)
	}
	return nil
}
