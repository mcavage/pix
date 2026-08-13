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
// composes the stack, runs each pack's REQUIRED setup hooks, and only THEN
// registers its MCP servers.
//
// That order is the whole point. A pack's setup hooks install the commands its
// MCP servers are, so on a first install the binaries do not exist until the
// hooks have run — registering before them cannot work, and no amount of
// retrying makes it work any better. This used to register first and then talk
// about it: "needs the X command, which is not on PATH", a note about a step
// the user had not reached yet, and a retry twenty seconds later. All of that
// was pix narrating an ordering mistake instead of not making it. Nobody
// running `pix setup` needs to know any of it; they need the thing to work.
func adoptForSetup(env hostenv.Env, out io.Writer, register RegisterFn, wrap ProbeWrapFn, packs, with []string, assumeYes bool) error {
	var activated []string
	for _, requested := range packs {
		useArgs := []string{NormalizeSetupPackArg(requested)}
		if assumeYes {
			useArgs = append([]string{"--yes"}, useArgs...)
		}
		if err := RunPackUse(env, out, useArgs, skipRegistration); err != nil {
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
	var setupErr error
	for _, root := range activated {
		if err := RunPackSetup(env, out, root, requests[root], interactive, wrap); err != nil {
			setupErr = err
			break
		}
	}
	// Register even when a hook FAILED. A pack's steps are independent: an
	// unrelated step dying (a broken OAuth scope, an expired grant) says nothing
	// about the command an earlier step just installed, and skipping
	// registration would leave that command's server — and every remote server
	// that needed no setup at all — unregistered until someone ran `pix mcp add`
	// by hand. Both failures are reported; neither hides the other.
	errs := []error{setupErr}
	for _, root := range activated {
		if err := registerActivePackMCP(env, out, root, register); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// skipRegistration is the registrar adoption is handed inside `pix setup`: it
// registers nothing, because registration happens after the setup hooks have
// installed what it needs (see adoptForSetup). `pix pack use` on its own still
// registers inline — it runs no hooks, so there is nothing to wait for, and a
// missing command there is a real answer rather than a stage in a sequence.
func skipRegistration(*config.Config, hostenv.Env, io.Writer, []string, map[string]config.MCPServer) error {
	return nil
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
