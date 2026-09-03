package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkDir != "" || cfg.MCP.Addr != "" || cfg.Pipeline.ControlAddr != "" ||
		cfg.Pipeline.RegistryAddr != "" || cfg.Pipeline.AgentIngestAddr != "" {
		t.Fatalf("expected zero config for missing file, got %+v", cfg)
	}
}

// 显式指定（如 -config）的配置文件不存在必须硬错误，不能静默退回默认配置。
func TestLoadExplicitMissingPathErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), true); err == nil {
		t.Fatal("expected error for explicitly-requested missing config")
	}
}

func TestLoadYAMLAndEnvFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gta.yaml")
	yaml := `
workdir: /data/gta
mcp:
  addr: ":8782"
pipeline:
  control_addr: ":9889"
  registry_addr: ":9093"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkDir != "/data/gta" || cfg.MCP.Addr != ":8782" ||
		cfg.Pipeline.ControlAddr != ":9889" || cfg.Pipeline.RegistryAddr != ":9093" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	// 未在文件中配置的字段由环境变量兜底。
	t.Setenv("GTA_AGENT_INGEST_ADDR", ":9094")
	t.Setenv("GTA_MCP_ALLOWED_ORIGINS", "http://a.example.com")
	cfg, err = Load(path, true)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Pipeline.AgentIngestAddr != ":9094" || cfg.MCP.AllowedOrigins != "http://a.example.com" {
		t.Fatalf("env fallback not applied: %+v", cfg)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gta.yaml")
	if err := os.WriteFile(path, []byte("pipeline:\n  registry_addr: \":9093\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GTA_REGISTRY_ADDR", ":9199")
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.RegistryAddr != ":9199" {
		t.Fatalf("env should win over file, got %q", cfg.Pipeline.RegistryAddr)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gta.yaml")
	if err := os.WriteFile(path, []byte("mcp: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, true); err == nil {
		t.Fatal("expected parse error")
	}
}

// chdir 切换工作目录并在测试结束时恢复（否则会影响其他依赖相对路径的用例）。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestResolveWorkDirPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GTA_HOME", home)
	cwdData := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwdData, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, cwdData)

	// 1. 显式 flag 最高。
	got, err := ResolveWorkDir("/explicit", true, "/fromcfg")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "explicit" {
		t.Fatalf("flag should win, got %q", got)
	}
	// 2. GTA_HOME 次之（覆盖配置文件与 CWD 数据探测）。
	got, err = ResolveWorkDir(".", false, "/fromcfg")
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Fatalf("GTA_HOME should win, got %q want %q", got, home)
	}
	// 3. GTA_HOME 未设置（置空等价）时配置文件次之。
	t.Setenv("GTA_HOME", "")
	got, err = ResolveWorkDir(".", false, "/fromcfg")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "fromcfg" {
		t.Fatalf("cfg workdir should win, got %q", got)
	}
	// 4. CWD 有既有数据时沿用 CWD（老用户兼容）。
	got, err = ResolveWorkDir(".", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != cwdData {
		t.Fatalf("CWD with existing data should be kept, got %q want %q", got, cwdData)
	}
}

func TestResolveWorkDirFallsBackToHomeDotGta(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("HOME", tmpHome)
	t.Setenv("GTA_HOME", "")
	empty := t.TempDir()
	chdir(t, empty)
	got, err := ResolveWorkDir(".", false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmpHome, ".gta")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestWriteAddrFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	WriteAddrFile(dir, "registry", "127.0.0.1:12345")
	b, err := os.ReadFile(AddrFilePath(dir, "registry"))
	if err != nil {
		t.Fatalf("addr file not written: %v", err)
	}
	if want := `"addr": "127.0.0.1:12345"`; !strings.Contains(string(b), want) {
		t.Fatalf("addr file missing %s: %s", want, b)
	}
}

func TestBindZeroPortAndReport(t *testing.T) {
	// 绑定 127.0.0.1:0 验证 :0 动态端口路径 + 地址回写（无网络抖动风险：回环随机端口）。
	dir := t.TempDir()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	if lis.Addr().String() == "127.0.0.1:0" {
		t.Fatalf("expected resolved port, got %q", lis.Addr().String())
	}
	WriteAddrFile(dir, "control", lis.Addr().String())
	if _, err := os.Stat(AddrFilePath(dir, "control")); err != nil {
		t.Fatalf("addr file missing: %v", err)
	}
}
