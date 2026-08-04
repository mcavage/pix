// Package task is the L1 task-checkout package (Story06 simplification of the
// former workflow/launch/task.go). It owns everything about a pix task's git
// checkout: naming, on-disk paths, persisted metadata, the checkout mechanism
// (clone or worktree), and the pure removal-guard decision.
//
// It deliberately knows NOTHING about sandboxes: whether one is running,
// launching one, or tearing one down is Story04/L4 territory
// (workflow/launch + workflow/launch/sandbox.go's `sbx` wrapper) and this
// package must never import "pix/host/workflow/launch", any sandbox-runner
// package, or "pix/host/lease". A caller that needs to fold sandbox liveness
// into a removal decision passes it in as the typed SandboxDisposition value
// (see disposition.go) — this package never probes for it itself.
//
// It is also independent of "pix/host/config" and "pix/host/workspace": every
// path this package touches (the state root, the main repo root) is a plain
// string argument the caller resolves, never a global default it reaches for
// itself. That keeps the package trivially unit-testable and free of the
// shared-root/config assumptions the rest of the launcher carries.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Mechanism selects how a task's checkout is materialized from the main repo.
type Mechanism string

const (
	// Clone is a host-side `git clone --local`: a fully independent .git
	// directory with hardlinked objects. This is the DEFAULT — see
	// docs/design/worktree-tasks.md for why (a linked worktree's .git is a
	// FILE pointing outside the checkout, which breaks a bind-mount launcher).
	Clone Mechanism = "clone"
	// Worktree is a `git worktree add`: the checkout shares the main repo's
	// object/ref store directly, so a commit made in the checkout is already
	// present in the main repo (nothing is ever "checkout-only"). Opt-in via
	// `--worktree` until the host-worktree-mount experiment is validated.
	Worktree Mechanism = "worktree"
)

// Valid reports whether m is a known mechanism.
func (m Mechanism) Valid() bool { return m == Clone || m == Worktree }

// MaxNameLen caps the sanitized <name> segment before it is composed into the
// sandbox name. An overflow is truncated and hash-tagged so it stays unique.
const MaxNameLen = 40

// MaxSandboxNameLen is the hard cap on the composed sandbox name
// (pix-t-<label>-<repokey>-<name>). 63 is the strictest common limit
// (RFC1123 label).
const MaxSandboxNameLen = 63

// MaxRepoLabelLen caps the human repo label. It is a legibility hint, not an
// identifier (the repokey guarantees uniqueness), so overflow is a plain
// truncation with no hash tag.
const MaxRepoLabelLen = 12

// NameTrimFloor is the minimum trimmed-name length hashTagTrim needs (a
// 10-hex tag + dash, plus at least one prefix rune).
const NameTrimFloor = 12

// RepoKey is the stable per-repo key: the first 8 hex of sha256 of the
// canonical absolute path of the main repo (its git-common-dir). Two repos
// with a task of the same name never collide.
func RepoKey(mainroot string) string {
	sum := sha256.Sum256([]byte(canonicalPath(mainroot)))
	return hex.EncodeToString(sum[:])[:8]
}

// RepoLabel is the human-readable repo hint stamped into the sandbox name and
// the on-disk path: the sanitized basename of the repo, capped at
// MaxRepoLabelLen. It is a hint only (the repokey guarantees uniqueness), so
// two repos that both basename to "api" still differ by their repokey.
func RepoLabel(mainroot string) string {
	base := filepath.Base(canonicalPath(mainroot))
	if base == ".git" {
		base = filepath.Base(filepath.Dir(canonicalPath(mainroot)))
	}
	base = strings.TrimSuffix(base, ".git")
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "repo"
	}
	if len(out) > MaxRepoLabelLen {
		out = strings.TrimRight(out[:MaxRepoLabelLen], "-")
		if out == "" {
			out = "repo"
		}
	}
	return out
}

// RepoDir is the per-repo state-dir segment: "<label>-<repokey>".
func RepoDir(mainroot string) string {
	return RepoLabel(mainroot) + "-" + RepoKey(mainroot)
}

func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// SanitizeName keeps a task name safe as a path + sandbox-name segment,
// capping its length and hash-tagging the tail when it would overflow.
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "task"
	}
	if len(out) > MaxNameLen {
		sum := sha256.Sum256([]byte(out))
		out = out[:MaxNameLen-7] + "-" + hex.EncodeToString(sum[:])[:6]
	}
	return out
}

// Paths resolves the checkout dir and metadata file for a task under
// stateRoot (the caller-resolved e.g. $XDG_STATE_HOME/pix/tasks dir).
// name is expected to already be sanitized.
func Paths(stateRoot, repoDir, name string) (co, meta string) {
	base := filepath.Join(stateRoot, repoDir)
	co = filepath.Join(base, "co", name)
	meta = filepath.Join(base, "meta", name+".json")
	return co, meta
}

// SandboxName is the collision-proof sandbox name for a task:
// "pix-t-" + label + "-" + repokey + "-" + sanitize(name), bounded to
// MaxSandboxNameLen by BoundSandboxName.
func SandboxName(label, repokey, name string) string {
	return BoundSandboxName(label, repokey, SanitizeName(name))
}

// BoundSandboxName composes "pix-t-<label>-<repokey>-<name>" and, if it
// exceeds MaxSandboxNameLen, trims to fit in priority order: the name first
// (hash-tagged so it stays unique), then the label (a cosmetic hint), NEVER
// the repokey (correctness). label and name are already sanitized.
func BoundSandboxName(label, repokey, name string) string {
	compose := func(l, n string) string { return "pix-t-" + l + "-" + repokey + "-" + n }
	if len(compose(label, name)) <= MaxSandboxNameLen {
		return compose(label, name)
	}
	for n := len(name); n >= NameTrimFloor; n-- {
		cand := hashTagTrim(name, n)
		if len(compose(label, cand)) <= MaxSandboxNameLen {
			return compose(label, cand)
		}
	}
	nameFloor := hashTagTrim(name, NameTrimFloor)
	for l := len(label); l >= 1; l-- {
		cand := strings.TrimRight(label[:l], "-")
		if cand == "" {
			continue
		}
		if len(compose(cand, nameFloor)) <= MaxSandboxNameLen {
			return compose(cand, nameFloor)
		}
	}
	s := compose("repo", nameFloor)
	if len(s) > MaxSandboxNameLen {
		s = s[:MaxSandboxNameLen]
	}
	return s
}

// hashTagTrim truncates s to n runes, appending a 10-hex tag of the ORIGINAL
// s so two different names that truncate to the same prefix stay distinct.
// n must be >= NameTrimFloor.
func hashTagTrim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < NameTrimFloor {
		n = NameTrimFloor
	}
	sum := sha256.Sum256([]byte(s))
	return s[:n-11] + "-" + hex.EncodeToString(sum[:])[:10]
}

// Meta is the single source of truth `ls`/`path`/`rm` read, written to disk
// as JSON. It carries no sandbox-lifecycle state (mode, harvest, gc, or a
// "recovered" ref namespace): those are removed as of Story06. Origin is
// always stripped of any embedded userinfo before it is written.
type Meta struct {
	Name      string    `json:"name"`
	Mechanism Mechanism `json:"mechanism"`
	Sandbox   string    `json:"sandbox"`
	Mainroot  string    `json:"mainroot"`
	Branch    string    `json:"branch"`
	Base      string    `json:"base"`
	Origin    string    `json:"origin,omitempty"`
	Repo      string    `json:"repo"`
	Created   string    `json:"created"`
}

// WriteMeta persists m to path (0700 dir, 0600 file: the origin URL, even
// stripped of userinfo, is not meant to be world-readable).
func WriteMeta(path string, m Meta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	m.Origin = StripURLUserinfo(m.Origin)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ReadMeta reads and decodes a task's metadata file.
func ReadMeta(path string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// StripURLUserinfo removes any user:pass@ userinfo from a URL, keeping only
// scheme, host, and path. A value that does not parse as a scheme+host URL
// (e.g. an scp-style `git@host:path` remote) is returned unchanged.
func StripURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// HardenMeta re-derives the security-sensitive fields (mainroot, branch,
// sandbox) from the validated task name so a tampered meta.json can never
// steer a git refspec or sandbox name. fileBase is the file's name without
// the .json suffix; the stored name MUST sanitize back to it, otherwise the
// file was renamed or hand-edited and is not trusted.
func HardenMeta(m Meta, mainroot, repokey, fileBase string) (Meta, error) {
	sane := SanitizeName(m.Name)
	if sane != fileBase {
		return m, fmt.Errorf("metadata name %q does not match its file %q.json", m.Name, fileBase)
	}
	label := RepoLabel(mainroot)
	m.Mainroot = mainroot
	m.Repo = label
	m.Branch = "pix/" + sane
	m.Sandbox = SandboxName(label, repokey, m.Name)
	if m.Mechanism == "" {
		m.Mechanism = Clone
	}
	return m, nil
}

// Resolve is the read-path convenience: given the state root, main repo, and
// a task name, it locates the checkout dir and returns its hardened
// metadata. It is what `task path`, `task run`, and `--task NAME` all need.
func Resolve(stateRoot, mainroot, name string) (co string, m Meta, err error) {
	sane := SanitizeName(name)
	repoDir := RepoDir(mainroot)
	co, metaPath := Paths(stateRoot, repoDir, sane)
	m, err = ReadMeta(metaPath)
	if err != nil {
		return "", Meta{}, fmt.Errorf("no task %q for this repo: %w", name, err)
	}
	m, err = HardenMeta(m, mainroot, RepoKey(mainroot), sane)
	if err != nil {
		return "", Meta{}, err
	}
	return co, m, nil
}

// Path is the thin accessor `task path <name>` prints to stdout: the
// checkout directory, without reading git state.
func Path(stateRoot, mainroot, name string) (string, error) {
	co, _, err := Resolve(stateRoot, mainroot, name)
	return co, err
}
