// keys.go — the interactive 1Password ref collection for `pix models add
// <provider>`: the ONE place a provider credential is now solicited.
//
// `pix setup` no longer prompts for keys. It probes the key store (tri-state: a
// store that did not answer is unknown, never "no key") and names this command
// as the fix.
package models

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"pix/host/hostenv"
	"pix/host/secret"
)

// providerKeyPromptAttempts caps the reprompts for one provider, since a human
// who keeps mistyping (or an unattended TTY feeding garbage) must not hang the
// command forever.
const providerKeyPromptAttempts = 3

// promptProviderRef prompts for a NEW op:// ref for a provider with none
// configured yet. It validates that the ref resolves via `op read` to a
// non-empty value BEFORE returning it, and never echoes the resolved value.
// Empty input or EOF is a hard failure (a key is mandatory here, not optional
// to skip); an invalid or unresolvable ref explains why and reprompts.
func promptProviderRef(env hostenv.Env, sc *bufio.Scanner, out io.Writer, p secret.ProviderKeyRef) (ref, value string, ok bool) {
	for attempt := 1; attempt <= providerKeyPromptAttempts; attempt++ {
		fmt.Fprintf(out, "  %s: paste a 1Password ref (op://Vault/Item/field): ", p.Name)
		if !sc.Scan() {
			fmt.Fprintln(out, "")
			fmt.Fprintf(out, "  %s: no input — a 1Password ref is required; setup cannot continue.\n", p.Name)
			return "", "", false
		}
		ref = secret.NormalizeOpRef(sc.Text())
		switch {
		case ref == "":
			fmt.Fprintf(out, "    a ref is required for %s (it is not optional) — try again.\n", p.Name)
		case !validOpRefSyntax(ref):
			fmt.Fprintln(out, "    not a valid op:// ref (want op://Vault/Item/field) — try again.")
		default:
			if val, resolves := secret.OpReadNonEmpty(env, ref); resolves {
				return ref, val, true
			}
			fmt.Fprintf(out, "    could not resolve that ref for %s via `op read` (check the vault/item/field) — try again.\n", p.Name)
		}
	}
	fmt.Fprintf(out, "  %s: too many invalid attempts — aborting setup.\n", p.Name)
	return "", "", false
}

// validOpRefSyntax requires the op:// prefix, rejects an unfilled
// <vault>/<item>/<field> placeholder, and rejects control characters — defense
// in depth beside op read's own validation, so a pasted literal secret or a
// copy/paste artifact is never written as if it were a ref.
func validOpRefSyntax(ref string) bool {
	if !strings.HasPrefix(ref, "op://") || secret.HasPlaceholder(ref) {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// requireOnePassword is the ONE precondition of the direct-API-key path: `op`
// must be installed, because a 1Password ref is the only shape a provider
// credential may take. It is checked at the moment a ref is about to be
// solicited and nowhere else — a keyless host (a pack gateway, verified Ollama)
// must never be dragged through an irrelevant 1Password flow. Setup does not
// install Homebrew packages on the user's behalf, so this names the command.
func requireOnePassword(env hostenv.Env) error {
	if _, err := env.LookPath("op"); err != nil {
		return fmt.Errorf("1Password CLI (op) is required to store a provider key as a ref, and it is not on PATH\n  brew install 1password-cli")
	}
	return nil
}
