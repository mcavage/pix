// env_cmd.go — `pix env`: list | add | show | default | trust (docs/design/
// pix-v2-surface.md §3.4). An environment IS a directory under
// ~/.pix/envs/<name>/; there is no registration database and no
// edit/use/forget mutation path — those verbs are gone in v2. `add` (add.go)
// is the one narrow exception: it adopts an EXISTING source (a local
// directory or a git URL), never edits, registers by name alone, or
// replaces one that is already there. Selection
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
	"sort"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
	"pix/host/inference"
	"pix/host/pixhome"
	"pix/host/secret"
	"pix/host/sys"
	nativeenv "pix/host/workflow/env"
)

func (c *envCmd) Help() string {
	return `A named environment: a directory under ~/.pix/envs/<name>/ declaring
.sbxenv.yaml (native sbx grammar) and an optional pix.toml sidecar.

Five verbs: list, show, add, default, trust. There is no edit/use/forget:
edit, move, and remove an environment with ordinary filesystem and Git
tools under ~/.pix/envs. 'add' is the one narrow exception — it ADOPTS an
existing source (a local directory already on disk, or a git URL) as a
new environment; it never overwrites, merges, or replaces one that is
already there. 'pix setup' may scaffold a default one.

An environment that runs host code or handles a credential must be
approved with 'pix env trust NAME' before a launch will use it.`
}

// envCmd's field ORDER is the v2 verb surface; bare 'pix env' is
// 'env list'.
type envCmd struct {
	List    envListCmd    `cmd:"" default:"1" help:"List environments under ~/.pix/envs, the default, and trust state."`
	Add     envAddCmd     `cmd:"" help:"Adopt a git URL or local directory as a new environment."`
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

// ── add ──────────────────────────────────────────────────────────────────

// envAddCmd is `pix env add <git-url|local-directory> [name]`: the one
// narrow exception to "Pix does not provide general environment create,
// edit, add, forget, update, or delete commands" (docs/design/
// pix-v2-surface.md §3.4). It ADOPTS a source that already exists — a
// local directory already on disk gets a symlink to its canonical
// absolute path, a git URL gets cloned — as a new named environment. It is
// add-only: no overwrite, no merge, no replace, and it never selects a
// default, trusts, runs setup, or launches anything (workflow/env's Add
// owns the full contract). Unlike 'pix env trust', naming a source is
// itself the whole approval this command needs, so it never requires a
// TTY and never prompts.
type envAddCmd struct {
	Source string `arg:"" help:"A git URL to clone, or an existing local directory to link in."`
	Name   string `arg:"" optional:"" help:"Environment name (omit to derive one from Source)."`
}

func (c *envAddCmd) Run(d *cli.Deps) error {
	home, err := envHome()
	if err != nil {
		return err
	}
	res, err := nativeenv.Add(nativeenv.AddOptions{Home: home, Source: c.Source, Name: c.Name})
	if err != nil {
		return envRun(d, err)
	}
	verb := "cloned"
	if res.Kind == "local" {
		verb = "linked"
	}
	fmt.Fprintf(d.Out, "pix: %s %q -> %s\n", verb, res.Name, res.Root)
	fmt.Fprintln(d.Out, "next:")
	fmt.Fprintf(d.Out, "  pix env show %s\n", sys.ShellQuote(res.Name))
	fmt.Fprintf(d.Out, "  pix env trust %s\n", sys.ShellQuote(res.Name))
	fmt.Fprintf(d.Out, "  pix env default %s\n", sys.ShellQuote(res.Name))
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
		doc, err := nativeenv.RenderEffectiveDocument(home, c.Name, version)
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
	loaded, loadErr := nativeenv.LoadHome(sel, nil, nil)
	var model, modelSource string
	var modelErr error
	var agentModels map[string]string
	if loadErr == nil {
		model, modelSource, modelErr = resolveRunModel("", loaded.Sidecar, defaultShellEnv())
		if loaded.Sidecar != nil {
			agentModels = loaded.Sidecar.Agents
		}
	} else {
		modelErr = loadErr
	}
	// providers/catalog are the rest of R2's inspection surface: the
	// provider REFS this home configures (names only — ConfiguredModelRefs
	// never reads a value, only which env-var refs are filled) and where the
	// catalog resolveRunModel consulted actually lives on disk, so the
	// selection rule above stops being the only visible half of the story.
	providers, refsState := secret.ConfiguredModelRefs(defaultShellEnv())
	catalogInfo := inference.DescribeCatalogSource(home.Home, version)
	// integrations is the declared/registered/reachable answer for every MCP
	// server THIS environment declares (nativeenv.IntegrationStatuses). It
	// needs the same bill of materials trustAccepted already computed above;
	// bomForLoaded reuses the SAME already-parsed `loaded` rather than a
	// second LoadHome re-read, and is skipped entirely when loadErr means
	// there is no environment to compute it from.
	var integrations []nativeenv.IntegrationStatus
	if loadErr == nil {
		if bom, _, err := bomForLoaded(loaded); err == nil {
			integrations = computeIntegrationStatuses(bom)
		}
	}
	if c.JSON {
		fields := map[string]any{
			"name": sel.Name, "root": sel.Root, "symlinked": sel.Symlinked,
			"trusted": trusted, "fingerprint": fp,
		}
		if bomErr != nil {
			fields["trust_error"] = bomErr.Error()
		}
		if model != "" {
			fields["model"] = model
			fields["model_source"] = modelSource
			fields["agents"] = agentModels
		}
		if modelErr != nil {
			fields["model_error"] = modelErr.Error()
		}
		if refsState == secret.RefsAnswered {
			out := providers
			if out == nil {
				out = []string{}
			}
			fields["providers"] = out
		} else {
			fields["providers_error"] = "secrets.env unreadable"
		}
		catalog := map[string]any{
			"source": catalogInfo.Source, "runtime_path": catalogInfo.RuntimePath,
			"runtime_installed": catalogInfo.RuntimePathExists,
		}
		if catalogInfo.OverridePath != "" {
			catalog["override_path"] = catalogInfo.OverridePath
		}
		fields["catalog"] = catalog
		if len(integrations) > 0 {
			fields["integrations"] = integrationStatusesJSON(integrations)
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
	if modelErr != nil {
		fmt.Fprintf(d.Out, "model:       unresolved (%s)\n", modelErr)
	} else {
		fmt.Fprintf(d.Out, "model:       %s (%s)\n", model, modelSource)
		if len(agentModels) == 0 {
			fmt.Fprintln(d.Out, "agents:      inherit main model")
		} else {
			names := make([]string, 0, len(agentModels))
			for name := range agentModels {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Fprintln(d.Out, "agents:")
			for _, name := range names {
				fmt.Fprintf(d.Out, "  %s -> %s\n", name, agentModels[name])
			}
		}
	}
	fmt.Fprintf(d.Out, "providers:   %s\n", renderConfiguredProviders(providers, refsState))
	fmt.Fprintf(d.Out, "catalog:     %s\n", renderCatalogSource(catalogInfo))
	renderIntegrationStatuses(d.Out, integrations)
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

// renderConfiguredProviders is `pix env show`'s provider-refs line: NAMES
// only (never a value, never a resolved secret), and always a line even
// when nothing is configured or secrets.env could not be read — an omitted
// line reads as "nothing to report", which is exactly the invisible-input
// failure this surface exists to end.
func renderConfiguredProviders(providers []string, state secret.RefsProbeState) string {
	if state != secret.RefsAnswered {
		return "unknown (secrets.env unreadable)"
	}
	if len(providers) == 0 {
		return "(none configured)"
	}
	return strings.Join(providers, ", ") + " (from secrets.env; names only)"
}

// renderCatalogSource is `pix env show`'s catalog-provenance line: which
// catalog resolveRunModel actually consulted (an override on disk, else the
// embedded default) plus the release-materialized, on-disk copy of that
// embedded default — so "the shipped catalog" is something a user can `cat`,
// not only something baked into the binary.
func renderCatalogSource(info inference.CatalogSourceInfo) string {
	if info.Source == "override" {
		return "override " + info.OverridePath
	}
	installed := "not yet installed; run `pix setup`"
	if info.RuntimePathExists {
		installed = "installed"
	}
	return fmt.Sprintf("embedded default; materialized snapshot: %s (%s)", info.RuntimePath, installed)
}

// computeIntegrationStatuses is `pix env show`'s declared/registered/
// reachable answer for bom's own MCP servers (nativeenv.IntegrationStatuses,
// docs/design/integrations-remediation.md's own naming for the gap: "the
// single most load-bearing problem — registered is reported as working").
// It costs exactly one `sbx mcp ls` call, and only when bom actually
// declares at least one server: an environment with none pays nothing
// extra. sbx absent or the listing failing degrades to StatusUnknown
// registration for every server (mcp.McpRegEvidenceFrom's own fail-open
// rule) rather than aborting `env show` itself.
func computeIntegrationStatuses(bom nativeenv.BillOfMaterials) []nativeenv.IntegrationStatus {
	if len(bom.MCPServers) == 0 && len(bom.HostServices) == 0 {
		return nil
	}
	var statuses []nativeenv.IntegrationStatus
	if len(bom.MCPServers) > 0 {
		lsOut, _, lsErr := runSbxCapturedOut("mcp", "ls")
		run := nativeenv.RunnerFromEnv(defaultShellEnv())
		statuses = nativeenv.IntegrationStatuses(bom, lsOut, lsErr == nil, run)
	}
	// The resident [[host.services]] entries belong on the SAME surface: they
	// are the other half of what this environment integrates with, and they
	// are the half carrying an explicit health endpoint. Omitting them made
	// `pix env show` silent about a warehouse proxy that was not answering,
	// which reads as "nothing to report".
	return append(statuses, nativeenv.HostServiceStatuses(bom, nativeenv.LoopbackHTTPProbe)...)
}

// renderIntegrationStatuses is `pix env show`'s plain-text integrations
// section: one line per declared MCP server naming all three states by
// name (declared/registered/reachable), never collapsing any two of them
// into one verdict, then one line per [[host.services]] entry
// (declared/reachable; a service has no registration state). Declaring
// neither prints nothing: an empty section reads as "nothing to report".
func renderIntegrationStatuses(w io.Writer, statuses []nativeenv.IntegrationStatus) {
	if len(statuses) == 0 {
		return
	}
	fmt.Fprintln(w, "integrations:")
	for _, s := range statuses {
		// A host service has no registration state to report — pix registers
		// nothing and starts nothing for one — so the column is omitted
		// rather than filled with a word that would be a claim about a
		// registry this row does not live in.
		if s.Kind == nativeenv.ServiceKind {
			fmt.Fprintf(w, "  %-20s declared:yes service            reachable:%-8s\n", s.Name, s.Reachable)
		} else {
			fmt.Fprintf(w, "  %-20s declared:yes registered:%-8s reachable:%-8s\n", s.Name, s.Registered, s.Reachable)
		}
		if s.Reachable != health.StatusReady && s.ReachableDetail != "" {
			fmt.Fprintf(w, "  %-20s   (%s)\n", "", s.ReachableDetail)
		}
	}
}

// integrationStatusesJSON is `pix env show --json`'s shape for the same
// data renderIntegrationStatuses prints: every field machine-readable,
// including BOTH states' detail strings — a script deciding whether to
// alert on a gap needs the reason, not just the tri-state word.
func integrationStatusesJSON(statuses []nativeenv.IntegrationStatus) []map[string]any {
	out := make([]map[string]any, 0, len(statuses))
	for _, s := range statuses {
		entry := map[string]any{
			"name":              s.Name,
			"kind":              s.Kind,
			"url":               s.URL,
			"command":           s.Command,
			"declared":          s.Declared,
			"registered":        string(s.Registered),
			"registered_detail": s.RegisteredDetail,
			"reachable":         string(s.Reachable),
			"reachable_detail":  s.ReachableDetail,
		}
		// A service row carries no registration state; emitting empty strings
		// for it would let a consumer read "" as a fourth status word.
		if s.Kind == nativeenv.ServiceKind {
			delete(entry, "registered")
			delete(entry, "registered_detail")
			delete(entry, "url")
		}
		out = append(out, entry)
	}
	return out
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
// Receipt is the itemized form of the bill of materials that was accepted
// (workflow/env's Receipt): section, key, and a digest per reviewable fact,
// and no fact's detail. It exists so the NEXT gate can show what changed
// instead of a second full audit dump — see renderTrustReview. It is
// absent on a record written before receipts existed, and an absent receipt
// degrades to the full bill rather than to a silent or invented diff.
type envTrustRecord struct {
	Root        string                   `json:"root"`
	Fingerprint string                   `json:"fingerprint"`
	AcceptedAt  string                   `json:"accepted_at"`
	Receipt     []nativeenv.ReceiptEntry `json:"receipt,omitempty"`
}

// readTrustRecord returns the acceptance record on disk for name, if there
// is a readable, parseable one. Absent/corrupt is (zero, false): every
// caller treats it as "never accepted", never as an error to surface.
func readTrustRecord(home pixhome.Paths, name string) (envTrustRecord, bool) {
	data, err := os.ReadFile(trustRecordPath(home, name))
	if err != nil {
		return envTrustRecord{}, false
	}
	var rec envTrustRecord
	if json.Unmarshal(data, &rec) != nil {
		return envTrustRecord{}, false
	}
	return rec, true
}

// writeTrustRecord is the ONE writer of an environment acceptance record —
// `pix env trust` and `pix run`'s inline gate both go through it, so a
// receipt can never be recorded by one path and forgotten by the other
// (which would make the change screen depend on WHICH command a person
// happened to accept from).
//
// A receipt that cannot be computed is not fatal: the acceptance itself is
// what gates a launch, and refusing to record a fingerprint the operator
// just approved because its itemization failed would fail closed on the
// wrong thing. The record is written without one, and the next gate falls
// back to the full bill.
func writeTrustRecord(home pixhome.Paths, name, root, fp string, bom nativeenv.BillOfMaterials) error {
	if err := os.MkdirAll(home.StateTrustEnvironments, 0o700); err != nil {
		return err
	}
	rec := envTrustRecord{Root: root, Fingerprint: fp, AcceptedAt: time.Now().UTC().Format(time.RFC3339)}
	if receipt, err := nativeenv.Receipt(bom); err == nil {
		rec.Receipt = receipt
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	return os.WriteFile(trustRecordPath(home, name), b, 0o600)
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
	bom, fp, err := environmentBoM(sel)
	if err != nil {
		return false, "", err
	}
	// A zero-footprint environment reports trusted with no record on disk:
	// list/show must not label "nothing to review" as "untrusted", or the
	// generated default reads as a pending approval that no command can
	// ever satisfy (trustSatisfied owns the rule).
	return trustSatisfied(home, sel, bom, fp), fp, nil
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
// trustSatisfied is the ONE answer to "does this environment still need a
// review?", and every caller (`pix run`'s two gates, the safe-recreate
// Reviewed fact, `pix env trust`, env list/show, `pix setup --env`'s
// post-review assertion) asks it rather than reading an acceptance record
// directly. An environment whose canonical bill of materials is NOT Tier1
// executes nothing on this host, hands out no credential, and expands no
// mount, so there is nothing for a human to accept: it needs no acceptance
// record, is never prompted for, and never causes a trust-state write (the
// generated `default` environment is exactly this shape). Anything Tier1
// still requires fp to match a recorded acceptance, unchanged.
func trustSatisfied(home pixhome.Paths, sel nativeenv.Selected, bom nativeenv.BillOfMaterials, fp string) bool {
	if !bom.Tier1() {
		return true
	}
	return trustAcceptedForFingerprint(home, sel, fp)
}

func trustAcceptedForFingerprint(home pixhome.Paths, sel nativeenv.Selected, fp string) bool {
	rec, ok := readTrustRecord(home, sel.Name)
	if !ok {
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
	if trustSatisfied(home, sel, bom, fp) {
		if !bom.Tier1() {
			fmt.Fprintf(d.Out, "pix: environment %q runs nothing on this host, hands out no credential, and mounts nothing extra; there is nothing to accept.\n", sel.Name)
		}
		return nil
	}
	renderTrustReview(d.Out, sel.Name, bom, priorAcceptance(home, sel), verbose)
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
	if err := writeTrustRecord(home, sel.Name, sel.Root, fp, bom); err != nil {
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

// renderTrustBill prints workflow/env's canonical BillOfMaterials: by
// default (D15, revised) it shows ONLY the summary counts plus any risk
// CATEGORY that changes the shape of what a human is accepting (today:
// how many registries skip TLS verification) — never a per-item
// enumeration of names, destinations, or endpoints. Every host
// command/service name, credential destination, mount path, MCP server,
// kit, inference backend, and interpolation, plus the full argv and
// content-digest section, is --verbose-only: `pix env trust NAME
// --verbose` is the one place a reviewer who wants to READ the bill
// line-by-line asks for it. D15 previously required host
// commands/services, credential destinations, mount expansion, and every
// inference backend by default; user feedback on the live consent screen
// (still reading as a full audit dump before a single y/N) moved that
// detail behind --verbose too, matching the setup-hook and kit/host-service
// argv sections that were already there. The fingerprint (printed by every
// caller right after this function returns) still lets a reviewer confirm
// an ALREADY-reviewed acceptance did not silently change — it is a receipt,
// not an audit dump, so it stays outside verbose.
//
// Every value that can carry AUTHORED environment content
// (attacker-controlled for a cloned or shared environment) passes through
// sys.TerminalSafe before reaching the terminal a human is about to answer
// "y" on: a raw ESC/CSI/OSC could repaint or retitle the consent screen,
// and a raw newline could forge a renderer-owned line (a fake count, a
// fake prompt, a fake "trusted" verdict). This is the same discipline the
// deleted v1 environment-review renderer applied (docs/design/
// environments.md §9.1's Wave C security M1); it is not optional polish.
// Every rendered argv (a setup hook's check/apply, a host command's or
// host service's own argument list) additionally goes through safeArgv,
// never a bare strings.Join: each element is shell-quoted INDIVIDUALLY, so
// a reviewer sees exactly the argument boundaries os/exec will actually
// use, not a space-joined string a multi-word single argument could be
// misread as two arguments (or the reverse).
// renderTrustReview is the review screen a gate prints before its y/N, and
// the one place that decides WHICH screen that is.
//
// A first review has nothing to compare against, so it is the full bill
// (renderTrustBill) exactly as before. A RE-review is different in kind: the
// operator already read and accepted this environment once, and the only
// question in front of them is what moved since. Printing the whole bill
// again for a one-line kit bump is how a consent screen becomes scenery and
// `--yes` becomes reflex — the migration plan's own risk table names that
// failure ("kit fingerprint churn -> trust fatigue -> people run --yes
// reflexively"), and a review nobody reads is not a gate.
//
// The diff is derived from the SAME canonical document the fingerprint
// hashes, and workflow/env's receipt_test.go pins the property that makes
// this screen honest: a changed fingerprint always produces a non-empty
// diff. This renderer therefore cannot print "nothing changed" while the
// gate is refusing entry. It still falls back to the full bill in the two
// cases where it has no trustworthy comparison — a record from a pix that
// predates receipts, or a record whose root differs (a re-pointed
// environment name is a new subject, not an edit of the old one) — and says
// which case it is rather than implying it made a comparison.
func renderTrustReview(out io.Writer, name string, b nativeenv.BillOfMaterials, prev envTrustRecord, verbose bool) {
	cur, err := nativeenv.Receipt(b)
	switch {
	case prev.Fingerprint == "":
		renderTrustBill(out, name, b, verbose)
		return
	case err != nil, len(prev.Receipt) == 0:
		renderTrustBill(out, name, b, verbose)
		fmt.Fprintf(out, "\n  (you accepted an earlier version of %s on %s; pix cannot show what changed, so the whole bill is above)\n",
			sys.TerminalSafe(name), sys.TerminalSafe(prev.AcceptedAt))
		return
	}

	safe := sys.TerminalSafe
	changes := nativeenv.DiffReceipts(prev.Receipt, cur)
	unchanged := nativeenv.UnchangedCount(cur, changes)
	fmt.Fprintf(out, "pix env trust %s\n", safe(name))
	fmt.Fprintf(out, "  you accepted this environment on %s; %d reviewed fact(s) changed since, %d unchanged:\n\n",
		safe(prev.AcceptedAt), len(changes), unchanged)
	for _, c := range changes {
		fmt.Fprintf(out, "  %-8s %-14s %s\n", string(c.Kind), safe(c.Section), safe(c.Key))
	}
	if !verbose {
		fmt.Fprintf(out, "\n  full bill: pix env trust %s --verbose\n", sys.ShellQuote(name))
		return
	}
	fmt.Fprintln(out)
	renderTrustBill(out, name, b, true)
}

// priorAcceptance returns the record renderTrustReview may compare against:
// one that exists AND was accepted for the same root. A name re-pointed at
// a different directory is a different subject, not an edit of the old one
// (trustAcceptedForFingerprint refuses it for gating on the same grounds),
// so its old record must not become the base of a change diff that would
// read as "only these three things changed".
func priorAcceptance(home pixhome.Paths, sel nativeenv.Selected) envTrustRecord {
	rec, ok := readTrustRecord(home, sel.Name)
	if !ok || rec.Root != sel.Root {
		return envTrustRecord{}
	}
	return rec
}

func renderTrustBill(out io.Writer, name string, b nativeenv.BillOfMaterials, verbose bool) {
	safe := sys.TerminalSafe
	fmt.Fprintf(out, "pix env trust %s\n", safe(name))
	fmt.Fprintln(out, "  environment runs code on your host and hands it credentials:")
	fmt.Fprintf(out, "  %d host command(s), %d host service(s), %d setup hook(s), %d credential target(s), %d mount(s), %d MCP server(s), %d kit(s), %d inference backend(s)\n",
		len(b.HostCommands), len(b.HostServices), len(b.SetupHooks), len(b.CredentialTargets), len(b.EffectiveMounts), len(b.MCPServers), len(b.Kits), len(b.Inference))
	if n := len(b.NoVerifyRegistries()); n > 0 {
		fmt.Fprintf(out, "  %d of those registrie(s) skip TLS certificate verification (no-verify)\n", n)
	}
	if !verbose {
		fmt.Fprintf(out, "\n  full detail: pix env trust %s --verbose\n", sys.ShellQuote(name))
		return
	}
	fmt.Fprintln(out)
	for _, c := range b.HostCommands {
		fmt.Fprintf(out, "  runs on this host: %s\n", safe(c.Name))
	}
	for _, s := range b.HostServices {
		fmt.Fprintf(out, "  host service:      %s  port %d\n", safe(s.Name), s.Port)
	}
	// Every SetupHook is now --verbose-only (this whole block only runs once
	// !verbose has already returned above): id, kind, required/optional, the
	// full check/apply argv, and every content digest render together — a
	// reviewer who asks for detail at all gets the complete hook, not a
	// concise line first and a second flag for the rest of it.
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
		// The sbx-session injection wiring is fingerprinted, so it has to be
		// reachable from the review screen too (bom.go's own contract:
		// nothing is fingerprinted that renderBill cannot reach). Only
		// printed when the environment actually declares it.
		if inf.CredentialService != "" {
			header, format := inf.CredentialHeader, inf.CredentialFormat
			if header == "" {
				header = "Authorization"
			}
			if format == "" {
				format = "Bearer %s"
			}
			fmt.Fprintf(out, "                     credential %s -> header %s (%s)\n",
				safe(inf.CredentialService), safe(header), safe(format))
		}
	}
	for _, it := range b.Interpolations {
		src := fmt.Sprintf("${%s}", it.Var)
		if it.Default != nil {
			src = fmt.Sprintf("${%s:-%s}", it.Var, *it.Default)
		}
		fmt.Fprintf(out, "  interpolation:     %s -> %s\n", safe(src), safe(it.KeyPath))
	}
	// The rest of this function is the full argv/content-digest section —
	// reachable only through the verbose block above, since !verbose already
	// returned before any of this ran.
	fmt.Fprintln(out)
	for _, c := range b.HostCommands {
		fmt.Fprintf(out, "  argv %-20s %s\n", safe(c.Name), safeArgv(c.Argv))
	}
	for _, s := range b.HostServices {
		line := safeArgv(append([]string{s.Command}, s.Args...))
		fmt.Fprintf(out, "  argv %-20s %s\n", safe(s.Name), line)
		// Target is the command's resolved PHYSICAL path (a symlink chain
		// resolved to its real executable, per ResolveSymlinkedReference) —
		// rendered only when it actually differs from the authored Command,
		// so an unremarkable non-symlinked command still renders exactly as
		// it always has.
		if s.Target != "" && s.Target != s.Command {
			fmt.Fprintf(out, "       %-20s resolved: %s\n", "", safe(s.Target))
		}
		if s.SHA != "" {
			fmt.Fprintf(out, "       %-20s sha256:%s\n", "", safe(s.SHA))
		}
	}
	for _, k := range b.Kits {
		if !k.Local {
			continue
		}
		fmt.Fprintf(out, "  kit  %-20s %s\n", safe(k.Raw), safe(k.Resolved))
		if k.Target != "" && k.Target != k.Resolved {
			fmt.Fprintf(out, "       %-20s resolved: %s\n", "", safe(k.Target))
		}
		fmt.Fprintf(out, "       %-20s sha256:%s\n", "", safe(k.SHA))
	}
	// MCP servers with a local command render the SAME authored/resolved/
	// sha256 shape as a kit or host service above: the authored Command is
	// what the environment wrote, Target is where a symlink chain (an
	// ordinary Homebrew-style install, e.g. `gog`) actually led, and SHA is
	// always the hash of THAT resolved target — never of an unresolved
	// symlink path.
	for _, m := range b.MCPServers {
		if m.Command == "" {
			continue
		}
		line := safeArgv(append([]string{m.Command}, m.Args...))
		fmt.Fprintf(out, "  mcp  %-20s %s\n", safe(m.Name), line)
		if m.Target != "" && m.Target != m.Command {
			fmt.Fprintf(out, "       %-20s resolved: %s\n", "", safe(m.Target))
		}
		if m.SHA != "" {
			fmt.Fprintf(out, "       %-20s sha256:%s\n", "", safe(m.SHA))
		}
	}
}
