package onboard

import (
	"pix/host/knowledge"
	"pix/host/sys"
	"time"

	"fmt"
	"io"
	"path/filepath"
	"strings"

	"pix/host/config"
)

// Shared onboarding helpers. The old interactive `pix setup` wizard was
// deleted in favor of the agentic in-session flow (docs/design/onboarding.md);
// these are the reusable pieces that outlived it: the IO struct + TTY probe used
// by reconcile prompts and reset, the gog auth probe (also used by doctor +
// status), the knowledge-source setup, and a line reader.

// cli.IO carries the streams + a TTY flag so callers can exercise the non-TTY
// path hermetically.
// GogAuthed reports whether gog has usable auth for a specific account: it is on
// PATH and an account-scoped `gog --account <account> auth status` exits 0.
// Setting an account email does NOT imply completed OAuth, so callers pass the
// CONFIGURED account and probe THAT account before claiming gog is ready. The
// probe is BOUNDED (gogAuthTimeout) so a network round-trip can never hang a
// fast command: real callers wire env.probe and we run our own short-timeout
// exec; tests leave probe nil and use the hermetic env.Run. Best-effort: any gap
// (gog absent, a timeout, status errors) is "not authed", never a crash.
func GogAuthed(env sys.Exec, account string) bool {
	if _, err := env.LookPath("gog"); err != nil {
		return false
	}
	_, timedOut, err := env.RunWithin(gogAuthTimeout, "gog", "--account", account, "auth", "status")
	return !timedOut && err == nil
}

// setupKnowledge sets up the global knowledge base from a user-supplied source,
// reusing the `knowledge` verb's logic (no duplicated OKF scaffold or config
// wiring). A git URL is cloned/pulled and used in place (knowledge.Use); a local
// path is scaffolded-if-new and wired (knowledge.Init, which never clobbers an
// existing bundle). Both add the bundle to knowledge_bundles + enable the
// knowledge service and Save().
func setupKnowledge(cfg *config.Config, ref string, out io.Writer) error {
	ref = strings.TrimSpace(ref)
	if knowledge.IsGitURL(ref) {
		return knowledge.Use(cfg, ref, out)
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", ref, err)
	}
	return knowledge.Init(cfg, abs, out)
}

// gogAuthTimeout bounds the `gog auth status` probe so the fast, read-only
// `status` command can never hang on a network round-trip (mirrors doctor's
// bounded probes). On timeout or error gog is treated as not-authed.
const gogAuthTimeout = 2 * time.Second
