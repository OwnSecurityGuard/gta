package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePluginTmpl 是被测插件源码模板：把收到的 GTA_* 环境变量写入编译时
// 固化的 OUTPATH（agent 拉起插件时不传 argv）；MODE=="exit" 时立即以码 3
// 退出（模拟崩溃），否则常驻。
const fakePluginTmpl = `package main

import (
	"os"
	"time"
)

func main() {
	f, _ := os.Create(OUTPATH)
	if f != nil {
		f.WriteString("registry=" + os.Getenv("GTA_REGISTRY_ADDR") + "\n")
		f.WriteString("tunnel=" + os.Getenv("GTA_TUNNEL") + "\n")
		f.WriteString("token=" + os.Getenv("GTA_AUTH_TOKEN") + "\n")
		f.Close()
	}
	if MODE == "exit" {
		os.Exit(3)
	}
	time.Sleep(600 * time.Second)
}
`

// buildFakePlugin 把 fakePluginTmpl 编译成本机可执行文件，
// 按 plugindev.ListPlugins 认可的 <root>/<name>/<name>[.exe] 布局落盘。
func buildFakePlugin(t *testing.T, root, name, outPath, mode string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "main.go")
	r := strings.NewReplacer("OUTPATH", fmt.Sprintf("%q", outPath), "MODE", fmt.Sprintf("%q", mode))
	if err := os.WriteFile(src, []byte(r.Replace(fakePluginTmpl)), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name+exeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake plugin: %v\n%s", err, out)
	}
}

func exeSuffix() string {
	if strings.HasSuffix(os.Args[0], ".exe") {
		return ".exe"
	}
	return ""
}

// waitForFileContent 轮询等待 path 出现且内容前缀匹配，超时 Fatal。
func waitForFileContent(t *testing.T, path, prefix string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.HasPrefix(string(b), prefix) {
			return string(b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared with prefix %q", path, prefix)
	return ""
}

func TestSpawnEnvAnonymous(t *testing.T) {
	env := spawnEnv("host:9091", "")
	if !containsKV(env, "GTA_REGISTRY_ADDR=host:9091") || !containsKV(env, "GTA_TUNNEL=1") {
		t.Fatalf("missing required env: %v", env)
	}
	if containsKV(env, "GTA_AUTH_TOKEN=") {
		t.Fatalf("anonymous mode must not set GTA_AUTH_TOKEN: %v", env)
	}
}

// startSupervisor 启动 supervisor 并返回 cancel；defer 调用可确保测试结束时
// 子进程与日志句柄全部释放（否则 t.TempDir 清理会失败）。
func startSupervisor(t *testing.T, sup *pluginSupervisor, expect int) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if n := sup.run(ctx); n != expect {
		cancel()
		t.Fatalf("expected %d plugins, got %d", expect, n)
	}
	return func() {
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if sup.runningCount() == 0 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestSpawnEnvPropagationFull(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "env.txt")
	buildFakePlugin(t, dir, "demo", outPath, "stay")

	sup := &pluginSupervisor{dir: dir, registryAddr: "10.1.2.3:9091", token: "gta_test", bk: newBackoff()}
	stop := startSupervisor(t, sup, 1)
	defer stop()

	got := waitForFileContent(t, outPath, "registry=", 10*time.Second)
	want := "registry=10.1.2.3:9091\ntunnel=1\ntoken=gta_test\n"
	if got != want {
		t.Fatalf("env mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestSuperviseRestartsAfterCrash(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "env.txt")
	buildFakePlugin(t, dir, "crashy", outPath, "exit")

	// 预写一份「上次运行」的痕迹，模拟重启前的写入。
	if err := os.WriteFile(outPath, []byte("PREV\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sup := &pluginSupervisor{dir: dir, registryAddr: "h:9091", token: "", bk: newBackoff()}
	stop := startSupervisor(t, sup, 1)
	defer stop()

	// 崩溃 → 1s 退避 → 重启 → 再次写文件覆盖 PREV。
	got := waitForFileContent(t, outPath, "registry=h:9091\ntunnel=1", 20*time.Second)
	if !strings.HasPrefix(got, "registry=h:9091\ntunnel=1\ntoken=\n") {
		t.Fatalf("env mismatch after restart: %q", got)
	}
}

// TestShutdownStopsPlugin 验证 agent ctx 取消后插件子进程被终止。
func TestShutdownStopsPlugin(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "env.txt")
	buildFakePlugin(t, dir, "sticky", outPath, "stay")

	sup := &pluginSupervisor{dir: dir, registryAddr: "h:9091", token: "", bk: newBackoff()}
	stop := startSupervisor(t, sup, 1)
	// 等插件真正起来再停机。
	waitForFileContent(t, outPath, "registry=", 10*time.Second)

	stop()
}

func containsKV(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
