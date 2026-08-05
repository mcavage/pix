// sandboxmcpstate.go — the launcher-owned per-sandbox MCP receipt: durable
// evidence that PIX ITSELF (not the sandbox, not the gateway) has
// successfully created a sandbox with a given static-MCP preload, and/or
package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/sys"
)

// MCPStateSchema is the current receipt schema version. A receipt
// written under a different schema is REJECTED on read (MCPStateSchemaMismatch),
// never partially trusted — a future schema change gets a bump here plus an
const MCPStateSchema = 1

// sandboxMCPStateFileName is the receipt file's name within its per-sandbox
// directory.
const sandboxMCPStateFileName = "mcp.json"

// sandboxMCPStateLockName is the per-sandbox advisory lock file, a sibling of
// mcp.json in the same directory — never the receipt file itself, so a lock
// holder never blocks a concurrent reader on file content.
const sandboxMCPStateLockName = "mcp.json.lock"

// MCPLoadReceipt is one successful `pix mcp load` for a server
// name, at the FIRST time it was observed to succeed (AppendLoadReceipt never
// updates At for a name already present).
type MCPLoadReceipt struct {
	Name string `json:"name"`
	At   string `json:"at"` // RFC3339 UTC
}

// MCPReceipt is the schema-1 on-disk receipt. Sandbox is the identity
// check (compared against the caller's requested name on every read);
// CreatedAt/Preloaded are set once by WriteCreateReceipt and are empty on a
type MCPReceipt struct {
	Schema    int    `json:"schema"`
	Sandbox   string `json:"sandbox"`
	CreatedAt string `json:"created_at,omitempty"`
	// Workspace is the canonical Workspace directory the sandbox was created
	// FOR (CanonicalPath at create time) — the launcher-owned
	// Workspace->sandbox identity that lets a custom-named sandbox
	Workspace string           `json:"Workspace,omitempty"`
	Preloaded []string         `json:"preloaded,omitempty"`
	Loads     []MCPLoadReceipt `json:"loads,omitempty"`
}

// IsPartial reports whether r is a PARTIAL receipt: one synthesized by
// AppendLoadReceipt for a sandbox whose creation pix never observed
// (empty CreatedAt). A partial receipt proves ONLY the loads it lists — it
func (r *MCPReceipt) IsPartial() bool {
	return r != nil && r.CreatedAt == ""
}

// MCPStateStatus is the typed outcome of reading a receipt — the
// "unavailable reason" doctor/status render instead of collapsing every
// failure into "absent".
type MCPStateStatus int

const (
	// MCPStateOK: a receipt exists, matches this schema, and matches
	// the requested sandbox identity.
	MCPStateOK MCPStateStatus = iota
	// MCPStateAbsent: no receipt file — a genuinely untouched sandbox
	// (never create/load-receipted, e.g. predates this feature). Legitimately
	// empty, NOT unverifiable.
	MCPStateAbsent
	// MCPStateUnreadable: an I/O-level problem reading the file itself
	// (permission denied, a symlinked path refused, etc.) — not a content
	// problem.
	MCPStateUnreadable
	// MCPStateCorrupt: the file exists but is not valid JSON for this
	// type.
	MCPStateCorrupt
	// MCPStateSchemaMismatch: valid JSON, but the schema field is not
	// the one this binary understands.
	MCPStateSchemaMismatch
	// MCPStateIdentityMismatch: valid JSON, correct schema, but the
	// receipt's own Sandbox field does not match the sandbox whose directory
	// it was read from — never trust a reused/renamed directory's leftover
	MCPStateIdentityMismatch
)

// String renders the status for log/report lines (doctor/status).
func (s MCPStateStatus) String() string {
	switch s {
	case MCPStateOK:
		return "ok"
	case MCPStateAbsent:
		return "absent"
	case MCPStateUnreadable:
		return "unreadable"
	case MCPStateCorrupt:
		return "corrupt"
	case MCPStateSchemaMismatch:
		return "schema-mismatch"
	case MCPStateIdentityMismatch:
		return "identity-mismatch"
	default:
		return "unknown"
	}
}

// Unverifiable reports whether status means "a receipt is present but cannot
// be trusted" (corrupt bytes, an unreadable path, a schema this binary
// doesn't know, or an identity mismatch) as opposed to MCPStateOK or
func (s MCPStateStatus) Unverifiable() bool {
	return s != MCPStateOK && s != MCPStateAbsent
}

// ValidateStateName rejects a sandbox name that could traverse or
// escape its directory: empty, ".", "..", or containing any path separator
// (either OS's, checked unconditionally so a receipt written on one platform
func ValidateStateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("sandbox name is empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid sandbox name %q", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("sandbox name %q must not contain a path separator", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("invalid sandbox name %q", name)
	}
	return nil
}

// defaultSandboxMCPStateDir resolves the production state root
// (config.StateDir(), i.e. XDG_STATE_HOME/pix) for real callers. Tests
// call WriteCreateReceipt/AppendLoadReceipt/ReadMCPReceipt directly
func defaultSandboxMCPStateDir() (string, error) {
	return config.StateDir()
}

// MCPStateDirFn is the ONE seam run.go's recordCreateReceipt and
// mcp.go's recordMcpLoadReceipt resolve the state root through — production
// wiring never calls defaultSandboxMCPStateDir (nor config.StateDir())
var MCPStateDirFn = defaultSandboxMCPStateDir

// ReceiptRecordError wraps a failure to durably record an otherwise-successful
// pix operation (a sandbox create, or an `mcp load` attach) in the
// per-sandbox MCP receipt. It is kept as a DISTINCT typed error — never folded
type ReceiptRecordError struct {
	Op      string // "create" or "mcp load"
	Sandbox string
	Name    string // mcp server name; set only for a load receipt
	Err     error
}

func (e *ReceiptRecordError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("%s %q on sandbox %q succeeded, but recording it in local state failed: %v", e.Op, e.Name, e.Sandbox, e.Err)
	}
	return fmt.Sprintf("%s of sandbox %q succeeded, but recording it in local state failed: %v", e.Op, e.Sandbox, e.Err)
}

func (e *ReceiptRecordError) Unwrap() error { return e.Err }

// MCPStateRoot is <stateDir>/sandboxes, the parent of every
// per-sandbox receipt directory.
func MCPStateRoot(stateDir string) string {
	return filepath.Join(stateDir, "sandboxes")
}

// mkdirSymlinkSafe creates dir (and its parents) if absent, then Lstat-refuses
// it if the leaf turned out to be a symlink — MkdirAll alone treats an
// existing symlink-to-directory as "already there" and happily proceeds, so a
func mkdirSymlinkSafe(dir string, perm os.FileMode) error {
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to use it as sandbox mcp state directory", dir)
	}
	return nil
}

// ensureSandboxMCPStateDir validates sandbox, then symlink-safely creates (if
// needed) and returns <stateDir>/sandboxes/<sandbox> — the directory holding
// mcp.json and mcp.json.lock. Both the "sandboxes" root and the per-sandbox
func ensureSandboxMCPStateDir(stateDir, sandbox string) (string, error) {
	if err := ValidateStateName(sandbox); err != nil {
		return "", err
	}
	root := MCPStateRoot(stateDir)
	if err := mkdirSymlinkSafe(root, 0o700); err != nil {
		return "", err
	}
	dir := filepath.Join(root, sandbox)
	if err := mkdirSymlinkSafe(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// withSandboxMCPStateLock ensures the per-sandbox state directory exists
// (symlink-safe) then runs fn under that sandbox's exclusive flock
// (mcp.json.lock beside mcp.json) — the serialization point for every
func withSandboxMCPStateLock(stateDir, sandbox string, fn func(dir string) error) error {
	dir, err := ensureSandboxMCPStateDir(stateDir, sandbox)
	if err != nil {
		return err
	}
	return sys.Lock(filepath.Join(dir, sandboxMCPStateLockName), func() error {
		return fn(dir)
	})
}

// writeSandboxMCPReceiptFile marshals and writes r to dir/mcp.json,
// symlink-safe + atomic (never opens the destination directly — a same-dir
// temp file is fsync'd then renamed over it, replacing any symlink rather
func writeSandboxMCPReceiptFile(dir string, r *MCPReceipt) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return sys.AtomicWriteInDir(dir, sandboxMCPStateFileName, append(b, '\n'), 0o600)
}

// ReadMCPReceiptFile reads dir/mcp.json without following a symlinked
// destination (Lstat first, refuse if it is a symlink), then validates schema
// and sandbox identity. See MCPStateStatus for the full outcome space.
func ReadMCPReceiptFile(dir, sandbox string) (*MCPReceipt, MCPStateStatus, error) {
	path := filepath.Join(dir, sandboxMCPStateFileName)
	fi, lerr := os.Lstat(path)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return nil, MCPStateAbsent, nil
		}
		return nil, MCPStateUnreadable, lerr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, MCPStateUnreadable, fmt.Errorf("%s is a symlink; refusing to read through it", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, MCPStateUnreadable, err
	}
	var r MCPReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, MCPStateCorrupt, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.Schema != MCPStateSchema {
		return nil, MCPStateSchemaMismatch, fmt.Errorf("%s: schema %d, want %d", path, r.Schema, MCPStateSchema)
	}
	if r.Sandbox != sandbox {
		return nil, MCPStateIdentityMismatch, fmt.Errorf("%s: sandbox identity %q, want %q", path, r.Sandbox, sandbox)
	}
	return &r, MCPStateOK, nil
}

// ReadMCPReceipt reads the receipt for sandbox under stateDir. It
// never creates anything: an absent directory or file is
// MCPStateAbsent, not an error. See MCPStateStatus.Unverifiable
func ReadMCPReceipt(stateDir, sandbox string) (*MCPReceipt, MCPStateStatus, error) {
	if err := ValidateStateName(sandbox); err != nil {
		return nil, MCPStateUnreadable, err
	}
	dir := filepath.Join(MCPStateRoot(stateDir), sandbox)
	return ReadMCPReceiptFile(dir, sandbox)
}

// WriteCreateReceipt is the plain-REPLACE create record: fresh CreatedAt
// (now(), UTC RFC3339) and Preloaded (a copy of the requested static-MCP
// set), Loads reset to none, whatever was on disk discarded. It is the
func WriteCreateReceipt(stateDir, sandbox, Workspace string, preloaded []string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		r := &MCPReceipt{
			Schema:    MCPStateSchema,
			Sandbox:   sandbox,
			CreatedAt: now().UTC().Format(time.RFC3339),
			Workspace: Workspace,
			Preloaded: append([]string(nil), preloaded...),
		}
		return writeSandboxMCPReceiptFile(dir, r)
	})
}

// CommitCreateReceipt records creation evidence for a sandbox the caller has
// JUST created (the creation-evidence poll saw it appear): under the
// per-sandbox lock it writes fresh CreatedAt/Preloaded while PRESERVING the
func CommitCreateReceipt(stateDir, sandbox, Workspace string, preloaded []string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		fresh := &MCPReceipt{
			Schema:    MCPStateSchema,
			Sandbox:   sandbox,
			CreatedAt: now().UTC().Format(time.RFC3339),
			Workspace: Workspace,
			Preloaded: append([]string(nil), preloaded...),
		}
		if r, status, _ := ReadMCPReceiptFile(dir, sandbox); status == MCPStateOK {
			fresh.Loads = r.Loads
		}
		return writeSandboxMCPReceiptFile(dir, fresh)
	})
}

// ClearMCPReceipt removes the receipt file for sandbox under the SAME
// per-sandbox lock every writer uses, so a clear can never interleave with a
// concurrent load's read-modify-write. Symlink-safe: the per-sandbox
func ClearMCPReceipt(stateDir, sandbox string) error {
	if err := ValidateStateName(sandbox); err != nil {
		return err
	}
	// Don't materialize a state directory just to clear nothing.
	if _, err := os.Lstat(filepath.Join(MCPStateRoot(stateDir), sandbox)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		if err := os.Remove(filepath.Join(dir, sandboxMCPStateFileName)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// ClearRemovedReceipt clears the receipt for a sandbox the LAUNCHER
// ITSELF just successfully removed (`pix rm`, task rm/gc teardown, the
// --replace pre-remove) — the receipt now describes a dead lifetime. Callers
func ClearRemovedReceipt(sandbox string) error {
	dir, err := MCPStateDirFn()
	if err != nil {
		return fmt.Errorf("resolving pix state dir: %w", err)
	}
	return ClearMCPReceipt(dir, sandbox)
}

// AppendLoadReceipt records a successful `pix mcp load <name>`: called by
// the caller only AFTER its own load succeeded. It read-modify-writes under
// the per-sandbox lock so concurrent loads for the same sandbox never lose an
func AppendLoadReceipt(stateDir, sandbox, name string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("mcp server name is empty")
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		r, status, err := ReadMCPReceiptFile(dir, sandbox)
		switch status {
		case MCPStateOK:
			// use r as read
		case MCPStateAbsent:
			r = &MCPReceipt{Schema: MCPStateSchema, Sandbox: sandbox}
		default:
			return fmt.Errorf("sandbox mcp receipt for %q is unusable (%s): %w", sandbox, status, err)
		}
		for _, l := range r.Loads {
			if l.Name == name {
				return nil // dedupe: first-success timestamp preserved
			}
		}
		r.Loads = append(r.Loads, MCPLoadReceipt{Name: name, At: now().UTC().Format(time.RFC3339)})
		return writeSandboxMCPReceiptFile(dir, r)
	})
}
