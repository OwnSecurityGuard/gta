// Command gt-plugin-dev is the standalone Developer Plane binary. Run it to
// expose the PluginDev gRPC service on its own process, physically isolating
// all plugin development capabilities (scaffold/build/activate) from the gametrace
// runtime. gt-mcp connects to it via GT_PLUGINDEV_ADDR; when that is unset,
// gt-mcp starts an embedded instance for local development.
package main

import (
	"flag"
	"log/slog"
	"os"

	"gametrace/pkg/logging"
	"gametrace/pkg/plugindev/server"
)

func main() {
	addr := flag.String("addr", envOr("GT_PLUGINDEV_ADDR", ":8089"), "PluginDev gRPC listen address (host:port, unix:/path, npipe:\\.\\pipe\\name)")
	pluginsDir := flag.String("plugins-dir", envOr("GT_PLUGINS_DIR", "./plugins"), "root plugins directory the service is scoped to")
	// 日志（pkg/logging 统一初始化，T17）：默认仅 stderr，与历史行为一致；
	// 配置 -log-file 后启用落盘 + 轮转，默认与 stderr 双写（GT_LOG_STDERR_DISABLED=1 可关）。
	logFile := flag.String("log-file", envOr("GT_PLUGINDEV_LOG_FILE", ""), "log file path; empty = stderr only (rotating when set, dual-write to stderr by default)")
	logFormat := flag.String("log-format", envOr("GT_PLUGINDEV_LOG_FORMAT", "text"), "log output format: text | json")
	flag.Parse()

	logCfg := logging.DefaultConfig()
	logCfg.Format = logging.Format(*logFormat)
	logCfg.FilePath = *logFile
	logCfg = logging.FromEnv(logCfg)
	logging.MustInit(logCfg)

	slog.Info("starting gt-plugin-dev", "addr", *addr, "plugins_dir", *pluginsDir)
	srv := server.New(*pluginsDir)
	if err := srv.Start(*addr); err != nil {
		slog.Error("plugindev server exited", "error", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
