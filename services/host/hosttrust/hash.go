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
