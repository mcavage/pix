// secret_sync.go: resolve provider-key op:// refs from 1Password and push the
// values into sbx secrets (the sandbox proxy's store), so the sandbox reaches
// the models exactly as before while 1Password stays the single store and
// pi-stack owns only the REFS. No value ever lands on pi-stack's disk or in the
// VM: `op read` resolves in-process, we hand the value straight to `sbx secret
// set`, and drop it.
//
// This mirrors host mode, which already resolves op:// refs via `op run
// --env-file` (hostmode.env). The sandbox needs the extra sync step only because
// its keys ride the sbx proxy's secret store rather than pi's process env.
//
// All op/sbx calls go through env.run so this is unit-testable with fakes; the
// LIVE run (real 1Password + sbx) happens on the host (op is a host tool).
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// providerKeyRefOrder is the deterministic provider list for prompting (env var
// used in op-refs.env -> sbx secret name).
var providerKeyRefOrder = []struct{ envVar, name string }{
	{"ANTHROPIC_API_KEY", "anthropic"},
	{"OPENAI_API_KEY", "openai"},
	{"GEMINI_API_KEY", "google"},
}

// offerOnePasswordKeys is the OPT-IN (default-No) setup step that wires model
// keys to 1Password. It fires ONLY on a TTY when `op` is installed AND no
// provider-key refs exist yet — so it never nags someone who already chose, and
// never shows where op can't be used. Accepting writes op:// refs and force-syncs
// them into sbx (overwriting the raw secrets), making 1Password the source of
// truth. Declining (the default) leaves keys exactly as they were.
func offerOnePasswordKeys(env shellEnv, in io.Reader, out io.Writer, tty bool) {
	if !tty || in == nil || !opInstalled(env) || providerKeyRefsPresent(env) {
		return
	}
	fmt.Fprintln(out, "")
	if !confirmYN(in, out, "Manage model keys in 1Password (op:// refs) instead of raw sbx secrets? [y/N]: ", false) {
		return
	}
	fmt.Fprintln(out, "Paste an op:// ref per provider (op://Vault/Item/field), or Enter to skip each.")
	sc := bufio.NewScanner(in)
	wrote := false
	for _, p := range providerKeyRefOrder {
		fmt.Fprintf(out, "  %s: ", p.name)
		if !sc.Scan() {
			break
		}
		ref := normalizeOpRef(sc.Text())
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintf(out, "    skipped %s: not an op:// ref\n", p.name)
			continue
		}
		if err := writeOpRefQuiet(env, p.envVar, ref); err != nil {
			fmt.Fprintf(out, "    could not save %s: %v\n", p.name, err)
			continue
		}
		wrote = true
	}
	if !wrote {
		fmt.Fprintln(out, "No refs entered; keys unchanged.")
		return
	}
	fmt.Fprintln(out, "Resolving from 1Password into sbx...")
	syncProviderKeys(env, out) // force-overwrite sbx from the new refs
}

// writeOpRefQuiet upserts KEY=op://ref into op-refs.env without the CLI wrapper's
// os.Exit, so the interactive offer can loop. It VALIDATES the key as a shell env
// var name (so a malicious pack.toml integration name can't inject extra
// op-refs.env lines) and the value as a single-line op:// ref (never a literal
// secret) — defense in depth beside the caller's own op:// check.
func writeOpRefQuiet(env shellEnv, key, value string) error {
	if env.writeFile == nil {
		return fmt.Errorf("no writer available")
	}
	if !envVarNameRe.MatchString(key) {
		return fmt.Errorf("invalid env var name %q", key)
	}
	if !strings.HasPrefix(value, "op://") || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("value must be a single-line op:// ref")
	}
	path, content, _ := opRefsContent(env)
	return env.writeFile(path, []byte(upsertOpRef(content, key, value)), 0o600)
}

// providerKeyRefs maps each cloud model provider's op-refs.env ENV var to its
// sbx secret name. Extend here to add a provider.
var providerKeyRefs = map[string]string{
	"ANTHROPIC_API_KEY": "anthropic",
	"OPENAI_API_KEY":    "openai",
	"GEMINI_API_KEY":    "google",
}

// providerKeyRefsPresent reports whether op-refs.env declares at least one FILLED
// provider-key ref, so the keys gate can point at `pi-stack secret sync` and the
// host-state file can report source="1password".
func providerKeyRefsPresent(env shellEnv) bool {
	_, content, exists := opRefsContent(env)
	if !exists {
		return false
	}
	for _, r := range parseOpRefs(content) {
		if _, ok := providerKeyRefs[r.key]; ok && r.isRef && !r.placeholder {
			return true
		}
	}
	return false
}

// ensureProviderKeysFromRefs is the NO-RITUAL path: for each provider-key op://
// ref whose sbx secret is currently MISSING, resolve it from 1Password and push
// it into sbx. Because it acts only on missing keys, `op` (which may prompt) is
// touched at most once per key ever — once sbx has the secret, later launches
// skip op entirely. Best-effort and quiet: any failure just leaves the key unset
// and the keys gate guides the user. Called from `run` and `setup`; the explicit
// `pi-stack secret sync` is the force-resync (rotate/repair) counterpart.
func ensureProviderKeysFromRefs(env shellEnv, out io.Writer) {
	if env.run == nil {
		return
	}
	_, content, exists := opRefsContent(env)
	if !exists {
		return
	}
	sbxOut, err := env.run("sbx", "secret", "ls")
	if err != nil {
		return // can't tell what's set; don't guess
	}
	type pk struct{ name, ref string }
	var todo []pk
	for _, r := range parseOpRefs(content) {
		name, ok := providerKeyRefs[r.key]
		if !ok || !r.isRef || r.placeholder {
			continue
		}
		if grepWord(sbxOut, name) {
			continue // already in sbx — no op call, no prompt
		}
		todo = append(todo, pk{name, r.value})
	}
	if len(todo) == 0 {
		return // nothing missing that we can fill; never touch op
	}
	if !opInstalled(env) || !opSignedIn(env) {
		return // can't resolve now; the keys gate points at `pi-stack secret sync`
	}
	for _, p := range todo {
		val, err := env.run("op", "read", p.ref)
		if err != nil {
			continue
		}
		val = strings.TrimRight(val, "\r\n")
		if val == "" {
			continue
		}
		if _, err := env.run("sbx", "secret", "set", "-g", p.name, "-t", val); err == nil {
			fmt.Fprintf(out, "pi-stack: resolved %s from 1Password\n", p.name)
		}
	}
}

// syncProviderKeys is the testable core: resolve each present provider-key ref
// via `op read` and push it into sbx with `sbx secret set -g <name> -t <value>`.
// It writes per-key ✓/✗ to out (NEVER the value) and returns counts plus a fatal
// error for a precondition failure (op/sbx unavailable) so the CLI wrapper can
// pick an exit code and setup can degrade quietly.
func syncProviderKeys(env shellEnv, out io.Writer) (synced, failed int, fatal error) {
	_, content, exists := opRefsContent(env)
	if !exists {
		return 0, 0, fmt.Errorf("op-refs.env not found (%s)", defaultOpRefsPath(env))
	}
	if !opInstalled(env) {
		return 0, 0, fmt.Errorf("op (1Password CLI) not installed")
	}
	if !opSignedIn(env) {
		return 0, 0, fmt.Errorf("op installed but no account configured (run: op signin)")
	}
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err != nil {
			return 0, 0, fmt.Errorf("sbx not on PATH")
		}
	}

	present := map[string]opRef{}
	for _, r := range parseOpRefs(content) {
		if _, ok := providerKeyRefs[r.key]; ok && r.isRef {
			present[r.key] = r
		}
	}
	if len(present) == 0 {
		return 0, 0, nil // nothing to do (not fatal)
	}
	envVars := make([]string, 0, len(present))
	for k := range present {
		envVars = append(envVars, k)
	}
	sort.Strings(envVars) // deterministic output

	for _, envVar := range envVars {
		r := present[envVar]
		name := providerKeyRefs[envVar]
		if r.placeholder {
			fmt.Fprintf(out, "  \u2717 %s (%s): unfilled placeholder\n", name, envVar)
			failed++
			continue
		}
		val, err := env.run("op", "read", r.value)
		if err != nil {
			fmt.Fprintf(out, "  \u2717 %s (%s): op read failed\n", name, envVar)
			failed++
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			fmt.Fprintf(out, "  \u2717 %s (%s): resolved empty\n", name, envVar)
			failed++
			continue
		}
		// `sbx secret set -g <name> -t <value>` is sbx's own documented interface.
		// The value is briefly an argv element on the HOST; it is never written to
		// pi-stack's disk and never enters the VM.
		if sbxOut, err := env.run("sbx", "secret", "set", "-g", name, "-t", val); err != nil {
			detail := strings.TrimSpace(firstLine(sbxOut))
			if detail == "" {
				detail = err.Error()
			}
			fmt.Fprintf(out, "  \u2717 %s (%s): sbx secret set failed: %s\n", name, envVar, detail)
			failed++
			continue
		}
		fmt.Fprintf(out, "  \u2713 %s synced from 1Password\n", name)
		synced++
	}
	return synced, failed, nil
}

// firstLine returns the first non-empty line of s (sbx errors are one line;
// guards against echoing a value if sbx unexpectedly emits one).
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return ln
		}
	}
	return ""
}

// runSecretSync is the `pi-stack secret sync` entry: resolve provider-key op://
// refs -> sbx secrets, with exit codes for scripting.
func runSecretSync(env shellEnv, out io.Writer) {
	synced, failed, fatal := syncProviderKeys(env, out)
	if fatal != nil {
		fmt.Fprintf(out, "pi-stack secret sync: %v\n", fatal)
		fmt.Fprintln(out, "Add provider keys with: pi-stack secret set ANTHROPIC_API_KEY op://vault/item/field")
		os.Exit(3)
	}
	if synced == 0 && failed == 0 {
		fmt.Fprintln(out, "no provider-key refs in op-refs.env (add e.g. ANTHROPIC_API_KEY=op://vault/item/field)")
		return
	}
	if failed > 0 {
		fmt.Fprintf(out, "%d synced, %d failed.\n", synced, failed)
		os.Exit(1)
	}
	fmt.Fprintf(out, "%d provider key(s) synced from 1Password into sbx.\n", synced)
}
