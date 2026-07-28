package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"pix/host/config"
)

// Shared onboarding helpers. The old interactive `pix setup` wizard was
// deleted in favor of the agentic in-session flow (docs/design/onboarding.md);
// these are the reusable pieces that outlived it: the IO struct + TTY probe used
// by reconcile prompts and reset, the gog auth probe (also used by doctor +
// status), the knowledge-source setup, and a line reader.

// setupIO carries the streams + a TTY flag so callers can exercise the non-TTY
// path hermetically.
type setupIO struct {
	in    io.Reader
	out   io.Writer
	isTTY bool
}

// gogAuthed reports whether gog has usable auth for a specific account: it is on
// PATH and an account-scoped `gog --account <account> auth status` exits 0.
// Setting an account email does NOT imply completed OAuth, so callers pass the
// CONFIGURED account and probe THAT account before claiming gog is ready. The
// probe is BOUNDED (gogAuthTimeout) so a network round-trip can never hang a
// fast command: real callers wire env.probe and we run our own short-timeout
// exec; tests leave probe nil and use the hermetic env.run. Best-effort: any gap
// (gog absent, a timeout, status errors) is "not authed", never a crash.
func gogAuthed(env shellEnv, account string) bool {
	if env.lookPath == nil {
		return false
	}
	if _, err := env.lookPath("gog"); err != nil {
		return false
	}
	if env.probe != nil {
		_, timedOut, err := runWithTimeoutD(gogAuthTimeout, "gog", "--account", account, "auth", "status")
		return !timedOut && err == nil
	}
	if env.run == nil {
		return false
	}
	_, err := env.run("gog", "--account", account, "auth", "status")
	return err == nil
}

// setupKnowledge sets up the global knowledge base from a user-supplied source,
// reusing the `knowledge` verb's logic (no duplicated OKF scaffold or config
// wiring). A git URL is cloned/pulled and used in place (knowledgeUse); a local
// path is scaffolded-if-new and wired (knowledgeInit, which never clobbers an
// existing bundle). Both add the bundle to knowledge_bundles + enable the
// knowledge service and Save().
func setupKnowledge(cfg *config.Config, ref string, out io.Writer) error {
	ref = strings.TrimSpace(ref)
	if isGitURL(ref) {
		return knowledgeUse(cfg, ref, out)
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", ref, err)
	}
	return knowledgeInit(cfg, abs, out)
}

// promptLine reads a single trimmed line from sio.in after writing prompt.
func promptLine(sio setupIO, prompt string) string {
	fmt.Fprint(sio.out, prompt)
	line, _ := bufio.NewReader(sio.in).ReadString('\n')
	return strings.TrimSpace(line)
}

// isTTY reports whether r is an interactive terminal. Any non-*os.File (e.g. a
// test buffer) or a redirected/piped stdin is treated as non-interactive.
func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
