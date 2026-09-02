// parallel.go — the optional Parallel web-search tool key: whether THIS
// PIX_HOME configures it, and the one-shot, TTY-only, default-No offer to
// capture an op:// ref for it. ToolKeyRefOrder (sync.go) already says the
// rule this file exists to honor: absence is degraded search, never a
// launch blocker, so everything here only ever REPORTS or OFFERS, and
// never requires or resolves.
package secret

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"pix/host/cli"
	"pix/host/hostenv"
)

// ParallelSearchKeyRef names the Parallel web-search tool key, the exact
// EnvVar/Name pair ToolKeyRefOrder declares, so this file's explain/offer
// step and the sync set can never disagree about it.
var ParallelSearchKeyRef = ProviderKeyRef{EnvVar: "PARALLEL_API_KEY", Name: "parallel"}

// ConfiguredParallelSearchRef reports whether THIS PIX_HOME configures a
// filled PARALLEL_API_KEY op:// ref. False means fallback (degraded) web
// search, not a missing feature: pi-web-access's parallel.ts backend simply
// has no key to send, and every other configured search backend still runs.
func ConfiguredParallelSearchRef(env hostenv.Env) bool {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return false
	}
	r, ok := firstRefsIn(content, map[string]string{ParallelSearchKeyRef.EnvVar: ParallelSearchKeyRef.Name})[ParallelSearchKeyRef.EnvVar]
	return ok && !r.Placeholder
}

// OfferParallelSearchKey is the OPT-IN (default-No) setup step that captures
// an op:// ref for the optional Parallel web-search key. It fires ONLY on a
// TTY, and only when no PARALLEL_API_KEY ref is configured yet — never a
// repeat ask once one exists. Declining (the default, a bare Enter, or a
// pasted value that is not an op:// ref) leaves secrets.env exactly as it
// was; nothing here blocks a launch either way, and nothing it prints is a
// secret value — only the op:// REFERENCE the user pastes, which names a
// 1Password location, not a credential.
func OfferParallelSearchKey(env hostenv.Env, in io.Reader, out io.Writer, tty bool) {
	if !tty || in == nil || ConfiguredParallelSearchRef(env) {
		return
	}
	if !cli.ConfirmYN(in, out, "Configure Parallel web search now (op:// ref for PARALLEL_API_KEY)? [y/N]: ", false) {
		return
	}
	fmt.Fprint(out, "Paste an op:// ref (op://Vault/Item/field), or Enter to skip: ")
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return
	}
	ref := NormalizeOpRef(sc.Text())
	if ref == "" {
		return
	}
	if !strings.HasPrefix(ref, "op://") {
		fmt.Fprintln(out, "  skipped: not an op:// ref")
		return
	}
	if err := WriteOpRefQuiet(env, ParallelSearchKeyRef.EnvVar, ref); err != nil {
		fmt.Fprintf(out, "  could not save: %v\n", err)
		return
	}
	fmt.Fprintln(out, "Saved. Parallel web search is now configured; it resolves into that run's own sandbox at launch.")
}
