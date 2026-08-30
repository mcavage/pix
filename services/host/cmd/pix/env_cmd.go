// env_cmd.go — `pix env`: list | show | default | trust (docs/design/
// pix-v2-surface.md §3.4). An environment IS a directory under
// ~/.pix/envs/<name>/; there is no registration database and no
// add/edit/use/forget mutation path — those verbs are gone in v2. Selection
// and listing come from workflow/env's pixhome-based ResolveIn/List
// (home.go). `default` reads/writes the one config.toml field pixhome.Machine
// owns. `trust` is the explicit host-execution approval command: it
// fingerprints the environment's two authored files and records acceptance
// under ~/.pix/state/trust, outside the environment directory itself.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/pixhome"
	nativeenv "pix/host/workflow/env"
)

func (c *envCmd) Help() string {
	return `A named environment: a directory under ~/.pix/envs/<name>/ declaring
.sbxenv.yaml (native sbx grammar) and an optional pix.toml sidecar.

Four verbs: list, show, default, trust. There is no add/edit/use/forget:
create, edit, move, and remove an environment with ordinary filesystem and
Git tools under ~/.pix/envs. 'pix setup' may scaffold a default one.

An environment that runs host code or handles a credential must be
approved with 'pix env trust NAME' before a launch will use it.`
}

// envCmd's field ORDER is the v2 four-verb surface; bare 'pix env' is
// 'env list'.
type envCmd struct {
	List    envListCmd    `cmd:"" default:"1" help:"List environments under ~/.pix/envs, the default, and trust state."`
	Show    envShowCmd    `cmd:"" help:"What NAME is: files, resolved root, trust state."`
	Default envDefaultCmd `cmd:"" help:"Print, or set, the machine default environment."`
	Trust   envTrustCmd   `cmd:"" help:"Read and accept what NAME runs on your host."`
}

func envRun(d *cli.Deps, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "pix: ") {
		msg = "pix: " + msg
	}
	fmt.Fprintln(d.Err, msg)
	return cli.SilentError{Code: cli.ExitCode(err)}
}

func envHome() (pixhome.Paths, error) { return pixhome.Resolve() }

// ── list ─────────────────────────────────────────────────────────────────

type envListCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

type envListRow struct {
	Name      string `json:"name"`
	Root      string `json:"root"`
	Symlinked bool   `json:"symlinked"`
	Default   bool   `json:"default"`
	Trusted   bool   `json:"trusted"`
}

func (c *envListCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	sels, err := nativeenv.List(home)
	if err != nil {
		return envRun(d, err)
	}
	m, _ := pixhome.LoadMachine(home)
	rows := make([]envListRow, 0, len(sels))
	for _, s := range sels {
		trusted, _ := trustAccepted(home, s)
		rows = append(rows, envListRow{
			Name: s.Name, Root: s.Root, Symlinked: s.Symlinked,
			Default: s.Name == m.DefaultEnvironment, Trusted: trusted,
		})
	}
	if c.JSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(d.Out, string(b))
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(d.Out, "No environments yet. Create one: mkdir -p ~/.pix/envs/<name> && author .sbxenv.yaml there.")
		return nil
	}
	for _, r := range rows {
		mark := ""
		if r.Default {
			mark = " (default)"
		}
		trust := "untrusted"
		if r.Trusted {
			trust = "trusted"
		}
		fmt.Fprintf(d.Out, "%s\t%s\t%s%s\n", r.Name, r.Root, trust, mark)
	}
	return nil
}

// ── show ─────────────────────────────────────────────────────────────────

type envShowCmd struct {
	Name string `arg:"" help:"Exact environment name."`
	JSON bool   `help:"Emit machine-readable JSON."`
	Path bool   `help:"Print only the resolved root."`
}

func (c *envShowCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	sel, err := nativeenv.ResolveIn(home, c.Name)
	if err != nil {
		return envRun(d, err)
	}
	if c.Path {
		fmt.Fprintln(d.Out, sel.Root)
		return nil
	}
	trusted, fp := trustAccepted(home, sel)
	if c.JSON {
		b, _ := json.MarshalIndent(map[string]any{
			"name": sel.Name, "root": sel.Root, "symlinked": sel.Symlinked,
			"trusted": trusted, "fingerprint": fp,
		}, "", "  ")
		fmt.Fprintln(d.Out, string(b))
		return nil
	}
	fmt.Fprintf(d.Out, "name:        %s\n", sel.Name)
	fmt.Fprintf(d.Out, "root:        %s\n", sel.Root)
	fmt.Fprintf(d.Out, "symlinked:   %v\n", sel.Symlinked)
	fmt.Fprintf(d.Out, "sbxenv:      %s\n", presentIfExists(sel.SbxEnvPath()))
	fmt.Fprintf(d.Out, "sidecar:     %s\n", presentIfExists(sel.SidecarPath()))
	fmt.Fprintf(d.Out, "trusted:     %v\n", trusted)
	return nil
}

func presentIfExists(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return "(absent)"
}

// ── default ──────────────────────────────────────────────────────────────

type envDefaultCmd struct {
	Name string `arg:"" optional:"" help:"Set the machine default to this exact environment name (omit to print it)."`
}

func (c *envDefaultCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	if c.Name == "" {
		m, err := pixhome.LoadMachine(home)
		if err != nil {
			return err
		}
		if m.DefaultEnvironment == "" {
			fmt.Fprintln(d.Out, "no default environment set")
			return nil
		}
		fmt.Fprintln(d.Out, m.DefaultEnvironment)
		return nil
	}
	// Validate it resolves before recording it as the default: a typo must
	// not become every future launch's silent failure.
	if _, err := nativeenv.ResolveIn(home, c.Name); err != nil {
		return envRun(d, err)
	}
	if err := pixhome.SetDefaultEnvironment(home, c.Name); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "pix: environment %q is now the default.\n", c.Name)
	return nil
}

// ── trust ────────────────────────────────────────────────────────────────

// envTrustRecord is the acceptance record persisted at
// <PIX_HOME>/state/trust/environments/<name>.json — outside the environment
// root itself, per docs/design/pix-v2-surface.md §9 ("Approval is stored
// under ~/.pix/state, never inside the environment being approved").
type envTrustRecord struct {
	Root        string `json:"root"`
	Fingerprint string `json:"fingerprint"`
	AcceptedAt  string `json:"accepted_at"`
}

func trustRecordPath(home pixhome.Paths, name string) string {
	return filepath.Join(home.StateTrustEnvironments, name+".json")
}

// environmentFingerprint hashes the byte content of both files this package
// interprets (.sbxenv.yaml, pix.toml). A file that does not exist
// contributes its path only, so adding/removing either file changes the
// fingerprint too.
func environmentFingerprint(sel nativeenv.Selected) (string, error) {
	h := sha256.New()
	for _, p := range []string{sel.SbxEnvPath(), sel.SidecarPath()} {
		fmt.Fprintf(h, "path=%s\n", p)
		if data, err := os.ReadFile(p); err == nil {
			h.Write(data)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// trustAccepted reports whether sel's CURRENT fingerprint matches a
// recorded acceptance. A changed fingerprint (file edited since trust) or no
// record at all both report false: a stale approval never counts as trust.
func trustAccepted(home pixhome.Paths, sel nativeenv.Selected) (bool, string) {
	fp, err := environmentFingerprint(sel)
	if err != nil {
		return false, ""
	}
	data, err := os.ReadFile(trustRecordPath(home, sel.Name))
	if err != nil {
		return false, fp
	}
	var rec envTrustRecord
	if json.Unmarshal(data, &rec) != nil {
		return false, fp
	}
	return rec.Fingerprint == fp && rec.Root == sel.Root, fp
}

type envTrustCmd struct {
	Name string `arg:"" help:"Exact environment name."`
	Yes  bool   `help:"Accept without an interactive prompt (still prints what is being approved)."`
}

func (c *envTrustCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	sel, err := nativeenv.ResolveIn(home, c.Name)
	if err != nil {
		return envRun(d, err)
	}
	fp, err := environmentFingerprint(sel)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "pix env trust %s\n", sel.Name)
	fmt.Fprintf(d.Out, "  root:        %s\n", sel.Root)
	fmt.Fprintf(d.Out, "  sbxenv:      %s\n", presentIfExists(sel.SbxEnvPath()))
	fmt.Fprintf(d.Out, "  sidecar:     %s\n", presentIfExists(sel.SidecarPath()))
	fmt.Fprintf(d.Out, "  fingerprint: %s\n", fp)
	accept := c.Yes
	if !c.Yes {
		if !d.Interactive {
			return envRun(d, fmt.Errorf("env trust: refusing to accept on a non-interactive terminal without --yes"))
		}
		fmt.Fprint(d.Out, "Accept and record this exact fingerprint? [y/N] ")
		reader := bufio.NewReader(d.In)
		line, _ := reader.ReadString('\n')
		accept = strings.EqualFold(strings.TrimSpace(line), "y")
	}
	if !accept {
		fmt.Fprintln(d.Out, "pix: not accepted.")
		return cli.SilentError{Code: 1}
	}
	if err := os.MkdirAll(home.StateTrustEnvironments, 0o700); err != nil {
		return err
	}
	rec := envTrustRecord{Root: sel.Root, Fingerprint: fp, AcceptedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(trustRecordPath(home, sel.Name), b, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "pix: environment %q trusted.\n", sel.Name)
	return nil
}
