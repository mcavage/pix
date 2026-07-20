package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"pi-stack/host/config"
)

// secret is the CRUD home of the 1Password / op-refs.env concept. It never
// writes a resolved secret to disk (values live in 1Password); it only reads,
// classifies, and edits the REFS themselves (op://vault/item/field lines). See
// the mental model in config.OpRefsMentalModel.

// envVarNameRe validates a `secret set`/`secret rm` KEY looks like a shell env
// var name, so a typo'd flag or a pasted ref never lands as the key half of a
// line.
var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runSecretCmd is the `secret` verb tree: ls (default) | set | rm | check.
func runSecretCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(secretUsage)
		return
	}
	sub := "ls"
	var rest []string
	if len(argv) > 0 {
		sub = argv[0]
		rest = argv[1:]
	}
	env := defaultShellEnv()
	switch sub {
	case "ls", "check", "sync":
		// Reject trailing junk (unknown args/flags). Leading -h/--help is already
		// handled by the wantsHelp gate above.
		if len(rest) > 0 {
			fmt.Fprintf(os.Stderr, "pi-stack secret %s: unexpected argument %q\n", sub, rest[0])
			os.Exit(2)
		}
	case "set":
		if len(rest) != 2 {
			fmt.Fprintf(os.Stderr, "pi-stack secret set: want exactly 2 arguments: ENV_VAR op://vault/item/field (got %d)\n", len(rest))
			os.Exit(2)
		}
	case "rm":
		if len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "pi-stack secret rm: want exactly 1 argument: ENV_VAR (got %d)\n", len(rest))
			os.Exit(2)
		}
	default:
		fmt.Fprintf(os.Stderr, "pi-stack secret: unknown subcommand %q (want: ls, set, rm, check, sync)\n", sub)
		os.Exit(2)
	}
	switch sub {
	case "ls":
		runSecretLs(env, os.Stdout)
	case "set":
		runSecretSet(env, os.Stdout, rest[0], rest[1])
	case "rm":
		runSecretRm(env, os.Stdout, rest[0])
	case "check":
		runSecretCheck(env, os.Stdout)
	case "sync":
		runSecretSync(env, os.Stdout)
	}
}

// normalizeOpRef cleans a pasted op:// reference: trims whitespace and strips ONE
// layer of matching surrounding quotes. 1Password's "Copy Secret Reference" hands
// you the ref WITH double quotes ("op://Vault/Item/field"), which would otherwise
// fail the op:// prefix check. Applied at every paste boundary.
func normalizeOpRef(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
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

// runSecretLs prints op install/sign-in state + op-refs.env presence and, per
// configured ref, filled-vs-placeholder-vs-pasted-secret. It NEVER prints a
// secret value. This is the default `secret` action (no subcommand).
func runSecretLs(env shellEnv, out io.Writer) {
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
		fmt.Fprintf(out, "  op-refs.env: ✗ not present — create it with: pi-stack secret set <ENV_VAR> op://vault/item/field\n  (%s)\n", path)
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

// runSecretSet is the ONE authoring primitive: it upserts ENV_VAR=value into
// op-refs.env, no editor involved. It enforces the refs-only policy — value
// must be an op:// ref unless ENV_VAR is on config.NonSecretOpRefsKeys — and
// URL-encodes a raw space in a ref (a spaced 1Password field name, e.g. "api
// key") so `op run --env-file` can parse the line. It seeds the file (via the
// ONE seeder, config.SeedOpRefs) if absent, so the header/mental-model comment
// is always present, then upserts preserving every other line untouched. It
// prints the REF it stored (never a resolved secret — a ref is safe to echo).
func runSecretSet(env shellEnv, out io.Writer, key, value string) {
	if !envVarNameRe.MatchString(key) {
		fmt.Fprintf(out, "pi-stack secret set: %q does not look like an env var name (want %s)\n", key, envVarNameRe.String())
		os.Exit(2)
	}
	// 1Password's "Copy Secret Reference" wraps the ref in quotes; strip them so
	// a pasted `"op://…"` is accepted (only for an op:// ref — a genuine literal
	// value keeps its quotes and still trips the refs-only guard below).
	if nv := normalizeOpRef(value); strings.HasPrefix(nv, "op://") {
		value = nv
	}

	// Reject control characters (newline, carriage return, NUL, ...) in the value.
	// op-refs.env is line-oriented and consumed by `op run --env-file`, so a value
	// carrying a newline could inject a SECOND, attacker-controlled KEY=value line
	// (e.g. a pasted plaintext secret) into the file. One ref = one clean line.
	if i := strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		fmt.Fprintf(out, "pi-stack secret set: %s value contains a control character at byte %d; op-refs.env is one ref per line, so newlines/control chars are not allowed\n", key, i)
		os.Exit(2)
	}

	isRef := strings.HasPrefix(value, "op://")
	if !isRef && !config.NonSecretOpRefsKeys[key] {
		if config.LooksSecretShaped(key, value) {
			fmt.Fprintf(out, "pi-stack secret set: %s looks like a pasted secret — this file is refs-only; pass op://vault/item/field, or your secret would land on disk\n", key)
		} else {
			fmt.Fprintf(out, "pi-stack secret set: %s is not an op:// ref — this file is refs-only; pass op://vault/item/field, or your secret would land on disk\n", key)
		}
		os.Exit(2)
	}

	if isRef && strings.Contains(value, " ") {
		encoded := strings.ReplaceAll(value, " ", "%20")
		fmt.Fprintf(out, "note: encoded a space in the ref for %s (op run can't parse a literal space)\n", key)
		value = encoded
	}

	path, content, exists := opRefsContent(env)
	if !exists {
		seededPath, _, err := config.SeedOpRefs()
		if err != nil {
			fmt.Fprintf(out, "pi-stack secret set: could not seed %s: %v\n", seededPath, err)
			os.Exit(1)
		}
		path = seededPath
		content = config.OpRefsTemplate
		if env.readFile != nil {
			if c, err := env.readFile(path); err == nil {
				content = c
			}
		}
	}

	newContent := upsertOpRef(content, key, value)
	if env.writeFile == nil {
		fmt.Fprintf(out, "pi-stack secret set: cannot write %s (no writer available)\n", path)
		os.Exit(1)
	}
	if err := env.writeFile(path, []byte(newContent), 0o600); err != nil {
		fmt.Fprintf(out, "pi-stack secret set: could not write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "set %s = %s in %s\n", key, value, path)
}

// runSecretRm removes ENV_VAR's line from op-refs.env, preserving every other
// line (comments, blanks, other refs). A missing file or a key that was never
// present is a clean, exit-0 no-op — `rm` is idempotent.
func runSecretRm(env shellEnv, out io.Writer, key string) {
	path, content, exists := opRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "op-refs.env not found (%s) — nothing to remove\n", path)
		return
	}
	newContent, removed := removeOpRef(content, key)
	if !removed {
		fmt.Fprintf(out, "no ref named %s in %s\n", key, path)
		return
	}
	if env.writeFile == nil {
		fmt.Fprintf(out, "pi-stack secret rm: cannot write %s (no writer available)\n", path)
		os.Exit(1)
	}
	if err := env.writeFile(path, []byte(newContent), 0o600); err != nil {
		fmt.Fprintf(out, "pi-stack secret rm: could not write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "removed %s from %s\n", key, path)
}

// upsertOpRef returns content with KEY=value in place of an existing KEY= line
// (comments/blanks/other entries untouched), or appended at the end if KEY was
// not already present.
func upsertOpRef(content, key, value string) string {
	newLine := key + "=" + value
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // drop the trailing empty element from a final "\n"
	}
	found := false
	for i, ln := range lines {
		if opRefLineKey(ln) == key {
			lines[i] = newLine
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, newLine)
	}
	return strings.Join(lines, "\n") + "\n"
}

// removeOpRef returns content with KEY's line dropped (everything else
// preserved), and whether KEY was found at all.
func removeOpRef(content, key string) (out string, removed bool) {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if opRefLineKey(ln) == key {
			removed = true
			continue
		}
		kept = append(kept, ln)
	}
	if !removed {
		return content, false
	}
	if len(kept) == 0 {
		return "", true
	}
	return strings.Join(kept, "\n") + "\n", true
}

// opRefLineKey returns the KEY of a raw op-refs.env line, or "" if the line is
// blank, a comment, or not a KEY=VALUE line at all.
func opRefLineKey(ln string) string {
	t := strings.TrimSpace(ln)
	if t == "" || strings.HasPrefix(t, "#") {
		return ""
	}
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return ""
	}
	return strings.TrimSpace(t[:eq])
}

// runSecretCheck resolves every op:// ref in op-refs.env with `op read` and
// reports OK/FAIL per KEY. It NEVER prints the resolved value (only OK/FAIL).
// Degrades clearly when op is absent or not signed in.
func runSecretCheck(env shellEnv, out io.Writer) {
	path, content, exists := opRefsContent(env)
	if !exists {
		fmt.Fprintf(out, "op-refs.env not found (%s) — create it with: pi-stack secret set <ENV_VAR> op://vault/item/field\n", path)
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
// phantom `pi-stack secret set` TODO on a fresh gog-only install. Remote
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
