package pixhome

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// dirMode is applied to every directory Init creates. Architecture §5 asks
// specifically for machine-state parents to be 0700; Init applies it
// uniformly to the whole home tree rather than special-casing a subset,
// since PIX_HOME is a personal tool's private root end to end.
const dirMode = 0o700

// shareMode is applied to the two files Init writes that are meant to be
// read by anyone who clones this Git repository: README.md and .gitignore.
// Neither carries machine state, so 0600 does not apply to them the way it
// does to config.toml/secrets.env (written later, by setup — see
// docs/design/pix-v2-surface.md §4).
const shareMode = 0o644

// GitInitArgs is the exact argv Init issues to establish a fresh repository:
// `git init -b main` (pix-v2-surface.md §4). It is a package var, not an
// inline literal, so a test can assert the exact call a Runner received.
var GitInitArgs = []string{"init", "-b", "main"}

// Runner runs one host command with dir as its working directory. This is
// the "small interface" architecture §4 requires for every git/docker/sbx/op
// call, so a test supplies a deterministic double instead of a real git
// process ever running under `go test`.
type Runner interface {
	Run(dir, name string, args ...string) (string, error)
}

// DefaultRunner is the production Runner: os/exec, nothing else.
var DefaultRunner Runner = execRunner{}

type execRunner struct{}

func (execRunner) Run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitignoreContent is byte-for-byte the ignore rule pix-v2-surface.md §4
// specifies: exclude machine state and runtime material, track everything
// else (README, skills, agents, output styles, environments).
const gitignoreContent = "/config.toml\n/secrets.env\n/runtime/\n/.state/\n"

// readmeContent is the seed README.md Init writes into a fresh home. It is
// never overwritten once present — an existing README, however the user
// edited it, is what "preserving existing files" means.
const readmeContent = `# Pix home

This directory holds your Pix configuration, skills, agents, and
environments. It was initialized as an ordinary Git repository, but nothing
here is staged or committed on your behalf.

Machine state and runtime data (config.toml, secrets.env, runtime/, .state/)
are excluded by .gitignore. Everything else — this README, context/, and
environments — is yours to edit, commit, and share.
`

// InitResult reports what Init actually did on this call, so a caller
// (pix setup, or a test) can distinguish a fresh install from an idempotent
// rerun that changed nothing.
type InitResult struct {
	CreatedHome      bool
	CreatedGitRepo   bool
	CreatedGitignore bool
	CreatedREADME    bool
	// CreatedDirs lists every directory Init created on this call, relative
	// to Home, in the order they were created.
	CreatedDirs []string
}

// Init idempotently establishes p.Home: every directory in the layout,
// .gitignore, README.md, and a fresh `git init -b main` repository
// (pix-v2-surface.md §4). It never stages, commits, configures a remote, or
// overwrites a file that already exists — rerunning Init against an
// already-initialized home only fills in whatever a previous partial run (or
// a package manager that could not touch $HOME) left missing.
//
// runner is nil-safe: a nil Runner falls back to DefaultRunner, so
// production callers never have to thread one through by hand.
func Init(p Paths, runner Runner) (InitResult, error) {
	if runner == nil {
		runner = DefaultRunner
	}
	var res InitResult

	homeExisted := isDir(p.Home)
	if err := os.MkdirAll(p.Home, dirMode); err != nil {
		return res, fmt.Errorf("create pix home %s: %w", p.Home, err)
	}
	// Tighten an existing home that may have been created with a looser mode
	// by whatever made the directory first (a package manager, `mkdir -p`, a
	// test harness's own default) — MkdirAll only sets the mode of a
	// directory it CREATES, never one that already existed.
	if err := os.Chmod(p.Home, dirMode); err != nil {
		return res, fmt.Errorf("chmod pix home %s: %w", p.Home, err)
	}
	res.CreatedHome = !homeExisted

	dirs := []string{
		p.Context, p.ContextSkills, p.ContextOutputStyles, p.Envs, p.Runtime,
		p.State, p.StateEffective, p.StateMemory, p.StateMemoryBackups,
		p.StateSandboxes, p.StateSessions, p.StateTasks,
		p.StateTrust, p.StateTrustEnvironments,
	}
	for _, d := range dirs {
		existed := isDir(d)
		if err := os.MkdirAll(d, dirMode); err != nil {
			return res, fmt.Errorf("create %s: %w", d, err)
		}
		if !existed {
			rel, err := filepath.Rel(p.Home, d)
			if err != nil {
				rel = d
			}
			res.CreatedDirs = append(res.CreatedDirs, rel)
		}
	}

	createdREADME, err := writeIfAbsent(p.README, []byte(readmeContent), shareMode)
	if err != nil {
		return res, fmt.Errorf("write %s: %w", p.README, err)
	}
	res.CreatedREADME = createdREADME

	createdGitignore, err := writeIfAbsent(p.Gitignore, []byte(gitignoreContent), shareMode)
	if err != nil {
		return res, fmt.Errorf("write %s: %w", p.Gitignore, err)
	}
	res.CreatedGitignore = createdGitignore

	if !isDir(p.Git) {
		if out, err := runner.Run(p.Home, "git", GitInitArgs...); err != nil {
			return res, fmt.Errorf("git init -b main in %s: %w (%s)", p.Home, err, out)
		}
		res.CreatedGitRepo = true
	}

	return res, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// writeIfAbsent durably writes data to path with mode perm, but only if
// nothing is there yet. It publishes via a same-directory temp file, fsync,
// then a no-clobber hard link — never a rename, which would silently
// overwrite a file a concurrent Init (or the user) created in the same
// instant (architecture §5: "atomic rename or no-replace link").
func writeIfAbsent(path string, data []byte, perm os.FileMode) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
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
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	if err := os.Link(tmpPath, path); err != nil {
		if os.IsExist(err) {
			// Lost a race to a concurrent Init (or the file appeared between
			// our Stat and here) — not an error, the file exists either way.
			return false, nil
		}
		return false, err
	}
	cleanup = false
	_ = os.Remove(tmpPath)
	return true, nil
}
