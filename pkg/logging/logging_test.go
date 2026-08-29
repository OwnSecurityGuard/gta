package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureInit 在临时目录中按 cfg 初始化并写入一条日志，返回文件内容与 stderr 捕获。
// 通过 slog.Default() 的输出无法直接重定向 stderr（os.Stderr 是包级变量），
// 因此这里用 os.Pipe 捕获进程 stderr，验证双写行为。
func captureInit(t *testing.T, cfg Config) (fileContent, stderrContent string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() {
		os.Stderr = orig
	}()

	logger, err := Init(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello", "k", "v")
	t.Cleanup(closeRotatingForTest)

	// 先关掉 pipe 写端再读，保证读到已写入内容。
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	stderrContent = string(buf[:n])
	_ = r.Close()

	if cfg.FilePath != "" && !cfg.DisableFile {
		if b, err := os.ReadFile(cfg.FilePath); err == nil {
			fileContent = string(b)
		}
	}
	return
}

// closeRotatingForTest 关闭 Init 最近创建的 rotatingWriter，
// 避免 Windows 上文件句柄未释放导致 t.TempDir 清理失败。
func closeRotatingForTest() {
	if lastRotating != nil {
		_ = lastRotating.Close()
		lastRotating = nil
	}
}

func TestInitDefaultDualWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	file, stderr := captureInit(t, Config{
		Level:    slog.LevelInfo,
		Format:   FormatJSON,
		FilePath: path,
	})
	if !strings.Contains(file, `"msg":"hello"`) {
		t.Errorf("log file should contain the record, got: %s", file)
	}
	if !strings.Contains(stderr, `"msg":"hello"`) {
		t.Errorf("stderr should also receive the record (dual write), got: %s", stderr)
	}
}

func TestInitDisableStderr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	file, stderr := captureInit(t, Config{
		Level:         slog.LevelInfo,
		Format:        FormatJSON,
		FilePath:      path,
		DisableStderr: true,
	})
	if !strings.Contains(file, `"msg":"hello"`) {
		t.Errorf("log file should contain the record, got: %s", file)
	}
	if strings.Contains(stderr, "hello") {
		t.Errorf("stderr should be silent with DisableStderr, got: %s", stderr)
	}
}

func TestInitDisableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	file, stderr := captureInit(t, Config{
		Level:       slog.LevelInfo,
		Format:      FormatJSON,
		FilePath:    path,
		DisableFile: true,
	})
	if file != "" {
		t.Errorf("log file should not exist with DisableFile, got: %s", file)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("log file should not have been created, stat err = %v", err)
	}
	if !strings.Contains(stderr, `"msg":"hello"`) {
		t.Errorf("stderr should receive the record, got: %s", stderr)
	}
}

func TestInitDisableBothFallsBackToStderr(t *testing.T) {
	// 两个开关同时设置时 DisableFile 优先：永远保证有输出（stderr）。
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	_, stderr := captureInit(t, Config{
		Level:         slog.LevelInfo,
		Format:        FormatJSON,
		FilePath:      path,
		DisableFile:   true,
		DisableStderr: true,
	})
	if !strings.Contains(stderr, `"msg":"hello"`) {
		t.Errorf("stderr should receive the record as fallback, got: %s", stderr)
	}
}

func TestFromEnv(t *testing.T) {
	t.Run("unset keeps defaults", func(t *testing.T) {
		cfg := FromEnv(Config{FilePath: "x.log"})
		if cfg.DisableFile || cfg.DisableStderr {
			t.Errorf("unset env must not change config: %+v", cfg)
		}
	})
	t.Run("file disabled", func(t *testing.T) {
		t.Setenv(EnvFileDisabled, "true")
		cfg := FromEnv(Config{FilePath: "x.log"})
		if !cfg.DisableFile || cfg.DisableStderr {
			t.Errorf("expected DisableFile=true, got: %+v", cfg)
		}
	})
	t.Run("stderr disabled", func(t *testing.T) {
		t.Setenv(EnvStderrDisabled, "1")
		cfg := FromEnv(Config{FilePath: "x.log"})
		if cfg.DisableFile || !cfg.DisableStderr {
			t.Errorf("expected DisableStderr=true, got: %+v", cfg)
		}
	})
	t.Run("invalid value ignored", func(t *testing.T) {
		t.Setenv(EnvFileDisabled, "not-a-bool")
		cfg := FromEnv(Config{FilePath: "x.log"})
		if cfg.DisableFile {
			t.Errorf("invalid bool must be ignored, got: %+v", cfg)
		}
	})
}
