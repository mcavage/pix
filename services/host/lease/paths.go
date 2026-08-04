//go:build unix

package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// instanceIDRE is an ALLOWLIST, not a denylist of "..": only
// [A-Za-z0-9._-], starting with an alphanumeric, capped at 128 bytes. An
// allowlist is the traversal/symlink-adjacent refusal that survives a
// creative encoding a denylist would miss.
var instanceIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateInstanceID rejects any instance ID that could traverse or escape a
// path join: empty, oversized, containing a path separator, "." / "..", or
// outside the allowed charset (notably: no leading dot, so no hidden-file or
// "..*"-shaped surprise).
func ValidateInstanceID(id string) error {
	if id == "" {
		return errors.New("lease: empty instance id")
	}
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("lease: instance id %q contains a path separator", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("lease: instance id %q is a reserved path component", id)
	}
	if !instanceIDRE.MatchString(id) {
		return fmt.Errorf("lease: instance id %q contains characters outside [A-Za-z0-9._-] or exceeds 128 bytes", id)
	}
	return nil
}

// SandboxDir resolves the per-sandbox lease directory for id under root. It
// refuses to return a path that escapes root even after Clean — belt and
// suspenders alongside ValidateInstanceID, since the two checks guard
// different attack shapes (charset vs. lexical join with a root that
// happens to end oddly).
func SandboxDir(root, id string) (string, error) {
	if err := ValidateInstanceID(id); err != nil {
		return "", err
	}
	rootClean := filepath.Clean(root)
	dir := filepath.Join(rootClean, id)
	if dir != rootClean && !strings.HasPrefix(dir, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("lease: instance id %q escapes root %q", id, root)
	}
	return dir, nil
}

// refuseSymlink reports an error if path exists and is a symlink. A missing
// path is not an error here — callers that need existence check separately.
func refuseSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lease: refusing to follow symlink at %s", path)
	}
	return nil
}

// ensureSandboxDir creates dir 0700 if absent, refusing to create through or
// follow a symlink either before or after the create (closing the TOCTOU
// window where something replaces dir with a symlink between the check and
// the mkdir). It also refuses (rather than silently tightening) a dir that
// exists with looser permissions than 0700 left over from... nothing this
// package would ever write, but a hostile pre-existing directory either way.
func ensureSandboxDir(dir string) error {
	if err := refuseSymlink(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("lease: create sandbox dir %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("lease: stat sandbox dir %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lease: refusing to follow symlink at %s", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("lease: %s exists and is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("lease: chmod 0700 %s: %w", dir, err)
		}
	}
	return nil
}

// openNoFollow opens path with flag|O_NOFOLLOW|O_CLOEXEC via a raw syscall
// (not os.OpenFile), because O_NOFOLLOW has no os-package equivalent. Setting
// O_CLOEXEC explicitly here matters precisely BECAUSE this bypasses
// os.OpenFile: the os package would have set it automatically (see
// os/file_unix.go), but a raw syscall.Open does not, so every leaf file this
// package touches must ask for it by hand. A pre-existing symlink at path is
// refused as ELOOP, translated into a plain error, never followed.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("lease: refusing to follow symlink at %s", path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// fcntlGetFD returns the raw fd flags (F_GETFD) for fd — exported at package
// scope (rather than buried in _test.go) because it is the same primitive
// production code could use to assert its own invariant, not just a test
// helper reaching into internals.
func fcntlGetFD(fd uintptr) (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}
