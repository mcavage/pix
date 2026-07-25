// sandboxmcpstate.go — the launcher-owned per-sandbox MCP receipt: durable
// evidence that PI-STACK ITSELF (not the sandbox, not the gateway) has
// successfully created a sandbox with a given static-MCP preload, and/or
// successfully attached a server to it live via `pi-stack mcp load`.
//
// WHY THIS EXISTS: `doctor`/`status` want to answer "is this sandbox's MCP
// set what I think it is" without re-probing the gateway (which may be down,
// slow, or simply not asked) — the receipt is a local, offline record of past
// SUCCESSFUL pi-stack operations, not a live poll. It lives OUTSIDE the
// sandbox and outside any workspace: <state-dir>/sandboxes/<sandbox>/mcp.json,
// state-dir being XDG_STATE_HOME/pi-stack (config.StateDir()) in production —
// ephemeral runtime state, the same home as the serve pidfile/lock, never the
// config dir (a `pi-stack state reset` moving the config dir aside must not
// touch it, and it is keyed by sandbox identity, not config).
//
// SCOPE (schema 1): Sandbox (identity the receipt is FOR — checked on every
// read so a reused/renamed directory can never silently supply someone
// else's receipt), CreatedAt + Preloaded (committed once per lifetime by
// commitCreateReceipt, as soon as the caller's OWN `sbx run` create is
// observably done — the static-MCP set requested at create time), and Loads
// (appended by appendLoadReceipt right after the caller's OWN successful
// `mcp load`, one entry per server name, FIRST-success timestamp preserved —
// a receipt answers "has this ever worked", not "when did it last run").
//
// LIFECYCLE: a receipt is scoped to ONE lifetime of a sandbox NAME, bounded
// by pi-stack's own operations. A definite create/replace (run.go's
// execSbxRunAndRecordCreate) first CLEARS any stale receipt under the
// per-sandbox lock (clearSandboxMCPReceipt) — a load history from a previous
// incarnation of the name must never leak into the new one — then, once the
// freshly-created sandbox is observably present (the creation-evidence poll,
// while the interactive session is still ALIVE, so status/doctor can render
// preload provenance mid-session), COMMITS the create receipt
// (commitCreateReceipt): fresh CreatedAt/Preloaded, merging ONLY loads
// appended after the clear — a concurrent `pi-stack mcp load` racing the
// create is preserved, a prior lifetime's loads are not. Every successful
// launcher-side removal (`pi-stack rm`, task rm/gc, the --replace pre-remove)
// clears the receipt via clearRemovedSandboxReceipt; a FAILED or unknowable
// removal RETAINS it — evidence is discarded only on positive proof the
// lifetime ended. Appending a load to a sandbox that predates this feature
// (no receipt on disk yet) synthesizes a PARTIAL receipt (IsPartial — no
// CreatedAt/Preloaded): it proves ONLY the loads it lists, never the
// create-time preload set; evidence starts from the first thing pi-stack
// could actually observe, never backfilled with a guess.
//
// HONEST LIMITATION: sbx exposes no immutable per-sandbox ID, so a sandbox
// removed and recreated under the SAME NAME entirely outside the launcher
// (raw `sbx rm` + `sbx run`) cannot be distinguished from the lifetime this
// receipt describes — the receipt is keyed by name only, and we do not
// invent an identity sbx doesn't provide. The exposure is bounded, not
// eliminated: any LAUNCHER removal clears the receipt, and the next launcher
// create clears again before writing, so stale evidence survives only an
// external same-name churn with no launcher operation in between.
//
// TRUST: every read REJECTS rather than silently degrades — schema mismatch,
// malformed JSON, and sandbox-identity mismatch are all distinct typed
// reasons (sandboxMCPStateStatus), never folded into "absent" (a genuinely
// untouched sandbox, which is legitimately empty, not wrong). Callers
// (doctor/status) use Unverifiable() to tell "nothing recorded yet" apart
// from "something here can't be trusted" and render accordingly.
//
// HARDENING (same class as workspacestate.go / packtruststore.go): the state
// root ("sandboxes") and the per-sandbox leaf directory are each Lstat-refused
// if they are a symlink (never MkdirAll-and-trust); the receipt file is never
// opened for write directly — atomicWriteInDir's same-dir temp + fsync +
// rename never follows a symlinked destination; directories are 0700, the
// receipt file 0600. Every read-modify-write (appendLoadReceipt, and
// writeCreateReceipt for symmetry) is serialized by a per-sandbox flock
// (mcp.json.lock, alongside mcp.json in the same directory) so concurrent
// `pi-stack mcp load` calls for the same sandbox can never lose an update to
// a last-writer-wins race.
//
// TESTABILITY: every entry point takes stateDir explicitly (a t.TempDir() in
// tests, config.StateDir() in production) and an injectable clock
// (func() time.Time) — no test ever touches the real XDG state dir or wall
// clock.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pi-stack/host/config"
)

// sandboxMCPStateSchema is the current receipt schema version. A receipt
// written under a different schema is REJECTED on read (sandboxMCPStateSchemaMismatch),
// never partially trusted — a future schema change gets a bump here plus an
// explicit migration, not a silent best-effort decode.
const sandboxMCPStateSchema = 1

// sandboxMCPStateFileName is the receipt file's name within its per-sandbox
// directory.
const sandboxMCPStateFileName = "mcp.json"

// sandboxMCPStateLockName is the per-sandbox advisory lock file, a sibling of
// mcp.json in the same directory — never the receipt file itself, so a lock
// holder never blocks a concurrent reader on file content.
const sandboxMCPStateLockName = "mcp.json.lock"

// sandboxMCPLoadReceipt is one successful `pi-stack mcp load` for a server
// name, at the FIRST time it was observed to succeed (appendLoadReceipt never
// updates At for a name already present).
type sandboxMCPLoadReceipt struct {
	Name string `json:"name"`
	At   string `json:"at"` // RFC3339 UTC
}

// sandboxMCPReceipt is the schema-1 on-disk receipt. Sandbox is the identity
// check (compared against the caller's requested name on every read);
// CreatedAt/Preloaded are set once by writeCreateReceipt and are empty on a
// partial receipt synthesized by appendLoadReceipt for a pre-existing
// sandbox; Loads accumulates via appendLoadReceipt, deduped by name.
type sandboxMCPReceipt struct {
	Schema    int                     `json:"schema"`
	Sandbox   string                  `json:"sandbox"`
	CreatedAt string                  `json:"created_at,omitempty"`
	Preloaded []string                `json:"preloaded,omitempty"`
	Loads     []sandboxMCPLoadReceipt `json:"loads,omitempty"`
}

// IsPartial reports whether r is a PARTIAL receipt: one synthesized by
// appendLoadReceipt for a sandbox whose creation pi-stack never observed
// (empty CreatedAt). A partial receipt proves ONLY the loads it lists — it
// says nothing about the create-time preload set, so a consumer must never
// read "no entry" in it as "positively never attached"; for every other name
// the honest answer is unverifiable.
func (r *sandboxMCPReceipt) IsPartial() bool {
	return r != nil && r.CreatedAt == ""
}

// sandboxMCPStateStatus is the typed outcome of reading a receipt — the
// "unavailable reason" doctor/status render instead of collapsing every
// failure into "absent".
type sandboxMCPStateStatus int

const (
	// sandboxMCPStateOK: a receipt exists, matches this schema, and matches
	// the requested sandbox identity.
	sandboxMCPStateOK sandboxMCPStateStatus = iota
	// sandboxMCPStateAbsent: no receipt file — a genuinely untouched sandbox
	// (never create/load-receipted, e.g. predates this feature). Legitimately
	// empty, NOT unverifiable.
	sandboxMCPStateAbsent
	// sandboxMCPStateUnreadable: an I/O-level problem reading the file itself
	// (permission denied, a symlinked path refused, etc.) — not a content
	// problem.
	sandboxMCPStateUnreadable
	// sandboxMCPStateCorrupt: the file exists but is not valid JSON for this
	// type.
	sandboxMCPStateCorrupt
	// sandboxMCPStateSchemaMismatch: valid JSON, but the schema field is not
	// the one this binary understands.
	sandboxMCPStateSchemaMismatch
	// sandboxMCPStateIdentityMismatch: valid JSON, correct schema, but the
	// receipt's own Sandbox field does not match the sandbox whose directory
	// it was read from — never trust a reused/renamed directory's leftover
	// receipt.
	sandboxMCPStateIdentityMismatch
)

// String renders the status for log/report lines (doctor/status).
func (s sandboxMCPStateStatus) String() string {
	switch s {
	case sandboxMCPStateOK:
		return "ok"
	case sandboxMCPStateAbsent:
		return "absent"
	case sandboxMCPStateUnreadable:
		return "unreadable"
	case sandboxMCPStateCorrupt:
		return "corrupt"
	case sandboxMCPStateSchemaMismatch:
		return "schema-mismatch"
	case sandboxMCPStateIdentityMismatch:
		return "identity-mismatch"
	default:
		return "unknown"
	}
}

// Unverifiable reports whether status means "a receipt is present but cannot
// be trusted" (corrupt bytes, an unreadable path, a schema this binary
// doesn't know, or an identity mismatch) as opposed to sandboxMCPStateOK or
// sandboxMCPStateAbsent (no receipt yet — legitimately empty). Callers must
// render Unverifiable() distinctly from "empty": an absent receipt says
// "nothing recorded", an unverifiable one says "something is wrong here,
// don't trust the empty read".
func (s sandboxMCPStateStatus) Unverifiable() bool {
	return s != sandboxMCPStateOK && s != sandboxMCPStateAbsent
}

// validateSandboxStateName rejects a sandbox name that could traverse or
// escape its directory: empty, ".", "..", or containing any path separator
// (either OS's, checked unconditionally so a receipt written on one platform
// can never be abused on another). A valid name is used AS the per-sandbox
// directory's leaf component — there is no separate sanitize/hash step (unlike
// sanitizeTaskName): an invalid sandbox name here means the caller passed
// something that was never a real sandbox name, so refusing outright is
// correct and callers of writeCreateReceipt/appendLoadReceipt/
// readSandboxMCPReceipt already have the real sbx-assigned name in hand.
func validateSandboxStateName(name string) error {
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
// (config.StateDir(), i.e. XDG_STATE_HOME/pi-stack) for real callers. Tests
// call writeCreateReceipt/appendLoadReceipt/readSandboxMCPReceipt directly
// with a t.TempDir() stateDir instead — this helper is never on a test's path.
func defaultSandboxMCPStateDir() (string, error) {
	return config.StateDir()
}

// sandboxMCPStateDirFn is the ONE seam run.go's recordCreateReceipt and
// mcp.go's recordMcpLoadReceipt resolve the state root through — production
// wiring never calls defaultSandboxMCPStateDir (nor config.StateDir())
// directly. Tests override it to a t.TempDir()-backed stub so a wiring test
// never touches the real XDG state dir; a test that wants to exercise the
// REAL path contract (config.StateDir()'s XDG_STATE_HOME/pi-stack join, no
// doubled "pi-stack") sets $XDG_STATE_HOME instead and leaves this seam at
// its default.
var sandboxMCPStateDirFn = defaultSandboxMCPStateDir

// receiptRecordError wraps a failure to durably record an otherwise-successful
// pi-stack operation (a sandbox create, or an `mcp load` attach) in the
// per-sandbox MCP receipt. It is kept as a DISTINCT typed error — never folded
// into the underlying operation's own error — so run.go/mcp.go can report the
// honest, narrower truth: the operation itself worked; only the local
// bookkeeping didn't, so doctor/status must not be told it succeeded cleanly,
// and the caller must never print a plain success line over this failure.
type receiptRecordError struct {
	op      string // "create" or "mcp load"
	sandbox string
	name    string // mcp server name; set only for a load receipt
	err     error
}

func (e *receiptRecordError) Error() string {
	if e.name != "" {
		return fmt.Sprintf("%s %q on sandbox %q succeeded, but recording it in local state failed: %v", e.op, e.name, e.sandbox, e.err)
	}
	return fmt.Sprintf("%s of sandbox %q succeeded, but recording it in local state failed: %v", e.op, e.sandbox, e.err)
}

func (e *receiptRecordError) Unwrap() error { return e.err }

// sandboxMCPStateRoot is <stateDir>/sandboxes, the parent of every
// per-sandbox receipt directory.
func sandboxMCPStateRoot(stateDir string) string {
	return filepath.Join(stateDir, "sandboxes")
}

// mkdirSymlinkSafe creates dir (and its parents) if absent, then Lstat-refuses
// it if the leaf turned out to be a symlink — MkdirAll alone treats an
// existing symlink-to-directory as "already there" and happily proceeds, so a
// pre-planted symlink at either the state root or the per-sandbox leaf would
// otherwise have writes land wherever it points. Mirrors
// writeWorkspaceStateFile's dir check (workspacestate.go), generalized to any
// directory in this file's own tree.
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
// leaf are checked; a symlinked ancestor further up (stateDir itself) is out
// of scope, same posture as workspacestate.go's single-level check.
func ensureSandboxMCPStateDir(stateDir, sandbox string) (string, error) {
	if err := validateSandboxStateName(sandbox); err != nil {
		return "", err
	}
	root := sandboxMCPStateRoot(stateDir)
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
// read-modify-write in this file, so concurrent writers for the SAME sandbox
// (two `pi-stack mcp load` calls racing, or a load racing a create) can never
// interleave into a lost update. Different sandboxes never contend — the lock
// is per-directory.
func withSandboxMCPStateLock(stateDir, sandbox string, fn func(dir string) error) error {
	dir, err := ensureSandboxMCPStateDir(stateDir, sandbox)
	if err != nil {
		return err
	}
	return withFlock(filepath.Join(dir, sandboxMCPStateLockName), func() error {
		return fn(dir)
	})
}

// writeSandboxMCPReceiptFile marshals and writes r to dir/mcp.json,
// symlink-safe + atomic (never opens the destination directly — a same-dir
// temp file is fsync'd then renamed over it, replacing any symlink rather
// than following it) via the shared atomicWriteInDir helper, at 0600.
func writeSandboxMCPReceiptFile(dir string, r *sandboxMCPReceipt) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteInDir(dir, sandboxMCPStateFileName, append(b, '\n'), 0o600)
}

// readSandboxMCPReceiptFile reads dir/mcp.json without following a symlinked
// destination (Lstat first, refuse if it is a symlink), then validates schema
// and sandbox identity. See sandboxMCPStateStatus for the full outcome space.
func readSandboxMCPReceiptFile(dir, sandbox string) (*sandboxMCPReceipt, sandboxMCPStateStatus, error) {
	path := filepath.Join(dir, sandboxMCPStateFileName)
	fi, lerr := os.Lstat(path)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return nil, sandboxMCPStateAbsent, nil
		}
		return nil, sandboxMCPStateUnreadable, lerr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, sandboxMCPStateUnreadable, fmt.Errorf("%s is a symlink; refusing to read through it", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, sandboxMCPStateUnreadable, err
	}
	var r sandboxMCPReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, sandboxMCPStateCorrupt, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.Schema != sandboxMCPStateSchema {
		return nil, sandboxMCPStateSchemaMismatch, fmt.Errorf("%s: schema %d, want %d", path, r.Schema, sandboxMCPStateSchema)
	}
	if r.Sandbox != sandbox {
		return nil, sandboxMCPStateIdentityMismatch, fmt.Errorf("%s: sandbox identity %q, want %q", path, r.Sandbox, sandbox)
	}
	return &r, sandboxMCPStateOK, nil
}

// readSandboxMCPReceipt reads the receipt for sandbox under stateDir. It
// never creates anything: an absent directory or file is
// sandboxMCPStateAbsent, not an error. See sandboxMCPStateStatus.Unverifiable
// for how callers should distinguish "nothing recorded" from "don't trust
// this".
func readSandboxMCPReceipt(stateDir, sandbox string) (*sandboxMCPReceipt, sandboxMCPStateStatus, error) {
	if err := validateSandboxStateName(sandbox); err != nil {
		return nil, sandboxMCPStateUnreadable, err
	}
	dir := filepath.Join(sandboxMCPStateRoot(stateDir), sandbox)
	return readSandboxMCPReceiptFile(dir, sandbox)
}

// writeCreateReceipt is the plain-REPLACE create record: fresh CreatedAt
// (now(), UTC RFC3339) and Preloaded (a copy of the requested static-MCP
// set), Loads reset to none, whatever was on disk discarded. It is the
// DEGRADED create commit — used only when the pre-create clear could not be
// proven (clearSandboxMCPReceipt failed), where merging would risk
// resurrecting a prior lifetime's loads; the normal path is
// commitCreateReceipt, which preserves loads recorded since the clear.
//
// now defaults to time.Now when nil (production callers may still pass it
// explicitly for consistency; tests always inject a fixed clock).
func writeCreateReceipt(stateDir, sandbox string, preloaded []string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		r := &sandboxMCPReceipt{
			Schema:    sandboxMCPStateSchema,
			Sandbox:   sandbox,
			CreatedAt: now().UTC().Format(time.RFC3339),
			Preloaded: append([]string(nil), preloaded...),
		}
		return writeSandboxMCPReceiptFile(dir, r)
	})
}

// commitCreateReceipt records creation evidence for a sandbox the caller has
// JUST created (the creation-evidence poll saw it appear): under the
// per-sandbox lock it writes fresh CreatedAt/Preloaded while PRESERVING the
// Loads of a VALID receipt already on disk. The caller cleared the receipt
// (clearSandboxMCPReceipt) before starting the create, so any loads present
// now were appended by a concurrent `pi-stack mcp load` DURING this create
// window — this lifetime's own evidence, which a plain replace would erase
// (the lost-update the old post-exit write had). Anything on disk that is
// not a valid OK receipt (absent, corrupt, wrong schema/identity) is
// replaced outright: the caller positively owns this lifetime's start, so
// only its own valid appends may merge.
func commitCreateReceipt(stateDir, sandbox string, preloaded []string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		fresh := &sandboxMCPReceipt{
			Schema:    sandboxMCPStateSchema,
			Sandbox:   sandbox,
			CreatedAt: now().UTC().Format(time.RFC3339),
			Preloaded: append([]string(nil), preloaded...),
		}
		if r, status, _ := readSandboxMCPReceiptFile(dir, sandbox); status == sandboxMCPStateOK {
			fresh.Loads = r.Loads
		}
		return writeSandboxMCPReceiptFile(dir, fresh)
	})
}

// clearSandboxMCPReceipt removes the receipt file for sandbox under the SAME
// per-sandbox lock every writer uses, so a clear can never interleave with a
// concurrent load's read-modify-write. Symlink-safe: the per-sandbox
// directory is refused if symlinked (withSandboxMCPStateLock), and os.Remove
// on the receipt file removes a planted symlink itself, never what it points
// to. A missing directory or file is a clean no-op — clearing an
// already-empty lifetime is not an error.
func clearSandboxMCPReceipt(stateDir, sandbox string) error {
	if err := validateSandboxStateName(sandbox); err != nil {
		return err
	}
	// Don't materialize a state directory just to clear nothing.
	if _, err := os.Lstat(filepath.Join(sandboxMCPStateRoot(stateDir), sandbox)); err != nil {
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

// clearRemovedSandboxReceipt clears the receipt for a sandbox the LAUNCHER
// ITSELF just successfully removed (`pi-stack rm`, task rm/gc teardown, the
// --replace pre-remove) — the receipt now describes a dead lifetime. Callers
// must invoke it ONLY on positive removal success; a failed or unknowable
// removal retains the receipt (evidence is discarded only on proof). Resolves
// the state root through the same sandboxMCPStateDirFn seam as the writers.
func clearRemovedSandboxReceipt(sandbox string) error {
	dir, err := sandboxMCPStateDirFn()
	if err != nil {
		return fmt.Errorf("resolving pi-stack state dir: %w", err)
	}
	return clearSandboxMCPReceipt(dir, sandbox)
}

// appendLoadReceipt records a successful `pi-stack mcp load <name>`: called by
// the caller only AFTER its own load succeeded. It read-modify-writes under
// the per-sandbox lock so concurrent loads for the same sandbox never lose an
// update:
//
//   - No receipt yet (sandboxMCPStateAbsent — an old sandbox that predates
//     this feature, or one that was never create-receipted): a PARTIAL
//     receipt is synthesized with no CreatedAt/Preloaded — evidence starts
//     from what pi-stack could actually observe, never backfilled.
//   - A receipt exists and is OK: name is appended to Loads UNLESS already
//     present, in which case this is a no-op — the FIRST success timestamp is
//     preserved, never overwritten by a later reload of the same server.
//   - A receipt exists but is unverifiable (corrupt/schema/identity mismatch)
//     or unreadable: appendLoadReceipt FAILS CLOSED rather than silently
//     clobbering it — an unverifiable receipt is a signal something is wrong,
//     not a blank slate to overwrite.
func appendLoadReceipt(stateDir, sandbox, name string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("mcp server name is empty")
	}
	return withSandboxMCPStateLock(stateDir, sandbox, func(dir string) error {
		r, status, err := readSandboxMCPReceiptFile(dir, sandbox)
		switch status {
		case sandboxMCPStateOK:
			// use r as read
		case sandboxMCPStateAbsent:
			r = &sandboxMCPReceipt{Schema: sandboxMCPStateSchema, Sandbox: sandbox}
		default:
			return fmt.Errorf("sandbox mcp receipt for %q is unusable (%s): %w", sandbox, status, err)
		}
		for _, l := range r.Loads {
			if l.Name == name {
				return nil // dedupe: first-success timestamp preserved
			}
		}
		r.Loads = append(r.Loads, sandboxMCPLoadReceipt{Name: name, At: now().UTC().Format(time.RFC3339)})
		return writeSandboxMCPReceiptFile(dir, r)
	})
}
