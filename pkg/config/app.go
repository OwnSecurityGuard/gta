// app.go — 统一配置（T10/T11）。
//
// gta.yaml 是可选的统一配置文件：不存在时所有字段取默认值。
// 每个字段支持 GTA_* 环境变量兜底，整体优先级：
//
//	flag（命令行显式传入） > 环境变量 GTA_* > gta.yaml > 默认值
//
// 鉴权 token 不进配置文件：GTA_AUTH_TOKENS 保持仅环境变量（避免密钥落盘）。
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// 各监听地址的默认值（与各 main.go 既有 flag 默认值一致，保证无配置时行为不变）。
const (
	DefaultMCPAddr         = ":8781"          // gta-mcp HTTP/SSE 监听地址
	DefaultControlAddr     = ":9888"          // CaptureControl gRPC 监听地址
	DefaultRegistryAddr    = ":9091"          // PluginRegistry gRPC 监听地址
	DefaultAgentIngestAddr = ":9092"          // AgentIngest gRPC 监听地址
	DefaultProxyServerAddr = "127.0.0.1:9090" // mobile Source gRPC 监听地址（T11）
)

// MCPServerConfig 是 gta-mcp 相关配置。
type MCPServerConfig struct {
	// Addr 是 HTTP/SSE 监听地址，支持 ":0" 动态分配（实际地址回写 <workdir>/addr.mcp.json）。
	Addr string `yaml:"addr"`
	// AllowedOrigins 是 CORS 允许的 Origin 列表（逗号分隔），等价 GTA_MCP_ALLOWED_ORIGINS。
	AllowedOrigins string `yaml:"allowed_origins"`
}

// PipelineServerConfig 是 gta-pipeline 的 gRPC 监听配置。
// 各地址支持 ":0" 动态分配（实际地址回写 <workdir>/addr.<name>.json）。
type PipelineServerConfig struct {
	ControlAddr     string `yaml:"control_addr"`
	RegistryAddr    string `yaml:"registry_addr"`
	AgentIngestAddr string `yaml:"agent_ingest_addr"`
}

// ProxyOverrides 是代理抓包相关覆盖项（T11）。
type ProxyOverrides struct {
	// ServerAddr 覆盖 mobile Source 的 gRPC 监听地址（proxy.json 未指定 server_addr 时生效）。
	// 对应环境变量 GTA_PROXY_SERVER_ADDR；默认 127.0.0.1:9090。
	ServerAddr string `yaml:"server_addr"`
}

// Config 是 gta.yaml 的顶层结构（统一配置）。
type Config struct {
	// WorkDir 是工作目录。为空时按 ResolveWorkDir 的规则解析（GTA_HOME / 既有数据探测 / ~/.gta）。
	WorkDir  string               `yaml:"workdir"`
	MCP      MCPServerConfig      `yaml:"mcp"`
	Pipeline PipelineServerConfig `yaml:"pipeline"`
	Proxy    ProxyOverrides       `yaml:"proxy"`
}

// Load 读取统一配置文件（gta.yaml，可选）。path 为空或（未显式指定时）文件不存在，
// 返回仅含环境变量兜底值（其余为零值，由调用方按默认值补齐）的 Config。
//
// explicit 表示 path 是否来自用户显式指定（如 -config flag）：显式指定的路径
// 若文件不存在必须硬错误（用户拼错路径不应静默退回默认配置）；只有调用方
// 未指定配置文件（path 为空或 explicit=false）时，缺失才视为"无配置"。
func Load(path string, explicit bool) (Config, error) {
	var cfg Config
	path = strings.TrimSpace(path)
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) && !explicit {
				slog.Debug("config file not found, using defaults", "path", path)
			} else {
				return cfg, fmt.Errorf("read config %s: %w", path, err)
			}
		} else if err := yaml.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	cfg.applyEnvFallback()
	return cfg, nil
}

// applyEnvFallback 用 GTA_* 环境变量覆盖配置文件值（优先级：环境变量 > 配置文件）。
// 环境变量未设置时保留文件值（可能为零值，由调用方补默认值），这样能区分
// "未配置"与"显式配置"，保证 flag 显式值始终最高优先。
func (c *Config) applyEnvFallback() {
	c.WorkDir = firstNonEmpty(os.Getenv("GTA_WORKDIR"), c.WorkDir)
	c.MCP.Addr = firstNonEmpty(os.Getenv("GTA_MCP_ADDR"), c.MCP.Addr)
	c.MCP.AllowedOrigins = firstNonEmpty(os.Getenv("GTA_MCP_ALLOWED_ORIGINS"), c.MCP.AllowedOrigins)
	c.Pipeline.ControlAddr = firstNonEmpty(os.Getenv("GTA_CONTROL_ADDR"), c.Pipeline.ControlAddr)
	c.Pipeline.RegistryAddr = firstNonEmpty(os.Getenv("GTA_REGISTRY_ADDR"), c.Pipeline.RegistryAddr)
	c.Pipeline.AgentIngestAddr = firstNonEmpty(os.Getenv("GTA_AGENT_INGEST_ADDR"), c.Pipeline.AgentIngestAddr)
	c.Proxy.ServerAddr = firstNonEmpty(os.Getenv("GTA_PROXY_SERVER_ADDR"), c.Proxy.ServerAddr)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ResolveWorkDir 解析工作目录（优先级：显式 flag > GTA_HOME > 配置文件 workdir >
// 既有数据探测 > ~/.gta）。
//
// 兼容性说明（T10）：默认工作目录从 CWD 改为 ~/.gta（B6），但为了不破坏老用户的
// 数据发现——若 GTA_HOME 未设置、flag 未显式传入、配置文件也未指定，且 CWD 中已存在
// gta 数据（control.sqlite / sessions / runs），则继续使用 CWD 并打印提示。
func ResolveWorkDir(flagValue string, flagExplicit bool, cfgWorkDir string) (string, error) {
	if flagExplicit && strings.TrimSpace(flagValue) != "" {
		return filepath.Abs(flagValue)
	}
	if home := strings.TrimSpace(os.Getenv("GTA_HOME")); home != "" {
		return filepath.Abs(home)
	}
	if strings.TrimSpace(cfgWorkDir) != "" {
		return filepath.Abs(cfgWorkDir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	if HasExistingGTAData(cwd) {
		slog.Info("found existing gta data in current directory, using it as workdir (set GTA_HOME to override)", "dir", cwd)
		return cwd, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// 无法定位 home 时回退 CWD，保证可用。
		slog.Warn("cannot resolve home directory, falling back to CWD as workdir", "error", err)
		return cwd, nil
	}
	return filepath.Join(home, ".gta"), nil
}

// HasExistingGTAData 判断目录中是否已有 gta 运行数据。
func HasExistingGTAData(dir string) bool {
	for _, name := range []string{"control.sqlite", "sessions", "runs"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// AddrFilePath 返回监听地址回写文件路径：<workdir>/addr.<name>.json。
// 支持 ":0" 动态端口时，同机可跑多套实例，外部通过该文件获知实际地址。
func AddrFilePath(workDir, name string) string {
	return filepath.Join(workDir, "addr."+name+".json")
}

// WriteAddrFile 将监听地址回写为 <workdir>/addr.<name>.json（原子写）。
// 失败仅告警不阻断：地址文件是便利设施，不是正确性依赖。
func WriteAddrFile(workDir, name, addr string) {
	if strings.TrimSpace(workDir) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(addr) == "" {
		return
	}
	path := AddrFilePath(workDir, name)
	b, err := json.MarshalIndent(map[string]string{"name": name, "addr": addr}, "", "  ")
	if err != nil {
		slog.Warn("marshal addr file", "path", path, "error", err)
		return
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		slog.Warn("create workdir for addr file", "path", workDir, "error", err)
		return
	}
	// 用 CreateTemp 而非固定 .tmp 名：同机多实例共享 workdir 时避免互相覆盖临时文件。
	tmp, err := os.CreateTemp(workDir, "addr."+name+".*.tmp")
	if err != nil {
		slog.Warn("create addr file temp", "path", path, "error", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		slog.Warn("write addr file", "path", path, "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("write addr file", "path", path, "error", err)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("commit addr file", "path", path, "error", err)
		return
	}
	slog.Debug("addr file written", "path", path, "addr", addr)
}
