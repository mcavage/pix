package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/pixhome"
	"pix/host/sys"
)

// This file is the Pix v2 secrets.env surface (docs/design/
// pix-v2-surface.md §3.5, §4, pix-v2-architecture.md §11): a single file
// under PIX_HOME holding `NAME=op://vault/item/field` references ONLY. There
// is no NonSecret allowlist here — v2 has no pack layer to author one — and
// no literal value is ever accepted, unlike the v1 secrets.env path this
// package also serves. It reuses the existing line-oriented primitives
// (upsertOpRef, removeOpRef, ParseOpRefs, NormalizeOpRef, EnvVarNameRe)
// rather than re-deriving parsing, since a second secrets.env grammar would
// be exactly the kind of drift this package exists to prevent.
//
// secret is L1 capability; pixhome is L0 foundation, so this import is a
// down-only reference and does not need home passed as a bare string the way
// config's L0 machine.go does.

// RefsEnvPath resolves <home>/secrets.env.
func RefsEnvPath(home pixhome.Paths) string { return home.SecretsEnv }

// LoadRefs reads and classifies every reference in <home>/secrets.env. A
// missing file returns an empty slice and a nil error: no secrets configured
// yet is not a failure. Every classified entry's IsRef is expected true in
// v2 — SetRef never lets a non-op:// line land there — but a file a user
// hand-edited may carry one anyway, so callers should still check IsRef
// rather than assume it.
func LoadRefs(home pixhome.Paths) ([]OpRef, error) {
	path := RefsEnvPath(home)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseOpRefs(string(data), nil), nil
}

// NotAnOpRefError is SetRef's refusal for a value that is not an op://
// reference — the one rule this file exists to enforce
// (pix-v2-surface.md §3.5: "accepts only an op:// reference").
type NotAnOpRefError struct {
	Key   string
	Value string
}

func (e *NotAnOpRefError) Error() string {
	return fmt.Sprintf("pix secret set: %s is not an op:// reference — secrets.env holds op://vault/item/field references only, never a literal value (got %q)", e.Key, e.Value)
}

// InvalidEnvVarNameError is SetRef's/RemoveRef's refusal for a key that does
// not look like a shell environment variable name.
type InvalidEnvVarNameError struct{ Key string }

func (e *InvalidEnvVarNameError) Error() string {
	return fmt.Sprintf("pix secret set: %q does not look like an env var name (want %s)", e.Key, EnvVarNameRe.String())
}

// secretsEnvLockName is the advisory transaction lock guarding
// <home>/secrets.env's read-modify-write, a sibling of the file itself
// (security re-review MEDIUM: SetRef/RemoveRef used to race a concurrent
// `pix secret set`/`rm` in another process, silently losing whichever
// reference lost the race — the same class of bug v1's secrets.env already
// guards against via WithProviderRefsLock). It is a DIFFERENT file from v1's
// provider-refs.lock: that one guards the v1 config-dir secrets.env, this
// one guards the v2 PIX_HOME secrets.env, and the two must never share a
// lock (a v1 and v2 transaction over two unrelated files must not be able to
// block each other).
const secretsEnvLockName = ".secrets.lock"

// secretsEnvLockPath is <home>/.secrets.lock, adjacent to secrets.env.
func secretsEnvLockPath(home pixhome.Paths) string {
	return filepath.Join(filepath.Dir(RefsEnvPath(home)), secretsEnvLockName)
}

// SetRef validates key and value, then durably upserts KEY=op://... into
// <home>/secrets.env: a same-directory temp file, fsync, then atomic rename
// — never a value written anywhere else, and never a value this function
// returns or logs. value is normalized first (NormalizeOpRef strips a
// 1Password "Copy Secret Reference" paste's surrounding quotes) so a pasted
// ref is accepted the same way v1's RunSecretSet accepts one. The whole
// read-modify-write runs under secretsEnvLockPath (an O_NOFOLLOW flock, see
// sys.Lock), so a concurrent SetRef/RemoveRef in another process can never
// interleave with this one and silently drop a reference.
func SetRef(home pixhome.Paths, key, value string) error {
	if !EnvVarNameRe.MatchString(key) {
		return &InvalidEnvVarNameError{Key: key}
	}
	value = NormalizeOpRef(value)
	if !strings.HasPrefix(value, "op://") {
		return &NotAnOpRefError{Key: key, Value: value}
	}
	if i := strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return fmt.Errorf("pix secret set: %s value contains a control character at byte %d; secrets.env is one ref per line", key, i)
	}
	// A literal space is required verbatim (op 2.35+ rejects a percent-encoded
	// one in a spaced 1Password field name), same as v1's RunSecretSetLocked.
	value = strings.ReplaceAll(value, "%20", " ")

	return sys.Lock(secretsEnvLockPath(home), func() error {
		path := RefsEnvPath(home)
		content := ""
		if data, err := os.ReadFile(path); err == nil {
			content = string(data)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}

		newContent := upsertOpRef(content, key, value)
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		return atomicWriteSecrets(dir, path, []byte(newContent), 0o600)
	})
}

// RemoveRef removes key's line from <home>/secrets.env. A missing file or a
// key never present is a clean, idempotent no-op — matching v1's RunSecretRm
// posture (surface §3.5: "removes one reference, never a 1Password item").
// Locked the same way SetRef is, and against the SAME lock file, so a set
// and a remove for two different keys still serialize rather than racing
// each other's read-modify-write.
func RemoveRef(home pixhome.Paths, key string) error {
	if !EnvVarNameRe.MatchString(key) {
		return &InvalidEnvVarNameError{Key: key}
	}
	return sys.Lock(secretsEnvLockPath(home), func() error {
		path := RefsEnvPath(home)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read %s: %w", path, err)
		}
		newContent, removed := removeOpRef(string(data), key)
		if !removed {
			return nil
		}
		dir := filepath.Dir(path)
		return atomicWriteSecrets(dir, path, []byte(newContent), 0o600)
	})
}

// OpReader resolves one op:// reference without ever returning its value —
// the injectable seam CheckRef uses so a test never has to run the real `op`
// binary, and so no caller anywhere in this file can accidentally print a
// resolved secret.
type OpReader interface {
	// ReadRef attempts to resolve ref (e.g. by running `op read <ref>`
	// against a discarded stdout) and reports only whether it succeeded.
	ReadRef(ref string) error
}

// CheckRef resolves ref through reader and reports only success or failure,
// exactly the surface's "resolves references through op without printing
// their values" contract (surface §3.5). A caller that wants a value has the
// wrong function; nothing in this package ever returns one.
func CheckRef(reader OpReader, ref string) error {
	if !strings.HasPrefix(strings.TrimSpace(ref), "op://") {
		return &NotAnOpRefError{Value: ref}
	}
	if reader == nil {
		return fmt.Errorf("no op reader configured")
	}
	return reader.ReadRef(ref)
}

// atomicWriteSecrets writes data to a temp file in dir, fsyncs it, then
// renames it over path. Mirrors config.writeFileAtomic's shape without
// importing it (secret already imports config for other reasons, but this
// keeps the write primitive local and free of v1 assumptions).
func atomicWriteSecrets(dir, path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
