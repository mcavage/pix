// agent.go resolves the subagent roster directory (agents/*.md) so `pix run`
// can fold it into the inference roster's SHIPPED model set (E3.1/E3.3). The
// roster's own read-only status screen (`pix agent ls`) and `pix models` were
// both cut from the v2 verb table (docs/design/pix-v2-surface.md §3): neither
// is reachable from root.go, so only what run still calls survives here.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// isDir reports whether p exists and is a directory.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// resolveAgentsDir finds the roster directory WITHOUT depending on the
// caller's cwd (the old "./agents, run from the repo root" contract broke on
// a packaged install, which has no repo checkout, and even inside a
// checkout broke the moment you cd'd into a subdirectory).
//
// $PIX_AGENTS_DIR wins outright, and must exist — a typo there must fail
// loudly, not silently fall through to a different roster. Otherwise this
// tries beside the running binary (a packaged install's bundled copy, if the
// distribution ships one under a share/pix/agents-shaped path next to it),
// then climbs from cwd looking for a repo checkout's agents/ dir — a dev
// convenience, and now depth-independent: `pix run` resolves the roster from
// any subdirectory of a checkout, not only its root.
func resolveAgentsDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("PIX_AGENTS_DIR")); d != "" {
		if !isDir(d) {
			return "", fmt.Errorf("$PIX_AGENTS_DIR=%q does not exist", d)
		}
		return d, nil
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		bin := filepath.Dir(exe)
		for _, rel := range []string{"agents", filepath.Join("share", "pix", "agents"), filepath.Join("..", "share", "pix", "agents")} {
			if cand := filepath.Join(bin, rel); isDir(cand) {
				return cand, nil
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; {
			if cand := filepath.Join(dir, "agents"); isDir(cand) {
				return cand, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("no agent roster found: a packaged install (e.g. Homebrew) does not currently bundle one, " +
		"and no agents/ dir was found in this checkout or its parents; set $PIX_AGENTS_DIR, or run from inside a pix repo checkout")
}

// listAgents resolves the roster dir (see resolveAgentsDir) and returns the
// agent names found in it, plus the dir itself so a caller can build file
// paths without re-resolving (and re-risking a different answer).
func listAgents() (names []string, dir string, err error) {
	dir, err = resolveAgentsDir()
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, dir, fmt.Errorf("read agents dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	sort.Strings(names)
	return names, dir, nil
}
