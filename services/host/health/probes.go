package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	SbxInstallFix = "brew install docker/tap/sbx"
	SecretSetFix  = "pix secret set %s op://vault/item/field"
	// ModelKeyFix repairs the ANY-OF gap: pix launches a model with one
	// provider key, and `pix setup` is the one place that key interview still
	// runs (the standalone `pix models add` verb was cut from the v2 surface).
	ModelKeyFix = "pix setup"
	// SbxUpgradeFix repairs a too-old or unparsable sbx version — a different
	// problem from a missing binary (SbxInstallFix), so it gets its own exact
	// command rather than reusing that one.
	SbxUpgradeFix = "brew upgrade docker/tap/sbx"
)

// SbxMinVersion is the lowest sbx release native environments require (PRD
// docs/design/environments.md section 4, section 5.6; AC-20). It is a
// package const, read by SbxVersionGate and SbxVersionGateMessage, so a
// future bump changes exactly one line.

const SbxMinVersion = "0.39.0"

// sbxVersionNumber matches the dotted numeric run at the heart of a real sbx
// version string: an optional "v" prefix (the observed `sbx version: v0.39.0
// <hash>` fallback banner, docs/upstream/sbx-0.39-environments.md), then two
// or more dot-separated integer components. Component COUNT is deliberately
// permissive — "0.39" (partial) and "0.39.0.1" (an extra trailing
// component) both parse whole, because sbx's dotted-version grammar has
// never been pinned to exactly three components anywhere this repo has
// observed it — but WHERE it may appear is not: see sbxVersionLabeled and
// sbxVersionOnly below, the only two contexts this file trusts as an actual
// version answer rather than an arbitrary dotted number occurring elsewhere
// in chattier output (a Go build banner, a commit hash).
const sbxVersionNumber = `v?([0-9]+(?:\.[0-9]+)+)`

// sbxVersionSuffix matches a prerelease/build tag glued directly onto the
// version number with no intervening space — "-rc1", ".beta2", "rc1" — so
// "sbx version 0.39.0-rc1 (unstable)" reads the tag as part of the version
// rather than losing it to a bare digit-run match. It must start with a
// letter: a fourth numeric component ("0.39.0.1") is not a suffix, it is
// more of the version number itself — sbxVersionNumber already consumed it.
const sbxVersionSuffix = `([-.]?[A-Za-z][A-Za-z0-9.]*)?`

// sbxVersionOnly recognizes the one unlabeled shape this parser trusts: the
// ENTIRE trimmed output is a version and nothing else, for a build whose
// exact `--version`/`version` grammar prints the bare number with no banner
// at all. Anything else unlabeled — a bare number buried in chattier text,
// a Go build banner's "go1.21.5" — is deliberately not trusted: guessing
// which number in multi-line noise is the real one is exactly the "first
// dotted numeric substring" bug this parser replaces.
var sbxVersionOnly = regexp.MustCompile(`^` + sbxVersionNumber + sbxVersionSuffix + `$`)

// sbxVersionLabeled recognizes a version explicitly INTRODUCED by the word
// "version" — optionally scoped by an immediately preceding "sbx" (the real
// `sbx version: v0.39.0 <hash>` banner and the fixtures' `sbx version
// 0.39.0`), optionally followed by a colon (the real banner's exact
// spelling), then the version number and its optional suffix. Capture group
// 1 is the "sbx " scope (empty when absent), group 2 the numeric version,
// group 3 the suffix.
var sbxVersionLabeled = regexp.MustCompile(`(?i)(sbx\s+)?version:?\s+` + sbxVersionNumber + sbxVersionSuffix)

// sbxVersionMatch is one recognized version answer: the numeric part alone
// (used for the min-version compare), the exact text as seen including any
// suffix (used for Detail/evidence so a prerelease tag is never silently
// dropped), and whether that suffix means the release is not "explicitly
// known stable".
type sbxVersionMatch struct {
	number     string
	raw        string
	prerelease bool
}

func newSbxVersionMatch(number, suffix string) sbxVersionMatch {
	return sbxVersionMatch{number: number, raw: number + suffix, prerelease: suffix != ""}
}

// parseSbxVersion is the one honest reading of an sbx version probe's raw
// output: a version is trusted ONLY when it is either the entire (trimmed)
// output on its own, or explicitly introduced by the word "version" — never
// a dotted number picked out of chattier text on the theory that it looked
// close enough. That is the fix for the low finding this replaces: a bare
// "first dotted numeric substring" scan reads "built with go 1.21.5 ...
// sbx version 0.38.2" as "1.21.5" — comfortably past SbxMinVersion — and
// fails OPEN on a too-old sbx, exactly backwards from this package's
// fail-closed model.
//
// When the labeled form finds more than one candidate and they disagree, or
// an unlabeled scan (no "sbx" scope present at all) turns up more than one,
// that is ambiguous chatter, not a version: it fails closed exactly like
// output with no version at all, because guessing which of several numbers
// is the real one is the same failure mode with extra steps. An explicit
// "sbx version" match always wins over a bare "version" match elsewhere in
// the same output — a Go build banner's "go version go1.21.5" never carries
// the "sbx" scope, so it never even competes.
//
// A version whose text carries anything after the dotted number itself (a
// "-rc1"/"beta2"/etc. build tag with no space before it) is reported with
// prerelease=true: the fail-closed prerelease policy trusts as "explicitly
// known stable" only a bare release number, never a tagged one, regardless
// of what the tag itself says.
func parseSbxVersion(out string) (sbxVersionMatch, bool) {
	if trimmed := strings.TrimSpace(out); trimmed != "" {
		if m := sbxVersionOnly.FindStringSubmatch(trimmed); m != nil {
			return newSbxVersionMatch(m[1], m[2]), true
		}
	}
	all := sbxVersionLabeled.FindAllStringSubmatch(out, -1)
	if len(all) == 0 {
		return sbxVersionMatch{}, false
	}
	var scoped, unscoped [][]string
	for _, m := range all {
		if strings.TrimSpace(m[1]) != "" {
			scoped = append(scoped, m)
		} else {
			unscoped = append(unscoped, m)
		}
	}
	candidates := scoped
	if len(candidates) == 0 {
		candidates = unscoped
	}
	first := candidates[0]
	for _, m := range candidates[1:] {
		if m[2] != first[2] || m[3] != first[3] {
			return sbxVersionMatch{}, false
		}
	}
	return newSbxVersionMatch(first[2], first[3]), true
}

// sbxUnparsableDetail is SbxProbe's own Detail wording for a version reply
// that ran to completion but carried no recognizable version at all. It is a
// package const, not a literal repeated in two places, because
// SbxVersionGate matches against this EXACT string to decide the same
// question independently of SbxProbe's own (unchanged) Unknown
// classification for this case.
const sbxUnparsableDetail = "unrecognized version output"

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
	// WaitDelay makes the budget REAL. Without it, killing the process still
	// blocks until every descendant holding our stdout/stderr pipe exits, so a
	// probe could outlive its deadline indefinitely — and this package now runs
	// probes through `op run -- <cmd>`, which always has such a descendant. Same
	// value and same reason as sys.Real's runner.
	cmd.WaitDelay = 2 * time.Second
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
	match, ok := parseSbxVersion(o.out)
	if !ok {
		return Result{Name: name, Status: StatusUnknown, Detail: sbxUnparsableDetail,
			Evidence: "sbx --version printed no version"}
	}
	evidence := "sbx --version = " + match.raw
	if usedFallback {
		evidence = "sbx version = " + match.raw + " (fell back from --version, which this sbx build rejected)"
	}
	// A prerelease/build-tagged answer is never "explicitly known stable":
	// fail closed on it exactly like a too-old version, regardless of what its
	// numeric part alone would say, and regardless of what the tag itself
	// reads as.
	if match.prerelease {
		return Result{Name: name, Status: StatusAbsent, Detail: match.raw, Fix: SbxUpgradeFix,
			Evidence: evidence + "; a prerelease/build-tagged version is not treated as a stable release"}
	}
	// Native environments require SbxMinVersion or later (PRD section 4/5.6,
	// AC-20): a version that answered and parsed cleanly, but is too old, is a
	// VERIFIED gap with its own exact fix — distinct from SbxInstallFix, which
	// repairs a missing binary, not an old one.
	if !sbxVersionAtLeast(match.number, SbxMinVersion) {
		return Result{Name: name, Status: StatusAbsent, Detail: match.number, Fix: SbxUpgradeFix,
			Evidence: evidence + fmt.Sprintf("; native environments require %s or later", SbxMinVersion)}
	}
	return Result{Name: name, Status: StatusReady, Detail: match.number, Evidence: evidence}
}

// sbxVersionAtLeast reports whether v (a dotted version SbxProbe already
// parsed, e.g. "0.39.0") is at least min, comparing NUMERICALLY component by
// component rather than lexicographically — "0.40.1" must read as newer than
// "0.38.2", and a lexical compare would also get "0.9" vs "0.10" backwards.
// A component that fails to parse as a number reads as 0: an ambiguous
// component must never be read as "obviously satisfies", because this feeds
// a fail-closed gate.
func sbxVersionAtLeast(v, min string) bool {
	vp, mp := sbxVersionParts(v), sbxVersionParts(min)
	for i := 0; i < len(vp) || i < len(mp); i++ {
		var a, b int
		if i < len(vp) {
			a = vp[i]
		}
		if i < len(mp) {
			b = mp[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// ValidateSbxVersionOutput validates the banner returned by `sbx version` for
// callers that already own process execution, such as `pix setup`'s
// mutation-before-preflight gate. It shares the exact parser and minimum with
// SbxProbe so setup, run, and doctor cannot disagree about a host release.
func ValidateSbxVersionOutput(out string) error {
	match, ok := parseSbxVersion(out)
	if !ok {
		return fmt.Errorf("unrecognized sbx version output")
	}
	if match.prerelease {
		return fmt.Errorf("sbx %s is a prerelease/build-tagged version; stable %s or later is required", match.raw, SbxMinVersion)
	}
	if !sbxVersionAtLeast(match.number, SbxMinVersion) {
		return fmt.Errorf("sbx %s is too old; %s or later is required", match.number, SbxMinVersion)
	}
	return nil
}

func sbxVersionParts(v string) []int {
	fields := strings.Split(v, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}

// SbxVersionGate answers the fail-closed check native environments require on
// top of SbxProbe's own classification (PRD docs/design/environments.md
// section 5.6, AC-20): sbx must be SbxMinVersion or later, and a version that
// could not be read AT ALL is refused exactly like a too-old one. It reads an
// ALREADY-COMPUTED SbxProbe Result rather than probing again, so a caller
// that already ran the probe (doctor's Snapshot) pays for exactly one exec,
// and `pix run`'s own gate call is the only other one.
//
// It fires ONLY on a POSITIVE read: sbx missing, refused by policy, timed
// out, or failed for a reason SbxProbe could not interpret is a DIFFERENT,
// already honest gap (SbxInstallFix is its remedy) — turning "could not
// check" into a version refusal would be exactly the dishonesty this
// package's model (see health.go's package doc) exists to prevent.
func SbxVersionGate(r Result) (blocked bool, found string) {
	switch {
	case r.Status == StatusReady:
		return false, r.Detail
	case r.Status == StatusAbsent && r.Fix == SbxUpgradeFix:
		return true, r.Detail
	case r.Status == StatusUnknown && r.Detail == sbxUnparsableDetail:
		return true, "unknown (sbx --version was not understood)"
	default:
		return false, ""
	}
}

// SbxVersionGateMessage renders the exact PRD section 5.6 copy, byte for
// byte, for a version SbxVersionGate has already ruled blocked. Both `pix
// run` and `pix doctor` call this ONE function so the two surfaces can never
// drift onto slightly different wording for the same requirement.
func SbxVersionGateMessage(found string) string {
	return fmt.Sprintf("pix: native environments require sbx %s or later.\n     found: %s\n     upgrade it: %s\n",
		SbxMinVersion, found, SbxUpgradeFix)
}

// --- providers / model keys -------------------------------------------------

// ProviderKeyProbe answers "does this host HAVE a model key" from the key
// store. Note what that is not: a key in the store is not proof the router can
// call that vendor, because routing resolves over probed BINDINGS (`pix models
// add <provider>`), not over the key store. A host with all three keys but only
// Anthropic bound reads as three ready providers while every intent that
// prefers OpenAI silently falls back, which is exactly how the default session
// model became a vendor nobody chose. Set Callable to close that gap in the
// report.
//
// It is TRI-STATE on purpose: only a store that ANSWERED and did not list
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
	// Callable is the subset of Want the ROUTER can actually reach: providers
	// with a probed, callable binding. A key present here but absent from
	// Callable is reported as wired-but-unrouted, because that state is invisible
	// otherwise and produces surprising model choices.
	//
	// nil means "not computed" and preserves the original key-only report, so a
	// caller that cannot cheaply answer callability (or is checking
	// infrastructure keys, which route nowhere by definition) is unaffected.
	Callable []string
	// Keyless names the configured inference that reaches a model WITHOUT any
	// provider key (a pack's sbx-session backends, whose credential the sbx
	// proxy injects inside the sandbox and which by design cannot be replayed
	// from the host). Set means ready without asking the key store: here "no
	// anthropic key" is not a gap, and `pix models add anthropic` would add a
	// key nothing reads. It is the carve-out `pix run`'s launch gate already
	// applies (inference.ConfiguredKeylessInference) — a host told it is broken
	// by one verb and started by the other teaches you to ignore both.
	//
	// Distinct from Callable, which answers "this key routes nowhere". Keyless
	// answers "no key was ever the credential", so it is decided before the
	// store is consulted rather than against what the store returned.
	//
	// Resolve WINS over the static value and is called fresh on every Check,
	// same seam and reason as PackProbe.Resolve: a probe built before setup
	// adopts a pack would otherwise grade the post-adoption host on the
	// pre-adoption answer.
	Keyless        string
	ResolveKeyless func() string
}

func (p ProviderKeyProbe) Name() string {
	if strings.TrimSpace(p.Label) != "" {
		return p.Label
	}
	return "providers"
}
func (ProviderKeyProbe) Required() bool { return true }

func (p ProviderKeyProbe) Check(ctx context.Context) Result {
	// Answered from config, before any exec: a key store that lists nothing is
	// not evidence of a gap on a host whose models need no key.
	keyless := p.Keyless
	if p.ResolveKeyless != nil {
		keyless = p.ResolveKeyless()
	}
	if k := strings.TrimSpace(keyless); k != "" {
		return Result{Name: p.Name(), Status: StatusReady, Detail: "no provider key needed",
			Evidence: "configured inference reaches a model without one: " + k}
	}
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
			detail, evidence := strings.Join(present, ", "), "key store lists "+strings.Join(present, ", ")
			if unrouted := p.unrouted(present); len(unrouted) > 0 {
				// Still READY: one callable provider is all a launch needs, and an
				// unrouted key is a smaller roster, not a fault. But say it, because
				// the alternative is a user watching roles resolve to models they did
				// not pick with every line on this screen green.
				// Final findings: `pix models add` is a removed v1 verb (exit 2 in
				// v2). ModelKeyFix ("pix setup") is the ONE real place a provider
				// binding is (re)configured now, so it is the one named here too.
				detail += fmt.Sprintf(" (%s: key set, no model wired \u2014 `%s`)",
					strings.Join(unrouted, ", "), ModelKeyFix)
				evidence += "; no callable binding for " + strings.Join(unrouted, ", ")
			}
			return Result{Name: p.Name(), Status: StatusReady, Detail: detail, Evidence: evidence}
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

// unrouted returns the providers whose key is present but which the router has
// no callable binding for, preserving Want's order. A nil Callable means the
// caller did not compute callability, which reports nothing rather than
// reporting everything as unrouted.
func (p ProviderKeyProbe) unrouted(present []string) []string {
	if p.Callable == nil {
		return nil
	}
	callable := make(map[string]bool, len(p.Callable))
	for _, c := range p.Callable {
		callable[c] = true
	}
	var out []string
	for _, name := range present {
		if !callable[name] {
			out = append(out, name)
		}
	}
	return out
}
