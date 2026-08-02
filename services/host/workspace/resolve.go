package workspace

// workspaceresolve.go — the hardened Workspace->sandbox resolver. A sandbox
// created with a CUSTOM name (`pix run --name pix-demo`) breaks the
// old assumption that a Workspace's sandbox is always
// `pix-<basename>` (DeriveSandboxName). The create receipt
// (sandboxmcpstate.go) now records the canonical Workspace it was created
// for, so verbs that only know a DIR (`pix mcp load NAME [DIR]`, doctor's
// Workspace sandbox context) can find the box pix itself created for it.
//
// TRUST POSTURE (same class as the receipt reads): the resolver only ever
// TARGETS a sandbox it can positively justify —
//
//   - exactly ONE trustworthy receipt maps the canonical Workspace -> that
//     sandbox (SandboxMapped);
//   - the scan completed cleanly and NO receipt maps it -> the derived
//     default name (SandboxDefault: an old sandbox predating the
//     Workspace field, or none yet);
//   - MORE than one trustworthy receipt claims the Workspace ->
//     SandboxAmbiguous, no target (never pick one arbitrarily);
//   - any receipt in the store is untrustworthy (corrupt, wrong schema,
//     identity mismatch, unreadable, a symlinked or invalid directory) ->
//     WorkspaceSandboxUntrusted, no target: an unreadable receipt could be
//     the very mapping being asked about, so "no mapping found" is not a
//     positive conclusion. Mutating callers (`mcp load`) MUST refuse;
//     read-only callers (doctor) may fall back to the derived name for
//     REPORTING, where the box's own receipt state still governs rendering.
//
// The resolver never creates anything and never follows a symlinked receipt
// (ReadMCPReceiptFile's own hardening applies per entry).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceSandboxOutcome is the typed resolution result — see the file doc.
type workspaceSandboxOutcome int

const (
	// SandboxMapped: exactly one trustworthy receipt maps the
	// Workspace to Sandbox.
	SandboxMapped workspaceSandboxOutcome = iota
	// SandboxDefault: a clean scan found no mapping; Sandbox is the
	// derived default name (DeriveSandboxName).
	SandboxDefault
	// SandboxAmbiguous: two or more trustworthy receipts claim the
	// Workspace. No Sandbox is returned — picking one would target an
	// arbitrary box.
	SandboxAmbiguous
	// WorkspaceSandboxUntrusted: the receipt store holds something that
	// cannot be trusted (or could not be scanned), so "no mapping" cannot be
	// concluded. No Sandbox is returned.
	WorkspaceSandboxUntrusted
)

// String renders the outcome for messages/tests.
func (o workspaceSandboxOutcome) String() string {
	switch o {
	case SandboxMapped:
		return "mapped"
	case SandboxDefault:
		return "default"
	case SandboxAmbiguous:
		return "ambiguous"
	case WorkspaceSandboxUntrusted:
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

// CanonicalPath resolves workspace to ONE canonical absolute form — the form
// the create receipt records and every resolver comparison uses, so `run .`,
// `run ./proj/../proj`, and a later `mcp load NAME /abs/proj` all agree.
// Symlinks are resolved when the path exists (macOS /tmp vs /private/tmp);
// a nonexistent path degrades to the cleaned absolute spelling.
func CanonicalPath(ws string) string {
	abs, err := filepath.Abs(ws)
	if err != nil {
		return filepath.Clean(ws)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// ResolveSandbox scans the launcher's receipt store under stateDir
// for a trustworthy receipt whose recorded Workspace is workspace (canonicalized on
// both sides). See the file doc for the outcome contract.
func ResolveSandbox(stateDir, ws string) workspaceSandboxResolution {
	canon := CanonicalPath(ws)
	fallback := workspaceSandboxResolution{
		Sandbox: DeriveSandboxName(ws),
		Outcome: SandboxDefault,
		Detail:  "no receipt maps " + canon + "; using the derived default name",
	}
	root := MCPStateRoot(stateDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return fallback // no store at all — nothing recorded, clean default
		}
		return workspaceSandboxResolution{Outcome: WorkspaceSandboxUntrusted,
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
		if verr := ValidateStateName(name); verr != nil {
			problems = append(problems, name+": invalid sandbox directory name")
			continue
		}
		r, status, rerr := ReadMCPReceiptFile(filepath.Join(root, name), name)
		switch status {
		case MCPStateOK:
			if r.Workspace != "" && filepath.Clean(r.Workspace) == canon {
				matches = append(matches, name)
			}
		case MCPStateAbsent:
			// no receipt file in this directory — nothing recorded, nothing
			// to distrust
		default:
			problems = append(problems, fmt.Sprintf("%s: receipt %s (%v)", name, status, rerr))
		}
	}
	switch {
	case len(matches) > 1:
		return workspaceSandboxResolution{Outcome: SandboxAmbiguous,
			Detail: fmt.Sprintf("%d receipts map %s (%s) — refusing to pick one", len(matches), canon, strings.Join(matches, ", "))}
	case len(problems) > 0:
		// Even a single clean match cannot be trusted as UNIQUE past an
		// unreadable receipt: the corrupt one could map the same Workspace.
		return workspaceSandboxResolution{Outcome: WorkspaceSandboxUntrusted,
			Detail: "untrustworthy receipt(s) in " + root + ": " + strings.Join(problems, "; ")}
	case len(matches) == 1:
		return workspaceSandboxResolution{Sandbox: matches[0], Outcome: SandboxMapped,
			Detail: "receipt maps " + canon + " -> " + matches[0]}
	default:
		return fallback
	}
}

// DeriveSandboxName is the default sandbox name for a Workspace: "pix-" plus
// the directory's base name. It lived in run.go, which is where it was called
// from, but naming a Workspace is this package's job — run.go was reaching down
// into Workspace semantics it does not own.
func DeriveSandboxName(ws string) string {
	abs, err := filepath.Abs(ws)
	if err != nil {
		abs = ws
	}
	base := filepath.Base(abs)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "Workspace"
	}
	return "pix-" + base
}
