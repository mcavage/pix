package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"pi-stack/host/config"
)

// secret is the read-mostly home of the 1Password / op-refs.env concept. It
// never writes a secret to disk (values belong in 1Password); it only seeds the
// refs TEMPLATE, opens it in $EDITOR, and reports op / ref state. See the mental
// model in config.OpRefsMentalModel.

// runSecretCmd is the `secret` verb tree: status (default), edit, check.
func runSecretCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(secretUsage)
		return
	}
	sub := "status"
	var rest []string
	if len(argv) > 0 {
		sub = argv[0]
		rest = argv[1:]
	}
	env := defaultShellEnv()
	switch sub {
	case "status", "edit", "check":
		// Reject trailing junk (unknown args/flags). Leading -h/--help is already
		// handled by the wantsHelp gate above.
		if len(rest) > 0 {
			fmt.Fprintf(os.Stderr, "pi-stack secret %s: unexpected argument %q\n", sub, rest[0])
			os.Exit(2)
		}
	default:
		fmt.Fprintf(os.Stderr, "pi-stack secret: unknown subcommand %q (want: status, edit, check)\n", sub)
		os.Exit(2)
	}
	switch sub {
	case "status":
		runSecretStatus(env, os.Stdout)
	case "edit":
		runSecretEdit(env, os.Stdout)
	case "check":
		runSecretCheck(env, os.Stdout)
	}
}

// opRef is one parsed KEY=VALUE line of op-refs.env.
type opRef struct {
	key         string
	value       string
	isRef       bool // value starts with op://
	placeholder bool // value still carries an unfilled <...> placeholder
	nonSecret   bool // KEY is on the documented non-secret allowlist
}

// parseOpRefs parses op-refs.env content into its non-comment KEY=VALUE lines,
// classifying each value as a filled op:// ref, an unfilled placeholder, or a
// documented non-secret literal. It NEVER surfaces the raw value to callers that
// print — classification only.
func parseOpRefs(content string) []opRef {
	var refs []opRef
	for _, ln := range strings.Split(content, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		eq := strings.IndexByte(t, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(t[:eq])
		val := strings.TrimSpace(t[eq+1:])
		refs = append(refs, opRef{
			key:         key,
			value:       val,
			isRef:       strings.HasPrefix(val, "op://"),
			placeholder: hasPlaceholder(val),
			nonSecret:   config.NonSecretOpRefsKeys[key],
		})
	}
	return refs
}

// hasPlaceholder reports whether a value still carries an unfilled template
// placeholder (an angle-bracketed token like <vault> / <item> / <field>).
func hasPlaceholder(val string) bool {
	i := strings.IndexByte(val, '<')
	if i < 0 {
		return false
	}
	return strings.IndexByte(val[i:], '>') > 0
}

// opInstalled reports whether the 1Password CLI (op) is on PATH.
func opInstalled(env shellEnv) bool {
	if env.lookPath == nil {
		return false
	}
	_, err := env.lookPath("op")
	return err == nil
}

// opSignedIn reports whether op has an account CONFIGURED, using ONLY safe
// metadata (`op account list`) — never `op read` or an on-disk `op signin`. A
// non-empty account list proves an account is configured, NOT that the session
// is unlocked/usable. Best-effort: any error is "no account configured", never a
// crash.
func opSignedIn(env shellEnv) bool {
	if !opInstalled(env) || env.run == nil {
		return false
	}
	out, err := env.run("op", "account", "list")
	return err == nil && strings.TrimSpace(out) != ""
}

// opRefsContent resolves + reads op-refs.env through the injected env, returning
// its path, contents, and whether it exists.
func opRefsContent(env shellEnv) (path, content string, exists bool) {
	path = defaultOpRefsPath(env)
	if env.readFile == nil {
		return path, "", false
	}
	c, err := env.readFile(path)
	if err != nil {
		return path, "", false
	}
	return path, c, true
}

// runSecretStatus prints op install/sign-in state + op-refs.env presence and,
// per configured ref, filled-vs-placeholder. It NEVER prints a secret value.
func runSecretStatus(env shellEnv, out io.Writer) {
	fmt.Fprintln(out, "Secrets (1Password):")
	fmt.Fprintln(out, indent(config.OpRefsMentalModel))
	fmt.Fprintln(out)

	switch {
	case !opInstalled(env):
		fmt.Fprintln(out, "  op (1Password CLI): ✗ not installed — https://developer.1password.com/docs/cli")
	case !opSignedIn(env):
		fmt.Fprintln(out, "  op (1Password CLI): · installed, no account configured — run: op signin")
	default:
		fmt.Fprintln(out, "  op (1Password CLI): ✓ installed + account configured (advisory)")
	}

	path, content, exists := opRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "  op-refs.env: ✗ not present — create it with: pi-stack secret edit\n  (%s)\n", path)
		return
	}
	fmt.Fprintf(out, "  op-refs.env: ✓ %s\n", path)
	refs := parseOpRefs(content)
	if len(refs) == 0 {
		fmt.Fprintln(out, "  refs: (none set yet — add ENV_VAR=op://vault/item/field lines)")
		return
	}
	for _, r := range refs {
		switch {
		case r.nonSecret:
			fmt.Fprintf(out, "    · %s (non-secret env)\n", r.key)
		case r.isRef && r.placeholder:
			fmt.Fprintf(out, "    ✗ %s = placeholder (fill in the op:// ref)\n", r.key)
		case r.isRef:
			fmt.Fprintf(out, "    ✓ %s = op:// ref\n", r.key)
		case r.placeholder:
			fmt.Fprintf(out, "    ✗ %s = placeholder (fill in the op:// ref)\n", r.key)
		case looksSecretShaped(r.key, r.value):
			// A non-ref that looks like a pasted secret: flag WITHOUT printing the value.
			fmt.Fprintf(out, "    ✗ %s = possible pasted secret — replace with op://vault/item/field\n", r.key)
		default:
			// Any other non-ref, non-allowlisted value: refs-only policy => flag it
			// WITHOUT printing the value.
			fmt.Fprintf(out, "    ✗ %s = not an op:// ref — this file is refs-only; use op://vault/item/field or move it to the non-secret allowlist\n", r.key)
		}
	}
}

// runSecretEdit seeds op-refs.env (via the ONE seeder) when absent, then opens
// it in $EDITOR/$VISUAL. With no editor set it prints the path so the user can
// open it themselves. It never writes a secret — only the refs template.
func runSecretEdit(env shellEnv, out io.Writer) {
	path, created, err := config.SeedOpRefs()
	if err != nil {
		fmt.Fprintf(out, "pi-stack secret edit: could not seed %s: %v\n", path, err)
		os.Exit(1)
	}
	if created {
		fmt.Fprintf(out, "seeded a template op-refs.env at %s\n", path)
	}
	editor := firstNonEmptyEnv(env, "VISUAL", "EDITOR")
	if editor == "" {
		fmt.Fprintf(out, "no $EDITOR/$VISUAL set — edit it yourself:\n  %s\n", path)
		return
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "pi-stack secret edit: %s %s: %v\n", editor, path, err)
		os.Exit(1)
	}
}

// runSecretCheck resolves every op:// ref in op-refs.env with `op read` and
// reports OK/FAIL per KEY. It NEVER prints the resolved value (only OK/FAIL).
// Degrades clearly when op is absent or not signed in.
func runSecretCheck(env shellEnv, out io.Writer) {
	path, content, exists := opRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "op-refs.env not found (%s) — create it with: pi-stack secret edit\n", path)
		os.Exit(3)
	}
	if !opInstalled(env) {
		fmt.Fprintln(out, "op (1Password CLI) not installed — https://developer.1password.com/docs/cli")
		os.Exit(3)
	}
	if !opSignedIn(env) {
		fmt.Fprintln(out, "op is installed but no account configured — run: op signin, then retry")
		os.Exit(3)
	}
	refs := parseOpRefs(content)
	var checked, failed int
	for _, r := range refs {
		if !r.isRef {
			continue
		}
		checked++
		if r.placeholder {
			fmt.Fprintf(out, "  ✗ %s: FAIL (unfilled placeholder)\n", r.key)
			failed++
			continue
		}
		// op read resolves the ref; we discard the value (stdout) and only look
		// at the exit status, so no secret is ever printed.
		if _, err := env.run("op", "read", r.value); err != nil {
			fmt.Fprintf(out, "  ✗ %s: FAIL\n", r.key)
			failed++
		} else {
			fmt.Fprintf(out, "  ✓ %s: OK\n", r.key)
		}
	}
	if checked == 0 {
		fmt.Fprintln(out, "no op:// refs to check (add ENV_VAR=op://vault/item/field lines)")
		return
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d of %d refs failed to resolve.\n", failed, checked)
		os.Exit(1)
	}
	fmt.Fprintf(out, "all %d refs resolve.\n", checked)
}

// firstNonEmptyEnv returns the first non-empty value among the given env vars.
func firstNonEmptyEnv(env shellEnv, names ...string) string {
	if env.getenv == nil {
		return ""
	}
	for _, n := range names {
		if v := strings.TrimSpace(env.getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// indent prefixes every line of s with two spaces (for nested help/status text).
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}

// anyOpWrappedServer reports whether any configured MCP server makes the Secrets
// (1Password) group relevant: a NON-gog server. gog is deliberately excluded —
// it authenticates via OAuth (`gog auth login`), never an op-refs token, so a
// gog-only config needs no op-refs.env (mcp-register registers gog BARE for
// exactly this reason, and setup's Step 4 skips it via hasNonGogMCP). gog's ONE
// conditional op-refs need — a headless keyring password — is owned by the gog
// group's headless-spawn check, not this group, so counting gog here produced a
// phantom `pi-stack secret edit` TODO on a fresh gog-only install. Remote
// gateway-catalog servers don't strictly need op-refs either, but distinguishing
// them requires probing pi-stack-host; for this coarse gate any non-gog name
// counts (mirrors setup's hasNonGogMCP).
func anyOpWrappedServer(cfg *config.Config) bool {
	for _, m := range cfg.MCP {
		if m != "gog" {
			return true
		}
	}
	return false
}
