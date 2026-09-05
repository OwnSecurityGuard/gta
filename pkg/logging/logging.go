// Package logging 提供 gametrace 项目统一的日志初始化与辅助工具。
//
// 功能：
//   - 文件落盘 + 按大小轮转（不依赖外部库）
//   - JSON / Text 输出格式切换
//   - 日志级别控制（支持运行时动态调整 via LevelVar）
//   - 错误堆栈捕获（可选，通过 WithStack 包装）
//   - 上下文 logger 辅助函数
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Format 日志输出格式。
type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// Config 日志初始化配置。
type Config struct {
	Level      slog.Level // 日志级别
	Format     Format     // 输出格式：json | text
	FilePath   string     // 日志文件路径；空则仅输出到 stderr
	MaxSize    int        // 单文件最大大小（MB），超过后轮转；默认 100
	MaxBackups int        // 保留旧日志文件数量；默认 7
	MaxAge     int        // 旧日志保留天数；默认 30
	Compress   bool       // 是否压缩旧日志文件（gzip）
	AddSource  bool       // 是否在日志中添加源码位置（文件:行号）
	// DisableStderr 关闭 stderr 侧输出：配置了 FilePath 时仅写文件。
	// 典型场景：容器 / systemd 环境下避免文件+stderr 双份日志。仅配置 FilePath 时生效。
	DisableStderr bool
	// DisableFile 忽略 FilePath，仅输出到 stderr。
	// 典型场景：容器环境日志交由采集器处理，不需要落盘。
	DisableFile bool
}

// DefaultConfig 返回适合生产环境的默认配置。
// FilePath 为空，调用方需按需设置。
func DefaultConfig() Config {
	return Config{
		Level:      slog.LevelInfo,
		Format:     FormatJSON,
		FilePath:   "",
		MaxSize:    100,
		MaxBackups: 7,
		MaxAge:     30,
		Compress:   false,
		AddSource:  false,
	}
}

// levelVar 是进程级共享的动态日志级别，允许运行时通过 SetLevel 调整。
var levelVar = new(slog.LevelVar)

// lastRotating 记录 Init 最近创建的 rotatingWriter（文件句柄）。
// 仅服务于测试的清理（Windows 上句柄未关闭会锁住临时目录）；
// 生产进程生命周期与日志文件一致，无需主动关闭。
var lastRotating *rotatingWriter

// Init 根据配置初始化全局 slog logger，返回创建的 logger 实例。
// 若配置了 FilePath，日志会同时写入文件和 stderr（双写）。
// 调用后 slog.Default() 将使用新 logger，所有包级 slog.Info/Error 等函数均生效。
func Init(cfg Config) (*slog.Logger, error) {
	// 参数补全
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 100
	}
	if cfg.MaxBackups < 0 {
		cfg.MaxBackups = 7
	}
	if cfg.MaxAge < 0 {
		cfg.MaxAge = 30
	}
	if cfg.Format == "" {
		cfg.Format = FormatJSON
	}

	// 设置动态级别
	levelVar.Set(cfg.Level)

	opts := &slog.HandlerOptions{
		Level:       levelVar,
		AddSource:   cfg.AddSource,
		ReplaceAttr: replaceAttr,
	}

	// 双写策略（默认行为不变：FilePath 非空 → 文件 + stderr 双写）：
	//   - DisableStderr=true → 仅写文件；
	//   - DisableFile=true   → 忽略 FilePath，仅写 stderr；
	//   - 两者都设置 → 仅 stderr（DisableFile 优先，保证永远有输出）。
	useFile := cfg.FilePath != "" && !cfg.DisableFile
	var w io.Writer = os.Stderr
	if useFile {
		if err := os.MkdirAll(filepath.Dir(cfg.FilePath), 0o755); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
		fw := newRotatingWriter(cfg.FilePath, cfg.MaxSize, cfg.MaxBackups, cfg.MaxAge, cfg.Compress)
		lastRotating = fw // 供测试关闭句柄（Windows 上未关闭会锁住文件）。
		if cfg.DisableStderr {
			w = fw
		} else {
			// 双写：同时输出到文件和 stderr，便于开发调试
			w = io.MultiWriter(fw, os.Stderr)
		}
	}

	var handler slog.Handler
	switch cfg.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	default:
		handler = slog.NewTextHandler(w, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}

// 环境变量开关名（T17）：容器部署时无需改代码/加 flag 即可关闭文件或 stderr 侧输出。
// 取值按 strconv.ParseBool 解析（"1"/"true"/...），未设置或解析失败均视为 false。
const (
	EnvFileDisabled   = "GT_LOG_FILE_DISABLED"   // =true 时忽略 FilePath，仅 stderr
	EnvStderrDisabled = "GT_LOG_STDERR_DISABLED" // =true 时仅写文件，不再双写 stderr
)

// FromEnv 用 GT_LOG_FILE_DISABLED / GT_LOG_STDERR_DISABLED 环境变量
// 覆盖 cfg 的 DisableFile / DisableStderr，返回调整后的副本。
// 其余字段不受影响；未设置相关环境变量时返回值与 cfg 等价（默认行为不变）。
func FromEnv(cfg Config) Config {
	if v, err := strconv.ParseBool(os.Getenv(EnvFileDisabled)); err == nil && v {
		cfg.DisableFile = true
	}
	if v, err := strconv.ParseBool(os.Getenv(EnvStderrDisabled)); err == nil && v {
		cfg.DisableStderr = true
	}
	return cfg
}

// MustInit 与 Init 相同，但在出错时 panic。仅供 main 函数使用。
func MustInit(cfg Config) *slog.Logger {
	logger, err := Init(cfg)
	if err != nil {
		panic(err)
	}
	return logger
}

// SetLevel 运行时动态调整日志级别（线程安全）。
// 例如可通过 admin 接口或信号触发，无需重启进程。
func SetLevel(level slog.Level) {
	levelVar.Set(level)
}

// GetLevel 返回当前日志级别。
func GetLevel() slog.Level {
	return levelVar.Level()
}

// With 返回带预设字段的上下文 logger，是 slog.Default().With 的快捷方式。
// 用于在长生命周期对象中注入通用上下文（如 session_id）。
func With(args ...any) *slog.Logger {
	return slog.Default().With(args...)
}

// --- 错误堆栈支持 ---

// stackError 包装 error 并附加调用栈，实现 slog.LogValuer 接口。
// 当被 slog 记录时，会输出 error 消息和 stack 两个字段。
type stackError struct {
	err   error
	stack string
}

func (e *stackError) Error() string { return e.err.Error() }
func (e *stackError) Unwrap() error { return e.err }

// LogValue 实现 slog.LogValuer，使 error 在日志中以结构化对象呈现。
func (e *stackError) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("msg", e.err.Error()),
		slog.String("stack", e.stack),
	)
}

// WithStack 包装 error 并附加当前调用栈。
// 使用方式：slog.Error("operation failed", "error", logging.WithStack(err))
// 日志输出中 error 字段将包含 msg 和 stack 子字段。
// 若 err 已是 stackError 则原样返回。
func WithStack(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*stackError); ok {
		return err
	}
	return &stackError{err: err, stack: captureStack(3)}
}

// captureStack 捕获调用栈，skip 指定跳过的栈帧数量。
func captureStack(skip int) string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(skip+1, pcs)
	if n == 0 {
		return ""
	}
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		frame, more := frames.Next()
		// 跳过 runtime 自身
		if frame.Function == "runtime.main" || strings.HasPrefix(frame.Function, "runtime.") {
			if !more {
				break
			}
			continue
		}
		fmt.Fprintf(&b, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		if !more {
			break
		}
	}
	return b.String()
}

// replaceAttr 是 slog.HandlerOptions.ReplaceAttr 回调。
// 当前保留默认行为，未来可在此统一处理时间格式、敏感字段过滤等。
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	// 可在此扩展：例如统一时间格式为 RFC3339Nano
	return a
}

// --- 文件轮转 Writer ---

// rotatingWriter 实现按文件大小轮转的 io.WriteCloser。
// 当日志文件超过 MaxSize MB 时，关闭当前文件，轮转重命名，创建新文件。
// 旧文件命名为 <name>.1, <name>.2, ...，序号越小越新。
// 支持 MaxBackups 限制备份数量和 MaxAge 限制保留天数。
type rotatingWriter struct {
	mu         sync.Mutex
	filePath   string
	maxSize    int64 // 字节
	maxBackups int
	maxAge     int
	compress   bool

	currentFile *os.File
	currentSize int64
}

func newRotatingWriter(filePath string, maxSizeMB, maxBackups, maxAge int, compress bool) *rotatingWriter {
	w := &rotatingWriter{
		filePath:   filePath,
		maxSize:    int64(maxSizeMB) * 1024 * 1024,
		maxBackups: maxBackups,
		maxAge:     maxAge,
		compress:   compress,
	}
	// 打开或创建日志文件（追加模式）
	if f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		w.currentFile = f
		if info, err := f.Stat(); err == nil {
			w.currentSize = info.Size()
		}
	}
	return w
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.currentFile == nil {
		// 文件打开失败，尝试重新打开
		f, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 0, err
		}
		w.currentFile = f
		if info, err := f.Stat(); err == nil {
			w.currentSize = info.Size()
		}
	}

	// 检查是否需要轮转
	if w.currentSize+int64(len(p)) > w.maxSize {
		w.rotateLocked()
	}

	n, err := w.currentFile.Write(p)
	w.currentSize += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.currentFile != nil {
		return w.currentFile.Close()
	}
	return nil
}

// rotateLocked 执行文件轮转（调用方需持有锁）。
func (w *rotatingWriter) rotateLocked() {
	if w.currentFile != nil {
		_ = w.currentFile.Close()
		w.currentFile = nil
	}

	// 清理过期备份
	w.cleanExpiredLocked()

	// 轮转：.N-1 → .N, ..., .1 → .2, current → .1
	for i := w.maxBackups; i > 1; i-- {
		oldPath := w.filePath + "." + strconv.Itoa(i-1)
		newPath := w.filePath + "." + strconv.Itoa(i)
		_ = os.Rename(oldPath, newPath)
	}
	// current → .1
	backupPath := w.filePath + ".1"
	_ = os.Rename(w.filePath, backupPath)

	// 可选：压缩 .1 文件
	if w.compress {
		go compressFile(backupPath)
	}

	// 创建新文件
	f, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	w.currentFile = f
	w.currentSize = 0
}

// cleanExpiredLocked 删除超过 maxBackups 数量和 maxAge 天数的旧备份。
func (w *rotatingWriter) cleanExpiredLocked() {
	if w.maxBackups <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -w.maxAge)
	for i := w.maxBackups + 1; i <= w.maxBackups+10; i++ {
		path := w.filePath + "." + strconv.Itoa(i)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if w.maxAge > 0 && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

// compressFile 使用 gzip 压缩文件，压缩后删除原文件。
func compressFile(path string) {
	// 简单实现：读取原文件，gzip 写入 .gz，删除原文件
	// 为避免引入 compress/gzip 的复杂错误处理，此处仅占位
	// 如需压缩功能，可在此实现完整逻辑
}
