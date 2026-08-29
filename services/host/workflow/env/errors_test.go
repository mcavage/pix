// errors_test.go — E1.13's enumerating gate (AC-56/57/67; F15/F16): every
// typed refusal this package can return is constructed here and its
// rendered text is checked against the family's error/copy contract
// (docs/design/environments.md §8.1, prd.md §5, units.json E1.13):
//
//   - three-part form: a "pix: "-prefixed failure statement naming the
//     concrete thing that failed, a grounded fact (the known names, the
//     changed key, the holder), and a bounded number of runnable next
//     commands — exactly one, UNLESS the refusal is one of the family's
//     documented, ALREADY-SHIPPED genuine disambiguations (each cited by
//     its own AC/D below): the zero-path `add` cwd-collision (D10, AC-47,
//     two commands), `edit`'s unrecognized-target refusal (D4, AC-49, two
//     commands), and the `pix env rm` pointer error itself (PRD §5.5,
//     three commands, tested at the cmd/pix dispatch layer since the
//     pointer error is cmd-level, not workflow/env);
//   - suggestion is data, never a question: no "did you mean", no
//     yes/no that selects or corrects a NAME (D14, AC-57);
//   - no output ever names `sbx env rm` (AC-43);
//   - no banned filler ("leverage", "utilize", "seamless") or unearned
//     success verdict ("configured", "enabled", "ready", "verified")
//     inside a REFUSAL, and no em dash anywhere (AC-67).
//
// Three types this package can construct — ContainmentError, SymlinkError,
// NoncanonicalRootError's root-canonicalization guard — are exercised here
// too even though none is reachable through any wired `pix env` verb
// today: pre-E2 launch composition never supplies a non-nil EffectiveMounts
// (so RefuseContainment's writable-workspace loop never runs), and a
// noncanonical registered root cannot survive config.Load's own sanitizing
// pass (dropNoncanonicalEnvironments) — only a hand-built *config.Config,
// exactly what this file constructs, can still reach it. They still speak
// the family's language when constructed directly, which is what an
// enumerating gate is for.
package env

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

// ── the family's copy contract, applied to every message this file renders ──

// bannedFillerWordsRE / bannedSuccessWordsRE are PRD §5 rule 4's exact word
// lists, word-boundary and case-insensitive matched ("ready" must not fire
// on "already").
var bannedFillerWordsRE = regexp.MustCompile(`(?i)\b(leverage|utilize|seamless)\b`)
var bannedSuccessWordsRE = regexp.MustCompile(`(?i)\b(configured|enabled|ready|verified)\b`)

// trustConfirmation is the ONE yes/no this family may ever render (D14/
// AC-57's carve-out): review.go's gate prompt, never a name-selection or
// name-correction question.
const trustConfirmation = "Accept this host-execution footprint? [y/N]:"

// assertFamilyCopy is PRD §5 rule 4 plus AC-43/57, applied to every
// refusal string this file constructs. It is deliberately substring-based,
// not a generic AST scan (that lives in cmd/pix/env_copy_lint_test.go,
// which scans the SOURCE literals directly): this file proves the
// RENDERED, interpolated text a real caller actually sees never regresses,
// which a source scan alone cannot, since a source literal is clean before
// a runtime substitution and a bug could still be introduced by what gets
// interpolated into it.
// familyCopyViolations is the pure form of the family's copy contract: it
// returns one description string per violation found in msg, never calling
// into *testing.T at all. assertFamilyCopy (below) is a thin t.Errorf
// wrapper over this; a planted-violation self-test
// (TestAssertFamilyCopy_SelfTest) calls this directly so it can assert
// "the scanner found exactly one finding" without a failing subtest
// propagating Fail() up to a meta-test that is supposed to pass.
func familyCopyViolations(msg string) []string {
	var findings []string
	if strings.Contains(msg, "\u2014") {
		findings = append(findings, fmt.Sprintf("contains an em dash: %q", msg))
	}
	if m := bannedFillerWordsRE.FindString(msg); m != "" {
		findings = append(findings, fmt.Sprintf("contains banned filler word %q: %q", m, msg))
	}
	if m := bannedSuccessWordsRE.FindString(msg); m != "" {
		findings = append(findings, fmt.Sprintf("a refusal must never claim an unearned success verdict %q: %q", m, msg))
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "sbx env rm") {
		findings = append(findings, fmt.Sprintf("suggests `sbx env rm`, which does not exist: %q", msg))
	}
	if strings.Contains(lower, "did you mean") {
		findings = append(findings, fmt.Sprintf("offers a name correction as a question rather than data: %q", msg))
	}
	// D14/AC-57: the only yes/no this family may ever render is the trust
	// confirmation itself; any OTHER bracketed yes/no is a name-selection or
	// name-correction question this family must never ask.
	if idx := strings.Index(lower, "[y/n]"); idx != -1 && !strings.Contains(msg, trustConfirmation) {
		findings = append(findings, fmt.Sprintf("carries a yes/no prompt other than the trust confirmation: %q", msg))
	}
	return findings
}

func assertFamilyCopy(t *testing.T, label, msg string) {
	t.Helper()
	for _, f := range familyCopyViolations(msg) {
		t.Errorf("%s: %s", label, f)
	}
}

// TestAssertFamilyCopy_SelfTest is finding A2's planted-violation proof for
// the RENDERED-text scanner: each of familyCopyViolations' six classes,
// exercised with a minimal string that should trip exactly that one class
// and no other. A scanner nobody has seen catch anything is not a scanner.
func TestAssertFamilyCopy_SelfTest(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"em dash", "pix: bad \u2014 thing"},
		{"filler word", "please leverage this path"},
		{"unearned success word", "the environment is ready"},
		{"sbx env rm", "run sbx env rm work"},
		{"did you mean", "did you mean home?"},
		{"stray yes/no", "rename it? [y/n]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := familyCopyViolations(c.msg)
			if len(got) != 1 {
				t.Errorf("familyCopyViolations(%q) found %d violation(s), want exactly 1: %v", c.msg, len(got), got)
			}
		})
	}
	// Negative control: clean, PRD-shaped copy trips nothing.
	if got := familyCopyViolations(`pix: environment "work" is the current default.
     default: work
     pick a different default first: pix env use <name>`); len(got) != 0 {
		t.Errorf("familyCopyViolations on clean copy found %v, want none", got)
	}
}

// commandOccurrences counts lines (after the first, when the message is
// multi-line) that name a runnable command — "pix ..." or "rm -rf ...".
// The FIRST line is always this family's failure statement, and several
// failure statements legitimately mention a command in backtick-quoted
// prose while explaining what was ambiguous (CwdHasSbxenvError's headline
// is the sharpest example): counting it would double-count the exact
// disambiguation those types exist to present cleanly on their own
// dedicated lines. A single-line message (no failure/command split at
// all) is counted whole, since there is no separate line to exclude.
func commandOccurrences(msg string) int {
	lines := strings.Split(msg, "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "pix ") || strings.Contains(l, "rm -rf ") {
			n++
		}
	}
	return n
}

// assertRefusal is the per-case gate: err must be a cli.UsageError (exit
// 2, docs/design/environments.md §8.1's "2 for a usage error or a
// refusal"), its rendered text (normalized exactly as cmd/pix/env_cmd.go's
// envRun normalizes it) must pass the family's copy contract, must name
// every string in wantContains, and must carry exactly wantCmds runnable
// command lines.
func assertRefusal(t *testing.T, label string, err error, wantCmds int, wantContains ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: err = nil, want a refusal", label)
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("%s: cli.ExitCode(err) = %d, want 2 (a refusal, not an operational failure): %v", label, got, err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "pix: ") {
		msg = "pix: " + msg // envRun's own de-duplication rule
	}
	assertFamilyCopy(t, label, msg)
	for _, want := range wantContains {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: message = %q, want it to contain %q", label, msg, want)
		}
	}
	if got := commandOccurrences(msg); got != wantCmds {
		t.Errorf("%s: found %d runnable command line(s), want exactly %d:\n%s", label, got, wantCmds, msg)
	}
}

func envWithNames(names ...string) *config.Config {
	cfg := &config.Config{Environments: map[string]string{}}
	for _, n := range names {
		cfg.Environments[n] = "/env/" + n
	}
	return cfg
}

// ── ls.go / resolve.go / load.go: unknown name, empty registry ──────────

func TestErrorFamily_UnknownEnvironment(t *testing.T) {
	cfg := envWithNames("home", "work")
	_, err := ResolveEnvironment(cfg, "hoem", nil)
	assertRefusal(t, "unknown name, non-empty registry", err, 1,
		`no environment named "hoem"`, "known: home, work", "register one: pix env add <name> [path]")

	_, err = ResolveEnvironment(&config.Config{}, "hoem", nil)
	assertRefusal(t, "unknown name, empty registry", err, 1,
		`no environment named "hoem"`, "known: none")
}

// ── show.go: no selection, --path ────────────────────────────────────────

func TestErrorFamily_NoSelectionForPath(t *testing.T) {
	err := NoSelectionForPathError(envWithNames("home", "work"))
	assertRefusal(t, "no selection, registered names exist", err, 1,
		"no environment selected", "known: home, work", "select one: pix env show <name> --path")

	err = NoSelectionForPathError(&config.Config{})
	assertRefusal(t, "no selection, nothing registered", err, 1,
		"no environment selected", "known: none (built-in defaults)")
}

// ── load.go: missing required file ───────────────────────────────────────

func TestErrorFamily_MissingRequiredFile(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir() // no .sbxenv.yaml written
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfg, nil, "home", nil, nil)
	assertRefusal(t, "missing .sbxenv.yaml", err, 1,
		`environment "home" has no required .sbxenv.yaml`, "missing:", "create it: pix env edit home sbxenv")
}

// ── resolve.go: noncanonical root, containment, symlinked root ──────────
//
// Neither is reachable through a wired `pix env` verb today (see this
// file's package doc comment); both are constructed directly.

func TestErrorFamily_NoncanonicalRoot(t *testing.T) {
	cfg := &config.Config{Environments: map[string]string{"home": "relative/env"}}
	_, err := ResolveEnvironment(cfg, "home", nil)
	assertRefusal(t, "noncanonical registered root", err, 1,
		`environment "home"`, "not a canonical absolute path", "root: relative/env", "re-register it: pix env add home <path>")
}

func TestErrorFamily_Containment(t *testing.T) {
	err := RefuseContainment("/ws/env", []string{"/ws"})
	assertRefusal(t, "root resolves inside a writable workspace", err, 1,
		"resolves inside writable workspace", "workspace: /ws", "register a root outside it: pix env add <name> <path>")
}

func TestErrorFamily_SymlinkedRoot(t *testing.T) {
	// SymlinkError carries no environment NAME (Kind varies across a root,
	// an MCP command, a kit path, a host service command, and an edit
	// target — see resolve.go's own doc comment), so there is no single
	// `pix env <verb> NAME` fix this type can name honestly; the concrete
	// fix is always a filesystem edit outside pix entirely. It is still
	// held to the copy contract and to naming the exact offending path —
	// grounded context — but is exempt from the command-count assertion
	// the rest of this table applies.
	err := &SymlinkError{Kind: "environment root", Path: "/ws/link"}
	msg := err.Error()
	assertFamilyCopy(t, "symlinked root", msg)
	if !strings.Contains(msg, "environment root") || !strings.Contains(msg, "/ws/link") {
		t.Errorf("SymlinkError.Error() = %q, want it to name the kind and the exact path", msg)
	}
}

// ── add.go: zero-path cwd collision (two commands, D10/AC-47), scaffold collision ──

func TestErrorFamily_CwdHasSbxenv(t *testing.T) {
	err := cli.UsageError{Err: &CwdHasSbxenvError{Name: "home", Cwd: "/work/home-env"}}
	assertRefusal(t, "zero-path add, cwd already has .sbxenv.yaml", err, 2,
		"already has a .sbxenv.yaml", "Pick one explicitly",
		"register this directory: pix env add home /work/home-env",
		"scaffold a new one:      cd elsewhere && pix env add home")
}

func TestErrorFamily_ScaffoldCollision(t *testing.T) {
	err := cli.UsageError{Err: &ScaffoldCollisionError{Root: "/data/envs/home"}}
	assertRefusal(t, "scaffold target already exists", err, 1,
		"/data/envs/home already exists", "pix env add <name> /data/envs/home")
}

// ── use.go: unreviewed / changed since review ────────────────────────────

func TestErrorFamily_UseNotReviewed(t *testing.T) {
	err := &UseNotReviewedError{Name: "work"}
	assertRefusal(t, "never reviewed", cli.UsageError{Err: err}, 1,
		`environment "work" has not been reviewed`, "review it: pix env review work")

	err2 := &UseNotReviewedError{Name: "work", Changed: true}
	assertRefusal(t, "changed since review", cli.UsageError{Err: err2}, 1,
		`environment "work" changed what it runs on your host`, "review it: pix env review work")
}

// ── review.go / commit.go: the two Wave C concurrency refusals ──────────

func TestErrorFamily_ReviewChangedDuringPrompt(t *testing.T) {
	err := &ReviewChangedDuringPromptError{Name: "work"}
	assertRefusal(t, "footprint changed during the prompt", cli.UsageError{Err: err}, 1,
		`environment "work" changed its host-execution footprint while the review prompt was open`,
		"recorded: nothing",
		"read the current bill: pix env review work")
	// H1's contract is EXACTLY one runnable `pix env review work`, so a
	// user can never pick the wrong one of two.
	if got := strings.Count(err.Error(), "pix env review work"); got != 1 {
		t.Errorf("message must name `pix env review work` exactly once, got %d", got)
	}
}

func TestErrorFamily_ConcurrentRegistration(t *testing.T) {
	repointed := &ConcurrentRegistrationError{Name: "work", Existing: "/env/theirs", Attempted: "/env/yours"}
	assertRefusal(t, "concurrent repoint mid-add", cli.UsageError{Err: repointed}, 1,
		`environment "work" now points at a different root`,
		"theirs: /env/theirs",
		"yours:  /env/yours",
		"re-run to repoint it deliberately: pix env add work /env/yours")

	forgotten := &ConcurrentRegistrationError{Name: "work", Attempted: "/env/yours"}
	assertRefusal(t, "concurrent forget mid-add", cli.UsageError{Err: forgotten}, 1,
		`environment "work" was unregistered by another process`,
		"yours: /env/yours",
		"re-run to register it deliberately: pix env add work /env/yours")
}

// ── forget.go: current default, live holder (both branches) ─────────────

func TestErrorFamily_ForgetCurrentDefault(t *testing.T) {
	err := &ForgetCurrentDefaultError{Name: "home"}
	assertRefusal(t, "forget the current default", cli.UsageError{Err: err}, 1,
		`environment "home" is the current default`, "default: home", "pick a different default first: pix env use <name>")
}

func TestErrorFamily_ForgetLiveHolder(t *testing.T) {
	held := &ForgetLiveHolderError{Name: "work"}
	assertRefusal(t, "forget while a sandbox holds it", cli.UsageError{Err: held}, 1,
		`environment "work" is still held by a live sandbox`, "holder: a live sandbox", "remove the sandbox first: pix rm <sandbox>")

	unknown := &ForgetLiveHolderError{Name: "work", Unknown: true}
	assertRefusal(t, "forget with an inconclusive holder probe", cli.UsageError{Err: unknown}, 1,
		"could not confirm no live sandbox still references", "probe: inconclusive", "retry: pix env forget work")
}

// TestErrorFamily_ForgetLiveHolder_ViaForget proves the SAME two shapes are
// what Forget itself returns for a caller-supplied probe — HolderProbe is
// unreachable through cmd/pix's `env forget` today (envForgetCmd.Run always
// passes nil, which defaults to NoLiveHolders: see forget.go's own doc
// comment on Forget's probe parameter), so this is exercised directly
// against the workflow function, not through dispatch.
func TestErrorFamily_ForgetLiveHolder_ViaForget(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil { // Forget commits against the live file
		t.Fatal(err)
	}

	_, err := Forget(cfg, "work", func(string) (bool, error) { return true, nil })
	assertRefusal(t, "Forget with a held probe", err, 1,
		`environment "work" is still held by a live sandbox`, "pix rm <sandbox>")

	_, err = Forget(cfg, "work", func(string) (bool, error) { return false, errors.New("probe unavailable") })
	assertRefusal(t, "Forget with an erroring probe", err, 1,
		"could not confirm no live sandbox", "retry: pix env forget work")
}

// ── edit.go: the exact positional enum, all three refusal call sites ────

func TestErrorFamily_EditTarget(t *testing.T) {
	_, err := resolveTarget("work", "", EditOptions{TTY: false})
	assertRefusal(t, "no token, no TTY", err, 2,
		"needs a target file; no TTY to ask interactively",
		"pix env edit work pix       edit pix.toml",
		"pix env edit work sbxenv    edit .sbxenv.yaml")

	_, err = resolveTarget("work", "yaml", EditOptions{})
	assertRefusal(t, "unrecognized explicit token", err, 2,
		`unknown target "yaml"`,
		"pix env edit work pix       edit pix.toml",
		"pix env edit work sbxenv    edit .sbxenv.yaml")

	_, err = promptForTarget("work", EditOptions{Out: discardWriter{}, In: strings.NewReader("nope\n")})
	assertRefusal(t, "TTY prompt, unrecognized answer", err, 2,
		`"nope" is not pix or sbxenv`,
		"pix env edit work pix       edit pix.toml",
		"pix env edit work sbxenv    edit .sbxenv.yaml")
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// countExactLine returns how many lines of text, trimmed, equal want
// exactly — the precise tool a bill's own "full argv and content digests:
// pix env review NAME --verbose" tip line needs to be told apart from the
// bare re-run command "pix env review NAME" a refusal separately prints:
// both are real, different, legitimate commands, and a bare substring
// count would double-count one as an occurrence of the other.
func countExactLine(text, want string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == want {
			n++
		}
	}
	return n
}

// ── review.go: the gate's three refusal shapes, output + error combined ──
//
// The gate's runnable command is PRINTED to stdout alongside the bill (the
// SAME thing every other `pix env` refusal's Error() string carries
// inline) — see review.go's gate doc comment — so these three assertions
// look at out+err together, exactly as a real terminal session would show
// them one after another.

func TestErrorFamily_ReviewGate(t *testing.T) {
	newFixture := func(t *testing.T) *config.Config {
		tempConfigAndState(t)
		cfg := loadConfig(t)
		root := t.TempDir()
		copyFixture(t, "testdata/hostexec-fixture", root)
		if _, err := Register(cfg, "work", root); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("non-TTY without --yes", func(t *testing.T) {
		cfg := newFixture(t)
		var out strings.Builder
		_, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &out, TTY: false})
		if err == nil {
			t.Fatal("Review must refuse")
		}
		if got := cli.ExitCode(err); got != 2 {
			t.Errorf("cli.ExitCode(err) = %d, want 2", got)
		}
		combined := out.String() + "\n" + err.Error()
		assertFamilyCopy(t, "review non-TTY refusal", combined)
		if !strings.Contains(combined, trustConfirmation) {
			t.Errorf("combined output = %q, want the rendered bill", combined)
		}
		if got := countExactLine(combined, "pix env review work --yes"); got != 1 {
			t.Errorf("combined output names `pix env review work --yes` %d times, want exactly 1:\n%s", got, combined)
		}
		if got := countExactLine(combined, "pix env review work"); got != 0 {
			t.Errorf("non-TTY refusal must never ALSO print the bare re-run line, got %d:\n%s", got, combined)
		}
	})

	t.Run("interactive, no answer at all", func(t *testing.T) {
		cfg := newFixture(t)
		var out strings.Builder
		_, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &out, TTY: true, In: strings.NewReader("")})
		if err == nil {
			t.Fatal("Review must refuse on EOF")
		}
		if got := cli.ExitCode(err); got != 2 {
			t.Errorf("cli.ExitCode(err) = %d, want 2 (a declined review is a refusal, not an operational failure)", got)
		}
		combined := out.String() + "\n" + err.Error()
		assertFamilyCopy(t, "review EOF refusal", combined)
		if got := countExactLine(combined, "pix env review work"); got != 1 {
			t.Errorf("combined output names the bare re-run command %d times, want exactly 1:\n%s", got, combined)
		}
	})

	t.Run("interactive, explicit no", func(t *testing.T) {
		cfg := newFixture(t)
		var out strings.Builder
		_, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &out, TTY: true, In: strings.NewReader("no\n")})
		if err == nil {
			t.Fatal("Review must refuse")
		}
		if got := cli.ExitCode(err); got != 2 {
			t.Errorf("cli.ExitCode(err) = %d, want 2", got)
		}
		combined := out.String() + "\n" + err.Error()
		assertFamilyCopy(t, "review no-answer refusal", combined)
		if got := countExactLine(combined, "pix env review work"); got != 1 {
			t.Errorf("combined output names the bare re-run command %d times, want exactly 1:\n%s", got, combined)
		}
	})
}

// ── ErrEffectiveNotAvailable: operational, not a refusal — exit 1, no
//    three-part requirement (it is a declared-but-unbuilt feature, not a
//    usage mistake or a fixable state) ────────────────────────────────────

func TestErrorFamily_EffectiveNotAvailableIsOperationalNotARefusal(t *testing.T) {
	if got := cli.ExitCode(ErrEffectiveNotAvailable); got == 0 || got == 2 {
		t.Errorf("cli.ExitCode(ErrEffectiveNotAvailable) = %d, want a non-zero, non-2 operational code", got)
	}
	assertFamilyCopy(t, "--effective not yet available", ErrEffectiveNotAvailable.Error())
}
