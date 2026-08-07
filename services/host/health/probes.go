package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"pix/host/rpc"
	"pix/host/sys"
)

// probes.go holds the concrete probes. Every one of them classifies a real
// boundary — an exec'd process, a TCP listener, a directory on disk — into the
// four-status model, and every one of them obeys the same rule: a failure we
// cannot INTERPRET is unknown. Only a failure that positively identifies the
// gap (a binary that is not there, a connection refused, a launchd domain that
// says the label is not loaded, a key store that answered and did not list the
// key) may render absent and hand out a repair command.

// The exact fixes. They are constants because two surfaces printing slightly
// different repair commands for the same gap is how a user learns to ignore
// both.
const (
	SbxInstallFix   = "brew install docker/tap/sbx@nightly"
	ServeInstallFix = "pix serve install"
	ServeStartFix   = "pix serve"
	// ServeRestartFix composes two real, existing verbs — `serve stop` is
	// mode-aware (goes through the managed supervisor when there is one) and
	// `serve start` is the (re)start alias — rather than naming a bare
	// `restart` subcommand kong has never answered to.
	ServeRestartFix = "pix serve stop && pix serve start"
	PackUseFix      = "pix pack use <path|owner/repo>"
	// MonitorStartFix: `pix monitor` is a pure offline reader over the
	// on-disk event store (see monitorCmd) — it starts nothing. The thing
	// that actually starts the monitor ingest listener is `pix serve start`.
	MonitorStartFix = "pix serve start"
	SecretSetFix    = "pix secret set %s op://vault/item/field"
	// ModelKeyFix repairs the ANY-OF gap: pix launches a model with one
	// provider key, so the repair names one provider rather than listing three
	// commands a user must choose between.
	ModelKeyFix = "pix models add anthropic"
)

// versionish matches the digit.digit any real `--version` banner carries. It
// is the difference between "the tool answered" and "something printed bytes".
var versionish = regexp.MustCompile(`[0-9]+\.[0-9]+`)

// execOutcome is one bounded exec, classified.
type execOutcome struct {
	out      string
	notFound bool // the binary is not there — a POSITIVE absence
	timedOut bool // hit the deadline — unknown
	denied   bool // an explicit policy/permission refusal
	failed   bool // ran and exited non-zero (or died on a signal) — unknown
}

// runBounded execs argv under ctx and classifies the outcome. It never returns
// the raw error text: a registered command's argv can carry pasted secrets, so
// the diagnostics here are deliberately value-free.
func runBounded(ctx context.Context, bin string, args ...string) execOutcome {
	if strings.TrimSpace(bin) == "" {
		return execOutcome{notFound: true}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := buf.String()
	switch {
	case err == nil:
		return execOutcome{out: out}
	case ctx.Err() != nil:
		return execOutcome{out: out, timedOut: true}
	}
	var xe *exec.Error
	if errors.As(err, &xe) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return execOutcome{out: out, notFound: true}
	}
	if sys.ClassifyProbeFailure(out, err) == sys.ProbeDenied {
		return execOutcome{out: out, denied: true}
	}
	return execOutcome{out: out, failed: true}
}

// unknownExec renders the shared "we learned nothing" result for an exec probe.
func unknownExec(name string, o execOutcome, what string) Result {
	switch {
	case o.timedOut:
		return Result{Name: name, Status: StatusUnknown, Detail: "probe timed out", Evidence: what + ": deadline exceeded"}
	default:
		return Result{Name: name, Status: StatusUnknown, Detail: "probe failed", Evidence: what + ": exited non-zero"}
	}
}

// --- sbx --------------------------------------------------------------------

// SbxProbe proves the sbx CLI is installed and runnable. A missing binary is
// the one verified gap here; a broken, crashed, hung or unintelligible sbx is
// unknown, because "sbx is angry" is not "sbx is not installed".
type SbxProbe struct {
	Bin  string
	Args []string // defaults to --version
}

func (SbxProbe) Name() string   { return "sbx" }
func (SbxProbe) Required() bool { return true }
func (p SbxProbe) argv() []string {
	if len(p.Args) > 0 {
		return p.Args
	}
	return []string{"--version"}
}

// sbxVersionFallback names the ONE known alternate argv for a version probe:
// `sbx --version` (this host's default) and `sbx version` (the grammar a
// newer sbx CLI generation may require instead, having dropped the root
// `--version` flag). Any other argv (a test fixture mode, a future probe
// arg) has no known alternate, so Check never retries it — the fallback
// table is a fixed pair, not a loop over guesses.
func sbxVersionFallback(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	switch argv[0] {
	case "--version":
		return append([]string{"version"}, argv[1:]...)
	case "version":
		return append([]string{"--version"}, argv[1:]...)
	default:
		return nil
	}
}

func (p SbxProbe) Check(ctx context.Context) Result {
	bin := p.Bin
	if strings.TrimSpace(bin) == "" {
		bin = "sbx"
	}
	o := runBounded(ctx, bin, p.argv()...)
	// A bounded, ONE-shot fallback: retry with the known alternate grammar
	// ONLY when sbx's own output positively says it does not understand this
	// argv (sys.IsUsageMismatch). A denied, timed-out, missing-binary, or
	// generic non-zero exit never retries — those failures mean the same
	// thing under either grammar, so a second attempt would only obscure the
	// real cause.
	if o.failed && sys.IsUsageMismatch(o.out) {
		if alt := sbxVersionFallback(p.argv()); alt != nil {
			if o2 := runBounded(ctx, bin, alt...); !o2.failed && !o2.notFound && !o2.denied && !o2.timedOut {
				return sbxProbeResult(p.Name(), o2, true)
			}
		}
	}
	return sbxProbeResult(p.Name(), o, false)
}

// sbxProbeResult renders the classified outcome of ONE sbx version attempt.
// usedFallback is true only when the alternate grammar (see
// sbxVersionFallback) is the attempt actually being reported — the evidence
// then says so explicitly; the common, unchanged case keeps the exact
// literal wording doctor/status have always printed.
func sbxProbeResult(name string, o execOutcome, usedFallback bool) Result {
	switch {
	case o.notFound:
		return Result{Name: name, Status: StatusAbsent, Detail: "not installed", Fix: SbxInstallFix,
			Evidence: "sbx is not on PATH"}
	case o.denied:
		return Result{Name: name, Status: StatusDenied, Detail: "refused by policy", Fix: SbxInstallFix,
			Evidence: "sbx --version was refused"}
	case o.timedOut || o.failed:
		return unknownExec(name, o, "sbx --version")
	}
	v := versionish.FindString(o.out)
	if v == "" {
		return Result{Name: name, Status: StatusUnknown, Detail: "unrecognized version output",
			Evidence: "sbx --version printed no version"}
	}
	if usedFallback {
		return Result{Name: name, Status: StatusReady, Detail: v,
			Evidence: "sbx version = " + v + " (fell back from --version, which this sbx build rejected)"}
	}
	return Result{Name: name, Status: StatusReady, Detail: v, Evidence: "sbx --version = " + v}
}

// --- launchd ----------------------------------------------------------------

// notLoaded is what launchctl says when the label is genuinely not in the
// domain. It is the ONLY launchctl failure that proves the agent is unloaded;
// everything else (a permission error, a wedged launchd, a bad domain) is
// unknown.
var notLoaded = []string{"could not find service", "no such process", "not find service"}

// LaunchdProbe proves the pix LaunchAgent is loaded in the user's gui domain.
type LaunchdProbe struct {
	Bin   string
	Label string
	UID   int
	Args  []string // overrides the launchctl argv (tests point this at a fixture)
}

func (LaunchdProbe) Name() string   { return "launchd" }
func (LaunchdProbe) Required() bool { return false }

func (p LaunchdProbe) argv() []string {
	if len(p.Args) > 0 {
		return p.Args
	}
	return []string{"print", fmt.Sprintf("gui/%d/%s", p.UID, p.Label)}
}

func (p LaunchdProbe) Check(ctx context.Context) Result {
	bin := p.Bin
	if strings.TrimSpace(bin) == "" {
		bin = "launchctl"
	}
	o := runBounded(ctx, bin, p.argv()...)
	low := strings.ToLower(o.out)
	switch {
	case o.notFound:
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "launchctl not available",
			Evidence: "launchctl is not on PATH"}
	case o.denied:
		return Result{Name: p.Name(), Status: StatusDenied, Detail: "launchd refused the query",
			Fix: ServeInstallFix, Evidence: "launchctl print was refused"}
	case (o.failed || o.timedOut) && containsAny(low, notLoaded):
		return Result{Name: p.Name(), Status: StatusAbsent, Detail: "agent not loaded", Fix: ServeInstallFix,
			Evidence: "launchctl print: " + p.Label + " not in domain"}
	case o.timedOut || o.failed:
		return unknownExec(p.Name(), o, "launchctl print")
	}
	return Result{Name: p.Name(), Status: StatusReady, Detail: "agent loaded", Evidence: "launchctl print: " + p.Label}
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// --- memory unit ------------------------------------------------------------

// MemoryUnitProbe reports the memory unit as the SUPERVISOR sees it: it asks
// the unit's own identity method, so the answer carries the Suture unit state
// (running/backoff/failed, and the reason) rather than "something holds the
// port". A dial alone is not evidence — a surviving daemon from an older
// install answers a dial perfectly well.
type MemoryUnitProbe struct {
	Port    int
	Enabled bool // in the configured services set
}

func (MemoryUnitProbe) Name() string     { return "memory" }
func (p MemoryUnitProbe) Required() bool { return p.Enabled }

func (p MemoryUnitProbe) Check(ctx context.Context) Result {
	id, err := identityAt(ctx, p.Port)
	if err != nil {
		if refused(err) {
			return Result{Name: p.Name(), Status: StatusAbsent, Required: p.Enabled,
				Detail: fmt.Sprintf("unit down (:%d refused)", p.Port), Fix: ServeStartFix,
				Evidence: fmt.Sprintf("connection refused on :%d", p.Port)}
		}
		return Result{Name: p.Name(), Status: StatusUnknown, Required: p.Enabled,
			Detail: "unit did not answer", Evidence: fmt.Sprintf("identity on :%d: %s", p.Port, classifyNetErr(ctx, err))}
	}
	switch {
	case id.Name != rpc.MemoryName:
		return Result{Name: p.Name(), Status: StatusAbsent, Required: p.Enabled,
			Detail: fmt.Sprintf("port held by %q, not the memory unit", id.Name), Fix: ServeRestartFix,
			Evidence: fmt.Sprintf(":%d identity name = %q", p.Port, id.Name)}
	case !id.Ready:
		detail := "unit not ready"
		if id.DegradedReason != "" {
			detail = "unit not ready: " + id.DegradedReason
		}
		return Result{Name: p.Name(), Status: StatusAbsent, Required: p.Enabled, Detail: detail,
			Fix: ServeRestartFix, Evidence: fmt.Sprintf(":%d identity ready=false", p.Port)}
	}
	return Result{Name: p.Name(), Status: StatusReady, Required: p.Enabled,
		Detail: fmt.Sprintf("unit running (:%d)", p.Port), Evidence: fmt.Sprintf(":%d identity = %s", p.Port, id.Name)}
}

// identityAt makes the real identity JSON-RPC call, bounded by ctx. A body we
// cannot parse is an error, never a zero-valued "not ready": inventing a
// verdict out of an unreadable answer is the exact failure this model bans.
func identityAt(ctx context.Context, port int) (rpc.ServiceIdentity, error) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"identity","params":{}}`)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return rpc.ServiceIdentity{}, err
	}
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return rpc.ServiceIdentity{}, err
	}
	defer res.Body.Close()
	var parsed struct {
		Result struct {
			Name           string `json:"name"`
			Version        string `json:"version"`
			Port           int    `json:"port"`
			Ready          bool   `json:"ready"`
			DegradedReason string `json:"degraded_reason"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&parsed); err != nil {
		return rpc.ServiceIdentity{}, fmt.Errorf("unreadable identity payload: %w", err)
	}
	if parsed.Result.Name == "" {
		return rpc.ServiceIdentity{}, errors.New("identity payload named nothing")
	}
	return rpc.ServiceIdentity{Name: parsed.Result.Name, Version: parsed.Result.Version, Port: parsed.Result.Port,
		Ready: parsed.Result.Ready, DegradedReason: parsed.Result.DegradedReason}, nil
}

// refused reports a connection REFUSED — positive evidence that nothing is
// listening, as opposed to a timeout, which is evidence of nothing.
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

func classifyNetErr(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if strings.Contains(err.Error(), "unreadable identity payload") {
		return "unreadable answer"
	}
	return "no usable answer"
}

// --- monitor ----------------------------------------------------------------

// MonitorProbe proves the monitor's HTTP surface is alive. Liveness is the
// question here, so ANY HTTP answer below 500 counts (the monitor mux serves a
// narrow route set and 404s the rest); a 5xx is a verified gap, and a refused
// connection means it is simply not running.
type MonitorProbe struct {
	Port    int
	Enabled bool
}

func (MonitorProbe) Name() string     { return "monitor" }
func (p MonitorProbe) Required() bool { return p.Enabled }

func (p MonitorProbe) Check(ctx context.Context) Result {
	url := fmt.Sprintf("http://127.0.0.1:%d/", p.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{Name: p.Name(), Status: StatusUnknown, Required: p.Enabled, Detail: "could not build probe"}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		if refused(err) {
			return Result{Name: p.Name(), Status: StatusAbsent, Required: p.Enabled,
				Detail: fmt.Sprintf("not running (:%d refused)", p.Port), Fix: MonitorStartFix,
				Evidence: fmt.Sprintf("connection refused on :%d", p.Port)}
		}
		return Result{Name: p.Name(), Status: StatusUnknown, Required: p.Enabled, Detail: "no answer",
			Evidence: fmt.Sprintf(":%d: %s", p.Port, classifyNetErr(ctx, err))}
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	if res.StatusCode >= 500 {
		return Result{Name: p.Name(), Status: StatusAbsent, Required: p.Enabled,
			Detail: fmt.Sprintf("unhealthy (HTTP %d)", res.StatusCode), Fix: MonitorStartFix,
			Evidence: fmt.Sprintf(":%d answered %d", p.Port, res.StatusCode)}
	}
	return Result{Name: p.Name(), Status: StatusReady, Required: p.Enabled,
		Detail: fmt.Sprintf("serving (:%d)", p.Port), Evidence: fmt.Sprintf(":%d answered %d", p.Port, res.StatusCode)}
}

// --- pack -------------------------------------------------------------------

// PackProbe reports the active pack. A host with no pack is a perfectly good
// host, so this axis is optional — but "you configured a pack and it is not
// there" is a verified gap with an exact fix.
type PackProbe struct {
	Root string
}

func (PackProbe) Name() string   { return "pack" }
func (PackProbe) Required() bool { return false }

func (p PackProbe) Check(context.Context) Result {
	if strings.TrimSpace(p.Root) == "" {
		return Result{Name: p.Name(), Status: StatusAbsent, Detail: "no active pack", Fix: PackUseFix,
			Evidence: "no pack root configured"}
	}
	info, err := os.Stat(p.Root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Result{Name: p.Name(), Status: StatusAbsent, Detail: "active pack is missing from disk",
			Fix: PackUseFix, Evidence: p.Root + " does not exist"}
	case err != nil:
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "pack root unreadable",
			Evidence: p.Root + ": " + classifyStatErr(err)}
	case !info.IsDir():
		return Result{Name: p.Name(), Status: StatusAbsent, Detail: "active pack is not a directory",
			Fix: PackUseFix, Evidence: p.Root + " is not a directory"}
	}
	manifest := filepath.Join(p.Root, "pack.toml")
	if _, err := os.Stat(manifest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Name: p.Name(), Status: StatusAbsent, Detail: "active pack has no pack.toml",
				Fix: PackUseFix, Evidence: manifest + " does not exist"}
		}
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "pack manifest unreadable",
			Evidence: manifest + ": " + classifyStatErr(err)}
	}
	return Result{Name: p.Name(), Status: StatusReady, Detail: filepath.Base(p.Root),
		Evidence: "active pack at " + p.Root}
}

func classifyStatErr(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "permission denied"
	}
	return "unreadable"
}

// --- providers / model keys -------------------------------------------------

// ProviderKeyProbe answers "can this host call a model" from the key store,
// and it is TRI-STATE on purpose: only a store that ANSWERED and did not list
// the key is a no-key verdict. A store that failed, crashed or hung is
// unknown, so a transient `sbx secret ls` failure can never be mistaken for a
// missing key (and can never refuse a launch).
type ProviderKeyProbe struct {
	Bin  string
	Args []string
	Want []string // the key names to look for
	// AnyOf switches the verdict from "every name in Want" to "at least one".
	// It is what the MODEL keys need: pix launches with anthropic OR openai OR
	// google, so reporting the other two as gaps would print two repair
	// commands for a host that is already able to run.
	AnyOf bool
	// Label names what this probe is checking when a host runs more than one
	// of them (model keys vs infrastructure keys). Defaults to "providers".
	Label string
}

func (p ProviderKeyProbe) Name() string {
	if strings.TrimSpace(p.Label) != "" {
		return p.Label
	}
	return "providers"
}
func (ProviderKeyProbe) Required() bool { return true }

func (p ProviderKeyProbe) Check(ctx context.Context) Result {
	if len(p.Want) == 0 {
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "no provider keys declared",
			Evidence: "nothing to check"}
	}
	o := runBounded(ctx, p.Bin, p.Args...)
	switch {
	case o.notFound:
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "key store not available",
			Evidence: "the key-store command is not on PATH"}
	case o.denied:
		return Result{Name: p.Name(), Status: StatusDenied, Detail: "key store refused the query",
			Fix: fmt.Sprintf(SecretSetFix, strings.Join(p.Want, "|")), Evidence: "key listing was refused"}
	case o.timedOut || o.failed:
		return unknownExec(p.Name(), o, "key listing")
	}
	have := map[string]bool{}
	for _, field := range strings.Fields(strings.ReplaceAll(o.out, "=", " ")) {
		have[strings.Trim(field, "\"',:")] = true
	}
	var missing, present []string
	for _, w := range p.Want {
		if have[w] {
			present = append(present, w)
			continue
		}
		missing = append(missing, w)
	}
	if p.AnyOf {
		if len(present) > 0 {
			return Result{Name: p.Name(), Status: StatusReady, Detail: strings.Join(present, ", "),
				Evidence: "key store lists " + strings.Join(present, ", ")}
		}
		return Result{Name: p.Name(), Status: StatusAbsent,
			Detail: "none of " + strings.Join(p.Want, ", ") + " is set", Fix: ModelKeyFix,
			Evidence: "key store answered without " + strings.Join(p.Want, ", ")}
	}
	if len(missing) == 0 {
		return Result{Name: p.Name(), Status: StatusReady, Detail: fmt.Sprintf("%d key(s) wired", len(p.Want)),
			Evidence: "key store lists " + strings.Join(p.Want, ", ")}
	}
	return Result{Name: p.Name(), Status: StatusAbsent, Detail: "missing " + strings.Join(missing, ", "),
		Fix: fmt.Sprintf(SecretSetFix, missing[0]), Evidence: "key store answered without " + strings.Join(missing, ", ")}
}

// ProbeBudget is the per-probe budget a command should use when it wants a
// snappy answer (status) rather than a thorough one (doctor).
const (
	StatusBudget = 2 * time.Second
	DoctorBudget = 8 * time.Second
)
