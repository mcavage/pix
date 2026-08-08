package task

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one task's full picture: its metadata, git-hygiene facts, the
// sandbox disposition the caller supplied (via probe, or SandboxUnknown when
// probe is nil), and whether `Remove` would currently refuse it — the
// "ls JSON predicts removal" contract.
type Entry struct {
	Meta        Meta
	Sandbox     SandboxDisposition
	Git         GitState
	WouldRefuse bool
	Reasons     []string
	// Unreadable carries a metadata read/harden failure for a task whose
	// meta.json exists but could not be trusted or parsed. Meta.Name is still
	// populated (from the filename) so `ls` can name the offending task.
	Unreadable string
}

// readMetas reads every persisted task's metadata under stateRoot for
// mainroot. metas holds every task whose metadata parsed and hardened
// cleanly (Meta.Sandbox populated, ready for a caller to probe); unreadable
// holds a stub Entry for every task whose meta.json couldn't be trusted or
// parsed at all.
func readMetas(stateRoot, mainroot string) (metas []Meta, unreadable []Entry, err error) {
	repoDir := RepoDir(mainroot)
	repokey := RepoKey(mainroot)
	metaDir := filepath.Join(stateRoot, repoDir, "meta")
	des, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(de.Name(), ".json")
		_, metaPath := Paths(stateRoot, repoDir, name)
		m, rerr := ReadMeta(metaPath)
		if rerr != nil {
			unreadable = append(unreadable, Entry{Meta: Meta{Name: name}, Unreadable: fmt.Sprintf("unreadable metadata: %v", rerr)})
			continue
		}
		m, herr := HardenMeta(m, mainroot, repokey, name)
		if herr != nil {
			unreadable = append(unreadable, Entry{Meta: m, Unreadable: herr.Error()})
			continue
		}
		metas = append(metas, m)
	}
	return metas, unreadable, nil
}

// SandboxNames returns the sandbox name every currently-valid task recorded
// for mainroot would resolve to (skipping tasks whose metadata is unreadable
// or fails hardening — List surfaces those separately as Unreadable
// entries). It probes nothing itself: a caller uses it to learn which
// sandbox names to probe, THEN builds the plain SandboxDisposition map List
// takes, rather than handing List a callback to invoke on its own schedule.
func SandboxNames(stateRoot, mainroot string) ([]string, error) {
	metas, _, err := readMetas(stateRoot, mainroot)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(metas))
	for _, m := range metas {
		names = append(names, m.Sandbox)
	}
	return names, nil
}

// List returns every task recorded for mainroot under stateRoot, sorted by
// name. dispositions is the caller-resolved sandbox liveness for every name
// SandboxNames returned (typically probed just before this call); List
// itself never shells out to a sandbox runner. A task's sandbox name missing
// from dispositions (including a nil map) reads as the zero value,
// SandboxUnknown, which — by RemoveGuard's fail-safe rule — always predicts
// WouldRefuse=true; callers that want an accurate prediction must populate
// every name SandboxNames reported.
func List(stateRoot, mainroot string, dispositions map[string]SandboxDisposition) ([]Entry, error) {
	metas, out, err := readMetas(stateRoot, mainroot)
	if err != nil {
		return nil, err
	}
	repoDir := RepoDir(mainroot)
	for _, m := range metas {
		co, _ := Paths(stateRoot, repoDir, SanitizeName(m.Name))
		git := GatherGitState(m.Mechanism, mainroot, co)
		disp := dispositions[m.Sandbox]
		reasons, ok := RemoveGuard(git, disp, false)
		out = append(out, Entry{Meta: m, Sandbox: disp, Git: git, WouldRefuse: !ok, Reasons: reasons})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out, nil
}

// Forget deletes a task's metadata file only (the checkout is removed
// separately via RemoveCheckout, in whichever order the caller's teardown
// sequence requires).
func Forget(stateRoot, mainroot, name string) error {
	_, metaPath := Paths(stateRoot, RepoDir(mainroot), SanitizeName(name))
	err := os.Remove(metaPath)
	if err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

func isNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || err == fs.ErrNotExist)
}
