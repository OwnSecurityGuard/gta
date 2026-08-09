// Command gta-plugin-dev is the standalone Developer Plane binary. Run it to
// expose the PluginDev gRPC service on its own process, physically isolating
// all plugin development capabilities (scaffold/build/activate) from the gta
// runtime. gta-mcp connects to it via GTA_PLUGINDEV_ADDR; when that is unset,
// gta-mcp starts an embedded instance for local development.
package main

import (
	"flag"
	"log/slog"
	"os"

	"gta/pkg/plugindev/server"
)

func main() {
	addr := flag.String("addr", envOr("GTA_PLUGINDEV_ADDR", ":8089"), "PluginDev gRPC listen address (host:port, unix:/path, npipe:\\.\\pipe\\name)")
	pluginsDir := flag.String("plugins-dir", envOr("GTA_PLUGINS_DIR", "./plugins"), "root plugins directory the service is scoped to")
	flag.Parse()

	slog.Info("starting gta-plugin-dev", "addr", *addr, "plugins_dir", *pluginsDir)
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
