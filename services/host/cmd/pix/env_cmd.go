// env_cmd.go — `pix env`: list | show | default | trust (docs/design/
// pix-v2-surface.md §3.4). An environment IS a directory under
// ~/.pix/envs/<name>/; there is no registration database and no
// add/edit/use/forget mutation path — those verbs are gone in v2. Selection
// and listing come from workflow/env's pixhome-based ResolveIn/List
// (home.go). `default` reads/writes the one config.toml field config.Config
// owns (DefaultEnvironment, the sole config.toml schema). `trust` is the
// explicit host-execution approval command: it
// computes workflow/env's canonical host bill of materials (bom.go) and
// records acceptance of its fingerprint under ~/.pix/state/trust, outside
// the environment directory itself — never a hash of just the two authored
// files, which would miss a change that only shows up once the document is
// composed (an authored `${VAR}` losing its default, a sidecar host service
// gaining an argument that resolves through a symlink, ...).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/pixhome"
	"pix/host/sys"
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
	Show    envShowCmd    `cmd:"" help:"What NAME is: files, resolved root, trust state. --path/--effective/--json."`
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
	cfg, _ := config.LoadFrom(config.PathAt(home.Home))
	rows := make([]envListRow, 0, len(sels))
	for _, s := range sels {
		trusted, _, _ := trustAccepted(home, s)
		rows = append(rows, envListRow{
			Name: s.Name, Root: s.Root, Symlinked: s.Symlinked,
			Default: s.Name == cfg.DefaultEnvironment, Trusted: trusted,
		})
	}
	if c.JSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(d.Out, string(b))
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(d.Out, "pix: no environments registered; pix run launches with Pix's own built-in defaults.")
		fmt.Fprintln(d.Out, "     create one: mkdir -p ~/.pix/envs/<name> && author .sbxenv.yaml there.")
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

// envShowCmd implements docs/design/pix-v2-surface.md §3.4's
// `pix env [NAME] [--path|--effective|--json]`. Name is OPTIONAL: omitted,
// it resolves the machine default exactly as a launch would
// (docs/design/pix-v2-architecture.md §6.1); with no default set either,
// this is D17's `none` state, never an error.
type envShowCmd struct {
	Name      string `arg:"" optional:"" help:"Exact environment name (omit to use the machine default)."`
	JSON      bool   `help:"Emit machine-readable JSON."`
	Path      bool   `help:"Print only the resolved root."`
	Effective bool   `help:"Print the exact native sbx environment a new sandbox would use, without creating one."`
}

func (c *envShowCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}

	// --effective renders through the SAME compiler a real launch uses
	// (workflow/env's RenderEffectiveDocument -> envinfo.RenderEffective),
	// including the reserved pix-memory/pix-session built-ins, with no
	// sandbox in existence (D8). It resolves name itself (explicit, else
	// the machine default, else D17's `none`), so it is handled before
	// this command's own name resolution below.
	if c.Effective {
		doc, err := nativeenv.RenderEffectiveDocument(home, c.Name)
		if err != nil {
			return envRun(d, err)
		}
		d.Out.Write(doc)
		return nil
	}

	name := strings.TrimSpace(c.Name)
	if name == "" {
		cfg, err := config.LoadFrom(config.PathAt(home.Home))
		if err != nil {
			return err
		}
		name = strings.TrimSpace(cfg.DefaultEnvironment)
	}

	if name == "" {
		// D17: no environment registered or selected. Never an error —
		// `pix run` launches with Pix's own built-in defaults.
		switch {
		case c.Path:
			return envRun(d, fmt.Errorf("env show --path: no environment selected (none); nothing to print"))
		case c.JSON:
			b, _ := json.MarshalIndent(map[string]any{"environment": "none"}, "", "  ")
			fmt.Fprintln(d.Out, string(b))
		default:
			fmt.Fprintln(d.Out, "environment: none (no environment selected; pix run launches with Pix's own built-in defaults)")
		}
		return nil
	}

	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		return envRun(d, err)
	}
	if c.Path {
		fmt.Fprintln(d.Out, sel.Root)
		return nil
	}
	trusted, fp, bomErr := trustAccepted(home, sel)
	if c.JSON {
		fields := map[string]any{
			"name": sel.Name, "root": sel.Root, "symlinked": sel.Symlinked,
			"trusted": trusted, "fingerprint": fp,
		}
		if bomErr != nil {
			fields["trust_error"] = bomErr.Error()
		}
		b, _ := json.MarshalIndent(fields, "", "  ")
		fmt.Fprintln(d.Out, string(b))
		return nil
	}
	fmt.Fprintf(d.Out, "name:        %s\n", sel.Name)
	fmt.Fprintf(d.Out, "root:        %s\n", sel.Root)
	fmt.Fprintf(d.Out, "symlinked:   %v\n", sel.Symlinked)
	fmt.Fprintf(d.Out, "sbxenv:      %s\n", presentIfExists(sel.SbxEnvPath()))
	fmt.Fprintf(d.Out, "sidecar:     %s\n", presentIfExists(sel.SidecarPath()))
	fmt.Fprintf(d.Out, "trusted:     %v\n", trusted)
	if trusted {
		fmt.Fprintf(d.Out, "fingerprint: %s\n", fp)
	}
	if bomErr != nil {
		fmt.Fprintf(d.Out, "trust check: could not compute (%s)\n", bomErr)
	}
	fmt.Fprintf(d.Out, "effective:   pix env %s --effective\n", sys.ShellQuote(sel.Name))
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
		cfg, err := config.LoadFrom(config.PathAt(home.Home))
		if err != nil {
			return err
		}
		if cfg.DefaultEnvironment == "" {
			fmt.Fprintln(d.Out, "no default environment set")
			return nil
		}
		fmt.Fprintln(d.Out, cfg.DefaultEnvironment)
		return nil
	}
	// Validate it resolves before recording it as the default: a typo must
	// not become every future launch's silent failure.
	if _, err := nativeenv.ResolveIn(home, c.Name); err != nil {
		return envRun(d, err)
	}
	if err := config.SetDefaultEnvironmentAt(home.Home, c.Name); err != nil {
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
// Fingerprint is workflow/env's canonical BillOfMaterials.Fingerprint, never
// a raw hash of the two authored files: two environments byte-identical on
// disk but composing to a different host-exec surface (a resolved `${VAR}`
// losing its default value between runs, for example) must re-gate.
type envTrustRecord struct {
	Root        string `json:"root"`
	Fingerprint string `json:"fingerprint"`
	AcceptedAt  string `json:"accepted_at"`
}

func trustRecordPath(home pixhome.Paths, name string) string {
	return filepath.Join(home.StateTrustEnvironments, name+".json")
}

// environmentBoM loads sel and computes its canonical host bill of
// materials (workflow/env's ComputeBoM) plus its fingerprint
// (BillOfMaterials.Fingerprint) — the "complete canonical host BOM" every
// `pix env trust`/list/show trust check reads, never a two-file content
// hash. effective is nil: no runtime mount is known outside an actual
// launch, exactly as workflow/env's own preview compiler
// (ComputeEffective) composes none either.
func environmentBoM(sel nativeenv.Selected) (nativeenv.BillOfMaterials, string, error) {
	loaded, err := nativeenv.LoadHome(sel, nil, nil)
	if err != nil {
		return nativeenv.BillOfMaterials{}, "", err
	}
	return bomForLoaded(loaded)
}

// bomForLoaded is environmentBoM's other half: a caller that ALREADY holds
// an *nativeenv.Environment (run_trust.go's launch-time snapshot, resolved
// exactly once) computes its BOM/fingerprint from THAT in-memory value —
// never a second LoadHome re-read of the same environment directory. This
// is the M1 security re-review fix (trust TOCTOU): the fingerprint a gate
// decision binds to must be the exact bytes/tree already compiled into the
// launch, not a fresh, independently-re-read copy that could disagree with
// it.
func bomForLoaded(loaded *nativeenv.Environment) (nativeenv.BillOfMaterials, string, error) {
	bom, err := nativeenv.ComputeBoM(loaded, nil, nil)
	if err != nil {
		return nativeenv.BillOfMaterials{}, "", err
	}
	fp, err := nativeenv.Fingerprint(bom)
	if err != nil {
		return nativeenv.BillOfMaterials{}, "", err
	}
	return bom, fp, nil
}

// trustAccepted reports whether sel's CURRENT canonical bill-of-materials
// fingerprint matches a recorded acceptance. A changed fingerprint (the
// environment's host-exec surface changed since trust), no record at all,
// or a load/BOM-compute failure (an unsafe symlink, an undefined bare
// interpolation, ...) all report untrusted: a stale or unfingerprintable
// environment never counts as trusted. The third return is non-nil only
// for that last case, so a caller can surface WHY trust could not even be
// checked rather than silently printing "untrusted" for a load failure
// same as for "never reviewed".
func trustAccepted(home pixhome.Paths, sel nativeenv.Selected) (bool, string, error) {
	_, fp, err := environmentBoM(sel)
	if err != nil {
		return false, "", err
	}
	return trustAcceptedForFingerprint(home, sel, fp), fp, nil
}

// trustAcceptedForFingerprint reports whether fp — the caller's OWN,
// already-computed fingerprint for sel — matches sel's recorded
// acceptance. Unlike trustAccepted above (the env_cmd.go list/show/trust
// path, which is not part of a launch's TOCTOU-sensitive gate and may
// freely compute its own fresh fingerprint), this NEVER recomputes the
// BOM/fingerprint itself: the caller supplies it, so a launch's trust
// decision always compares the ONE in-memory fingerprint it resolved once
// (run_trust.go's envTrustSnapshot) against the accepted record — never a
// second, independent disk read standing in as "the" fingerprint (M1,
// security re-review: trust TOCTOU).
func trustAcceptedForFingerprint(home pixhome.Paths, sel nativeenv.Selected, fp string) bool {
	data, err := os.ReadFile(trustRecordPath(home, sel.Name))
	if err != nil {
		return false
	}
	var rec envTrustRecord
	if json.Unmarshal(data, &rec) != nil {
		return false
	}
	return rec.Fingerprint == fp && rec.Root == sel.Root
}

type envTrustCmd struct {
	Name    string `arg:"" help:"Exact environment name."`
	Yes     bool   `help:"Accept without an interactive prompt (still prints what is being approved)."`
	Verbose bool   `help:"Print full argv and content digests, not just counts and destinations."`
}

func (c *envTrustCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	return runEnvTrust(d, home, c.Name, c.Yes, c.Verbose)
}

// runEnvTrust is `pix env trust NAME`'s whole body, factored out so `pix
// setup --env NAME` can perform the EXACT SAME complete, default-No trust
// operation before it checks anything else about that environment (docs/
// design/pix-v2-surface.md §3.6: "When setup reaches an untrusted
// environment, it performs the same complete, default-No trust operation
// as `pix env trust NAME`"). A caller ALREADY trusted for the current
// fingerprint returns nil having printed and mutated nothing — trust is
// idempotent, never re-prompted for an unchanged environment.
func runEnvTrust(d *cli.Deps, home pixhome.Paths, name string, yes, verbose bool) error {
	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		return envRun(d, err)
	}
	bom, fp, err := environmentBoM(sel)
	if err != nil {
		return envRun(d, err)
	}
	if trustAcceptedForFingerprint(home, sel, fp) {
		return nil
	}
	renderTrustBill(d.Out, sel.Name, bom, verbose)
	fmt.Fprintf(d.Out, "  fingerprint: %s\n\n", fp)

	accept := yes
	if !yes {
		if !d.Interactive {
			return envRun(d, fmt.Errorf("env trust: refusing to accept on a non-interactive terminal without --yes"))
		}
		fmt.Fprint(d.Out, "Accept this host-execution footprint? [y/N] ")
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

// safeArgv renders argv the way a human must review it before answering
// y/N: EVERY element individually shell-quoted (sys.ShellQuote), not
// space-joined. A bare strings.Join is ambiguous — ["rm", "-rf /"] and
// ["rm", "-rf", "/"] render identically — which is exactly the gap between
// "what a reviewer read" and "what os/exec will actually receive" a
// rendered consent screen must never have. Each element also passes through
// sys.TerminalSafe first, the same discipline safe() applies to every other
// authored fact rendered here, before it is ever quoted; joining the
// already-quoted results with a plain space is unambiguous, because every
// individual token is self-delimiting.
func safeArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = sys.ShellQuote(sys.TerminalSafe(a))
	}
	return strings.Join(parts, " ")
}

// renderTrustBill prints workflow/env's canonical BillOfMaterials: counts
// plus every host command/service name, credential destination, mount
// expansion, and inference backend by default (D15); full argv and content
// digests behind --verbose. Inference facts render by default (never only
// under --verbose) because a model-traffic endpoint is one of the four
// things docs/design/environments.md §9.1 names the trust fingerprint
// exists to gate, alongside host execution, credential disclosure, and
// mount expansion: a human must see every backend name, driver, base URL,
// and auth mode an accepted environment would route a session's model
// traffic through before answering y/N, not just count it. Every value
// that can carry AUTHORED environment content (attacker-controlled for a
// cloned or shared environment) passes through sys.TerminalSafe before
// reaching the terminal a human is about to answer "y" on: a raw ESC/CSI/OSC
// could repaint or retitle the consent screen, and a raw newline could
// forge a renderer-owned line (a fake count, a fake prompt, a fake
// "trusted" verdict). This is the same discipline the deleted v1
// environment-review renderer applied (docs/design/environments.md §9.1's
// Wave C security M1); it is not optional polish. Every rendered argv
// (a setup hook's check/apply, a host command's or host service's own
// argument list) additionally goes through safeArgv, never a bare
// strings.Join: each element is shell-quoted INDIVIDUALLY, so a reviewer
// sees exactly the argument boundaries os/exec will actually use, not a
// space-joined string a multi-word single argument could be misread as
// two arguments (or the reverse).
func renderTrustBill(out io.Writer, name string, b nativeenv.BillOfMaterials, verbose bool) {
	safe := sys.TerminalSafe
	fmt.Fprintf(out, "pix env trust %s\n", safe(name))
	fmt.Fprintln(out, "  environment runs code on your host and hands it credentials:")
	fmt.Fprintf(out, "  %d host command(s), %d host service(s), %d setup hook(s), %d credential target(s), %d mount(s), %d MCP server(s), %d kit(s), %d inference backend(s)\n\n",
		len(b.HostCommands), len(b.HostServices), len(b.SetupHooks), len(b.CredentialTargets), len(b.EffectiveMounts), len(b.MCPServers), len(b.Kits), len(b.Inference))
	for _, c := range b.HostCommands {
		fmt.Fprintf(out, "  runs on this host: %s\n", safe(c.Name))
	}
	for _, s := range b.HostServices {
		fmt.Fprintf(out, "  host service:      %s  port %d\n", safe(s.Name), s.Port)
	}
	// Setup hooks render BY DEFAULT, never only under --verbose, and with
	// their full argv: they are the one thing in this bill that `pix setup
	// --env NAME` will execute on this host with the human's own stdio
	// attached, so "3 setup hook(s)" alone is not consent. Required/optional
	// and install/auth are shown too, because those two bits decide whether
	// a failure stops the run and whether the hook may talk to the terminal.
	for _, h := range b.SetupHooks {
		need := "optional"
		if h.Required {
			need = "required"
		}
		fmt.Fprintf(out, "  setup hook:        %s (%s, %s) %s\n", safe(h.ID), safe(h.Kind), need, safe(h.Command))
		fmt.Fprintf(out, "                     check: %s %s\n", safe(h.Command), safeArgv(h.CheckArgs))
		fmt.Fprintf(out, "                     apply: %s %s\n", safe(h.Command), safeArgv(h.ApplyArgs))
		fmt.Fprintf(out, "                     sha256:%s\n", safe(h.SHA))
		for _, in := range h.Inputs {
			fmt.Fprintf(out, "                     input: %s  sha256:%s\n", safe(in.Path), safe(in.SHA))
		}
	}
	for _, t := range b.CredentialTargets {
		fmt.Fprintf(out, "  credential:        %s -> %s\n", safe(t.Source), safe(t.Destination))
	}
	for _, m := range b.EffectiveMounts {
		ro := "rw"
		if m.ReadOnly {
			ro = "ro"
		}
		fmt.Fprintf(out, "  mount:             %s (%s)\n", safe(m.Path), ro)
	}
	for _, r := range b.NoVerifyRegistries() {
		fmt.Fprintf(out, "  no-verify registry: %s\n", safe(r.Host))
	}
	for _, inf := range b.Inference {
		fmt.Fprintf(out, "  inference:         %s  driver %s  base_url %s  auth %s\n",
			safe(inf.Name), safe(inf.Driver), safe(inf.BaseURL), safe(inf.Auth))
	}
	for _, it := range b.Interpolations {
		src := fmt.Sprintf("${%s}", it.Var)
		if it.Default != nil {
			src = fmt.Sprintf("${%s:-%s}", it.Var, *it.Default)
		}
		fmt.Fprintf(out, "  interpolation:     %s -> %s\n", safe(src), safe(it.KeyPath))
	}
	if !verbose {
		fmt.Fprintf(out, "\n  full argv and content digests: pix env trust %s --verbose\n", sys.ShellQuote(name))
		return
	}
	fmt.Fprintln(out)
	for _, c := range b.HostCommands {
		fmt.Fprintf(out, "  argv %-20s %s\n", safe(c.Name), safeArgv(c.Argv))
	}
	for _, s := range b.HostServices {
		line := safeArgv(append([]string{s.Command}, s.Args...))
		fmt.Fprintf(out, "  argv %-20s %s\n", safe(s.Name), line)
		if s.SHA != "" {
			fmt.Fprintf(out, "       %-20s sha256:%s\n", "", safe(s.SHA))
		}
	}
	for _, k := range b.Kits {
		if !k.Local {
			continue
		}
		fmt.Fprintf(out, "  kit  %-20s %s\n", safe(k.Raw), safe(k.Resolved))
		fmt.Fprintf(out, "       %-20s sha256:%s\n", "", safe(k.SHA))
	}
}
