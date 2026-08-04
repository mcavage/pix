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

// List returns every task recorded for mainroot under stateRoot, sorted by
// name. probe (nilable) resolves a sandbox name to its live disposition; List
// itself never shells out to a sandbox runner. A nil probe leaves every
// entry's Sandbox at SandboxUnknown, which — by RemoveGuard's fail-safe rule
// — always predicts WouldRefuse=true; callers that want an accurate
// prediction must supply a real probe.
func List(stateRoot, mainroot string, probe func(sandboxName string) SandboxDisposition) ([]Entry, error) {
	repoDir := RepoDir(mainroot)
	repokey := RepoKey(mainroot)
	metaDir := filepath.Join(stateRoot, repoDir, "meta")
	des, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(de.Name(), ".json")
		_, metaPath := Paths(stateRoot, repoDir, name)
		m, err := ReadMeta(metaPath)
		if err != nil {
			out = append(out, Entry{Meta: Meta{Name: name}, Unreadable: fmt.Sprintf("unreadable metadata: %v", err)})
			continue
		}
		m, herr := HardenMeta(m, mainroot, repokey, name)
		if herr != nil {
			out = append(out, Entry{Meta: m, Unreadable: herr.Error()})
			continue
		}
		co, _ := Paths(stateRoot, repoDir, name)
		git := GatherGitState(m.Mechanism, mainroot, co)
		disp := SandboxUnknown
		if probe != nil {
			disp = probe(m.Sandbox)
		}
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
