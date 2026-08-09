// Package plugindev is the Developer Plane: it owns the filesystem and
// subprocesses needed to scaffold, build, discover, activate and deactivate
// decoder plugins. It deliberately contains no gRPC or MCP code — those layers
// live in pkg/plugindev/server and cmd/gta-mcp respectively — so the same logic
// can run in-process (embedded in gta-mcp for dev) or as a separate
// gta-plugin-dev binary for physical isolation in production.
package plugindev

import "time"

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

// ActivateRequest launches the local plugin binary at Root/Name and injects
// RegistryAddr so the plugin can register with the runtime. The Developer Plane
// owns only the process it launches (per design §1.4); production environments
// launch plugins via systemd/k8s instead.
type ActivateRequest struct {
	Root         string
	Name         string
	RegistryAddr string
}

// ActivateResponse reports the launch outcome. InstanceID is a Developer-Plane
// tracking handle (dev-<name>-<pid); the runtime-assigned instance_id appears
// in plugin.status once the plugin registers.
type ActivateResponse struct {
	InstanceID string
	OK         bool
	Message    string
}

// DeactivateRequest stops the process the Developer Plane launched for Name.
type DeactivateRequest struct {
	Root string
	Name string
}

// DeactivateResponse reports the teardown outcome.
type DeactivateResponse struct {
	OK      bool
	Message string
}

// StatusRequest asks for the dual-state view of a single plugin.
type StatusRequest struct {
	Root string
	Name string
}

// ArtifactState is the Developer Plane's view of the code: unknown → scaffolded
// → compiled → validated (see design §2.1). It is derived purely from disk.
type ArtifactState struct {
	State       string // unknown | scaffolded | compiled | validated
	SourceDir   string
	BinaryPath  string
	BinaryStale bool
}

// DevProcess is the Developer Plane's view of the process it launched for a
// plugin. It is empty (Launched=false) when the plugin was started externally
// (systemd/k8s) — in that case runtime state comes from the registry instead.
type DevProcess struct {
	Launched      bool
	PID           int
	InstanceID    string
	Alive         bool
	LaunchedAt    time.Time
}

// LastAttempt is the most recent build/activate/deactivate outcome. It is how
// the AI gets attribution for a failed step (design §2.3: failures are attached
// here rather than modelled as states). P3a's explain_ref will point back to a
// plugin.explain conclusion.
type LastAttempt struct {
	Action    string // build | activate | deactivate
	OK        bool
	At        time.Time
	Duration  time.Duration
	Errors    []*BuildError
	Message   string
	ExplainRef string
}

// ValidatedProof records the cross-plane evidence that an artifact reached the
// validated state. It is set by plugin.verify (P4) and cleared on every
// successful build (design §2.2 invalidation rule).
type ValidatedProof struct {
	VerifyRunID string
	SessionID   string
	Verdict     string
	At          time.Time
}

// PluginStatus is the aggregated dual-state view returned by Status. Runtime
// state from the registry is filled in by the MCP layer (which talks to the
// Runtime Plane); the Developer Plane contributes Artifact, DevProcess and
// LastAttempt.
type PluginStatus struct {
	Name        string
	Artifact    *ArtifactState
	DevProcess  *DevProcess
	LastAttempt *LastAttempt
}
