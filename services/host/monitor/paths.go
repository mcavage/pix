package monitor

// paths.go is the safety layer every on-disk write in this package goes
// through: 0700 directories, 0600 files, no symlink is ever created OR
// followed, and no attacker-influenced string (sandboxId/sessionId arrive
// off the wire, see event.go's capEnvelopeIDs doc) is ever used to build a
// path without first being reduced to a fixed, safe charset.
//
// This mirrors the idiom services/host/lease/paths.go already established
// (allowlist charset, refuse-symlink-before-mkdir, chmod-if-loose) rather
// than inventing a second one; the difference is monitor stays a portable
// (non-unix-build-tagged) package like the hub/ring it replaces, so it uses
// Lstat-based refusal instead of a raw O_NOFOLLOW syscall open. That is a
// real, accepted tradeoff (a TOCTOU window between the Lstat check and the
// Open remains in principle) rather than an oversight: this is a host-local
// debug wiretap with no auth already (see cmd/pix/monitor.go's non-loopback
// bind warning), not the sandbox-lifecycle lock lease/ protects.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// hexHashRE matches a lowercase hex sha256 digest exactly — the ALLOWLIST a
// content-addressed blob hash must satisfy before it is ever used to build a
// path (blobPath). Unlike a stream key (arbitrary wire-supplied
// sandboxId/sessionId, which must be slugified — see slugify/streamDirName),
// a blob hash is computed by this package itself from bl.Text (BlobStore.Put
// verifies it), so rejecting anything outside this shape is a strict
// equality check, not a lossy transform.
var hexHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validHash reports whether hash is exactly a lowercase hex sha256 digest.
func validHash(hash string) bool {
	return hexHashRE.MatchString(hash)
}

// slugify reduces s to a filesystem-safe, printable ASCII string: only
// [A-Za-z0-9._-] survive, everything else (including a path separator, a
// NUL, or terminal control bytes) becomes '_'. It never returns a path
// traversal shape ("", ".", ".." and any run of only dots/underscores/dashes
// all collapse to "_"), and it is capped at 80 bytes so one absurd wire
// field can't produce an absurd directory name. It is NOT injective — two
// different inputs can slugify to the same string — which is exactly why
// streamDirName appends a content hash suffix rather than using slugify's
// output alone as the path component.
func slugify(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	if strings.Trim(out, "._-") == "" {
		return "_"
	}
	return out
}

// streamDirName derives the on-disk directory name for one (sandboxID,
// sessionID) event stream: a slugified, human-legible prefix (for a support
// bundle / `ls` to read at a glance) plus a 12-hex-char sha256 suffix of the
// EXACT (unslugified) pair, so two different wire ids that happen to
// slugify identically (e.g. one all-control-bytes, one merely "_") still
// land in different directories rather than silently merging their event
// streams. This is the ONLY place a wire-supplied id becomes a path
// component; every path built from it afterward (streamDir, Tail, List) is
// already a plain, safe, program-computed string.
func streamDirName(sandboxID, sessionID string) string {
	sum := sha256.Sum256([]byte(sandboxID + "\x00" + sessionID))
	return fmt.Sprintf("%s--%s--%s", slugify(sandboxID), slugify(sessionID), hex.EncodeToString(sum[:])[:12])
}

// blobPath returns the on-disk path for hash, sharded two hex chars deep (a
// plain flat directory of thousands of blob files is unpleasant to `ls`,
// and some filesystems degrade with very large directories). hash MUST have
// already passed validHash; blobPath itself re-validates and returns an
// error rather than trust a caller that skipped the check, since this
// string becomes a path component.
func blobPath(root, hash string) (string, error) {
	if !validHash(hash) {
		return "", fmt.Errorf("monitor: invalid blob hash %q", hash)
	}
	return root + string(os.PathSeparator) + hash[:2] + string(os.PathSeparator) + hash + ".json", nil
}

// refuseSymlink errors if path exists and is a symlink. A missing path is
// not an error — callers that need existence check separately. This is the
// check half of the check-then-open pattern documented at the top of this
// file: it closes the common case (a symlink planted before we ever look at
// path) without claiming to close the race against one planted between this
// call and the Open that follows it.
func refuseSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("monitor: refusing to follow symlink at %s", path)
	}
	return nil
}

// ensureDir0700 creates dir (and, since streams nest one level under root,
// its parent) at 0700 if absent, refusing to create through or follow a
// symlink at any level, and tightening an existing directory's mode back to
// 0700 if it was left looser. It does not touch a directory's mode if it is
// already exactly 0700 (the common, fast path).
func ensureDir0700(dir string) error {
	if err := refuseSymlink(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("monitor: create dir %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("monitor: stat dir %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("monitor: refusing to follow symlink at %s", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("monitor: %s exists and is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("monitor: chmod 0700 %s: %w", dir, err)
		}
	}
	return nil
}

// openAppend0600 opens path for append, creating it at 0600 if absent, after
// refusing an existing symlink at that exact path (see the file-level doc
// comment on the accepted TOCTOU tradeoff).
func openAppend0600(path string) (*os.File, error) {
	if err := refuseSymlink(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	// os.OpenFile with O_CREATE on an EXISTING file does not re-apply the
	// mode bits; if the file predates this run with looser permissions
	// (or another process created it), tighten it explicitly rather than
	// trusting whatever was already there.
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != 0o600 {
		_ = f.Chmod(0o600)
	}
	return f, nil
}

// writeFileAtomic0600 writes data to path via a temp file in the same
// directory + rename, refusing a pre-existing symlink at path first. The
// temp-then-rename is what makes a concurrent reader (Tail/List) never
// observe a half-written file; the symlink refusal is the same
// check-then-open tradeoff as everywhere else in this file.
func writeFileAtomic0600(path string, data []byte) error {
	if err := refuseSymlink(path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := refuseSymlink(tmp); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("monitor: create temp file %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("monitor: write temp file %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("monitor: close temp file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("monitor: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
