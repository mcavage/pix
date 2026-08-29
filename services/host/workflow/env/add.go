// add.go — E1.10's `pix env add NAME [PATH]`: docs/design/environments.md
// §8.1, D10, Story 1 (register/scaffold). Two entry shapes share ONE
// commit discipline (Add's own doc comment below):
//
//   - `add NAME PATH` registers a canonical local directory the caller
//     already authored.
//   - `add NAME` (no PATH) scaffolds a fresh, runnable, Tier0 directory
//     under config.EnvsDir() equivalent to Pix's own built-in defaults —
//     never an empty stub — UNLESS $PWD already holds a `.sbxenv.yaml`, in
//     which case the omitted token is ambiguous (D10) and Add refuses
//     outright, naming both the register and the scaffold forms, and
//     creates nothing.
//
// Both shapes end the SAME way: E1.7's Load semantics (via E1.8's Review,
// which loads internally — see review.go's own TOCTOU doc comment) strictly
// validate the required `.sbxenv.yaml` and optional `pix.toml`, host review
// ALWAYS runs (a Tier0 environment's bill is empty and needs no prompt;
// Tier1 gates on explicit consent), and only a successful, accepted review
// ever reaches cfg.Save(). See Add's own doc comment for the transactional
// ordering that makes an invalid or refused review leave cfg and the
// environment trust store byte-for-byte unchanged.
package env

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/sys"
)

// sbxenvFilename is the required native document's fixed name everywhere in
// this package — the same literal load.go's Load joins against a
// registered root.
const sbxenvFilename = ".sbxenv.yaml"

// scaffoldSbxenv is a zero-path `add`'s entire generated environment: the
// SAME minimal shape env_cmd_test.go's registerTier0Env already exercises
// (schemaVersion "1", agent "pix", no kits/mcp/secrets/registries/
// bindings/ports) — a real, schema-valid, Tier0 document with an empty
// host-execution bill, which docs/design/environments.md §8.1 calls
// "runnable and equivalent to the current default, not an empty stub": the
// current default (D17's `none`) is exactly "no host-executing facet at
// all", and this document declares precisely that, explicitly, under a
// name the user can now select. No `pix.toml` sidecar is written alongside
// it: nothing here needs metadata sbx cannot already express, so writing
// one would be an unused file, not a "minimal" one.
const scaffoldSbxenv = "schemaVersion: \"1\"\nagent: pix\n"

// scaffoldDirMode/scaffoldFileMode are deliberately restrictive — matching
// every other launcher-owned generated tree in this codebase (e.g.
// inference.SynthesizeInferenceKit's own 0o700 dir / 0o600 file pair): a
// scaffolded environment can grow secrets/registries references, so it
// starts life no more permissive than that.
const (
	scaffoldDirMode  = 0o700
	scaffoldFileMode = 0o600
)

// AddOptions groups add's I/O and review mode — the same shape
// ReviewOptions already establishes for `pix env review`, since Add ALWAYS
// ends by calling Review (see Add's own doc comment) and forwards every one
// of these fields straight through, unmodified.
type AddOptions struct {
	Verbose bool
	Yes     bool
	TTY     bool
	In      io.Reader
	Out     io.Writer
	// Getwd resolves the current directory for the zero-path cwd-collision
	// refusal (D10). Nil defaults to os.Getwd; a test overrides it to pin a
	// directory without actually os.Chdir-ing the whole test binary (which
	// would leak across parallel tests).
	Getwd func() (string, error)
	// LookPath is forwarded to Load/Review unchanged; nil defaults to the
	// real exec.LookPath (see resolve.go's ResolveLocalCommand).
	LookPath func(string) (string, error)
}

// AddResult is what Add did, for a caller to report.
type AddResult struct {
	Name       string
	Root       string
	Scaffolded bool
	Review     ReviewResult
}

// CwdHasSbxenvError is D10's zero-path refusal: `pix env add NAME` (no
// PATH) is ambiguous when $PWD already holds a `.sbxenv.yaml`, since the
// one omitted token could mean either "register the file that's already
// right here" or "scaffold an unrelated new environment under
// config.EnvsDir()". Error() names BOTH the `pix env add NAME PATH`
// register form and the bare `pix env add NAME` scaffold form explicitly,
// so the refusal never silently picks one on the user's behalf; Add itself
// creates nothing when this fires.
type CwdHasSbxenvError struct {
	Name string
	Cwd  string
}

func (e *CwdHasSbxenvError) Error() string {
	return fmt.Sprintf(
		"pix: %s already has a %s; a zero-path `pix env add %s` is ambiguous between registering it and scaffolding an unrelated new environment. Pick one explicitly:\n"+
			"     register this directory: pix env add %s %s\n"+
			"     scaffold a new one:      cd elsewhere && pix env add %s",
		e.Cwd, sbxenvFilename, e.Name, e.Name, e.Cwd, e.Name,
	)
}

// AddPathError is `pix env add NAME PATH`'s PATH-prevalidation refusal
// (C8): PATH must already be a real, existing directory before anything
// else is attempted — checked BEFORE the candidate ever reaches Review's
// Load, whose own `.sbxenv.yaml` os.Stat, several calls later, would
// otherwise name the wrong thing (the missing FILE) when the problem is
// actually the missing or non-directory PATH itself. Kind grounds exactly
// what failed ("does not exist", "is not a directory"); the only honest
// next step is the scaffold form — the register form this refusal fires
// from cannot possibly work against this PATH, so naming it again would be
// an impossible retry.
type AddPathError struct {
	Name string
	Path string
	Kind string
}

func (e *AddPathError) Error() string {
	return fmt.Sprintf(
		"pix: %s %s.\n     path: %s\n     scaffold instead: pix env add %s",
		e.Path, e.Kind, e.Path, sys.ShellQuote(e.Name),
	)
}

// validateAddPath is registerAdd's prevalidation gate: canonRoot (the exact
// canonical path that would be stored) must already exist and be a
// directory. A permissions failure or anything else os.Stat cannot
// classify as "missing"/"not a directory" is an operational failure (exit
// 1), never this refusal — there is nothing wrong with what the caller
// authored, pix simply could not check it.
func validateAddPath(name, canonRoot string) error {
	fi, err := os.Stat(canonRoot)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return cli.UsageError{Err: &AddPathError{Name: name, Path: canonRoot, Kind: "is not a directory"}}
	case os.IsNotExist(err):
		return cli.UsageError{Err: &AddPathError{Name: name, Path: canonRoot, Kind: "does not exist"}}
	default:
		return fmt.Errorf("pix: checking %s: %w", canonRoot, err)
	}
}

// addMissingRequiredFileRetry rewrites Load's MissingRequiredFileError,
// reached through Review's own Load call, for add's context: NAME is not
// registered in the real config yet — Review only ever sees add's
// throwaway candidateConfig (Add's own doc comment) — so
// MissingRequiredFileError's own "create it: pix env edit NAME sbxenv" (the
// correct fix for an ALREADY-REGISTERED environment load.go's other callers
// resolve) would itself fail with an unknown-name refusal here: an
// impossible retry against a name that does not exist yet. The only
// alternative that is actually runnable is the scaffold form, which builds
// its own fresh, valid `.sbxenv.yaml` under pix's own control rather than
// asking a caller to hand-author one inside a directory add cannot safely
// write into on their behalf. Every other error Review can return (an
// unknown-command refusal, a strict parse failure, a containment/symlink
// refusal, ...) already names a fix that is equally runnable whether or not
// NAME is registered yet, so it passes through unchanged.
func addMissingRequiredFileRetry(name string, err error) error {
	var missing *MissingRequiredFileError
	if !errors.As(err, &missing) {
		return err
	}
	return cli.UsageError{Err: fmt.Errorf(
		"pix: environment %q has no required %s.\n     missing: %s\n     scaffold instead: pix env add %s",
		name, missing.File, filepath.Join(missing.Root, missing.File), sys.ShellQuote(name),
	)}
}

// ScaffoldCollisionError is a zero-path `add`'s refusal when its computed
// target (config.EnvsDir()/NAME) already names something — a directory, a
// regular file, or a symlink, all three indistinguishable to the os.Mkdir
// this package performs the actual check with (see scaffoldDirectory).
// Add never overwrites an existing entry under any of those shapes.
type ScaffoldCollisionError struct {
	Root string
}

func (e *ScaffoldCollisionError) Error() string {
	return fmt.Sprintf(
		"pix: %s already exists; refusing to overwrite it. Pick a different name, or register it as-is: pix env add <name> %s",
		e.Root, e.Root,
	)
}

// Add is E1.10's composed entry point for `pix env add NAME [PATH]`. An
// empty path scaffolds (scaffoldAdd); any other value registers a
// caller-authored directory (registerAdd).
//
// # Transactional safety
//
// Neither branch ever mutates the REAL cfg (nor calls cfg.Save) until
// AFTER Review has returned a successful, accepted result. Both branches
// build a throwaway candidate config (candidateConfig) — a shallow copy of
// cfg with its own independent Environments map — register the
// name/canonical-root pair into THAT copy only, and run the full E1.7
// Load + E1.8 Review pipeline against it. Review's own contract already
// guarantees a refused or failed gate writes nothing to the environment
// trust store (review.go's own doc comment); routing every check through a
// candidate cfg extends the identical guarantee to config.toml itself: an
// invalid document, a location refusal, or a declined host-execution bill
// leaves the real cfg — and therefore config.toml on disk — byte-for-byte
// as it was before Add was called. Only once Review reports success does
// Add repeat the identical AddEnvironment call against the real cfg and
// save it — the ONE commit point, after which repointing an existing name
// to a new root is ordinary config mutation (AC-16's "acceptance is keyed
// by root, never by name" already makes a repoint's new Subject
// unaccepted, so it goes through this same gate fresh).
func Add(cfg *config.Config, name, path string, opts AddOptions) (*AddResult, error) {
	if path == "" {
		return scaffoldAdd(cfg, name, opts)
	}
	return registerAdd(cfg, name, path, opts)
}

// candidateConfig returns a shallow copy of cfg whose Environments map is
// an independent copy (never cfg's own map) — the throwaway target every
// pre-registration validation/review step in this file runs against, so
// nothing commits to the real cfg before a caller decides to. Every other
// field is Load/Review's to read only; neither ever writes through cfg, so
// sharing them by shallow copy is safe.
func candidateConfig(cfg *config.Config) *config.Config {
	clone := *cfg
	clone.Environments = make(map[string]string, len(cfg.Environments))
	for k, v := range cfg.Environments {
		clone.Environments[k] = v
	}
	return &clone
}

// commitAddRegistration is Add's ONE commit point, shared by both entry
// shapes: under the env-registry lock, fresh-load the live config, enforce
// the expected-state precondition, register, save (commit.go). The
// precondition is optimistic-concurrency, deliberately: the lock is NOT
// held across Review's interactive prompt (an unbounded human wait inside
// a 30-second-budget flock would make every concurrent env mutation time
// out spuriously), so whatever another process committed while the prompt
// was open is re-read here and judged fresh:
//
//   - name absent, or already registered to this SAME canonical root:
//     proceed (two identical concurrent adds are idempotent);
//   - name registered to a DIFFERENT root: refuse deterministically
//     (ConcurrentRegistrationError, exit 2) — never silently repoint a
//     registration this add's review never saw.
//
// The expected state is what THIS add observed in its caller's cfg when it
// started (observed/observedOK, captured before Review could wait on a
// human): a deliberate repoint — `add NAME NEWPATH` over an existing
// registration the user could see — is ordinary and proceeds when the live
// config still matches what they saw; ANY intervening change to this name
// (registered where it was absent, repointed elsewhere, or forgotten)
// refuses deterministically instead of being silently overwritten. The
// name already registered to canonRoot itself is always fine: two
// identical concurrent adds are idempotent.
//
// A refusal here can leave an acceptance record for canonRoot in the trust
// store (Review persisted it before this commit ran). That is harmless and
// honest: the human really did review that root; acceptance is keyed by
// root, never by name (AC-16), so the record grants nothing to the name's
// current registration.
func commitAddRegistration(cfg *config.Config, name, path, canonRoot, observed string, observedOK bool) error {
	return commitEnvRegistryMutation(cfg, func(fresh *config.Config) error {
		existing, ok := fresh.Environments[name]
		switch {
		case ok && existing == canonRoot:
			// idempotent: the live config already says exactly this
		case ok == observedOK && existing == observed:
			// unchanged since this add started: absent both times, or
			// still the same root the user is deliberately repointing from
		case ok:
			return cli.UsageError{Err: &ConcurrentRegistrationError{Name: name, Existing: existing, Attempted: canonRoot}}
		default:
			return cli.UsageError{Err: &ConcurrentRegistrationError{Name: name, Attempted: canonRoot}}
		}
		_, err := fresh.AddEnvironment(name, path)
		return err
	})
}

// reviewOptionsFrom adapts AddOptions straight into ReviewOptions — the
// one seam that would otherwise have to be repeated in both registerAdd
// and scaffoldAdd — and supplies add's own origin-appropriate retry
// commands (ReviewOptions.Retry/NonTTYRetry's own doc comment): the exact
// `pix env add NAME [PATH]` invocation the caller just made, POSIX-shell-
// quoted (sys.ShellQuote) so a NAME or PATH containing a space or shell
// metacharacter still round-trips through copy-paste-and-run. path is ""
// for scaffoldAdd's bare `pix env add NAME` form — never the internal
// scaffold target under config.EnvsDir(), which the caller never typed and
// a retry must not name in their place.
func reviewOptionsFrom(opts AddOptions, out io.Writer, name, path string) ReviewOptions {
	retry := fmt.Sprintf("pix env add %s", sys.ShellQuote(name))
	if path != "" {
		retry = fmt.Sprintf("pix env add %s %s", sys.ShellQuote(name), sys.ShellQuote(path))
	}
	return ReviewOptions{
		Verbose: opts.Verbose, Yes: opts.Yes, TTY: opts.TTY, In: opts.In, Out: out,
		Retry: retry, NonTTYRetry: retry + " --yes",
	}
}

// registerAdd implements `pix env add NAME PATH` — see Add's own doc
// comment for the transactional ordering every step here follows.
func registerAdd(cfg *config.Config, name, path string, opts AddOptions) (*AddResult, error) {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	// The expected-state snapshot commitAddRegistration compares against:
	// what the user could actually see when they asked for this add.
	observed, observedOK := cfg.Environments[name]

	candidate := candidateConfig(cfg)
	canonRoot, err := candidate.AddEnvironment(name, path)
	if err != nil {
		// config.AddEnvironment's own refusals (an unsafe name, an empty
		// path) are the user's mistake to fix, not an operational failure.
		return nil, cli.UsageError{Err: err}
	}

	// C8: PATH must already be a real directory before Review/Load ever
	// gets to its own `.sbxenv.yaml` check — see AddPathError's own doc
	// comment for why a later, deeper refusal would misname the problem.
	if err := validateAddPath(name, canonRoot); err != nil {
		return nil, err
	}

	result, err := Review(candidate, name, nil, opts.LookPath, reviewOptionsFrom(opts, out, name, path))
	if err != nil {
		// Review failed or was refused against the CANDIDATE cfg only; the
		// real cfg was never touched, so there is nothing to roll back.
		// A missing-`.sbxenv.yaml` refusal needs add's own context-aware
		// retry (addMissingRequiredFileRetry's doc comment); every other
		// error already names a fix that works whether or not NAME is
		// registered yet, so it passes through unchanged.
		return nil, addMissingRequiredFileRetry(name, err)
	}

	if err := commitAddRegistration(cfg, name, path, canonRoot, observed, observedOK); err != nil {
		if cli.ExitCode(err) == 2 {
			return nil, err // a commit-time refusal (concurrent repoint), already fully worded
		}
		return nil, fmt.Errorf(
			"pix: environment %q was reviewed and accepted, but saving the registration failed: %w (re-run `pix env add %s %s` to retry)",
			name, err, name, path,
		)
	}

	printAddSuccess(out, name, canonRoot, false)
	return &AddResult{Name: name, Root: canonRoot, Review: *result}, nil
}

// printAddSuccess is the ONE success line both registerAdd and scaffoldAdd
// end on: it names the literal next command, `pix env use NAME`, and
// nothing that reads as an unearned success verdict ("configured",
// "enabled", "ready", "verified" never appear here — review.go's own
// acceptance line, printed just above this one for a Tier1 environment,
// already said what was actually checked).
func printAddSuccess(out io.Writer, name, root string, scaffolded bool) {
	verb := "registered"
	if scaffolded {
		verb = "scaffolded"
	}
	fmt.Fprintf(out, "pix: environment %q %s at %s.\n\npix env use %s\n", name, verb, root, name)
}

// scaffoldAdd implements `pix env add NAME` (no PATH). See Add's own doc
// comment for the transactional ordering, and this file's package doc
// comment for why the generated environment is Tier0 by construction.
func scaffoldAdd(cfg *config.Config, name string, opts AddOptions) (*AddResult, error) {
	getwd := opts.Getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd, err := getwd()
	if err != nil {
		return nil, fmt.Errorf("pix: resolving the current directory: %w", err)
	}
	switch _, statErr := os.Stat(filepath.Join(cwd, sbxenvFilename)); {
	case statErr == nil:
		// D10: one omitted token must not silently pick between registering
		// the file that's already here and scaffolding an unrelated new
		// one. Refuse outright; create nothing.
		return nil, cli.UsageError{Err: &CwdHasSbxenvError{Name: name, Cwd: cwd}}
	case os.IsNotExist(statErr):
		// The common case: no ambiguity, proceed.
	default:
		return nil, fmt.Errorf("pix: checking %s for %s: %w", cwd, sbxenvFilename, statErr)
	}

	// Validate the name and compute the canonical scaffold root the exact
	// same way registerAdd validates a caller-supplied path —
	// config.AddEnvironment is the ONE place a name is judged safe
	// (config/environment.go's validEnvironmentName) — before anything
	// touches disk. A throwaway *config.Config{} is enough here: this call
	// only canonicalizes name+path, it never reads or needs cfg's own
	// registry.
	root, err := (&config.Config{}).AddEnvironment(name, filepath.Join(config.EnvsDir(), name))
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}

	if err := scaffoldDirectory(root); err != nil {
		return nil, err
	}
	// committed guards the deferred cleanup below: false at every return
	// before Review has actually accepted means Add created a directory
	// nobody will ever be told about and must remove it whole — never a
	// partial scaffold left for a later `pix env add` to trip over as a
	// stale collision.
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(root)
		}
	}()

	if err := sys.AtomicWriteInDir(root, sbxenvFilename, []byte(scaffoldSbxenv), scaffoldFileMode); err != nil {
		return nil, fmt.Errorf("pix: writing %s: %w", filepath.Join(root, sbxenvFilename), err)
	}

	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	// The scaffold's absolute root is the FIRST line of output, printed as
	// soon as the directory exists — before Review ever runs, since a
	// caller scripting this needs the created path even if a later step
	// (an operational failure, never a refusal: this document is always
	// Tier0) fails.
	fmt.Fprintln(out, root)

	// Same expected-state snapshot as registerAdd's (see
	// commitAddRegistration): scaffolding an already-registered name is
	// a repoint like any other and gets the same concurrent-change gate.
	observed, observedOK := cfg.Environments[name]

	candidate := candidateConfig(cfg)
	if _, err := candidate.AddEnvironment(name, root); err != nil {
		return nil, err // unreachable: the same inputs were just accepted above
	}
	result, err := Review(candidate, name, nil, opts.LookPath, reviewOptionsFrom(opts, out, name, ""))
	if err != nil {
		return nil, err
	}

	if err := commitAddRegistration(cfg, name, root, root, observed, observedOK); err != nil {
		// committed stays false on EITHER failure shape: the deferred
		// cleanup above removes the scaffold whole, so a retry starts clean
		// rather than tripping the scaffold-collision refusal against a
		// directory nobody ever finished registering ("no partial dirs"
		// holds for a commit failure too, not only a load/review failure).
		if cli.ExitCode(err) == 2 {
			return nil, err // a commit-time refusal (concurrent repoint), already fully worded
		}
		return nil, fmt.Errorf(
			"pix: environment %q was scaffolded and accepted, but saving the registration failed: %w (re-run `pix env add %s` to retry)",
			name, err, name,
		)
	}

	committed = true
	printAddSuccess(out, name, root, true)
	return &AddResult{Name: name, Root: root, Scaffolded: true, Review: *result}, nil
}

// scaffoldDirectory creates root (and config.EnvsDir() itself, if absent)
// with no partial state ever left behind on the common failure: os.Mkdir on
// root is the ONE atomic gate — it fails EEXIST for an existing directory,
// regular file, OR symlink alike, so scaffoldAdd's caller never overwrites
// any of the three, and nothing is created at all when it does.
func scaffoldDirectory(root string) error {
	if err := os.MkdirAll(config.EnvsDir(), scaffoldDirMode); err != nil {
		return fmt.Errorf("pix: creating %s: %w", config.EnvsDir(), err)
	}
	if err := os.Mkdir(root, scaffoldDirMode); err != nil {
		if os.IsExist(err) {
			return cli.UsageError{Err: &ScaffoldCollisionError{Root: root}}
		}
		return fmt.Errorf("pix: creating %s: %w", root, err)
	}
	return nil
}
