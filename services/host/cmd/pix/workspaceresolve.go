package main

// workspaceresolve.go — the hardened workspace->sandbox resolver. A sandbox
// created with a CUSTOM name (`pix run --name pix-demo`) breaks the
// old assumption that a workspace's sandbox is always
// `pix-<basename>` (deriveSandboxName). The create receipt
// (sandboxmcpstate.go) now records the canonical Workspace it was created
// for, so verbs that only know a DIR (`pix mcp load NAME [DIR]`, doctor's
// workspace sandbox context) can find the box pix itself created for it.
//
// TRUST POSTURE (same class as the receipt reads): the resolver only ever
// TARGETS a sandbox it can positively justify —
//
//   - exactly ONE trustworthy receipt maps the canonical workspace -> that
//     sandbox (workspaceSandboxMapped);
//   - the scan completed cleanly and NO receipt maps it -> the derived
//     default name (workspaceSandboxDefault: an old sandbox predating the
//     Workspace field, or none yet);
//   - MORE than one trustworthy receipt claims the workspace ->
//     workspaceSandboxAmbiguous, no target (never pick one arbitrarily);
//   - any receipt in the store is untrustworthy (corrupt, wrong schema,
//     identity mismatch, unreadable, a symlinked or invalid directory) ->
//     workspaceSandboxUntrusted, no target: an unreadable receipt could be
//     the very mapping being asked about, so "no mapping found" is not a
//     positive conclusion. Mutating callers (`mcp load`) MUST refuse;
//     read-only callers (doctor) may fall back to the derived name for
//     REPORTING, where the box's own receipt state still governs rendering.
//
// The resolver never creates anything and never follows a symlinked receipt
// (readSandboxMCPReceiptFile's own hardening applies per entry).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceSandboxOutcome is the typed resolution result — see the file doc.
type workspaceSandboxOutcome int

const (
	// workspaceSandboxMapped: exactly one trustworthy receipt maps the
	// workspace to Sandbox.
	workspaceSandboxMapped workspaceSandboxOutcome = iota
	// workspaceSandboxDefault: a clean scan found no mapping; Sandbox is the
	// derived default name (deriveSandboxName).
	workspaceSandboxDefault
	// workspaceSandboxAmbiguous: two or more trustworthy receipts claim the
	// workspace. No Sandbox is returned — picking one would target an
	// arbitrary box.
	workspaceSandboxAmbiguous
	// workspaceSandboxUntrusted: the receipt store holds something that
	// cannot be trusted (or could not be scanned), so "no mapping" cannot be
	// concluded. No Sandbox is returned.
	workspaceSandboxUntrusted
)

// String renders the outcome for messages/tests.
func (o workspaceSandboxOutcome) String() string {
	switch o {
	case workspaceSandboxMapped:
		return "mapped"
	case workspaceSandboxDefault:
		return "default"
	case workspaceSandboxAmbiguous:
		return "ambiguous"
	case workspaceSandboxUntrusted:
		return "untrusted"
	default:
		return "unknown"
	}
}

// workspaceSandboxResolution is one resolver answer: Sandbox is set only for
// the two positive outcomes (mapped/default); Detail carries the concrete
// evidence or refusal reason for messages.
type workspaceSandboxResolution struct {
	Sandbox string
	Outcome workspaceSandboxOutcome
	Detail  string
}

// canonicalWorkspacePath resolves ws to ONE canonical absolute form — the form
// the create receipt records and every resolver comparison uses, so `run .`,
// `run ./proj/../proj`, and a later `mcp load NAME /abs/proj` all agree.
// Symlinks are resolved when the path exists (macOS /tmp vs /private/tmp);
// a nonexistent path degrades to the cleaned absolute spelling.
func canonicalWorkspacePath(ws string) string {
	abs, err := filepath.Abs(ws)
	if err != nil {
		return filepath.Clean(ws)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// resolveWorkspaceSandbox scans the launcher's receipt store under stateDir
// for a trustworthy receipt whose recorded Workspace is ws (canonicalized on
// both sides). See the file doc for the outcome contract.
func resolveWorkspaceSandbox(stateDir, ws string) workspaceSandboxResolution {
	canon := canonicalWorkspacePath(ws)
	fallback := workspaceSandboxResolution{
		Sandbox: deriveSandboxName(ws),
		Outcome: workspaceSandboxDefault,
		Detail:  "no receipt maps " + canon + "; using the derived default name",
	}
	root := sandboxMCPStateRoot(stateDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return fallback // no store at all — nothing recorded, clean default
		}
		return workspaceSandboxResolution{Outcome: workspaceSandboxUntrusted,
			Detail: fmt.Sprintf("cannot scan the sandbox receipt store %s: %v", root, err)}
	}
	var matches []string
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if e.Type()&os.ModeSymlink != 0 {
			// A symlinked per-sandbox directory is the same planted-symlink
			// class every writer refuses (mkdirSymlinkSafe) — never read
			// through it, and never conclude "no mapping" past it.
			problems = append(problems, name+": symlinked receipt directory")
			continue
		}
		if !e.IsDir() {
			continue // a stray file at the root is not a receipt directory
		}
		if verr := validateSandboxStateName(name); verr != nil {
			problems = append(problems, name+": invalid sandbox directory name")
			continue
		}
		r, status, rerr := readSandboxMCPReceiptFile(filepath.Join(root, name), name)
		switch status {
		case sandboxMCPStateOK:
			if r.Workspace != "" && filepath.Clean(r.Workspace) == canon {
				matches = append(matches, name)
			}
		case sandboxMCPStateAbsent:
			// no receipt file in this directory — nothing recorded, nothing
			// to distrust
		default:
			problems = append(problems, fmt.Sprintf("%s: receipt %s (%v)", name, status, rerr))
		}
	}
	switch {
	case len(matches) > 1:
		return workspaceSandboxResolution{Outcome: workspaceSandboxAmbiguous,
			Detail: fmt.Sprintf("%d receipts map %s (%s) — refusing to pick one", len(matches), canon, strings.Join(matches, ", "))}
	case len(problems) > 0:
		// Even a single clean match cannot be trusted as UNIQUE past an
		// unreadable receipt: the corrupt one could map the same workspace.
		return workspaceSandboxResolution{Outcome: workspaceSandboxUntrusted,
			Detail: "untrustworthy receipt(s) in " + root + ": " + strings.Join(problems, "; ")}
	case len(matches) == 1:
		return workspaceSandboxResolution{Sandbox: matches[0], Outcome: workspaceSandboxMapped,
			Detail: "receipt maps " + canon + " -> " + matches[0]}
	default:
		return fallback
	}
}
