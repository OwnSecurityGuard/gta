// Package plugindev is the Developer Plane: it owns the filesystem and
// subprocesses needed to scaffold, build, discover and activate decoder
// plugins. It deliberately contains no gRPC or MCP code — those layers live in
// pkg/plugindev/server and cmd/gta-mcp respectively — so the same logic can run
// in-process (embedded in gta-mcp for dev) or as a separate gta-plugin-dev
// binary for physical isolation in production.
package plugindev

// ScaffoldRequest asks the Developer Plane to render the create_plugin skeleton
// into Root/Name. Root is injected by the server from its configured plugins
// directory; clients (MCP) never specify it.
type ScaffoldRequest struct {
	Name            string
	Protocol        string
	ProtocolVersion string
	Hints           []string
	Root            string
}

// ScaffoldResponse reports what Scaffold produced.
type ScaffoldResponse struct {
	Name      string
	OutputDir string
	Created   []string
}

// DiscoveredPlugin is a plugin found on disk by ListPlugins.
type DiscoveredPlugin struct {
	Name   string
	Binary string
	Dir    string
}

// BuildRequest asks the Developer Plane to compile the plugin project at
// Root/Name. TimeoutSec bounds the build (default 120).
type BuildRequest struct {
	Root       string
	Name       string
	TimeoutSec int
}

// BuildError is a single structured compiler diagnostic.
type BuildError struct {
	File    string
	Line    int
	Col     int
	Message string
}

// BuildResponse is the result of a build. A non-zero exit is a normal result
// (OK=false with parsed Errors), not a transport error.
type BuildResponse struct {
	OK     bool
	Errors []*BuildError
	Output string
}
