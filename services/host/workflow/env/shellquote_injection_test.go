// shellquote_injection_test.go — 811dbde9's post-merge review BLOCK: every
// typed refusal in this package that names a caller-supplied NAME/PATH/
// ROOT/CWD inside a "pix ..." runnable command line must shell-quote that
// token (sys.ShellQuote) before interpolating it, so a value containing a
// space or a shell metacharacter still round-trips through a REAL POSIX
// tokenizer (sys.ShellSplit — the same dependency-free splitter
// cmd/pix/env_cmd_test.go's shellTokenize hands to a real `sh -c`) as ONE
// inert literal argument, never as a second command or an expanded
// substitution.
//
// This file is the RED-FIRST proof for the fix: every case below fails
// against the pre-fix source (which interpolated e.Name/e.Cwd/e.Root/name
// raw), because ShellSplit(line) either errors on the unbalanced
// metacharacters, or splits the malicious payload into MULTIPLE argv
// tokens instead of the one token round-tripping to the original string.
package env

import (
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/sys"
)

// spaceAndMetaPayload is the family's one injection fixture: a space (so a
// naive %s interpolation already breaks argv splitting) plus a `$(...)`
// command substitution and a `;` command separator (so an unquoted
// interpolation would both split into extra tokens AND, handed to a real
// shell, execute a second command). shellSplitLine below proves it comes
// back as exactly one token, unexpanded.
const spaceAndMetaPayload = `needs quoting; $(rm -rf /) & more`

// quotePayload additionally exercises the one character sys.ShellQuote's
// single-quote wrapping cannot represent directly (a literal single
// quote), proving the close-escape-reopen shape survives a real
// tokenizer, not just ShellQuote's own unit tests.
const quotePayload = `it's a trap`

// shellSplitLine finds the line of msg containing anchor, ShellSplits that
// line (a real POSIX-shell-shaped tokenization, not a hand-rolled string
// check), and returns the resulting argv. Failing to find the anchor, or a
// ShellSplit error (unbalanced quote — exactly what an unquoted metachar
// payload used to produce), fails the test outright.
func shellSplitLine(t *testing.T, msg, anchor string) []string {
	t.Helper()
	// The LAST occurrence: several of this family's refusals mention the
	// same command TEXT twice — once inline, in prose, describing what was
	// attempted (never itself meant to be copy-pasted as a whole line), and
	// once on its own dedicated, indented retry line (which IS) — and it is
	// always the dedicated line that comes last.
	idx := strings.LastIndex(msg, anchor)
	if idx < 0 {
		t.Fatalf("message %q has no line containing anchor %q", msg, anchor)
	}
	rest := msg[idx:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	rest = strings.TrimSpace(rest)
	argv, err := sys.ShellSplit(rest)
	if err != nil {
		t.Fatalf("ShellSplit(%q): %v (a copy-pasted refusal line must always be valid, balanced shell syntax)", rest, err)
	}
	return argv
}

// assertPayloadIsOneToken proves payload survives shellSplitLine's
// tokenization as exactly one argv element, unchanged — the property that
// makes it inert to a real shell (never expanded, never split into a
// second command) regardless of what metacharacters it contains.
func assertPayloadIsOneToken(t *testing.T, msg, anchor, payload string) {
	t.Helper()
	argv := shellSplitLine(t, msg, anchor)
	found := false
	for _, a := range argv {
		if a == payload {
			found = true
		}
	}
	if !found {
		t.Errorf("shellSplitLine(msg, %q) = %v, want it to contain payload %q as one intact token\nmsg:\n%s", anchor, argv, payload, msg)
	}
}

// ── CwdHasSbxenvError: Name, Cwd (both the register-this-directory line's
//    NAME/CWD pair and the scaffold line's cd-elsewhere NAME) ────────────

func TestShellInjection_CwdHasSbxenv(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		err := &CwdHasSbxenvError{Name: payload, Cwd: "/work/" + payload}
		msg := err.Error()
		assertPayloadIsOneToken(t, msg, "register this directory: pix env add ", payload)
		assertPayloadIsOneToken(t, msg, "scaffold a new one:      cd elsewhere && pix env add ", payload)
		// The register line carries BOTH dynamic tokens (NAME then CWD) —
		// prove the second (CWD, "/work/"+payload) round-trips too, not
		// just the first.
		line := msg[strings.Index(msg, "register this directory: pix env add "):]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}
		argv, err2 := sys.ShellSplit(strings.TrimSpace(line))
		if err2 != nil {
			t.Fatalf("ShellSplit(%q): %v", line, err2)
		}
		want := "/work/" + payload
		if len(argv) < 4 || argv[len(argv)-1] != want {
			t.Errorf("ShellSplit(%q) = %v, want the last token to equal CWD %q intact", line, argv, want)
		}
	}
}

// ── ScaffoldCollisionError: Root ──────────────────────────────────────────

func TestShellInjection_ScaffoldCollision(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		root := "/data/envs/" + payload
		err := &ScaffoldCollisionError{Root: root}
		assertPayloadIsOneToken(t, err.Error(), "register it as-is: pix env add <name> ", root)
	}
}

// ── ConcurrentRegistrationError: Name, Attempted (both branches) ────────

func TestShellInjection_ConcurrentRegistration(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		attempted := "/env/" + payload
		repointed := &ConcurrentRegistrationError{Name: payload, Existing: "/env/theirs", Attempted: attempted}
		msg := repointed.Error()
		assertPayloadIsOneToken(t, msg, "re-run to repoint it deliberately: pix env add ", payload)
		assertPayloadIsOneToken(t, msg, "re-run to repoint it deliberately: pix env add ", attempted)

		forgotten := &ConcurrentRegistrationError{Name: payload, Attempted: attempted}
		msg2 := forgotten.Error()
		assertPayloadIsOneToken(t, msg2, "re-run to register it deliberately: pix env add ", payload)
		assertPayloadIsOneToken(t, msg2, "re-run to register it deliberately: pix env add ", attempted)
	}
}

// ── UseNotReviewedError: Name (both branches) ────────────────────────────

func TestShellInjection_UseNotReviewed(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		err := &UseNotReviewedError{Name: payload}
		assertPayloadIsOneToken(t, err.Error(), "review it: pix env review ", payload)

		changed := &UseNotReviewedError{Name: payload, Changed: true}
		assertPayloadIsOneToken(t, changed.Error(), "review it: pix env review ", payload)
	}
}

// ── ForgetLiveHolderError: Name when dynamic (Unknown branch); the Held
//    branch's `<sandbox>` is a fixed placeholder (no sandbox identity is
//    ever resolved — see forget.go's own doc comment), so there is no
//    dynamic sandbox token to quote there at all — only Name in the
//    Unknown branch's retry line is ever attacker-supplied. ────────────

func TestShellInjection_ForgetLiveHolder(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		unknown := &ForgetLiveHolderError{Name: payload, Unknown: true}
		assertPayloadIsOneToken(t, unknown.Error(), "retry: pix env forget ", payload)

		held := &ForgetLiveHolderError{Name: payload}
		msg := held.Error()
		if !strings.Contains(msg, "pix rm <sandbox>") {
			t.Errorf("ForgetLiveHolderError{Name: %q}.Error() = %q, want the fixed `pix rm <sandbox>` placeholder (no live sandbox identity is ever resolved)", payload, msg)
		}
	}
}

// ── NoncanonicalRootError: Name ───────────────────────────────────────────

func TestShellInjection_NoncanonicalRoot(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		err := &NoncanonicalRootError{Name: payload, Root: "relative/env"}
		assertPayloadIsOneToken(t, err.Error(), "re-register it: pix env add ", payload)
	}
}

// ── MissingRequiredFileError: Name ────────────────────────────────────────

func TestShellInjection_MissingRequiredFile(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		err := &MissingRequiredFileError{Name: payload, Root: "/env/x", File: ".sbxenv.yaml"}
		assertPayloadIsOneToken(t, err.Error(), "create it: pix env edit ", payload)
	}
}

// ── AddPathError: Name ────────────────────────────────────────────────────

func TestShellInjection_AddPath(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		err := &AddPathError{Name: payload, Path: "/no/such/dir", Kind: "does not exist"}
		assertPayloadIsOneToken(t, err.Error(), "scaffold instead: pix env add ", payload)
	}
}

// ── addMissingRequiredFileRetry: Name (add's context-rewritten form) ────

func TestShellInjection_AddMissingRequiredFileRetry(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		orig := cli.UsageError{Err: &MissingRequiredFileError{Name: payload, Root: "/env/x", File: ".sbxenv.yaml"}}
		got := addMissingRequiredFileRetry(payload, orig)
		assertPayloadIsOneToken(t, got.Error(), "scaffold instead: pix env add ", payload)
	}
}

// ── review.go: ReviewChangedDuringPromptError, gate()'s own retry lines,
//    and renderBill's --verbose tip, all keyed on Name ───────────────────

func TestShellInjection_ReviewChangedDuringPrompt(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		err := &ReviewChangedDuringPromptError{Name: payload}
		assertPayloadIsOneToken(t, err.Error(), "read the current bill: pix env review ", payload)
	}
}

func TestShellInjection_ReviewGateRetryLines(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		cfg := envWithNames(payload)
		var out strings.Builder
		err := gate(strings.NewReader(""), &out, false, false, payload, "", "", BillOfMaterials{}, false)
		if err == nil {
			t.Fatalf("gate() must refuse a non-TTY caller with no --yes")
		}
		combined := out.String() + "\n" + err.Error()
		assertPayloadIsOneToken(t, combined, "retry: pix env review ", payload)
		_ = cfg
	}
}

func TestShellInjection_RenderBillVerboseTip(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		var out strings.Builder
		renderBill(&out, payload, BillOfMaterials{}, false)
		assertPayloadIsOneToken(t, out.String(), "full argv and content digests: pix env review ", payload)
	}
}

// ── edit.go: the two headline call sites that name the invocation that
//    just failed (resolveTarget's no-TTY/unrecognized-token branches,
//    promptForTarget's unrecognized-answer branch), plus the two ALWAYS-
//    present retry lines editTargetUsageError itself appends below every
//    headline ───────────────────────────────────────────────────────────
//
// The headline is prose ("`pix env edit NAME` needs a target file; no TTY
// ..."), never a standalone copy-pasteable command line — it has trailing
// words after the closing backtick, so it is not, and is not meant to be,
// valid whole-line shell syntax on its own; shellSplitLine's whole-line
// tokenization does not apply to it. What matters is that the payload was
// actually run through sys.ShellQuote before landing inside the backticks
// (substring match on the exact quoted form), while the two REAL retry
// lines underneath (which ARE meant to be copy-pasted whole) still get the
// full round-trip proof.
func TestShellInjection_EditTargetHeadlines(t *testing.T) {
	for _, payload := range []string{spaceAndMetaPayload, quotePayload} {
		quoted := sys.ShellQuote(payload)

		_, err := resolveTarget(payload, "", EditOptions{TTY: false})
		if !strings.Contains(err.Error(), "`pix env edit "+quoted+"`") {
			t.Errorf("resolveTarget(%q, \"\", non-TTY) = %q, want the headline to shell-quote NAME as %q", payload, err.Error(), quoted)
		}
		assertPayloadIsOneToken(t, err.Error(), "pix env edit ", payload)

		_, err = resolveTarget(payload, "garbage", EditOptions{})
		if !strings.Contains(err.Error(), "`pix env edit "+quoted+" ") {
			t.Errorf("resolveTarget(%q, \"garbage\") = %q, want the headline to shell-quote NAME as %q", payload, err.Error(), quoted)
		}
		assertPayloadIsOneToken(t, err.Error(), "pix env edit ", payload)

		_, err = promptForTarget(payload, EditOptions{Out: discardWriter{}, In: strings.NewReader("nope\n")})
		if !strings.Contains(err.Error(), "`pix env edit "+quoted+"`") {
			t.Errorf("promptForTarget(%q, ...) = %q, want the headline to shell-quote NAME as %q", payload, err.Error(), quoted)
		}
		assertPayloadIsOneToken(t, err.Error(), "pix env edit ", payload)
	}
}
