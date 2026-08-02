// oprefs_find.go — locating an EXISTING op-refs.env, the companion question to
// DefaultOpRefsPath's "where would one go". Doctor, gog setup and MCP
// registration all need the file the gateway will actually be handed, which is
// not always the one a fresh install would create.
package secret

import (
	"os"
	"path/filepath"

	"pix/host/hostenv"
)

// findUpward walks up from the current working directory looking for a directory
// that contains BOTH a Makefile and the given repo-relative file, returning the
// absolute path to that file (or "" if none is found before the filesystem root).
// This is how doctor locates a repo checkout's config files (op-refs.env)
// regardless of where it was invoked from within the tree.
func findUpward(env hostenv.Env, rel string) string {

	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if env.IsFile(filepath.Join(dir, "Makefile")) && env.IsFile(filepath.Join(dir, rel)) {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// FindOpRefs resolves config/op-refs.env to an ABSOLUTE, canonical location
// so doctor's headless probe matches the gateway registration exactly (`make
// mcp-register` registers the gog spawn with an absolute --env-file; a relative
// one here would resolve against doctor's cwd and could probe a different file
// than the gateway actually uses). It searches, in order, and returns the FIRST
// that exists:
//  1. $PIX_CONFIG's directory + op-refs.env,
//  2. a repo checkout's config/op-refs.env (walk up for Makefile + that file),
//  3. ~/.config/pix/op-refs.env.
//
// Returns "" when none exists, so the caller reports "cannot verify" rather than
// probing (and blessing) a file the gateway never uses.
func FindOpRefs(env hostenv.Env) string {
	// abs makes every resolved path ABSOLUTE regardless of doctor's cwd: a
	// relative $PIX_CONFIG (e.g. `config/config.toml`) would otherwise yield
	// a cwd-relative op-refs path that need not match the gateway's --env-file.
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	if p := env.Getenv("PIX_CONFIG"); p != "" {
		cand := filepath.Join(filepath.Dir(p), "op-refs.env")
		if env.IsFile(cand) {
			return abs(cand)
		}
	}
	if p := findUpward(env, filepath.Join("config", "op-refs.env")); p != "" {
		return abs(p)
	}
	{
		if home := env.HomeDir(); home != "" {
			cand := filepath.Join(home, ".config", "pix", "op-refs.env")
			if env.IsFile(cand) {
				return abs(cand)
			}
		}
	}
	return ""
}

// OpRefFilled reports whether op-refs.env has a FILLED op:// ref for env var key.
func OpRefFilled(env hostenv.Env, key string) bool {
	_, content, exists := OpRefsContent(env)
	if !exists {
		return false
	}
	for _, r := range ParseOpRefs(content) {
		if r.Key == key && r.IsRef && !r.Placeholder {
			return true
		}
	}
	return false
}
