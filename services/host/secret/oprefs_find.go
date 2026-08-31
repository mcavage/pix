// oprefs_find.go — locating an EXISTING op-refs.env, the companion question to
// DefaultOpRefsPath's "where would one go". Doctor, gog setup and MCP
// registration all need the file the gateway will actually be handed.
//
// There is exactly ONE candidate: <PIX_HOME>/op-refs.env (AGENTS.md safety
// invariant 1 — PIX_HOME is the single root, with no XDG split). The
// $PIX_CONFIG directory, the repo-checkout walk (Makefile + config/
// op-refs.env), and ~/.config/pix are all DELETED, not deprecated: a
// resolver with four candidates is a resolver that can hand the gateway a
// different file than `pix secret set` writes.
package secret

import (
	"pix/host/hostenv"
)

// FindOpRefs returns the absolute <PIX_HOME>/op-refs.env when that file
// exists, else "" — so a caller reports "cannot verify" rather than probing
// (and blessing) a file the gateway never uses. The path is canonical and
// absolute by construction (pixhome.Dir absolutizes PIX_HOME), so doctor's
// headless probe and the gateway registration name the same bytes.
func FindOpRefs(env hostenv.Env) string {
	path := DefaultOpRefsPath()
	if env.IsFile(path) {
		return path
	}
	return ""
}
