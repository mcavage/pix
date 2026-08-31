// baseline.go — the `pix setup` steps architecture §12 lists AROUND the
// memory container: verify prerequisites BEFORE any mutation (step 1),
// install the runtime archive and record the manifest (step 3), verify the
// digest-pinned images (step 4), and create a default environment only when
// none exists (step 5).
//
// Everything here is either pure filesystem work or goes through an injected
// seam, so the whole sequence is drivable from a test against a temp
// PIX_HOME with fake docker/git doubles — the same shape the rest of this
// package already uses.
package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/release"
)

// PrereqChecker reports whether a required host tool is usable. The
// production implementation runs the tool; a test answers from a table.
type PrereqChecker interface {
	// Check runs name with args and returns its combined output.
	Check(name string, args ...string) (string, error)
}

// requiredPrereqs are the host tools architecture §12 step 1 names as
// unconditional: Docker (pix-memory), sbx (every sandbox and the Gateway),
// and Git (`~/.pix` is a repo). `op` is deliberately NOT here: it is
// conditional on an environment that actually resolves an op:// reference
// (safety invariant 8), and setup must not fail a keyless host on it.
var requiredPrereqs = []struct {
	name    string
	args    []string
	remedy  string
	purpose string
}{
	{"docker", []string{"version", "--format", "{{.Server.Version}}"}, "install Docker Desktop (or start the daemon): https://docs.docker.com/get-docker/", "runs the pix-memory container"},
	{"sbx", []string{"--version"}, "install the sbx CLI: https://docs.docker.com/ai/sandboxes/", "creates sandboxes and owns the MCP Gateway"},
	{"git", []string{"--version"}, "install Git (xcode-select --install, or your package manager)", "initializes ~/.pix as an ordinary repo"},
}

// CheckPrereqs verifies every unconditional prerequisite and reports ALL
// failures at once, each with its exact remedy. It runs before setup mutates
// anything: a host missing Docker must not first get a half-initialized home
// and a registered MCP name it cannot serve.
func CheckPrereqs(c PrereqChecker) error {
	if c == nil {
		return nil
	}
	var problems []string
	for _, p := range requiredPrereqs {
		if _, err := c.Check(p.name, p.args...); err != nil {
			problems = append(problems, fmt.Sprintf("%s is required (%s): %v\n    fix: %s", p.name, p.purpose, err, p.remedy))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("pix setup: unmet prerequisites, nothing was changed:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ImageRepo is the published repository each release image lives in. The
// release manifest pins DIGESTS, not repositories, so the repo name has to
// come from somewhere: it comes from here, once, for both images.
const (
	AgentImageRepo  = "docker.io/mcavage/pix-agent"
	MemoryImageRepo = "docker.io/mcavage/pix-memory"
)

// AgentImageRef and MemoryImageRef are the immutable, PULLABLE references
// for a manifest — repo@sha256:… , never a tag (architecture §3:
// "published references are immutable digests; convenience tags are not
// launch identity").
func AgentImageRef(m release.Manifest) string  { return AgentImageRepo + "@" + m.PixAgentDigest }
func MemoryImageRef(m release.Manifest) string { return MemoryImageRepo + "@" + m.PixMemoryDigest }

// EnsureImages verifies both digest-pinned images are present locally and
// pulls whichever is not. A pull that fails is reported with the exact
// command the user can run themselves — setup never leaves "the image is
// missing" as an unexplained container failure later.
func EnsureImages(r container.Runner, m release.Manifest) error {
	if r == nil {
		r = container.DefaultRunner
	}
	for _, ref := range []string{AgentImageRef(m), MemoryImageRef(m)} {
		if _, err := r.Run("image", "inspect", ref); err == nil {
			continue
		}
		if out, err := r.Run("pull", ref); err != nil {
			return fmt.Errorf("pix setup: cannot obtain the release-pinned image %s: %v (%s)\n    fix: docker pull %s",
				ref, err, strings.TrimSpace(out), ref)
		}
	}
	return nil
}

// DefaultEnvironmentName is the environment `pix setup` creates on a host
// that has none at all.
const DefaultEnvironmentName = "default"

// EnsureDefaultEnvironment creates ONE minimal, runnable environment when
// (and only when) no environment exists yet. It never overwrites, never
// merges, and never touches an existing environment directory: an
// environment is a plain directory the user owns (surface §4), and setup's
// job is only to make sure a brand-new host is not left with zero.
func EnsureDefaultEnvironment(home pixhome.Paths, m release.Manifest) (bool, error) {
	entries, err := os.ReadDir(home.Envs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", home.Envs, err)
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return false, nil // the host already has an environment; leave everything alone
		}
	}

	root := home.EnvironmentDir(DefaultEnvironmentName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", root, err)
	}
	files := map[string]string{
		".sbxenv.yaml": defaultSbxenv(m),
		"pix.toml":     defaultSidecar(),
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		// O_EXCL: this function only ever CREATES. A file already there
		// (a half-made environment, a user's own draft) is left as authored.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return false, fmt.Errorf("create %s: %w", path, err)
		}
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			return false, fmt.Errorf("write %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return false, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return true, nil
}

// defaultSbxenv is the native document: schema version, the pinned agent
// identity, and nothing else. No workspace (the launch supplies it), no
// host-executing MCP server (that would need an explicit `pix env trust`),
// no secrets. A default environment must be runnable AND boring.
func defaultSbxenv(m release.Manifest) string {
	return `# Created by ` + "`pix setup`" + ` because this host had no environment yet.
# It is an ordinary native sbx document you own: edit it, copy it, or delete
# it. Pix never rewrites this file.
schemaVersion: "1"
agent: pix
`
}

// defaultSidecar is the thin pix.toml: the strict release identity setup
// installed (so a launch and `pix doctor` agree on what this environment
// pins) and nothing that native sbx already owns.
//
// It deliberately selects NO model and NO local inference backend. Model
// choice is by name from this file's [models].main, Pi's default otherwise
// (safety invariant 10), and a local inference backend is authored here per
// environment — setup does not interview for either, and `pix doctor`
// reports what an environment actually declares.
func defaultSidecar() string {
	return `# Created by ` + "`pix setup`" + `. Thin on purpose: it declares only what native
# sbx does not own. Pix never rewrites this file.
schema = 1

[models]
# main = "claude-sonnet-4-5"   # a model NAME; empty means Pi's own default

[memory]
scope = "shared"

# [inference.backends.ollama]  # a local backend is authored here, per
# driver = "ollama"            # environment; run 'pix doctor' to see whether
# protocol = "openai"          # this host can actually reach it.
# base_url = "http://host.docker.internal:11434/v1"
# auth = "none"
`
}

// runtimeKitRevision keeps the manifest's kit identity reachable from the
// setup result without a second lookup: a caller (the command's renderer,
// `pix doctor`) shows the exact release identity that was installed.
func runtimeKitRevision(m release.Manifest) string { return m.KitRevision }
