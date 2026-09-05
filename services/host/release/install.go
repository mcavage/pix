// install.go — installing the runtime archive under PIX_HOME
// (docs/design/pix-v2-architecture.md §12 step 3: "installs the runtime
// archive and records the release manifest").
//
// The extractor is deliberately paranoid. A runtime archive is content this
// process unpacks into the user's home with the user's own privileges, so it
// is treated as untrusted input even though its digest was just verified:
//
//   - only entries under the canonical `runtime/<version>/` prefix are
//     extracted, everything else is a hard error (never a silent skip);
//   - absolute paths, `..` traversal, and any escape from the staging root
//     are refused;
//   - only regular files and directories are materialized — a symlink,
//     hardlink, device, fifo, or socket entry fails the install, because
//     each of those is a way to write outside the destination or hand a
//     later reader a file it did not expect;
//   - the archive is unpacked into a staging directory and published with a
//     single rename, so an interrupted install never leaves a half-written
//     runtime/<version> for doctor or a launch to read.
//
// The destination is ALWAYS <home>/runtime/<version>. Nothing here can write
// to <home>/skills, <home>/agents, <home>/pi, or any other user-owned path:
// those are the user's forks (surface §4.2) and a release install never
// touches them.
package release

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MaxRuntimeArchiveBytes bounds the total uncompressed bytes an install will
// write. The runtime archive is skills, agent presets, and a few JSON files
// — tens of megabytes at the very most — so this is generous while still
// making a decompression bomb fail fast instead of filling the user's disk.
const MaxRuntimeArchiveBytes int64 = 256 << 20

// runtimeStampFile records, inside an installed runtime directory, the
// manifest digest it was extracted from. It is what makes a rerun of
// `pix setup` a no-op instead of a re-extract, and what makes a PARTIAL
// prior install (interrupted before the stamp was written, or written by an
// older/other release) re-install rather than be trusted.
const runtimeStampFile = ".pix-runtime-digest"

// RuntimeInstall reports what InstallRuntime did.
type RuntimeInstall struct {
	// Dir is <home>/runtime/<version>, installed or already present.
	Dir string
	// Extracted is true when this call unpacked the archive. False means a
	// matching runtime was already installed and was left untouched.
	Extracted bool
}

// RuntimeDir is where a version's runtime content lives under a Pix home.
func RuntimeDir(home, version string) string {
	return filepath.Join(home, "runtime", version)
}

// InstallRuntime verifies b's archive against its manifest and installs it
// at <home>/runtime/<version>. It is idempotent: an already-installed
// runtime carrying the same digest stamp is left exactly as it is.
func InstallRuntime(home string, b Bundle) (RuntimeInstall, error) {
	res := RuntimeInstall{Dir: RuntimeDir(home, b.Manifest.Version)}

	if stamp, err := os.ReadFile(filepath.Join(res.Dir, runtimeStampFile)); err == nil {
		if strings.TrimSpace(string(stamp)) == b.Manifest.RuntimeDigest {
			return res, nil
		}
	}

	if err := b.VerifyArchive(); err != nil {
		return res, err
	}

	parent := filepath.Dir(res.Dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", parent, err)
	}
	stage, err := os.MkdirTemp(parent, ".stage-"+b.Manifest.Version+"-")
	if err != nil {
		return res, fmt.Errorf("stage the runtime archive under %s: %w", parent, err)
	}
	defer os.RemoveAll(stage)

	if err := extractRuntimeArchive(b.ArchivePath, b.Manifest.Version, stage); err != nil {
		return res, fmt.Errorf("%w\n%s", err, installRemedy)
	}
	if err := os.WriteFile(filepath.Join(stage, runtimeStampFile), []byte(b.Manifest.RuntimeDigest+"\n"), 0o600); err != nil {
		return res, fmt.Errorf("stamp the staged runtime: %w", err)
	}

	// Publish by rename. An existing directory (a previous partial install,
	// or the same version re-installed from a different archive) is moved
	// aside first and deleted after, so the window where <version> does not
	// exist is one rename wide and never leaves a merged half-old tree.
	var trash string
	if _, err := os.Stat(res.Dir); err == nil {
		trash, err = os.MkdirTemp(parent, ".trash-"+b.Manifest.Version+"-")
		if err != nil {
			return res, fmt.Errorf("prepare to replace %s: %w", res.Dir, err)
		}
		old := filepath.Join(trash, "old")
		if err := os.Rename(res.Dir, old); err != nil {
			os.RemoveAll(trash)
			return res, fmt.Errorf("replace %s: %w", res.Dir, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("stat %s: %w", res.Dir, err)
	}

	if err := os.Rename(stage, res.Dir); err != nil {
		if trash != "" {
			// Put the previous runtime back rather than leaving nothing.
			_ = os.Rename(filepath.Join(trash, "old"), res.Dir)
			os.RemoveAll(trash)
		}
		return res, fmt.Errorf("install %s: %w", res.Dir, err)
	}
	if trash != "" {
		os.RemoveAll(trash)
	}
	res.Extracted = true
	return res, nil
}

// extractRuntimeArchive unpacks ONLY `runtime/<version>/**` from archivePath
// into dest (which becomes the content of runtime/<version> itself).
func extractRuntimeArchive(archivePath, version, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", archivePath, err)
	}
	defer gz.Close()

	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	prefix := "runtime/" + version
	var written int64

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archivePath, err)
		}

		rel, err := runtimeEntryPath(hdr.Name, prefix)
		if err != nil {
			return fmt.Errorf("%s: %w", archivePath, err)
		}
		if rel == "" {
			continue // the runtime/ and runtime/<version> directories themselves
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("%s: entry %q escapes the runtime directory", archivePath, hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if hdr.FileInfo().Mode()&0o100 != 0 {
				mode = 0o700
			}
			n, err := writeRuntimeFile(target, tr, mode, MaxRuntimeArchiveBytes-written)
			written += n
			if err != nil {
				return fmt.Errorf("%s: extract %q: %w", archivePath, hdr.Name, err)
			}
		default:
			// Symlink, hardlink, char/block device, fifo, socket, and every
			// GNU extension: refused by name so the failure is diagnosable.
			return fmt.Errorf("%s: refusing entry %q: only regular files and directories are allowed in a runtime archive (got tar type %q)",
				archivePath, hdr.Name, string(rune(hdr.Typeflag)))
		}
	}
	return nil
}

// runtimeEntryPath validates one tar entry name and returns its path
// RELATIVE to runtime/<version>, or "" for the prefix directories
// themselves. Anything outside the canonical prefix is an error, never a
// skip: an archive carrying unexpected members is not a runtime archive.
func runtimeEntryPath(name, prefix string) (string, error) {
	slashed := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(slashed, "/") || (len(slashed) > 1 && slashed[1] == ':') {
		return "", fmt.Errorf("refusing absolute entry %q", name)
	}
	clean := path.Clean(strings.TrimPrefix(slashed, "./"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("refusing entry %q: path traversal", name)
	}
	clean = strings.TrimSuffix(clean, "/")
	switch {
	case clean == "runtime" || clean == prefix:
		return "", nil
	case strings.HasPrefix(clean, prefix+"/"):
		rel := strings.TrimPrefix(clean, prefix+"/")
		if rel == "" {
			return "", nil
		}
		return rel, nil
	default:
		return "", fmt.Errorf("refusing entry %q: outside the canonical %s/ layout", name, prefix)
	}
}

func writeRuntimeFile(target string, r io.Reader, mode os.FileMode, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, fmt.Errorf("runtime archive exceeds the %d-byte install limit", MaxRuntimeArchiveBytes)
	}
	// O_EXCL: a duplicate entry name must fail rather than overwrite what a
	// previous entry already wrote into the staging tree.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(r, budget))
	if err != nil {
		return n, err
	}
	if n == budget {
		// Hit the cap exactly: there may be more, which means the archive is
		// over budget. Refuse rather than silently truncate.
		var probe [1]byte
		if extra, _ := r.Read(probe[:]); extra > 0 {
			return n, fmt.Errorf("runtime archive exceeds the %d-byte install limit", MaxRuntimeArchiveBytes)
		}
	}
	return n, nil
}
