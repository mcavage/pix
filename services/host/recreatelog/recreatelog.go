package recreatelog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxRecords is the hard retention cap: append past it and the oldest record
// is dropped, always exactly this many survive. It is a constant on purpose
// (see doc.go) — never a config.toml field, never an env var.
const MaxRecords = 100

const (
	// fileName is the ONE literal spelling of the on-disk diagnostic log's
	// name: PRD §5.9 names it exactly `recreates.log`, no `.json` extension
	// — this package still encodes the file as JSON (readRecordsFile /
	// writeRecordsFile), but the PRD names the file, not its encoding.
	// lockFileName derives from it (a `.lock` suffix) so the name is never
	// duplicated as a second, independent literal; Path is the only exported
	// accessor and the temp path (writeRecordsFile) derives from Path's
	// result the same way.
	fileName     = "recreates.log"
	lockFileName = fileName + ".lock"
)

// Path returns the exact on-disk path to the recreate log inside dir:
// <dir>/recreates.log (PRD §5.9). It is the sole accessor a later `pix
// doctor` wiring should call to locate the log; Append and Read use it
// internally too, so this join happens in exactly one place.
func Path(dir string) string {
	return filepath.Join(dir, fileName)
}

// appendLockTimeout bounds how long Append waits for the flock before giving
// up. It is not a liveness signal, only a fixed bound on a fast local
// read-modify-write against a bounded, small file: a caller stuck longer than
// this is stuck on something else (mirrors lease's keepGuardTimeout rationale).
const appendLockTimeout = 5 * time.Second

// Record is the ENTIRE shape this package ever writes or reads. See doc.go
// for why these three fields, and only these three, exist.
type Record struct {
	Timestamp       time.Time `json:"timestamp"`
	Environment     string    `json:"environment"`
	ChangedKeyPaths []string  `json:"changed_key_paths"`
}

var envNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Append records one recreate-boundary event: environment changed the
// canonical keys in changedKeyPaths. It is safe to call concurrently from
// separate host processes — a flock on a dedicated lock file inside dir
// serializes the read-modify-write, and the on-disk swap is a temp-file
// write plus rename so a reader never observes a half-written log.
func Append(dir, environment string, changedKeyPaths []string) error {
	if dir == "" {
		return errors.New("recreatelog: empty state dir")
	}
	if err := validateEnvironment(environment); err != nil {
		return err
	}
	canon, err := canonicalizeChangedKeyPaths(changedKeyPaths)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("recreatelog: create state dir %s: %w", dir, err)
	}
	path := Path(dir)
	return withAppendLock(dir, func() error {
		existing, err := readRecordsFile(path)
		if err != nil {
			return err
		}
		existing = append(existing, Record{
			Timestamp:       time.Now().UTC(),
			Environment:     environment,
			ChangedKeyPaths: canon,
		})
		if len(existing) > MaxRecords {
			existing = existing[len(existing)-MaxRecords:]
		}
		return writeRecordsFile(path, existing)
	})
}

// Read returns every retained record, oldest first. A missing or previously
// deleted log file returns zero records and no error — this is diagnostic
// history, not a durability contract. Malformed content is NOT the same case
// and returns an error: this package never silently discards an unreadable
// log.
func Read(dir string) ([]Record, error) {
	if dir == "" {
		return nil, errors.New("recreatelog: empty state dir")
	}
	return readRecordsFile(Path(dir))
}

func validateEnvironment(name string) error {
	if name == "" {
		return errors.New("recreatelog: empty environment name")
	}
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("recreatelog: environment name %q contains characters outside [A-Za-z0-9._-] or exceeds 128 bytes", name)
	}
	return nil
}

// canonicalizeChangedKeyPaths validates every entry, dedupes, and sorts —
// "canonical" means two callers that disagree on order or repeat a key still
// converge on the same stored record.
func canonicalizeChangedKeyPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("recreatelog: at least one changed key path is required")
	}
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if err := validateChangedKeyPath(p); err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// validateChangedKeyPath refuses anything that is not a bare canonical key
// path: empty, control characters, an absolute filesystem path (unix or
// windows-drive shaped), or a ".." traversal segment. This is the never-a-
// path-outside-the-environment-root guard at the one seam that accepts
// caller-supplied strings.
func validateChangedKeyPath(p string) error {
	if p == "" {
		return errors.New("recreatelog: empty changed key path")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("recreatelog: changed key path %q contains a control character", p)
		}
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || isWindowsAbs(p) {
		return fmt.Errorf("recreatelog: changed key path %q looks like an absolute filesystem path, not a canonical key path", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("recreatelog: changed key path %q contains a %q segment", p, "..")
	}
	return nil
}

func isWindowsAbs(p string) bool {
	return len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z'))
}

// withAppendLock serializes the read-modify-write around fn behind an
// advisory exclusive lock on a lock file DISTINCT from the data file: the
// lock is held on the lock file's own identity, and the data file's atomic
// swap replaces its inode/handle out from under any holder who locked the
// data file directly (the same reason lease keeps refs.lock separate from
// record.json). The actual lock primitive is platform-specific
// (tryLockExclusive/unlockExclusive: syscall.Flock on unix, LockFileEx on
// windows — see lock_unix.go/lock_windows.go); this retry loop, and the
// timeout it enforces, are identical on every platform.
func withAppendLock(dir string, fn func() error) error {
	lockPath := filepath.Join(dir, lockFileName)
	f, err := openNoFollow(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	deadline := time.Now().Add(appendLockTimeout)
	for {
		acquired, err := tryLockExclusive(f)
		if err != nil {
			return &os.PathError{Op: "flock", Path: lockPath, Err: err}
		}
		if acquired {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("recreatelog: timed out waiting for the append lock on %s", lockPath)
		}
		time.Sleep(2 * time.Millisecond)
	}
	defer unlockExclusive(f)
	return fn()
}

// readRecordsFile decodes path strictly: unknown fields fail the parse
// rather than being silently dropped, because an unknown field is exactly
// the shape a leaked facet value or credential name would take.
func readRecordsFile(path string) ([]Record, error) {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var records []Record
	if err := dec.Decode(&records); err != nil {
		return nil, fmt.Errorf("recreatelog: corrupt log at %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("recreatelog: trailing data after the JSON array in %s", path)
	}
	return records, nil
}

// writeRecordsFile writes records to path via a 0600 temp file plus rename,
// so a reader never observes a partially written log.
func writeRecordsFile(path string, records []Record) error {
	if records == nil {
		records = []Record{}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("recreatelog: marshal records: %w", err)
	}
	tmp := path + ".tmp"
	f, err := openNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("recreatelog: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("recreatelog: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("recreatelog: rename into place %s: %w", path, err)
	}
	return nil
}
