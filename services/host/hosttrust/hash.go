package hosttrust

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// IsSymlink reports whether path itself is a symlink (Lstat, no follow). It
// mirrors packinfo.IsSymlinkPath for the same reason CanonicalRoot mirrors
// packinfo.CanonicalizePackRoot: L1 may not import L1.
func IsSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// HashFile is the content-hashing half of a fingerprint: read path's bytes
// and hash them, refusing a symlink rather than following it. It opens path
// with O_NOFOLLOW (openNoFollow) rather than checking IsSymlink and then
// opening separately: the two-step check-then-open would leave a TOCTOU
// window where a symlink swapped in between the check and the read is
// silently followed. An unhashable surface is an ERROR — it can be neither
// accepted nor installed, so a caller fails closed rather than
// fingerprinting a partial surface.
func HashFile(path, label string) (string, error) {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("%s: %v (cannot fingerprint the host-exec surface; fail closed)", label, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("%s: %v (cannot fingerprint the host-exec surface; fail closed)", label, err)
	}
	return HashBytes(data), nil
}

// HashBytes is the raw-content half of the fingerprint: sha256 of data, hex
// encoded. HashFile is this plus the symlink-refused file read — a caller
// that already has the bytes in hand (e.g. an immutable setup-hook snapshot)
// uses this directly rather than round-tripping through a temp file.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadVerifiedFile is HashFile's byte-returning twin: it opens path with
// O_NOFOLLOW (refusing a symlink at the SAME syscall that does the open,
// never a separate Lstat beforehand) and then fstats the ALREADY-OPEN
// descriptor to require a regular file — fstat, not a second Lstat, because
// a descriptor that is already open cannot itself be raced into following a
// symlink between the check and the read. A caller that needs the exact
// bytes it is about to trust — a setup-hook TOCTOU snapshot copy, not just
// a digest — uses this instead of HashFile plus a second, independent
// re-read of the same path.
func ReadVerifiedFile(path string) ([]byte, error) {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s: is a directory, not a file", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: is not a regular file", path)
	}
	return io.ReadAll(f)
}
