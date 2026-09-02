package plugindev

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// activateLivenessWait is how long Activate waits after launch to confirm the
// process didn't fatal immediately (e.g. a bad GTA_REGISTRY_ADDR). Registration
// with the runtime is confirmed separately via plugin.status, not here — this
// is only a liveness gate so we don't report success for a process that died
// on startup.
const activateLivenessWait = 1500 * time.Millisecond

// Activate launches the local plugin binary for Name with GTA_REGISTRY_ADDR
// injected, so it can register with the runtime (design §1.4: the Developer
// Plane owns the process it launches; production uses systemd/k8s instead).
// The spawned process deliberately outlives the gRPC call — its lifecycle is
// managed by Deactivate, not by the request context.
func Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.RegistryAddr == "" {
		return nil, fmt.Errorf("registry_addr is required (pass it or set GTA_REGISTRY_ADDR)")
	}

	dir := filepath.Join(req.Root, req.Name)
	binary := filepath.Join(dir, req.Name+exeExt())
	if _, err := os.Stat(binary); err != nil {
		recordActivateFail(ctx, req.Name, 0, "binary not found: "+binary)
		return nil, fmt.Errorf("plugin binary not found: %s (did you build it?)", binary)
	}

	// Refuse to double-launch; the existing process must be deactivated first.
	if lp := defaultTracker.proc(req.Name); lp != nil && lp.alive() {
		recordActivateFail(ctx, req.Name, 0, "already active (pid "+itoa(lp.pid)+")")
		return nil, fmt.Errorf("plugin %q is already active (pid %d); deactivate first", req.Name, lp.pid)
	}

	logPath := filepath.Join(dir, req.Name+".dev.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// Non-fatal: we can still launch, just won't capture startup logs.
		logFile = nil
	}

	cmd := exec.Command(binary)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GTA_REGISTRY_ADDR="+req.RegistryAddr)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		recordActivateFail(ctx, req.Name, time.Since(start), "start failed: "+err.Error())
		return nil, fmt.Errorf("start plugin %q: %w", req.Name, err)
	}
	pid := cmd.Process.Pid
	instanceID := "dev-" + req.Name + "-" + itoa(pid)

	lp := &launchedProc{
		name:       req.Name,
		cmd:        cmd,
		pid:        pid,
		instanceID: instanceID,
		launchedAt: start,
	}
	defaultTracker.trackProc(lp)
	if logFile != nil {
		// Close the log file once the wait goroutine owns the process output.
		_ = logFile.Close()
	}

	// Liveness gate: give the binary a moment; if it already exited, surface
	// the startup log so the AI can attribute the failure.
	if !lp.alive() {
		recordActivateFail(ctx, req.Name, time.Since(start), "process exited immediately")
		return nil, fmt.Errorf("plugin %q exited immediately; check %s", req.Name, logPath)
	}
	time.Sleep(activateLivenessWait)
	if !lp.alive() {
		msg := "process exited during startup"
		if tail, te := os.ReadFile(logPath); te == nil && len(tail) > 0 {
			msg += ": " + tailString(tail, 512)
		}
		recordActivateFail(ctx, req.Name, time.Since(start), msg)
		return nil, fmt.Errorf("plugin %q failed to stay up: %s", req.Name, msg)
	}

	defaultTracker.RecordActivate(req.Name, time.Since(start), true, instanceID)
	return &ActivateResponse{InstanceID: instanceID, OK: true, Message: "launched pid " + itoa(pid)}, nil
}

// Deactivate stops the process the Developer Plane launched for Name. If no such
// process exists (the plugin was started externally), it returns OK=false with
// a message so the caller can fall back to a registry deregister.
func Deactivate(ctx context.Context, req *DeactivateRequest) (*DeactivateResponse, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	lp := defaultTracker.proc(req.Name)
	if lp == nil {
		defaultTracker.RecordDeactivate(req.Name, 0, false, "no dev-launched process for "+req.Name)
		return &DeactivateResponse{OK: false, Message: "no process was launched by the Developer Plane for " + req.Name + " — activate_plugin only manages processes it started. For a plugin you launched yourself, stop it directly, or call deregister_plugin to remove it from the registry."}, nil
	}
	start := time.Now()
	if err := lp.cmd.Process.Kill(); err != nil {
		defaultTracker.RecordDeactivate(req.Name, time.Since(start), false, "kill failed: "+err.Error())
		return &DeactivateResponse{OK: false, Message: "kill failed: " + err.Error()}, nil
	}
	defaultTracker.dropProc(req.Name)
	defaultTracker.RecordDeactivate(req.Name, time.Since(start), true, "killed pid "+itoa(lp.pid))
	return &DeactivateResponse{OK: true, Message: "killed pid " + itoa(lp.pid)}, nil
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// recordActivateFail records a failed activate attempt and auto-attributes it
// (P3a) so the failure surfaces via last_attempt.explain_ref. The explain
// conclusion is intentionally best-effort; activation failures must never be
// swallowed by a failed attribution.
func recordActivateFail(ctx context.Context, name string, dur time.Duration, msg string) {
	defaultTracker.RecordActivate(name, dur, false, msg)
	_, _ = Explain(context.Background(), &ExplainRequest{Name: name})
}

// tailString returns the last n bytes of b as a string, trimming a leading
// partial line.
func tailString(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	s := string(b[len(b)-n:])
	// Drop a possible partial first line.
	if i := indexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
