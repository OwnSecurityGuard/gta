package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRulesWithSchema(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	rulesPath := filepath.Join(dir, "rules.yaml")

	schema := `{
  "fields": {
    "name": {"type": "string"},
    "damage": {"type": "number"}
  }
}`
	rules := `rules:
  - name: hit
    filter: 'data.name == "hit" && data.damage > 0'
    schema: schema.json
    aggregate:
      type: count
      window: 1s
      group_by: []
      output: hits
`
	if err := os.WriteFile(schemaPath, []byte(schema), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath, []byte(rules), 0644); err != nil {
		t.Fatal(err)
	}

	compiled, err := LoadRules(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(compiled))
	}
}

func TestLoadRulesWithBadSchema(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	rulesPath := filepath.Join(dir, "rules.yaml")

	schema := `{"fields":{"name":{"type":"string"}}}`
	rules := `rules:
  - name: bad
    filter: 'data.missing == 1'
    schema: schema.json
    aggregate:
      type: count
      window: 1s
      group_by: []
      output: bad
`
	if err := os.WriteFile(schemaPath, []byte(schema), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath, []byte(rules), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRules(rulesPath); err == nil {
		t.Fatal("expected schema validation error")
	}
}

// TestProxyServerConfigNormalizeFilter 验证代理配置 Normalize 对插件与筛选字段的处理：
// 去空白/去重、端口校验、列表为空时归 nil。
func TestProxyServerConfigNormalizeFilter(t *testing.T) {
	cfg, err := ProxyServerConfig{
		ListenAddr:   "0.0.0.0:12000",
		Plugin:       " http ",
		IncludeHosts: []string{"api.x.com", " api.x.com ", "", "other.com", "api.x.com"},
		IncludePorts: []int{443, 443, 80},
	}.Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Plugin != "http" {
		t.Fatalf("plugin = %q, want http", cfg.Plugin)
	}
	if len(cfg.IncludeHosts) != 2 || cfg.IncludeHosts[0] != "api.x.com" || cfg.IncludeHosts[1] != "other.com" {
		t.Fatalf("include_hosts = %v, want [api.x.com other.com] deduped", cfg.IncludeHosts)
	}
	if len(cfg.IncludePorts) != 2 || cfg.IncludePorts[0] != 443 || cfg.IncludePorts[1] != 80 {
		t.Fatalf("include_ports = %v, want [443 80] (dup dropped)", cfg.IncludePorts)
	}
	if got := cfg.FilterHostList(); got != "api.x.com,other.com" {
		t.Fatalf("FilterHostList = %q", got)
	}
	if got := cfg.FilterPortList(); got != "443,80" {
		t.Fatalf("FilterPortList = %q", got)
	}

	// 非法端口应报错（0 与越界均非法）。
	for _, bad := range []int{0, 99999} {
		if _, err := (ProxyServerConfig{ListenAddr: "0.0.0.0:12000", IncludePorts: []int{bad}}).Normalize(); err == nil {
			t.Fatalf("expected error for include_port %d", bad)
		}
	}

	// 空列表归 nil（不再筛选）。
	cfg2, err := ProxyServerConfig{
		ListenAddr:   "0.0.0.0:12000",
		IncludeHosts: []string{" ", ""},
		IncludePorts: []int{},
	}.Normalize()
	if err != nil {
		t.Fatalf("normalize empty filter: %v", err)
	}
	if cfg2.IncludeHosts != nil || cfg2.IncludePorts != nil {
		t.Fatalf("empty filters should be nil, got hosts=%v ports=%v", cfg2.IncludeHosts, cfg2.IncludePorts)
	}
}
