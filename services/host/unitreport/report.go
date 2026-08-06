// unitreport is the ONE serializable view of the supervision tree.
//
// It is FOUNDATION (L0) on purpose: the tree that produces it lives at L2
// (supervise), the `serve status` renderer at L1 (service) and doctor at L3, and
// a shared data shape read by three layers cannot live in any one of them
// without an upward import.
//
// `pix serve status --json` and `pix doctor --json` do not get to guess at
// supervision state from a log line: `serve` publishes this snapshot, they read
// it. The shape is deliberately narrow — identity, state, restarts, generation,
// reattach, last error, last probe latency — because that is what an on-call
// engineer answers "is memory actually up, and has it been flapping?" with.
//
// Nothing here may carry a secret. The unit identity is the SHA-256 admission
// fingerprint (env GRANT values enter it only as digests, see UnitSpec.identity),
// and every error string goes through scrubError before it is published.

package unitreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// SchemaVersion is the on-disk contract of the published snapshot. A
// reader that finds a different number must re-read the shape, not the fields.
const SchemaVersion = 1

// Unit is one supervised unit, as published.
type Unit struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Identity is the admission fingerprint (sha256 hex) a reattach must match.
	// It is a digest of the spec, never its secret values.
	Identity string `json:"identity"`
	State    string `json:"state"`
	PID      int    `json:"pid,omitempty"`
	HealthOK bool   `json:"health_ok"`
	// Reattached is true when THIS generation adopted a surviving child rather
	// than spawning one — the difference between "serve restarted" and "the
	// unit restarted".
	Reattached bool `json:"reattached"`
	// Restarts counts generations after the first: a non-zero value with a
	// recent Since is the flap signal.
	Restarts int `json:"restarts"`
	// Generation is the 1-based generation number currently serving.
	Generation int `json:"generation"`
	// LastError is the most recent failure, scrubbed. Empty means none since
	// the unit last became healthy.
	LastError string `json:"last_error,omitempty"`
	// LastProbeUS is the wall time of the most recent health probe, in
	// MICROSECONDS: it is the unit's latency SLI, and a healthy in-process
	// plugin call rounds to 0 at millisecond resolution, which would publish a
	// number that cannot distinguish "fast" from "never probed".
	LastProbeUS int64 `json:"last_probe_us"`
	// SinceUnix is when the unit entered its current state.
	SinceUnix int64 `json:"since_unix"`
}

// Report is the whole tree, as published.
type Report struct {
	SchemaVersion int    `json:"schema_version"`
	PID           int    `json:"pid"`
	GeneratedUnix int64  `json:"generated_unix"`
	Units         []Unit `json:"units"`
}

// secretish matches the shapes a plugin error most plausibly drags a credential
// in as: a KEY=VALUE for a secret-named key, a bearer token, or an op:// ref's
// tail. Errors published to a status file are read by humans AND shipped in bug
// reports, so the redaction is deliberately over-broad: losing a token's tail
// from an error message costs nothing, leaking it costs an incident.
var secretish = regexp.MustCompile(`(?i)\b((?:[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|KEY|CREDENTIAL)[A-Z0-9_]*)\s*[=:]\s*)\S+|(?i)\b(bearer\s+)\S+|(op://\S+)`)

// ScrubError redacts credential-shaped text out of an error string bound for a
// published report. It never drops the error, only its values: an on-call
// engineer still gets the failure, without the report becoming a secret store.
func ScrubError(s string) string {
	if s == "" {
		return ""
	}
	return secretish.ReplaceAllString(s, "${1}${2}<redacted>")
}

// WriteReport publishes a snapshot to path atomically (temp + rename in the same
// directory), so a reader never sees a half-written file and a crash mid-write
// leaves the previous snapshot intact.
func WriteReport(path string, r Report) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".units-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ReadReport loads a published snapshot. A missing file is not an error: it
// means `serve` is not running (or predates this surface), which the caller
// renders as "unknown", never as "healthy".
func ReadReport(path string) (Report, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, false, err
	}
	return r, true, nil
}
