// setupadopt.go — the `pix setup --pack` adoption, on the pack side.
//
// `pix setup` states the step ("adopt the packs this invocation asked for") but
// must not own it: adoption is a trust decision — the Tier-1 bill of materials,
// the fingerprint, the rollback — and workflow/provision may not import a
// sibling workflow to make one. So provision declares an injected seam and the
// composition root binds this to it.
package pack

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/packinfo"
)

// SetupAdopter binds the MCP registrar into setup's pack adoption, producing the
// exact seam workflow/provision declares.
func SetupAdopter(register RegisterFn, wrap ProbeWrapFn) func(hostenv.Env, io.Writer, []string, []string, bool) error {
	return func(env hostenv.Env, out io.Writer, packs, with []string, assumeYes bool) error {
		return adoptForSetup(env, out, register, wrap, packs, with, assumeYes)
	}
}

// adoptForSetup adopts each requested pack through the ordinary pack trust
// transaction (same BoM review, fingerprint and rollback as `pix pack use`),
// composes the stack, and runs each pack's REQUIRED setup hooks — a pack that is
// adopted but not set up is exactly the half-state setup's second check would
// then report as a gap with no way to close it.
func adoptForSetup(env hostenv.Env, out io.Writer, register RegisterFn, wrap ProbeWrapFn, packs, with []string, assumeYes bool) error {
	var activated []string
	var deferred []error
	for _, requested := range packs {
		useArgs := []string{NormalizeSetupPackArg(requested)}
		if assumeYes {
			useArgs = append([]string{"--yes"}, useArgs...)
		}
		err := RunPackUse(env, out, useArgs, register)
		var regErr *mcpRegisterError
		switch {
		case err == nil:
		case errors.As(err, &regErr):
			// DEFERRED, not fatal. Registration is adoption's last post-commit
			// step, so this pack IS adopted; what it could not do is resolve a
			// command that is not installed yet. Installing it is precisely
			// what the pack's setup hooks below do — and the error even says
			// so, naming the very step that failing here made unreachable.
			//
			// That was the whole bug: a pack whose first adoption on a clean
			// machine declares any host command it also knows how to install
			// could never complete `pix setup`, and the advice printed was a
			// command the user could not successfully run.
			deferred = append(deferred, err)
			fmt.Fprintln(out, "  (registration deferred: setup installs what is missing, then it is retried)")
		default:
			return fmt.Errorf("adopting pack %s: %w", requested, err)
		}
		if cfg, err := config.Load(); err == nil && strings.TrimSpace(cfg.Pack) != "" {
			activated = append(activated, cfg.Pack)
		}
	}
	activated = packinfo.UniquePackRoots(activated)
	if len(activated) > 0 {
		if err := PersistPackStack(activated); err != nil {
			return fmt.Errorf("composing packs: %w", err)
		}
	}
	requests, err := PlanPackSetupRequests(activated, with)
	if err != nil {
		return err
	}
	// Interactivity is MEASURED, exactly as the trust gate and the credential
	// solicitor in this same flow measure it. It used to be hardcoded false, and
	// the cost fell entirely on new users: a step whose remediation needs a
	// terminal — `slack-mcp auth login`, `gog auth setup --login`, any browser
	// authorization — was refused on a run that had just prompted the user for a
	// y/N and two 1Password references. The refusal then said to "re-run without
	// --yes/--non-interactive", flags the user had not passed, so there was no
	// next step to take. A new user has no Slack grant by definition, so that
	// step could never pass and its fix could never run.
	interactive := setupInteractivity(assumeYes, cli.IsTTY(os.Stdin))
	for _, root := range activated {
		if err := RunPackSetup(env, out, root, requests[root], interactive, wrap); err != nil {
			return err
		}
	}
	if len(deferred) == 0 {
		return nil
	}
	// Nothing to retry means nothing ran that could have fixed it, so the
	// deferred failure is simply the answer. Without this, a pack that committed
	// and then vanished from cfg would have its registration failure deferred
	// into an empty retry loop and reported as success.
	if len(activated) == 0 {
		return errors.Join(deferred...)
	}
	// The setup hooks have run, so ask again — and only a failure that SURVIVES
	// them is a real one. Retrying every activated pack rather than only the one
	// that failed keeps this the same call adoption makes, and the registrar is
	// idempotent, so a pack that registered cleanly the first time is unchanged.
	fmt.Fprintln(out, "\nretrying mcp registration now that setup has run…")
	var errs []error
	for _, root := range activated {
		if err := registerActivePackMCP(env, out, root, register); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// setupInteractivity decides whether a pack's setup hooks may run a remediation
// that needs a terminal. It is a function, not an expression inline above,
// because it was a hardcoded `false` and nothing could observe that.
//
// Both conditions are load-bearing:
//   - --yes means "ask me nothing", and a browser authorization is a question.
//   - No TTY means there is nobody to answer, so prompting would hang a CI job.
//
// Neither alone is sufficient: measuring only the TTY would let --yes open a
// browser, and honouring only --yes would prompt into a pipe.
func setupInteractivity(assumeYes, tty bool) bool { return !assumeYes && tty }

// NormalizeSetupPackArg expands the `owner/repo` shorthand to a clone URL.
func NormalizeSetupPackArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Count(arg, "/") == 1 && !strings.Contains(arg, ":") && !strings.HasPrefix(arg, ".") && !strings.HasPrefix(arg, "~") {
		return "https://github.com/" + arg + ".git"
	}
	return arg
}
