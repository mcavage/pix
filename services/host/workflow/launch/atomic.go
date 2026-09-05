//go:build unix

// atomic.go — the one same-dir temp-file-plus-rename primitive every write in
// this package that must never leave a reader observing a partial file goes
// through. A FIXED ".tmp" name (what session-state and the teardown journal
// both used before this) is fine when a single writer is proven exclusive
// (e.g. lease's keep.json, guarded by keep.lock end to end) but is a real
// collision surface anywhere it is not: two writers racing the SAME fixed
// name can interleave their writes into one file, or one writer's rename can
// commit the OTHER writer's half-written temp. A unique same-directory temp
// name removes that surface outright; it does not, by itself, serialize a
// read-modify-write cycle spanning MULTIPLE files or one file's own prior
// contents — see reap.go's teardown-journal guard for that.
package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// writeFileAtomic writes data to path via a unique temp file in the SAME
// directory (so the final rename(2) is on one filesystem, atomic) plus
// rename, then best-effort cleans up the temp file on any failure. Never
// leaves path missing or containing a mix of two writers' data: the loser of
// a race always contends on the temp name's own O_EXCL (from CreateTemp),
// never on path itself, until the single atomic rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// openNoFollow opens path with O_NOFOLLOW so a symlink at path is refused at
// the SAME syscall that does the open, not a separate Lstat beforehand — an
// Lstat-then-open (or Lstat-then-ReadFile) sequence leaves a TOCTOU window in
// which a symlink swapped in between the two calls is silently followed by
// the second one and its target's bytes returned as if they belonged to the
// path asked for. This is the same primitive services/host/lease and
// services/host/hosttrust each carry their own copy of — an L1 package may
// not import an L1 sibling (see hosttrust/doc.go), and O_NOFOLLOW is a few
// lines, not a shared dependency worth a boundary exception for — duplicated
// here for the same reason, matching hosttrust's exact posture (see
// hosttrust/nofollow_unix.go).
//
// unix-only, and DELIBERATELY has no `//go:build !unix` counterpart: every
// file in this package already carries a bare `//go:build unix` (session.go
// and reap.go use raw syscall.Flock directly), so this package does not
// build on a non-unix GOOS at all today. A same-named fallback file here
// would be a fallback in name only — nothing else in the package could ever
// reach it. hosttrust/nofollow_other.go is what an HONEST fallback looks
// like on a package that genuinely does build cross-platform; this package
// is not (yet) one of those, so pretending otherwise here would be the
// dishonest version this comment exists to rule out.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("launch: refusing to follow symlink at %s", path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// removeAtomicTempSiblings removes every writeFileAtomic leftover temp file
// for path: every entry in path's directory whose name has the literal
// prefix filepath.Base(path)+".tmp-" (the shape os.CreateTemp gives
// writeFileAtomic's temp file, crash-orphaned when a writer dies between its
// CreateTemp and its Rename). It matches via os.ReadDir plus
// strings.HasPrefix rather than filepath.Glob(path+".tmp-*"): Glob treats
// '*', '?', '\\', '[' and ']' ANYWHERE in the pattern — including in
// directory-path segments that are not the final one — as metacharacters,
// not literal bytes. A directory that happens to contain one of them (an
// environment root a user chose, like ".../[archived]/project", or a state
// root under a $HOME containing one) makes Glob silently match the wrong
// set of entries — typically none at all — instead of the literal leftover
// file being asked for, leaving crash debris behind forever. ReadDir plus a
// literal prefix comparison has no such metacharacter surface: every byte in
// the prefix is compared as itself.
func removeAtomicTempSiblings(path string) error {
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + ".tmp-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			errs = append(errs, rerr)
		}
	}
	return errors.Join(errs...)
}
