package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// T11：ServerAddr 默认值可通过环境变量 GTA_PROXY_SERVER_ADDR 覆盖。
func TestDefaultProxyServerConfigEnvOverride(t *testing.T) {
	cfg := DefaultProxyServerConfig()
	if cfg.ServerAddr != DefaultProxyServerAddr {
		t.Fatalf("default server addr = %q, want %q", cfg.ServerAddr, DefaultProxyServerAddr)
	}
	t.Setenv("GTA_PROXY_SERVER_ADDR", "127.0.0.1:19090")
	cfg = DefaultProxyServerConfig()
	if cfg.ServerAddr != "127.0.0.1:19090" {
		t.Fatalf("env override not applied: %q", cfg.ServerAddr)
	}
}

// T11：proxy.json 显式 server_addr 优先于注入的兜底值；兜底值优先于环境变量默认。
func TestLoadProxyServerConfigWithDefault(t *testing.T) {
	t.Setenv("GTA_PROXY_SERVER_ADDR", "127.0.0.1:19091")

	// proxy.json 不存在：用注入的兜底值。
	dir := t.TempDir()
	cfg, err := LoadProxyServerConfigWithDefault(filepath.Join(dir, "proxy.json"), "127.0.0.1:9100")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerAddr != "127.0.0.1:9100" {
		t.Fatalf("defaultServerAddr should win over env default, got %q", cfg.ServerAddr)
	}

	// proxy.json 存在且带 server_addr：proxy.json 优先。
	path := filepath.Join(dir, "proxy.json")
	b, _ := json.Marshal(map[string]any{"server_addr": "127.0.0.1:9200", "listen_addr": "0.0.0.0:12000"})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadProxyServerConfigWithDefault(path, "127.0.0.1:9100")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerAddr != "127.0.0.1:9200" {
		t.Fatalf("proxy.json should win, got %q", cfg.ServerAddr)
	}

	// proxy.json 存在但 server_addr 为空：兜底值生效。
	b, _ = json.Marshal(map[string]any{"listen_addr": "0.0.0.0:12000"})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadProxyServerConfigWithDefault(path, "127.0.0.1:9100")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerAddr != "127.0.0.1:9100" {
		t.Fatalf("fallback should apply when json omits server_addr, got %q", cfg.ServerAddr)
	}
}
