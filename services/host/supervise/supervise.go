// Package supervise is the host's supervision tree: ONE root supervisor with a
// child supervisor per unit (Suture v4), running go-plugin subprocesses as its
// units. Suture owns restart policy; this package owns what Suture cannot know
// — what a unit IS (staged, sha-pinned, env-isolated, health-probed,
// reattachable) and what pix-host must SEE (typed status and events). One
// child supervisor per unit: a unit returning ErrDoNotRestart takes out only its own subtree.
package supervise

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
)

// Budgets are the time/failure budgets every unit runs under.
type Budgets struct {
	Handshake      time.Duration // go-plugin start + first dispense
	HealthInterval time.Duration // how often a running unit is probed
	HealthTimeout  time.Duration // one probe (MUST be < interval, or probes stack)
	HealthFailures int           // consecutive failed probes that evict a unit
	Drain          time.Duration // in-flight calls may finish before the kill
	Stop           time.Duration // whole-unit stop budget (drain + kill)

	FailureBackoff   time.Duration // Suture's restart policy
	FailureThreshold float64
	FailureDecay     float64
}

// DefaultBudgets are the production values, PINNED in code (not config) so nobody widens the stop budget past what launchd will wait for.
func DefaultBudgets() Budgets {
	return Budgets{
		Handshake:        30 * time.Second,
		HealthInterval:   5 * time.Second,
		HealthTimeout:    3 * time.Second,
		HealthFailures:   3,
		Drain:            5 * time.Second,
		Stop:             15 * time.Second,
		FailureBackoff:   2 * time.Second,
		FailureThreshold: 5,
		FailureDecay:     30,
	}
}

// UnitSpec is the SINGLE shape the supervisor consumes — built-in self-exec,
// [plugins.*] override, pack [[services]] entry — so no path skips Validate.
type UnitSpec struct {
	Name string // unit name (also the reattach state key)
	Kind string // go-plugin map key to dispense (memory, broker, ...)

	// Exactly one of SelfExec or Path. SelfExec re-execs THIS binary as
	// `<self> plugin <kind>` (no third-party bytes, nothing to pin); Path is an
	// external executable, which MUST carry a full sha256 pin.
	SelfExec bool
	Path     string
	SHA      string

	Argv     []string // arguments passed to the unit (uninterpreted)
	EnvAllow []string // env REFERENCE NAMES inherited from the parent
	EnvGrant []string // explicit KEY=VALUE grants for THIS unit only
}

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	shaHexRe  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// Validate fails closed. Every rejection here is a launch that does not happen.
func (s UnitSpec) Validate() error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return fmt.Errorf("unit: empty name")
	case strings.TrimSpace(s.Kind) == "":
		return fmt.Errorf("unit %s: empty kind", s.Name)
	case s.SelfExec == (s.Path != ""):
		return fmt.Errorf("unit %s: exactly one of self-exec or an external path is required", s.Name)
	}
	if s.Path != "" {
		switch {
		case s.SHA == "":
			return fmt.Errorf("unit %s: refusing an unpinned external executable %s (external units must be sha256-pinned)", s.Name, s.Path)
		case !shaHexRe.MatchString(strings.TrimSpace(s.SHA)):
			return fmt.Errorf("unit %s: sha256 pin must be 64 hex chars, got %q", s.Name, s.SHA)
		case !filepath.IsAbs(s.Path):
			return fmt.Errorf("unit %s: external path %q must be absolute", s.Name, s.Path)
		}
	}
	for _, n := range s.EnvAllow {
		// Names only: an '=' assignment, an op:// ref or a pasted token is a
		// VALUE, and values never travel in a unit declaration.
		if !envNameRe.MatchString(n) {
			return fmt.Errorf("unit %s: env %q is not a bare reference name (names only, never values)", s.Name, n)
		}
	}
	for _, kv := range s.EnvGrant {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || !envNameRe.MatchString(kv[:i]) {
			return fmt.Errorf("unit %s: env grant %q must be KEY=VALUE", s.Name, redactKV(kv))
		}
	}
	return nil
}

// identity is the fingerprint a reattach must match: change the executable, pin, kind or argv and the surviving process is NOT this unit.
func (s UnitSpec) identity() string {
	h := sha256.New()
	for _, part := range append([]string{s.Name, s.Kind, s.Path, strings.ToLower(s.SHA), fmt.Sprint(s.SelfExec)}, s.Argv...) {
		fmt.Fprintf(h, "%d:%s\x00", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// redactKV keeps a grant's key but never echoes its value into an error.
func redactKV(kv string) string {
	if i := strings.IndexByte(kv, '='); i > 0 {
		return kv[:i] + "=<redacted>"
	}
	return "<redacted>"
}

// NewExternalUnit is the constructor every EXTERNAL unit is wired through (an
// operator's [plugins.*] block, a pack-admitted [[services]] entry), so a pack can never obtain a unit the config path could not.
func NewExternalUnit(name, kind, path, sha string, argv, envAllow []string) (UnitSpec, error) {
	u := UnitSpec{Name: name, Kind: kind, Path: path, SHA: strings.ToLower(strings.TrimSpace(sha)), Argv: argv, EnvAllow: envAllow}
	if err := u.Validate(); err != nil {
		return UnitSpec{}, err
	}
	return u, nil
}

// FilterEnv builds a child environment from an allowlist of names plus explicit
// grants; nothing else crosses the process boundary (cloud creds, agent sockets, a bearer exactly one unit may see).
func FilterEnv(allow []string, grant []string) []string {
	allowed := make(map[string]bool, len(allow))
	for _, n := range allow {
		allowed[n] = true
	}
	out := make([]string, 0, len(allow)+len(grant))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && allowed[kv[:i]] {
			out = append(out, kv)
		}
	}
	return append(out, grant...)
}

// FileSHA256 is the hex sha256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// StageExecutable copies an external unit's binary into the supervisor-owned
// staging dir, verifies the pin against the bytes it copied, and returns the
// STAGED path. Execing that copy (never the original) closes the
// verify-then-exec TOCTOU; a mismatch leaves nothing behind.
func StageExecutable(stageDir, unit, src, sha string) (string, error) {
	want := strings.ToLower(strings.TrimSpace(sha))
	if !shaHexRe.MatchString(want) {
		return "", fmt.Errorf("unit %s: sha256 pin must be 64 hex chars", unit)
	}
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return "", fmt.Errorf("stage dir: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open unit binary: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(stageDir, unit+".stage-*")
	if err != nil {
		return "", fmt.Errorf("stage unit binary: %w", err)
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), in); err != nil {
		tmp.Close()
		return "", fmt.Errorf("stage unit binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("stage unit binary: %w", err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", fmt.Errorf("unit %s: %s sha256 mismatch: got %s, want %s (refusing to launch)", unit, src, got, want)
	}
	// 0500: executable by the owner, writable by nobody — including us.
	if err := os.Chmod(tmp.Name(), 0o500); err != nil {
		return "", fmt.Errorf("stage unit binary: %w", err)
	}
	dst := filepath.Join(stageDir, unit+"-"+want[:12])
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", fmt.Errorf("stage unit binary: %w", err)
	}
	return dst, nil
}

// Holder carries the dispensed client for one unit. A restart swaps it under
// the readers, so an HTTP shim never learns its backing process changed; Use tracks in-flight calls so a stop can DRAIN.
type Holder struct {
	mu       sync.RWMutex
	impl     any
	client   *goplugin.Client
	inflight sync.WaitGroup
}

// Get returns the dispensed impl, or nil when the unit is down.
func (h *Holder) Get() any {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.impl
}

// Set installs a newly dispensed impl.
func (h *Holder) Set(impl any, c *goplugin.Client) {
	h.mu.Lock()
	h.impl, h.client = impl, c
	h.mu.Unlock()
}

// Clear takes the unit out of service (callers see "unavailable" immediately).
func (h *Holder) Clear() { h.Set(nil, nil) }

// Use runs fn against the dispensed impl holding a drain reference, so a
// shutdown waits for it (up to the drain budget) instead of killing it midway.
func (h *Holder) Use(fn func(impl any) error) error {
	impl := h.Get()
	if impl == nil {
		return fmt.Errorf("unit unavailable")
	}
	h.inflight.Add(1)
	defer h.inflight.Done()
	return fn(impl)
}

// drain waits for in-flight Use calls, bounded by the drain budget.
func (h *Holder) drain(budget time.Duration) bool {
	done := make(chan struct{})
	go func() { h.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}
