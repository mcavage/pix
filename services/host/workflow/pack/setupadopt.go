// setupadopt.go — the `pix setup --pack` adoption, on the pack side.
//
// `pix setup` states the step ("adopt the packs this invocation asked for") but
// must not own it: adoption is a trust decision — the Tier-1 bill of materials,
// the fingerprint, the rollback — and workflow/provision may not import a
// sibling workflow to make one. So provision declares an injected seam and the
// composition root binds this to it.
package pack

import (
	"fmt"
	"io"
	"strings"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/packinfo"
)

// SetupAdopter binds the MCP registrar into setup's pack adoption, producing the
// exact seam workflow/provision declares.
func SetupAdopter(register RegisterFn) func(hostenv.Env, io.Writer, []string, []string, bool) error {
	return func(env hostenv.Env, out io.Writer, packs, with []string, assumeYes bool) error {
		return adoptForSetup(env, out, register, packs, with, assumeYes)
	}
}

// adoptForSetup adopts each requested pack through the ordinary pack trust
// transaction (same BoM review, fingerprint and rollback as `pix pack use`),
// composes the stack, and runs each pack's REQUIRED setup hooks — a pack that is
// adopted but not set up is exactly the half-state setup's second check would
// then report as a gap with no way to close it.
func adoptForSetup(env hostenv.Env, out io.Writer, register RegisterFn, packs, with []string, assumeYes bool) error {
	var activated []string
	for _, requested := range packs {
		useArgs := []string{NormalizeSetupPackArg(requested)}
		if assumeYes {
			useArgs = append([]string{"--yes"}, useArgs...)
		}
		if err := RunPackUse(env, out, useArgs, register); err != nil {
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
	for _, root := range activated {
		if err := RunPackSetup(env, out, root, requests[root], false); err != nil {
			return err
		}
	}
	return nil
}

// NormalizeSetupPackArg expands the `owner/repo` shorthand to a clone URL.
func NormalizeSetupPackArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Count(arg, "/") == 1 && !strings.Contains(arg, ":") && !strings.HasPrefix(arg, ".") && !strings.HasPrefix(arg, "~") {
		return "https://github.com/" + arg + ".git"
	}
	return arg
}
