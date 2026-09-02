package plugindev

import (
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// launchedProc is a Developer-Plane-launched plugin process. It is tracked so
// Deactivate can tear it down and Status can report the dev-side runtime view.
type launchedProc struct {
	name       string
	cmd        *exec.Cmd
	pid        int
	instanceID string
	launchedAt time.Time
	exited     atomic.Bool
}

// alive reports whether the process is still running. It is set false by the
// wait goroutine started in trackProc once cmd.Wait returns.
func (p *launchedProc) alive() bool {
	return !p.exited.Load()
}

// Tracker holds the Developer Plane's ephemeral, in-process state: the
// processes it launched, the last build/activate/deactivate attempt per plugin,
// and any validated proof. It is intentionally NOT persisted — the filesystem
// (artifact) and the registry (runtime) are the sources of truth; Tracker only
// caches live process handles and recent attempts for attribution.
//
// A single Developer Plane runs per process (embedded in gta-mcp or as the
// standalone gta-plugin-dev binary), so a package-level default instance is
// sufficient.
type Tracker struct {
	mu          sync.Mutex
	procs       map[string]*launchedProc
	lastTry     map[string]*LastAttempt
	validated   map[string]*ValidatedProof
	lastExplain map[string]*ExplainResult
	// lastVerify holds the most recent plugin.verify result per plugin so
	// plugin.explain (P3b) can attribute it without the AI re-supplying the
	// corpus. It is the cross-plane proof's live counterpart to validated.
	lastVerify map[string]*VerifyResult
}

// defaultTracker is the process-wide Developer Plane state.
var defaultTracker = NewTracker()

// DefaultTracker returns the process-wide Developer Plane tracker. It is exposed
// for test wiring and embedded-service setup that must share the same state.
func DefaultTracker() *Tracker { return defaultTracker }

// NewTracker constructs an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		procs:       make(map[string]*launchedProc),
		lastTry:     make(map[string]*LastAttempt),
		validated:   make(map[string]*ValidatedProof),
		lastExplain: make(map[string]*ExplainResult),
		lastVerify:  make(map[string]*VerifyResult),
	}
}

func (t *Tracker) recordAttempt(name string, a *LastAttempt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastTry[name] = a
}

// RecordBuild stores the result of a build as the plugin's last attempt, and
// clears any validated proof (design §2.2: a successful build downgrades
// validated → compiled).
func (t *Tracker) RecordBuild(name string, dur time.Duration, resp *BuildResponse) {
	ok := resp != nil && resp.OK
	a := &LastAttempt{Action: "build", OK: ok, At: time.Now(), Duration: dur}
	if resp != nil {
		a.Errors = resp.Errors
		a.Message = resp.Output
	}
	t.recordAttempt(name, a)
	if ok {
		t.mu.Lock()
		delete(t.validated, name)
		delete(t.lastVerify, name)
		t.mu.Unlock()
	}
}

// RecordActivate stores the result of an activate attempt.
func (t *Tracker) RecordActivate(name string, dur time.Duration, ok bool, msg string) {
	t.recordAttempt(name, &LastAttempt{Action: "activate", OK: ok, At: time.Now(), Duration: dur, Message: msg})
}

// RecordDeactivate stores the result of a deactivate attempt.
func (t *Tracker) RecordDeactivate(name string, dur time.Duration, ok bool, msg string) {
	t.recordAttempt(name, &LastAttempt{Action: "deactivate", OK: ok, At: time.Now(), Duration: dur, Message: msg})
}

// SetValidated records the cross-plane proof that an artifact reached
// validated. Called by plugin.verify (P4).
func (t *Tracker) SetValidated(name string, p *ValidatedProof) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.validated[name] = p
}

// ClearValidated forgets any validated proof for a plugin (e.g. after a build).
func (t *Tracker) ClearValidated(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.validated, name)
}

// LastAttempt returns the most recent attempt for a plugin, or nil.
func (t *Tracker) LastAttempt(name string) *LastAttempt {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastTry[name]
}

// ValidatedProof returns the current validated proof, or nil.
func (t *Tracker) ValidatedProof(name string) *ValidatedProof {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.validated[name]
}

// RecordExplain stores the latest plugin.explain conclusion for a plugin so its
// ref can be surfaced via last_attempt.explain_ref (design §2.3 / P3a).
func (t *Tracker) RecordExplain(name string, res *ExplainResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastExplain[name] = res
}

// ExplainResultOf returns the latest plugin.explain conclusion for a plugin, or nil.
func (t *Tracker) ExplainResultOf(name string) *ExplainResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastExplain[name]
}

// RecordVerify stores the most recent plugin.verify result for a plugin so
// plugin.explain (P3b) can attribute it later via action=verify. A successful
// build clears it (RecordBuild), consistent with the validated invalidation
// rule (design §2.2): a fresh binary invalidates prior verification evidence.
func (t *Tracker) RecordVerify(name string, r *VerifyResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastVerify[name] = r
}

// LastVerify returns the most recent verify result for a plugin, or nil.
func (t *Tracker) LastVerify(name string) *VerifyResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastVerify[name]
}

// RecordVerify is the package-level entry point for plugin.verify (P4) to store
// a plugin's latest verify result on the process-wide tracker.
func RecordVerify(name string, r *VerifyResult) { defaultTracker.RecordVerify(name, r) }

// trackProc records a launched process under name, replacing any prior entry,
// and starts a goroutine that marks it exited (and drops it from the map) once
// it terminates.
func (t *Tracker) trackProc(p *launchedProc) {
	t.mu.Lock()
	t.procs[p.name] = p
	t.mu.Unlock()
	go func() {
		_ = p.cmd.Wait()
		p.exited.Store(true)
		t.dropIf(p)
	}()
}

// proc returns the launched process for name, or nil.
func (t *Tracker) proc(name string) *launchedProc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.procs[name]
}

// dropProc forgets the launched process for name.
func (t *Tracker) dropProc(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.procs, name)
}

// dropIf removes name only if it still maps to p (so a re-launched instance
// isn't accidentally dropped by a stale wait goroutine).
func (t *Tracker) dropIf(p *launchedProc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.procs[p.name]; ok && cur == p {
		delete(t.procs, p.name)
	}
}
