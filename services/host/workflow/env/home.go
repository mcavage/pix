package env

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"pix/host/pixhome"
)

// This file is the v2 environment surface: an environment IS a directory
// under $PIX_HOME/envs, and that is the whole selection model
// (docs/design/pix-v2-surface.md §3.4). There is no registration database,
// no add/edit/use/forget mutation path, and no fuzzy lookup. An
// environment stored elsewhere is represented by a SYMLINK in that
// directory, resolved exactly once per invocation so every later read,
// containment check, and fingerprint uses the same resolved path.

// nameRE is the accepted environment-directory name. It excludes every
// path separator and every relative-path spelling, so a name can never
// steer a read out of envs/.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidName reports whether name is an acceptable environment name.
func ValidName(name string) bool {
	if !nameRE.MatchString(name) || len(name) > 64 {
		return false
	}
	return name != "." && name != ".."
}

// Selected is one resolved environment: the name the user typed, the
// canonical directory under envs/ that name addresses, and the ROOT every
// read must use — identical to Dir for an ordinary directory, and the
// symlink's one-hop target otherwise.
type Selected struct {
	Name      string
	Dir       string
	Root      string
	Symlinked bool
}

// SbxEnvPath and SidecarPath are the two files Pix interprets inside an
// environment (surface §5).
func (s Selected) SbxEnvPath() string  { return filepath.Join(s.Root, ".sbxenv.yaml") }
func (s Selected) SidecarPath() string { return filepath.Join(s.Root, "pix.toml") }

// UnknownError is the refusal for a name with no directory under envs/. It
// carries the known names so a caller can render its own presentation, and
// its message never echoes the typed name back as if it were a suggestion.
type UnknownError struct {
	Name  string
	Known []string
}

func (e *UnknownError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pix: no environment named %q.", e.Name)
	if len(e.Known) > 0 {
		fmt.Fprintf(&b, "\n     known: %s", strings.Join(e.Known, ", "))
	}
	fmt.Fprintf(&b, "\n     create it: mkdir -p <pix-home>/envs/%s and author .sbxenv.yaml there", e.Name)
	return b.String()
}

// ResolveIn resolves name against one Pix home. It follows AT MOST the
// named symlink itself: a symlink whose target is another symlink is
// refused rather than chased, because "resolve once per invocation" cannot
// be honestly claimed about an arbitrary chain, and each additional hop is
// another path a later repoint can change under an approved fingerprint.
func ResolveIn(home pixhome.Paths, name string) (Selected, error) {
	name = strings.TrimSpace(name)
	if !ValidName(name) {
		return Selected{}, fmt.Errorf("pix: %q is not a valid environment name (letters, digits, dot, dash, underscore)", name)
	}
	dir := home.EnvironmentDir(name)
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Selected{}, &UnknownError{Name: name, Known: mustList(home)}
		}
		return Selected{}, err
	}
	sel := Selected{Name: name, Dir: dir, Root: dir}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(dir)
		if err != nil {
			return Selected{}, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(dir), target)
		}
		target = filepath.Clean(target)
		tfi, err := os.Lstat(target)
		if err != nil {
			return Selected{}, fmt.Errorf("pix: environment %q is a symlink to %s, which does not exist", name, target)
		}
		if tfi.Mode()&os.ModeSymlink != 0 {
			return Selected{}, fmt.Errorf("pix: environment %q is a symlink to another symlink (%s); refusing to follow a chain", name, target)
		}
		sel.Root = target
		sel.Symlinked = true
		fi = tfi
	}
	if !fi.IsDir() {
		return Selected{}, fmt.Errorf("pix: environment %q is not a directory (%s)", name, sel.Root)
	}
	if err := refuseUnsafeMode(name, sel.Root, fi); err != nil {
		return Selected{}, err
	}
	return sel, nil
}

// refuseUnsafeMode refuses a group- or world-writable environment root: a
// directory anyone else can write is a directory anyone else can put a
// host-executing MCP command in, after this launch approved it.
func refuseUnsafeMode(name, root string, fi os.FileInfo) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("pix: environment %q at %s is group- or world-writable; refusing to read host-executing configuration from it.\n     fix it: chmod go-w %s", name, root, root)
	}
	return nil
}

// List returns every environment directory under envs/, sorted by name.
// An entry that fails resolution (a dangling symlink, a stray file) is
// skipped from the LIST but still refuses when selected by name: listing
// is a convenience, selection is the authority.
func List(home pixhome.Paths) ([]Selected, error) {
	entries, err := os.ReadDir(home.Envs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Selected
	for _, e := range entries {
		if !ValidName(e.Name()) {
			continue
		}
		sel, err := ResolveIn(home, e.Name())
		if err != nil {
			continue
		}
		out = append(out, sel)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func mustList(home pixhome.Paths) []string {
	sels, err := List(home)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(sels))
	for _, s := range sels {
		names = append(names, s.Name)
	}
	return names
}

// SelectName applies §3.1's selection precedence for one launch: an
// explicit `--env` wins and NEVER falls back to the default when it is
// invalid; then the task's recorded environment; then the machine default.
// An empty result means "no environment selected", which is a legitimate
// state, not an error.
func SelectName(explicit, taskEnv, machineDefault string) string {
	if e := strings.TrimSpace(explicit); e != "" {
		return e
	}
	if t := strings.TrimSpace(taskEnv); t != "" {
		return t
	}
	return strings.TrimSpace(machineDefault)
}

// SelectIn resolves the selected name against home. An explicit name that
// does not resolve is an error; no fallback to the default happens here or
// anywhere else.
func SelectIn(home pixhome.Paths, explicit, taskEnv, machineDefault string) (Selected, bool, error) {
	name := SelectName(explicit, taskEnv, machineDefault)
	if name == "" {
		return Selected{}, false, nil
	}
	sel, err := ResolveIn(home, name)
	if err != nil {
		return Selected{}, false, err
	}
	return sel, true, nil
}
