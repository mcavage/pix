// Package health is the one readiness model every pix command renders.
//
// The model is the smallest one that still tells the truth:
//
//	Probe    one thing we can go and check, bounded by a context
//	Result   what that check PROVED — ready, absent, denied, or unknown
//	Snapshot the results of one invocation, and the exit code derived from them
//
// Three properties are load-bearing, and each one is a bug this package exists
// to make unrepresentable:
//
//  1. unknown is never ready and never absent. "I could not check" is its own
//     answer. A probe that times out, crashes, or returns garbage says unknown,
//     so no surface can render a false ✓ or a false "you are missing this".
//  2. unknown alone is not a failure. A host nobody could fully check does not
//     fail the process; only a POSITIVELY VERIFIED gap in something REQUIRED
//     does. Diagnosing from a bad vantage point is not the same as being broken.
//  3. a repair command is emitted only by a verified gap. Fixes are stripped
//     from ready and unknown results in Run, centrally, so a green report can
//     never print a TODO underneath itself.
package health

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Status is what a probe proved.
type Status string

const (
	// StatusReady: verified working.
	StatusReady Status = "ready"
	// StatusAbsent: verified NOT working or not present, with an exact fix.
	StatusAbsent Status = "absent"
	// StatusDenied: positively refused by policy or permission. Blocks like
	// absent when required, but the remedy is organizational.
	StatusDenied Status = "denied"
	// StatusUnknown: could not be determined from here — a timeout, an
	// unreadable answer, a probe dependency that is itself missing. The zero
	// value, so an unset Status can only ever fail safe.
	StatusUnknown Status = "unknown"
)

// Result is one probe's answer.
type Result struct {
	Name     string
	Status   Status
	Required bool
	// Detail is the short human note rendered after the name.
	Detail string
	// Fix is the exact copy-pasteable repair command. Only ever surfaced for a
	// verified gap; Run clears it otherwise.
	Fix string
	// Evidence is the concrete proof behind the status — the command that ran,
	// the port that answered, the token that matched.
	Evidence string
	// Took is how long the probe spent.
	Took time.Duration
}

// Effective returns the status, treating anything unrecognized (including the
// zero value) as unknown.
func (r Result) Effective() Status {
	switch r.Status {
	case StatusReady, StatusAbsent, StatusDenied:
		return r.Status
	default:
		return StatusUnknown
	}
}

// OK reports a verified-working result. Unknown is never OK.
func (r Result) OK() bool { return r.Effective() == StatusReady }

// Missing reports a verified gap (absent or denied). Unknown is never missing.
func (r Result) Missing() bool {
	s := r.Effective()
	return s == StatusAbsent || s == StatusDenied
}

// Blocking reports whether this result alone fails the process: a verified gap
// in something required.
func (r Result) Blocking() bool { return r.Required && r.Missing() }

// Probe is one thing health can go and check. Check must honour ctx: Run
// bounds every probe, and a probe that ignores the deadline is reported
// unknown rather than allowed to wedge the command.
type Probe interface {
	Name() string
	Required() bool
	Check(ctx context.Context) Result
}

// Snapshot is one invocation's results, in probe order.
type Snapshot struct {
	Results []Result
	Elapsed time.Duration
}

// Exit codes. Two states plus the usage code every command already owns:
//
//	0 — nothing required is verifiably broken (including "could not check")
//	1 — at least one required probe verified a gap
//	2 — usage error, produced by argument parsing before any probe runs
const (
	ExitOK       = 0
	ExitNotReady = 1
	ExitUsage    = 2
)

// DefaultBudget bounds a single probe when a caller does not pick one.
const DefaultBudget = 5 * time.Second

// Run checks every probe concurrently under its own budget and returns the
// results in probe order. It is the ONE place a Result is normalized, so no
// probe can leak a fix onto a ready or unknown answer, and a panicking probe
// degrades to unknown instead of taking the command down with it.
func Run(ctx context.Context, budget time.Duration, probes ...Probe) Snapshot {
	if budget <= 0 {
		budget = DefaultBudget
	}
	start := time.Now()
	out := make([]Result, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func(i int, p Probe) {
			defer wg.Done()
			out[i] = runOne(ctx, budget, p)
		}(i, p)
	}
	wg.Wait()
	return Snapshot{Results: out, Elapsed: time.Since(start)}
}

func runOne(ctx context.Context, budget time.Duration, p Probe) (res Result) {
	began := time.Now()
	defer func() {
		if r := recover(); r != nil {
			// A probe bug must not be able to fail the host's readiness, and
			// must not be able to claim readiness either.
			res = Result{Name: p.Name(), Required: p.Required(), Status: StatusUnknown,
				Detail: "probe failed internally", Evidence: fmt.Sprintf("panic: %v", r)}
		}
		res.Name = orDefault(res.Name, p.Name())
		res.Required = p.Required()
		if res.Took == 0 {
			res.Took = time.Since(began)
		}
		if !res.Missing() {
			res.Fix = ""
		}
	}()
	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return p.Check(pctx)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// Ready reports whether every REQUIRED probe proved ready. Unknown is not
// ready: nothing was proven.
func (s Snapshot) Ready() bool {
	for _, r := range s.Results {
		if r.Required && !r.OK() {
			return false
		}
	}
	return true
}

// Blocking returns the required results that verified a gap — the only thing
// that fails the process.
func (s Snapshot) Blocking() []Result { return s.filter(func(r Result) bool { return r.Blocking() }) }

// Unknown returns the results nothing could be proven about.
func (s Snapshot) Unknown() []Result {
	return s.filter(func(r Result) bool { return r.Effective() == StatusUnknown })
}

// Gaps returns every verified gap, required or not, in probe order.
func (s Snapshot) Gaps() []Result { return s.filter(func(r Result) bool { return r.Missing() }) }

// OptionalGaps names the OPTIONAL checks that verified a gap. Optional means
// "does not fail the process" — it must not come to mean "is not mentioned".
// An optional check that proved something is broken proved it just as hard as a
// required one, and the headline is the only line some people read.
func (s Snapshot) OptionalGaps() []string {
	var out []string
	for _, r := range s.Results {
		if !r.Required && r.Missing() {
			out = append(out, r.Name)
		}
	}
	return out
}

func (s Snapshot) filter(keep func(Result) bool) []Result {
	var out []Result
	for _, r := range s.Results {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// Fixes returns the exact repair commands, de-duplicated, in probe order.
// Only verified gaps contribute (Run already stripped every other Fix).
func (s Snapshot) Fixes() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range s.Gaps() {
		fix := strings.TrimSpace(r.Fix)
		if fix == "" || seen[fix] {
			continue
		}
		seen[fix] = true
		out = append(out, fix)
	}
	return out
}

// Find returns one probe's result by name.
func (s Snapshot) Find(name string) (Result, bool) {
	for _, r := range s.Results {
		if r.Name == name {
			return r, true
		}
	}
	return Result{}, false
}

// Names lists the probes in order.
func (s Snapshot) Names() []string {
	out := make([]string, 0, len(s.Results))
	for _, r := range s.Results {
		out = append(out, r.Name)
	}
	return out
}

// ExitCode derives the process exit code: only a verified gap in a required
// probe fails. Unknown never does.
func (s Snapshot) ExitCode() int {
	if len(s.Blocking()) > 0 {
		return ExitNotReady
	}
	return ExitOK
}
