// scoped.go — the credential path Pix actually uses: this PIX_HOME's
// configured op:// refs, resolved at launch and written as SANDBOX-SCOPED sbx
// service secrets for the one sandbox this run is about to enter.
//
// Why not global. sbx's global secret scope stores one credential every
// sandbox on the host reads, including sandboxes another Pix stack (another
// PIX_HOME) created. That makes a host-wide store the real owner of a
// PIX_HOME's keys: rotating a ref changes nothing until something re-pushes
// the global, and a second stack silently inherits whatever the first one
// pushed. Neither is a property this launcher may have, so Pix writes the
// scoped form only (`--sandbox <name>`) and never reads a global as evidence
// that a key exists.
//
// Two consequences the rest of the launcher depends on:
//
//   - Every create AND every attach re-resolves and re-writes. There is no
//     "already set, skip it" branch, because that branch is exactly what makes
//     a rotated 1Password item invisible until someone recreates the sandbox.
//   - A resolved value never lands on disk, never appears in an argv, and
//     never reaches the VM through pix: `op read` resolves it in-process, it
//     is written to one `sbx secret set`'s STDIN (so no `ps` on the host ever
//     sees it), and every error string is redacted before it is printed (sbx
//     can echo back whatever it read).
package secret

import (
	"fmt"
	"io"
	"strings"

	"pix/host/hostenv"
)

// GitHubKeyRef is the GitHub credential as a scoped service secret:
// secrets.env's GITHUB_TOKEN becomes the sandbox's `github` service secret, so
// an agent can push from inside the box it was actually given. It is
// deliberately NOT in ProviderKeyRefOrder (it buys no model, so it must never
// be offered by a model prompt or block a launch) and not in ToolKeyRefOrder
// (whose members are pi extension keys); it is its own thing, and the only
// list it belongs to is the scoped set below.
var GitHubKeyRef = ProviderKeyRef{EnvVar: "GITHUB_TOKEN", Name: "github"}

// ScopedKeyRefOrder is the deterministic, CLOSED set of secrets.env variables
// Pix will resolve into a sandbox's service secrets: the model providers, the
// tool keys, and GitHub. Closed is the point — secrets.env also carries refs
// that belong to an MCP server the sbx Gateway resolves itself (`op run
// --env-file`), and pushing one of those into the sandbox's proxy store under
// its own name would invent a service secret nobody declared.
func ScopedKeyRefOrder() []ProviderKeyRef {
	out := make([]ProviderKeyRef, 0, len(ProviderKeyRefOrder)+len(ToolKeyRefOrder)+1)
	out = append(out, ProviderKeyRefOrder...)
	out = append(out, ToolKeyRefOrder...)
	return append(out, GitHubKeyRef)
}

// scopedKeyRefs is ScopedKeyRefOrder as a lookup, derived from the same lists
// so the two cannot disagree about membership.
var scopedKeyRefs = func() map[string]string {
	m := map[string]string{}
	for _, p := range ScopedKeyRefOrder() {
		m[p.EnvVar] = p.Name
	}
	return m
}()

// ScopedSecretOptions tunes how hard a preparation failure is.
type ScopedSecretOptions struct {
	// ModelKeyOptional degrades "no configured model key could be scoped"
	// from an error to a warning. Set by a launch whose inference needs no
	// provider key at all (a keyless/sbx-session backend): losing the
	// sandbox over a credential that session was never going to use is a
	// worse answer than starting it.
	ModelKeyOptional bool
}

// PrepareSandboxSecrets resolves every configured known ref and writes it as a
// service secret scoped to ONE sandbox. It is called on every create and every
// attach, before the session is exec'd.
//
// It holds the provider-refs transaction lock for the whole read/resolve/write
// pass, exactly as the CRUD verbs do, so a concurrent `pix secret set` cannot
// change the file between the snapshot this reads and the values it pushes.
//
// The error contract is narrow on purpose: the ONLY failure that refuses is
// "this PIX_HOME configures at least one model provider ref and not one of
// them could be scoped", because that is the case where the session would
// start with no way to reach a model. A tool or github ref that cannot be
// resolved degrades (a warning on out), since the session still works without
// web search or without push.
func PrepareSandboxSecrets(env hostenv.Env, sandbox string, out io.Writer, opts ScopedSecretOptions) error {
	if strings.TrimSpace(sandbox) == "" {
		// An empty --sandbox value is how a scoped write silently becomes a
		// host-wide one. Refuse rather than guess.
		return fmt.Errorf("refusing to write sbx secrets with no sandbox scope")
	}
	var inner error
	if lerr := WithProviderRefsLock(env, func() error {
		inner = prepareSandboxSecretsLocked(env, sandbox, out, opts)
		return nil
	}); lerr != nil {
		return fmt.Errorf("could not lock provider refs (%s): %w", ProviderRefsLockPath(env), lerr)
	}
	return inner
}

// prepareSandboxSecretsLocked is PrepareSandboxSecrets' transaction body.
// Caller MUST hold the provider-refs lock.
func prepareSandboxSecretsLocked(env hostenv.Env, sandbox string, out io.Writer, opts ScopedSecretOptions) error {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return nil // nothing configured here: nothing was promised
	}
	refs := firstRefsIn(content, scopedKeyRefs)
	var wantModel, gotModel int
	var failures []string
	for _, p := range ScopedKeyRefOrder() {
		r, ok := refs[p.EnvVar]
		if !ok || r.Placeholder {
			continue
		}
		model := isModelProviderKey(p.EnvVar)
		if model {
			wantModel++
		}
		val, resolved := OpReadNonEmpty(env, r.Value)
		if !resolved {
			fmt.Fprintf(out, "pix: %s (%s): could not resolve its 1Password ref\n", p.Name, p.EnvVar)
			failures = append(failures, p.Name)
			continue
		}
		if detail, err := setScopedSbxSecret(env, sandbox, p.Name, val); err != nil {
			fmt.Fprintf(out, "pix: %s (%s): sbx secret set --sandbox %s failed: %s\n", p.Name, p.EnvVar, sandbox, detail)
			failures = append(failures, p.Name)
			continue
		}
		if model {
			gotModel++
		}
	}
	if wantModel > 0 && gotModel == 0 {
		msg := fmt.Errorf("no configured model provider key could be given to %s (tried: %s); check them with `pix secret check`",
			sandbox, strings.Join(failures, ", "))
		if opts.ModelKeyOptional {
			fmt.Fprintf(out, "pix: warning: %v\n", msg)
			return nil
		}
		return msg
	}
	return nil
}

// setScopedSbxSecret is the ONE place `sbx secret set` is invoked anywhere in
// this launcher, and it can only write the SANDBOX-SCOPED form. `-f`
// overwrites (1Password is the source of truth, and sbx errors on an existing
// secret without it); `--sandbox <name>` is the scope; the SERVICE name is the
// last and only positional.
//
// The value is not in that argv at all: it goes over STDIN, the input form
// `sbx secret set` already supports (`gh auth token | sbx secret set github`).
// An argv is world-readable in the host's process table for the lifetime of
// the child, which on a shared machine is a real disclosure to any other user
// running `ps` at the wrong moment; a pipe is readable only by the two
// processes holding it.
//
// On failure the detail is still sbx's first output line (or the Go error
// text) with every occurrence of the value redacted. That redaction is
// defence in depth now rather than the primary control: sbx is free to echo
// back whatever it read on stdin, so the error string remains a leak vector
// even though the argv no longer is.
func setScopedSbxSecret(env hostenv.Env, sandbox, name, val string) (detail string, err error) {
	sbxOut, err := env.RunInput(val, "sbx", "secret", "set", "-f", "--sandbox", sandbox, name)
	if err == nil {
		return "", nil
	}
	detail = strings.TrimSpace(FirstLine(sbxOut))
	if detail == "" {
		detail = err.Error()
	}
	return RedactSecretValue(detail, val), err
}

// RefsProbeState is the tri-state every credential gate needs: an unreadable
// refs file is NOT the same answer as a readable one that configures nothing.
type RefsProbeState int

const (
	// RefsAnswered: secrets.env was read (or is definitively absent) and its
	// configured set is known.
	RefsAnswered RefsProbeState = iota
	// RefsUnreadable: the file exists but could not be read. Unknown, never a
	// refusal.
	RefsUnreadable
)

// ConfiguredModelRefs reports which MODEL provider service names this PIX_HOME
// configures a filled op:// ref for, and whether the question could be
// answered at all. It reads the refs file only — no `op`, no sbx, no prompt —
// so `pix run`'s gate is cheap and cannot be answered by another stack's
// global secret.
func ConfiguredModelRefs(env hostenv.Env) (present []string, state RefsProbeState) {
	path := DefaultOpRefsPath()
	content, err := env.ReadFile(path)
	if err != nil {
		if env.IsFile(path) {
			// It IS there and we could not read it: unknown, not absent.
			return nil, RefsUnreadable
		}
		return nil, RefsAnswered
	}
	refs := firstRefsIn(content, providerKeyRefs)
	for _, p := range ProviderKeyRefOrder {
		if r, ok := refs[p.EnvVar]; ok && !r.Placeholder {
			present = append(present, p.Name)
		}
	}
	return present, RefsAnswered
}
