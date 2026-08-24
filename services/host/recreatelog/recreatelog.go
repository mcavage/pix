//go:build unix

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
	"syscall"
	"time"
)

// MaxRecords is the hard retention cap: append past it and the oldest record
// is dropped, always exactly this many survive. It is a constant on purpose
// (see doc.go) — never a config.toml field, never an env var.
const MaxRecords = 100

const (
	fileName     = "recreate.log.json"
	lockFileName = "recreate.log.lock"
)

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
	path := filepath.Join(dir, fileName)
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
	return readRecordsFile(filepath.Join(dir, fileName))
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

// withAppendLock serializes the read-modify-write around fn behind an flock
// on a lock file DISTINCT from the data file: flock is held on an inode, and
// the data file's atomic swap replaces that inode out from under any holder
// who locked the data file directly (the same reason lease keeps refs.lock
// separate from record.json).
func withAppendLock(dir string, fn func() error) error {
	lockPath := filepath.Join(dir, lockFileName)
	f, err := openNoFollow(lockPath, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	deadline := time.Now().Add(appendLockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return &os.PathError{Op: "flock", Path: lockPath, Err: err}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("recreatelog: timed out waiting for the append lock on %s", lockPath)
		}
		time.Sleep(2 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// readRecordsFile decodes path strictly: unknown fields fail the parse
// rather than being silently dropped, because an unknown field is exactly
// the shape a leaked facet value or credential name would take.
func readRecordsFile(path string) ([]Record, error) {
	f, err := openNoFollow(path, syscall.O_RDONLY, 0)
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
	f, err := openNoFollow(tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, 0o600)
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

// openNoFollow opens path with O_NOFOLLOW so a symlink at path is refused
// (ELOOP), never followed — the same primitive services/host/lease uses,
// duplicated here rather than imported: recreatelog holds zero internal
// imports on purpose (see guard_test.go's F10 and doc.go's "no L1 siblings").
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("recreatelog: refusing to follow symlink at %s", path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
