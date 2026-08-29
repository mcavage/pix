// edit.go — E1.12's `pix env edit NAME pix|sbxenv`: docs/design/
// environments.md §8.1, PRD §5.4 (AC-49/50/51/52, and AC-18's post-edit
// half — the strict-parse error shape envinfo/sidecar.go's Error already
// establishes is exactly what an invalid edit's diagnostic renders).
//
// Target is an EXACT POSITIONAL ENUM, never a flag: "pix" opens the
// optional pix.toml sidecar, "sbxenv" opens the required native
// `.sbxenv.yaml` — there is deliberately no `--sbxenv` flag anywhere in
// this file or its command wiring (cmd/pix/env_cmd.go). An explicit,
// unrecognized token and an omitted token on a non-TTY are refused with
// the SAME message: both explicit, runnable forms, so a caller never has
// to guess the two exact spellings this verb accepts. An omitted token on
// a TTY prints a two-line file selection and reads exactly one bounded
// choice.
//
// # stdin discipline
//
// Edit reads opts.In in exactly one place — promptForTarget's single
// bufio.Scanner line, and only when a target was OMITTED on a TTY. Once a
// target is settled (explicit, or answered), nothing in this file ever
// reads opts.In again: the editor itself owns the terminal for the
// duration of RunInteractive (inheriting the real os.Stdin, never opts.In),
// and everything after the editor exits is read-only reload/validation —
// never an inline prompt. This is deliberate: §8.1 draws a hard line
// between "I meant that edit" (this file, no confirmation at all) and "I
// accept these host commands" (`pix env review NAME`, its own explicit
// [y/N] or --yes) — two different authorities, never collapsed into one
// prompt.
//
// # Never rolls back, never deletes a record
//
// An edit that leaves the file strictly invalid is reported (diagnostic +
// the exact edit command to try again) and left exactly as the editor
// saved it: Edit never rewrites, reverts, or deletes the user's file, and
// it never mutates config.toml or the environment-trust store either way
// — not on success, not on failure. A footprint that changed since the
// last accepted review is surfaced by FINGERPRINT MISMATCH against
// whatever record review.go already wrote (ts.Get, read-only here), never
// by deleting that record: the old acceptance stays exactly as it was,
// simply no longer matching the content on disk, which is what makes it
// unaccepted for THIS content without erasing the history of what was
// once reviewed.
package env

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/sys"
)

// TargetPix and TargetSbxenv are the only two values edit's second
// positional accepts — the exact positional enum §8.1 requires.
const (
	TargetPix    = "pix"
	TargetSbxenv = "sbxenv"
)

// EditOptions groups edit's I/O and mode, mirroring ReviewOptions.
type EditOptions struct {
	TTY bool
	In  io.Reader
	Out io.Writer
}

// EditResult is what Edit did. Verdict is "" exactly when no editor ever
// ran (nothing to validate: Edit only printed the target's absolute path
// because neither $VISUAL nor $EDITOR is set); otherwise it is one of
// "ok", "review", or "invalid" — the three §8.1/PRD §5.4 verdicts.
type EditResult struct {
	Path      string
	Target    string
	EditorRan bool
	Verdict   string
}

func targetFileName(target string) string {
	switch target {
	case TargetPix:
		return "pix.toml"
	case TargetSbxenv:
		return ".sbxenv.yaml"
	}
	return ""
}

// editTargetUsageError is the ONE refusal both an omitted token (non-TTY,
// or a TTY answer that named neither choice) and an explicit unrecognized
// token render: the same two explicit, runnable command forms either way.
func editTargetUsageError(name, headline string) error {
	return cli.UsageError{Err: fmt.Errorf(
		"pix: %s\n     pix env edit %s pix       edit pix.toml\n     pix env edit %s sbxenv    edit .sbxenv.yaml",
		headline, name, name)}
}

// resolveTarget settles edit's second positional to exactly TargetPix or
// TargetSbxenv. An explicit unrecognized token is refused outright — never
// silently coerced to either choice. An omitted token (target == "") asks
// interactively on a TTY (promptForTarget) and refuses outright, naming
// both explicit forms, on a non-TTY — no fallback default either way.
func resolveTarget(name, target string, opts EditOptions) (string, error) {
	switch target {
	case TargetPix, TargetSbxenv:
		return target, nil
	case "":
		if !opts.TTY {
			return "", editTargetUsageError(name, fmt.Sprintf(
				"`pix env edit %s` needs a target file; no TTY to ask interactively.", name))
		}
		return promptForTarget(name, opts)
	default:
		return "", editTargetUsageError(name, fmt.Sprintf(
			"`pix env edit %s %s` — unknown target %q.", name, target, target))
	}
}

// promptForTarget renders the exact two-line file selection and reads ONE
// bounded choice off opts.In directly — a single bufio.Scanner line, never
// a buffered reader retained for later. "1"/"pix" selects the sidecar,
// "2"/"sbxenv" selects the native file; anything else (a third word, a
// blank line, EOF) is refused with the same two-explicit-forms message an
// unrecognized positional token gets.
func promptForTarget(name string, opts EditOptions) (string, error) {
	fmt.Fprint(opts.Out,
		"1) pix       pix.toml (the Pix sidecar)\n"+
			"2) sbxenv    .sbxenv.yaml (the native environment file)\n"+
			"which file? [pix/sbxenv]: ")
	sc := bufio.NewScanner(opts.In)
	answer := ""
	if sc.Scan() {
		answer = strings.ToLower(strings.TrimSpace(sc.Text()))
	}
	switch answer {
	case "1", TargetPix:
		return TargetPix, nil
	case "2", TargetSbxenv:
		return TargetSbxenv, nil
	default:
		return "", editTargetUsageError(name, fmt.Sprintf(
			"`pix env edit %s` — %q is not pix or sbxenv.", name, answer))
	}
}

// resolveEditorArgv follows the standard $VISUAL-then-$EDITOR convention:
// $VISUAL wins when both are set, an empty/whitespace-only value is
// treated as unset, and the resolved value is split on whitespace into an
// argv — "code --wait" becomes ["code", "--wait"] — so a multi-word editor
// command is invoked directly (argv[0] plus its own flags), never through
// a shell. Both unset returns nil, the caller's signal to print only the
// path and stop.
func resolveEditorArgv(sysEnv sys.System) []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(sysEnv.Getenv(name)); v != "" {
			return strings.Fields(v)
		}
	}
	return nil
}

// prefixedPix ensures msg starts with "pix: " exactly once: some error
// types in this package self-prefix (MissingRequiredFileError,
// NoncanonicalRootError, ...), others do not (envinfo's own strict-parse
// *Error, AC-18's "<file>:<line>: <reason>: <key>" shape) — a caller that
// always wants one leading "pix: " needs this, the same de-duplication
// cmd/pix/env_cmd.go's envRun already applies for a returned error.
func prefixedPix(msg string) string {
	if strings.HasPrefix(msg, "pix: ") {
		return msg
	}
	return "pix: " + msg
}

// Edit is E1.12's composed entry point: resolve NAME to a canonical root
// exactly as every other verb does (ResolveEnvironment, nil effective —
// the same pre-E2 value review.go/show.go already pass), settle the
// target file, resolve $VISUAL/$EDITOR, and either print the target's
// absolute path (no editor configured) or hand the terminal to the editor
// via argv (RunInteractive — no shell interpolation) and wait for it to
// exit. After a successful editor exit, Edit reloads and strictly
// re-validates the environment and prints exactly one PRD §5.4 verdict —
// it never rolls back the file and never mutates config.toml or the
// environment-trust store either way (see this file's own doc comment).
func Edit(cfg *config.Config, sysEnv sys.System, name, target string, opts EditOptions) (*EditResult, error) {
	root, err := ResolveEnvironment(cfg, name, nil)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveTarget(name, target, opts)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, targetFileName(resolved))

	editorArgv := resolveEditorArgv(sysEnv)
	if len(editorArgv) == 0 {
		// §8.1's exit-code scheme names this explicitly: "0 only for a
		// completed operation, including printing a path because $EDITOR
		// was unset". Nothing else is printed, and no reload/validation
		// runs at all — no file was ever touched.
		fmt.Fprintln(opts.Out, path)
		return &EditResult{Path: path, Target: resolved}, nil
	}

	argv := append(append([]string(nil), editorArgv...), path)
	if err := sysEnv.RunInteractive(argv[0], argv[1:]...); err != nil {
		// Operational, not usage: the editor exists and was invoked
		// correctly, it simply failed or the process could not be
		// started. cli.ExitCode gives a plain error code 1 — non-2, per
		// this unit's own "editor failure operational non-2".
		return nil, fmt.Errorf("pix: editor %q failed: %w", strings.Join(argv, " "), err)
	}

	result := &EditResult{Path: path, Target: resolved, EditorRan: true}
	verdict, message, err := postEditVerdict(cfg, name, resolved)
	if err != nil {
		return nil, err
	}
	result.Verdict = verdict
	fmt.Fprint(opts.Out, message)
	return result, nil
}

// postEditVerdict reloads and strictly re-validates the environment after
// the editor exits, then renders exactly one of the three §5.4 verdicts:
//
//   - "invalid": the reload itself failed (a strict parse error, a now-
//     missing required file, a newly-introduced containment/symlink
//     refusal, ...). The file is left exactly as the editor saved it —
//     nothing here rewrites or reverts it — and the printed diagnostic is
//     the reload error's own message (AC-18's shape for a strict-parse
//     failure), followed by the exact command to edit the SAME target
//     again.
//   - "ok": the reload succeeded and the freshly computed host-exec
//     fingerprint MATCHES whatever record review.go last wrote for this
//     subject — no new host-execution surface to review.
//   - "review": the reload succeeded but the fingerprint does not match
//     (never reviewed at all, or reviewed under different content) — the
//     stored record is read here, never mutated or deleted; only a fresh,
//     successful `pix env review NAME` ever changes it.
func postEditVerdict(cfg *config.Config, name, target string) (verdict, message string, err error) {
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		return "", "", err
	}
	loaded, loadErr := Load(cfg, &ts.AcceptanceStore, name, nil, nil)
	if loadErr != nil {
		msg := fmt.Sprintf("%s\n     next: pix env edit %s %s\n", prefixedPix(loadErr.Error()), name, target)
		return "invalid", msg, nil
	}

	bom, err := ComputeBoM(loaded, nil, nil)
	if err != nil {
		return "", "", err
	}
	fp, err := Fingerprint(bom)
	if err != nil {
		return "", "", err
	}

	if rec, ok := ts.Get(loaded.Subject); ok && rec.Fingerprint == fp {
		msg := fmt.Sprintf("pix: environment %q is valid; host-execution footprint unchanged.\n     next: pix env use %s\n", name, name)
		return "ok", msg, nil
	}
	msg := fmt.Sprintf("pix: environment %q is valid, but its host-execution footprint changed (or was never reviewed).\n     next: pix env review %s\n", name, name)
	return "review", msg, nil
}
