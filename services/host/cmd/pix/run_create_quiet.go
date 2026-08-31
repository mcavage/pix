// run_create_quiet.go — the stdio wiring for the ONE non-interactive child
// a launch runs: `sbx env create <effective>`.
//
// sbx 0.41 renders its own plan for the document it is given and asks
// "Approve this plan?" before creating. For Pix that is a SECOND approval
// of a document this launcher already composed, already fingerprinted, and
// already put through its own trust gate (run_trust.go) — and its text
// includes the pix-memory Gateway URL with the bearer token in the query
// string, which must never reach a terminal, a diagnostic, or a log (the
// same rule container.RedactMemoryURLToken exists for).
//
// So the create child, and ONLY the create child, gets:
//   - the approval on stdin, written by this launcher, after its own gate
//     said yes;
//   - stdout/stderr captured into a bounded buffer instead of the user's
//     terminal; on success nothing is printed at all.
//
// On failure the captured text is still the best diagnostic there is, so it
// is printed — bounded, terminal-safe, and with the memory token and every
// configured secret value redacted. The raw plan is never displayed.
//
// The interactive `sbx exec` session keeps ordinary inherited stdio: it IS
// the user's session. The two children are told apart by the SessionDeps
// seam they come from (Spawn vs SpawnCreate), never by sniffing argv.
package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/secret"
	"pix/host/sys"
)

// sbxCreateApproval is what this launcher writes to the create child's
// stdin. It is deliberately more than one "y": sbx may ask nothing at all
// (an older build, or a non-TTY default), and a prompt-free create must
// simply ignore the bytes rather than block on a reader that closed.
const sbxCreateApproval = "y\ny\n"

// createCaptureLimit bounds what a failed create may print. Enough for a
// real sbx error, small enough that a runaway plan cannot flood a terminal.
const createCaptureLimit = 8 << 10

// createCapture is the bounded, concurrency-safe sink for the create
// child's stdout AND stderr (one child writes both, from two goroutines
// inside os/exec). It keeps the FIRST createCaptureLimit bytes: an sbx
// failure states its reason early, and keeping the head means a long plan
// cannot push the reason out of the buffer.
type createCapture struct {
	mu       sync.Mutex
	buf      []byte
	overflow bool
}

func (c *createCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := createCaptureLimit - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
			c.overflow = true
		} else {
			c.buf = append(c.buf, p...)
		}
	} else if len(p) > 0 {
		c.overflow = true
	}
	return len(p), nil
}

func (c *createCapture) text() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf), c.overflow
}

// quietCreateSpawn builds the create child: the approval on stdin, output
// into cap, and this process's environment. bin is the sbx binary name (the
// production caller always passes "sbx"; a test points it at a fixture).
func quietCreateSpawn(bin string, cap *createCapture) func(argv []string) *exec.Cmd {
	return func(argv []string) *exec.Cmd {
		cmd := exec.Command(bin, argv...)
		cmd.Stdin = strings.NewReader(sbxCreateApproval)
		cmd.Stdout, cmd.Stderr = cap, cap
		cmd.Env = os.Environ()
		return cmd
	}
}

// createFailureDiagnostic renders what a FAILED create printed: redacted,
// terminal-safe, and marked when it was truncated. An empty capture returns
// "" so a caller adds nothing to an error that already says enough.
func createFailureDiagnostic(cap *createCapture, secrets []string) string {
	raw, overflowed := cap.text()
	redacted := strings.TrimRight(redactCreateOutput(raw, secrets), "\n")
	if redacted == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("pix run: `sbx env create` said:\n")
	for _, line := range strings.Split(redacted, "\n") {
		b.WriteString("  " + sys.TerminalSafe(line) + "\n")
	}
	if overflowed {
		b.WriteString("  (output truncated)\n")
	}
	return b.String()
}

// redactCreateOutput removes every credential shape this text can carry:
// any literal value the caller knows (this home's pix-memory bearer
// token), the pix-memory URL's `?token=` value at EVERY occurrence, any
// Authorization/Bearer value, and anything assigned to a configured secret
// NAME. It is deliberately applied to the WHOLE captured text rather than
// to a parsed structure: this is sbx's output, not ours, and a redactor
// that only understood the shapes we expected would leak the first shape we
// did not.
func redactCreateOutput(raw string, secrets []string) string {
	out := raw
	for _, s := range secrets {
		if len(s) >= 8 {
			out = strings.ReplaceAll(out, s, container.RedactedTokenPlaceholder)
		}
	}
	out = redactAllTokenParams(out)
	out = redactBearer(out)
	return redactSecretAssignments(out, configuredSecretNames())
}

// redactSecretAssignments masks the VALUE of any `NAME=...` or `NAME: ...`
// this home has configured a credential ref for. The name itself stays
// visible: which credential a failed create was wiring is exactly the
// diagnostic a user needs, and it is not the secret.
func redactSecretAssignments(s string, names []string) string {
	if len(names) == 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		for _, n := range names {
			idx := strings.Index(line, n)
			if idx < 0 {
				continue
			}
			rest := line[idx+len(n):]
			sep := strings.IndexAny(rest, "=:")
			if sep < 0 || strings.TrimSpace(rest[:sep]) != "" {
				continue
			}
			value := strings.TrimSpace(rest[sep+1:])
			if value == "" {
				continue
			}
			lines[i] = line[:idx+len(n)] + rest[:sep+1] + " " + container.RedactedTokenPlaceholder
		}
	}
	return strings.Join(lines, "\n")
}

// configuredSecretNames is the NAME list from this PIX_HOME's own refs file
// (op:// references only; no value is ever resolved here — resolving one to
// build a redactor would be the very disclosure this file exists to
// prevent). An unreadable home yields no names and the shape-based
// redactions above still apply.
func configuredSecretNames() []string {
	home, err := pixhome.Resolve()
	if err != nil {
		return nil
	}
	refs, err := secret.LoadRefs(home)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range refs {
		if r.Key != "" {
			out = append(out, r.Key)
		}
	}
	return out
}

// redactAllTokenParams applies container.RedactMemoryURLToken to EVERY
// `token=` occurrence, not just the first: a plan can name the memory
// endpoint more than once (the registration, the effective document, an
// error echoing it back).
func redactAllTokenParams(s string) string {
	const key = "token="
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, key)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := i + len(key)
		for end < len(rest) && !strings.ContainsRune("&\"' \t\r\n<>", rune(rest[end])) {
			end++
		}
		b.WriteString(rest[:i+len(key)])
		b.WriteString(container.RedactedTokenPlaceholder)
		rest = rest[end:]
	}
}

// redactBearer masks an `Authorization: Bearer <value>` or a bare
// `Bearer <value>` however it is spelled.
func redactBearer(s string) string {
	lower := strings.ToLower(s)
	const key = "bearer "
	var b strings.Builder
	off := 0
	for {
		i := strings.Index(lower[off:], key)
		if i < 0 {
			b.WriteString(s[off:])
			return b.String()
		}
		start := off + i + len(key)
		end := start
		for end < len(s) && !strings.ContainsRune("\"' \t\r\n", rune(s[end])) {
			end++
		}
		b.WriteString(s[off:start])
		b.WriteString(container.RedactedTokenPlaceholder)
		off = end
	}
}

// createSecretValues is the one literal value this launch can read and must
// never print: this PIX_HOME's pix-memory bearer token. A home that cannot
// be resolved, or a token that was never generated, yields nothing and the
// shape-based redactions still apply.
func createSecretValues() []string {
	home, err := pixhome.Resolve()
	if err != nil {
		return nil
	}
	tok, err := container.ReadMemoryAuthToken(home)
	if err != nil || tok == "" {
		return nil
	}
	return []string{tok}
}

// interactiveSessionSpawn is the ORDINARY session child: real inherited
// stdio, because it is the user's terminal session. Kept beside the quiet
// create builder so the difference between the two is one file's worth of
// reading, not a hunt through run_cmd.go.
func interactiveSessionSpawn(bin string) func(argv []string) *exec.Cmd {
	return func(argv []string) *exec.Cmd {
		cmd := exec.Command(bin, argv...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		// No credential bearer: host MCP servers authenticate on the host, so
		// the sandbox never sees a token.
		cmd.Env = os.Environ()
		return cmd
	}
}

var _ io.Writer = (*createCapture)(nil)
