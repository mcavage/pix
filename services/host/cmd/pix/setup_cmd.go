// setup_cmd.go — `pix setup` as a typed root child (docs/design/
// pix-v2-surface.md §3.6, pix-v2-architecture.md §12). It is the whole of
// the v2 setup surface this launcher owns: idempotently initialize
// PIX_HOME, record the installed release manifest, reconcile the one named
// pix-memory container, and register/verify its reserved sbx MCP name —
// using real adapters (os/exec git, real Docker, a real HTTP prober, a real
// `sbx mcp` registrar). Nothing else runs here: no pack, no MCP allowlist,
// no model-provider interview, and no sandbox handoff. `pix doctor` reports
// the rest, and `pix run` starts a session.
package main

import (
	"bufio"
	"fmt"
	"sort"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/container"
	"pix/host/envsetup"
	"pix/host/hostenv"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/secret"
	nativeenv "pix/host/workflow/env"
	"pix/host/workflow/launch"
	"pix/host/workflow/provision"
	"pix/host/workspace"
)

// setupSeams is every external effect `pix setup` reaches for. Production
// fills it from productionSetupSeams(); a test substitutes doubles and
// drives the SAME Run body, so what is proven is the command's own
// sequencing (discover -> verify -> install -> record -> images -> env ->
// container -> MCP), not a parallel re-implementation of it.
type setupSeams struct {
	// DiscoverBundle finds the installed release bundle adjacent to this
	// binary. It is the one seam that must run BEFORE any Docker or
	// Gateway mutation: an incomplete installation is a refusal, not a
	// half-provisioned host.
	DiscoverBundle  func() (*release.Bundle, error)
	Prereqs         provision.PrereqChecker
	ContainerRunner container.Runner
	Prober          container.Prober
	MCP             provision.MCPRegistrar
}

func productionSetupSeams() setupSeams {
	return setupSeams{
		DiscoverBundle:  func() (*release.Bundle, error) { return release.DiscoverBundle(release.DefaultLocator) },
		Prereqs:         execChecker{},
		ContainerRunner: container.DefaultRunner,
		Prober:          startupProber{Inner: container.HTTPProber{}},
		MCP:             sbxMemoryRegistrar{},
	}
}

func (c *setupCmd) Help() string { return provision.Description }

// setupCmd provisions PIX_HOME. It takes no workspace argument and performs
// no agent handoff: `pix run` is the only thing that starts a sandbox.
type setupCmd struct {
	Verbose bool   `help:"Show the pix-memory container and MCP registration detail, not just the summary."`
	Env     string `help:"Also set up one existing environment: validate its declared requirements (docs/design/pix-v2-surface.md §3.6 step 7) and, if untrusted, run the same trust review pix env trust NAME does. It does not select that environment as the default."`
}

func (c *setupCmd) Run(d *cli.Deps) error { return c.run(d, productionSetupSeams()) }

// discoverVerifiedBundle is the ONE "what did this binary ship with"
// resolution: the release bundle adjacent to the resolved executable, with
// its runtime archive digest verified before anything reads it. `pix setup`
// and `pix run`'s automatic post-upgrade reconcile (upgrade_auto.go) both
// go through it, so neither can end up trusting a bundle the other would
// have refused.
func discoverVerifiedBundle(s setupSeams) (*release.Bundle, error) {
	bundle, err := s.DiscoverBundle()
	if err != nil {
		return nil, err
	}
	if err := bundle.VerifyArchive(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// machineSetup is the MACHINE-OWNED half of `pix setup`, and the whole of
// what an automatic post-upgrade reconcile is allowed to do: install the
// runtime, record the release, pull the digest-pinned images, ensure the
// generated default environment, reconcile THIS stack's pix-memory
// container, and register/verify its scoped MCP name. It is deliberately
// the only composition of provision.Setup in this package, so `pix setup`
// and `pix run`'s auto-reconcile cannot drift apart on what a release
// upgrade actually reconciles.
//
// What is NOT here is what makes it safe to run without being asked:
// credentials (setupCredentials), environment trust, and `[[setup]]` hooks
// all stay in the setup COMMAND, above.
func machineSetup(home pixhome.Paths, s setupSeams, bundle release.Bundle, confirmReplace func(container.Info, container.Spec) bool) (provision.Result, error) {
	spec := homeContainerSpec(home)
	spec.Image = provision.MemoryImageRef(bundle.Manifest)
	return provision.Setup(provision.Deps{
		Home:            home,
		Prereqs:         s.Prereqs,
		Manifest:        bundle.Manifest,
		Bundle:          &bundle,
		ContainerRunner: s.ContainerRunner,
		Prober:          s.Prober,
		ContainerSpec:   spec,
		ConfirmReplace:  confirmReplace,
		MCP:             s.MCP,
		EnsureImages: func(m release.Manifest) error {
			return provision.EnsureImages(s.ContainerRunner, m)
		},
	})
}

func (c *setupCmd) run(d *cli.Deps, s setupSeams) error {
	home, err := pixhome.Resolve()
	if err != nil {
		return err
	}

	// The release bundle is resolved FIRST, from beside the resolved
	// executable (symlinks followed, so the `make install` shape
	// ~/.local/bin/pix -> out/pix finds out/'s manifest and archive). A
	// missing, unparseable, or digest-mismatched bundle fails here with the
	// exact install remedy, before Docker or the Gateway is touched: the
	// pinned image digests and the strict kit identity every later step
	// needs come from this one document (architecture §3, §12).
	bundle, err := discoverVerifiedBundle(s)
	if err != nil {
		return fmt.Errorf("pix setup: %w", err)
	}
	if p, ok := s.Prober.(startupProber); ok {
		p.OnRetry = func(timeout time.Duration) {
			fmt.Fprintf(d.Out, "pix setup: waiting for the memory service to become ready (up to %s)...\n", timeout)
		}
		s.Prober = p
	}

	// The pix-memory bearer token is generated by provision.Setup INSIDE the
	// setup lock, before the container spec's AuthTokenFile mount is used
	// (`docker create -v <path>:...:ro` fails outright if that file does not
	// exist yet, and the token is never a literal `-e`/`--env-file` argument
	// — either would land it in the container's own Config.Env, which
	// `docker inspect` exposes to anything on this host with inspect
	// access). Nothing here holds a copy of it.
	//
	// The container spec is derived from the DISCOVERED manifest, not from
	// whatever release.json a previous run happened to leave behind: the
	// image this setup reconciles is the one this binary shipped with.
	res, err := machineSetup(home, s, *bundle, confirmContainerReplace(d, c.Verbose))
	if err != nil {
		return err
	}
	renderSetupResult(d, home, res, c.Verbose)
	// A named --env setup is not a base-install interview: the environment
	// already declares its own model roster ([models].main, [agents]), so
	// the base default-model picker and the optional Parallel-search offer
	// are noise here, not a prompt this run needs answered. Both stay in
	// full for a bare `pix setup` (baseSetup == true).
	baseSetup := c.Env == ""
	setupCredentials(d, baseSetup)
	if baseSetup {
		setupModelSelection(d, home, defaultShellEnv(), res.DefaultEnvCreated)
	}
	if c.Env != "" {
		if eerr := setupSelectedEnvironment(d, home, c.Env); eerr != nil {
			return eerr
		}
	}
	if !res.Ready() {
		return cli.SilentError{Code: 1}
	}
	return nil
}

// setupCredentials is setup's credential step, and it is deliberately small:
// establish THIS PIX_HOME's refs file, and offer to fill it when there is
// someone to ask. It never inspects, writes or repairs a host-global sbx
// secret — a global belongs to whoever pushed it, Pix reads only its own refs,
// and the values themselves are resolved per sandbox at launch. So a host
// covered in globals still gets its own refs file and still gets offered the
// 1Password prompt: inheriting another stack's credentials is not setup
// finishing early, it is setup never having run.
//
// It claims nothing about a model being ready. Nothing here resolved a ref,
// so the honest close is the command that configures one.
func setupCredentials(d *cli.Deps, baseSetup bool) {
	path, _, err := config.SeedOpRefs()
	if err != nil {
		fmt.Fprintf(d.Err, "pix setup: could not create the secrets file (%s): %v\n", path, err)
		return
	}
	env := defaultShellEnv()
	if d.Interactive {
		// Fires only when op is installed AND no provider ref is configured
		// yet (OfferOnePasswordKeys' own gate) — never a nag, never a claim.
		secret.OfferOnePasswordKeys(env, d.In, d.Out, true)
	}
	if secret.ProviderKeyRefsPresent(env) {
		fmt.Fprintln(d.Out, "model keys are configured as 1Password refs; each run resolves them into that run's own sandbox.")
	} else {
		fmt.Fprintln(d.Out, "no model provider key is configured yet. Next:")
		fmt.Fprintln(d.Out, "  pix secret set ANTHROPIC_API_KEY op://vault/item/field   (repeat per provider)")
		fmt.Fprintln(d.Out, "  pix secret check                                          (resolve every ref through op; no values printed)")
	}
	if baseSetup {
		setupParallelSearch(d, env)
	}
}

// setupParallelSearch is setup's explain step for the OPTIONAL Parallel
// web-search tool key: it never blocks a launch and it is never required,
// so this only ever offers (TTY, default-No) and reports, matching
// ToolKeyRefOrder's own contract (secret/sync.go). The offer runs BEFORE
// the report so a ref entered just now is reflected accurately, exactly
// like the model-key block above. Called only for a bare `pix setup`
// (baseSetup): a named `--env NAME` run has its own declared roster and
// gets no base-install prompts or reports (setup_cmd.go's Run).
func setupParallelSearch(d *cli.Deps, env hostenv.Env) {
	if d.Interactive {
		secret.OfferParallelSearchKey(env, d.In, d.Out, true)
	}
	if secret.ConfiguredParallelSearchRef(env) {
		fmt.Fprintln(d.Out, "Parallel web search is configured (PARALLEL_API_KEY ref present); pi-web-access uses it for that backend.")
		return
	}
	fmt.Fprintln(d.Out, "Parallel web search is optional and not configured; search falls back to other backends. To enable: pix secret set PARALLEL_API_KEY op://vault/item/field")
}

// setupSelectedEnvironment is `--env NAME`'s whole job (surface §3.6): sets
// up ONE EXISTING environment in addition to the machine-level
// prerequisites Run already handled above, and never selects it as the
// default (that is `pix env default NAME`'s job alone). It performs the
// same complete, default-No trust review `pix env trust NAME` does before
// checking anything else — an untrusted environment on a non-interactive
// terminal refuses here, naming that exact command, rather than silently
// skipping the review — then validates what this environment itself
// declares: its [inference.*] backends parse to a supported driver, and
// every roster reference ([models].main, each [agents] entry) resolves to
// a model machine config or this environment's own [[inference.models]]
// actually defines. Nothing here mutates config.toml or installs anything;
// a real reachability probe of a declared backend is `pix doctor`'s job
// (its own read-only probe set), not setup's.
func setupSelectedEnvironment(d *cli.Deps, home pixhome.Paths, name string) error {
	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		return envRun(d, err)
	}
	if terr := runEnvTrust(d, home, name, false, false); terr != nil {
		return terr
	}
	// ONE snapshot from here on. Everything below — the requirements check
	// AND any `[[setup]]` hook execution — reads this single in-memory
	// load, and the fingerprint proven accepted just below is the
	// fingerprint of THIS snapshot, never of a second, independently re-read
	// copy that could disagree with it.
	loaded, err := nativeenv.LoadHome(sel, nil, nil)
	if err != nil {
		return err
	}
	bom, fp, err := bomForLoaded(loaded)
	if err != nil {
		return err
	}
	if !trustSatisfied(home, sel, bom, fp) {
		// runEnvTrust accepted SOMETHING and this snapshot is not it: the
		// environment changed under us between review and use. Fail closed.
		return fmt.Errorf("pix setup --env %s: the environment changed after its trust review; re-run: pix env trust %s", name, name)
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return err
	}
	if _, ierr := launch.EffectiveInferenceConfig(cfg, loaded.Sidecar); ierr != nil {
		return fmt.Errorf("pix setup --env %s: %w", name, ierr)
	}
	shipped, _, _ := listAgents()
	if verr := validateRunRoster(cfg, launch.EnvSelection{Name: loaded.Name, Root: loaded.Root, Sidecar: loaded.Sidecar}, shipped); verr != nil {
		return fmt.Errorf("pix setup --env %s: %v", name, verr)
	}
	if verr := validateDeclaredEnvironmentValues(d, home, bom); verr != nil {
		return fmt.Errorf("pix setup --env %s: %w", name, verr)
	}
	fmt.Fprintf(d.Out, "environment %q declared requirements check passed.\n", name)
	return runSetupHooks(d, name, loaded.Root, bom)
}

// validateDeclaredEnvironmentValues is the declared-requirements check
// surface.md §3.6 step 7 promises: prove every credential and every plain
// value this environment's `[host.mcp.<name>]` entries declare
// (env_keys/plain_keys) is actually recorded, BEFORE any `[[setup]]` hook
// runs — so an environment never has to ship its own hook whose whole job
// is failing on purpose to print `pix secret set` commands (the pattern
// this replaces). A secret name (env_keys) is never prompted for here: the
// reviewed way in is `pix secret set`, and this only names the exact
// command. A plain name (plain_keys) IS collected here, on a TTY only — it
// is not a credential, so there is no reviewed-path reason to defer it to
// a second command — and is recorded with secret.SetPlainValue, never as
// an op:// reference. Either kind still missing after collection is a
// concise, actionable refusal naming every remaining name and its exact
// remedy, never a bare "setup failed".
func validateDeclaredEnvironmentValues(d *cli.Deps, home pixhome.Paths, bom nativeenv.BillOfMaterials) error {
	refs, _ := secret.LoadRefs(home)
	haveRef := map[string]bool{}
	for _, r := range refs {
		if r.IsRef && !r.Placeholder {
			haveRef[r.Key] = true
		}
	}

	var missingSecrets []string
	seenSecret := map[string]bool{}
	var missingPlain []string
	seenPlain := map[string]bool{}
	for _, m := range bom.HostMCP {
		for _, k := range m.EnvKeys {
			if !haveRef[k] && !seenSecret[k] {
				seenSecret[k] = true
				missingSecrets = append(missingSecrets, k)
			}
		}
		for _, k := range m.PlainKeys {
			if _, ok := secret.PlainValue(home, k); !ok && !seenPlain[k] {
				seenPlain[k] = true
				missingPlain = append(missingPlain, k)
			}
		}
	}
	sort.Strings(missingSecrets)
	sort.Strings(missingPlain)

	// Collect plain (non-secret) values right here, on a TTY: there is
	// nothing to review before recording an account address or a domain,
	// unlike a credential.
	if d.Interactive {
		var stillMissing []string
		reader := bufio.NewReader(d.In)
		for _, k := range missingPlain {
			fmt.Fprintf(d.Out, "  %s is a non-secret value this environment needs. Enter it now (blank to skip): ", k)
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)
			if line == "" {
				stillMissing = append(stillMissing, k)
				continue
			}
			if serr := secret.SetPlainValue(home, k, line); serr != nil {
				fmt.Fprintf(d.Out, "    could not record %s: %v\n", k, serr)
				stillMissing = append(stillMissing, k)
				continue
			}
			fmt.Fprintf(d.Out, "    recorded %s in %s\n", k, home.SecretsEnv)
		}
		missingPlain = stillMissing
	}

	if len(missingSecrets) == 0 && len(missingPlain) == 0 {
		return nil
	}

	fmt.Fprintln(d.Out, "this environment's declared requirements are not all recorded yet:")
	for _, k := range missingSecrets {
		fmt.Fprintf(d.Out, "  pix secret set %s op://<vault>/<item>/<field>\n", k)
	}
	for _, k := range missingPlain {
		fmt.Fprintf(d.Out, "  %s is a non-secret value; re-run `pix setup --env NAME` on a terminal to be prompted for it, or record it directly: printf '%s=<value>\\n' >> %s\n", k, k, home.SecretsEnv)
	}
	return fmt.Errorf("%d requirement(s) not recorded; see the exact commands above", len(missingSecrets)+len(missingPlain))
}

// runSetupHooks executes this environment's own `[[setup]]` hooks — the v2
// replacement for a pack's install/auth hook (docs/reference.md §"Setup
// hooks"). It runs ONLY here: `pix setup --env NAME`, after the default-No
// trust review above accepted the exact executables, argv, kinds and
// required bits it is about to run. `pix run` and `pix doctor` never reach
// it, and nothing outside this environment's own directory can contribute
// a hook.
func runSetupHooks(d *cli.Deps, name, root string, bom nativeenv.BillOfMaterials) error {
	if len(bom.SetupHooks) == 0 {
		return nil
	}
	res, err := envsetup.Run(root, bom.SetupHooks, envsetup.Options{
		EnvName:     name,
		Out:         d.Out,
		Err:         d.Err,
		In:          d.In,
		Interactive: d.Interactive,
	})
	for _, o := range res.Outcomes {
		if o.State == envsetup.StateSkipped {
			fmt.Fprintf(d.Out, "setup hook %s: skipped (%s)\n", o.ID, o.Detail)
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "environment %q setup hooks: %d checked, all required hooks ready.\n", name, len(res.Outcomes))
	return nil
}

// renderSetupResult prints what Setup did. The normal report on success is
// exactly one line: the PIX_HOME line — no separate "ready" narration,
// because a successful run's own zero exit status already says that.
// --verbose restores the full per-artifact detail (runtime, default env,
// container action, MCP registration) for anyone who wants to see it; a
// converged, idempotent rerun is exactly as quiet as a fresh success,
// because nothing changed is exactly as uninteresting either way. A FAILED
// outcome is never gated behind --verbose: renderSetupNotReady always
// prints the real container/MCP reason and the exact remedy, not a bare
// "run pix doctor" deflection.
func renderSetupResult(d *cli.Deps, home pixhome.Paths, res provision.Result, verbose bool) {
	switch {
	case res.Init.CreatedHome:
		fmt.Fprintf(d.Out, "initialized PIX_HOME at %s\n", home.Home)
	default:
		fmt.Fprintf(d.Out, "PIX_HOME already initialized at %s\n", home.Home)
	}
	if verbose {
		renderSetupArtifactDetail(d, home, res)
	}
	if !res.Ready() {
		renderSetupNotReady(d, res)
	}
}

// renderSetupArtifactDetail is the per-artifact narration --verbose
// restores: runtime install, default-env creation, container reconcile
// action, and MCP registration outcome. It is never the only place a
// FAILURE reason is printed — see renderSetupNotReady, which always runs on
// an unready result regardless of this flag.
func renderSetupArtifactDetail(d *cli.Deps, home pixhome.Paths, res provision.Result) {
	if res.ReleaseInstalled {
		verb := "already installed"
		if res.Runtime.Extracted {
			verb = "installed"
		}
		fmt.Fprintf(d.Out, "pix setup: runtime %s at %s (kit %s)\n", verb, res.Runtime.Dir, res.KitRevision)
	}
	if res.DefaultEnvCreated {
		fmt.Fprintf(d.Out, "pix setup: created the %s environment at %s (this host had none)\n",
			provision.DefaultEnvironmentName, home.EnvironmentDir(provision.DefaultEnvironmentName))
	}
	fmt.Fprintf(d.Out, "pix setup: pix-memory container: %s\n", res.Container.Action)
	switch {
	case !res.MCPRegistered:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: not attempted (no registrar wired)")
	case res.MCPState == provision.MCPRegistrationAdded:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: registered")
	case res.MCPState == provision.MCPRegistrationPresentVerified:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: already registered at this home's endpoint (read back and matched); left untouched")
	case res.MCPState == provision.MCPRegistrationPresentUnverified:
		// The unverified reason is printed unconditionally by
		// renderSetupNotReady (it is a failure reason, not artifact color),
		// so there is nothing additional to add here under --verbose.
	default:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: not attempted")
	}
	fmt.Fprintf(d.Err, "pix setup: pix home %s: container %s\n", home.Home, res.Container.Action)
}

// renderSetupNotReady prints the ACTUAL reason Setup left the host not
// ready and the EXACT next action — a failed readiness probe's own error, a
// declined container replace and its drift, a foreign-stack name collision,
// or an unverifiable MCP registration — never a bare "run pix doctor"
// deflection. It always runs when res.Ready() is false, independent of
// --verbose: a failure is not narration to suppress.
func renderSetupNotReady(d *cli.Deps, res provision.Result) {
	fmt.Fprintln(d.Out, "pix setup: not ready.")
	switch {
	case res.Container.ProbeErr != nil:
		fmt.Fprintf(d.Out, "  pix-memory did not pass its readiness probe: %v\n", res.Container.ProbeErr)
		fmt.Fprintf(d.Out, "  Check what's wrong, then rerun: docker logs %s ; pix setup\n", res.Container.ID)
	case res.Container.Action == container.ActionRefusedReplace:
		fmt.Fprintf(d.Out, "  the running pix-memory container (%s, fingerprint %s) does not match this release's pinned image, and the replace was declined.\n", res.Container.PreviousImage, res.Container.PreviousFingerprint)
		fmt.Fprintf(d.Out, "  Rerun `pix setup` and accept the replace, or remove it yourself: docker rm -f %s ; pix setup\n", res.Container.ID)
	case res.Container.Action == container.ActionRefusedForeignStack:
		fmt.Fprintf(d.Out, "  a container already exists under this name but is owned by a different Pix stack (%s); it was not adopted, started, or replaced.\n", res.Container.ForeignStackID)
		fmt.Fprintf(d.Out, "  If it is stale, remove it yourself: docker rm -f %s ; pix setup\n", res.Container.ID)
	case res.MCPRegistered && res.MCPState == provision.MCPRegistrationPresentUnverified:
		// NOT "ok", and NOT "verified": nothing could read the existing
		// entry's URL, so this run proved nothing about what the sandbox
		// would reach through that name (safety invariant 12). Nothing was
		// overwritten or removed; the user gets the exact commands.
		name := res.MCPName
		if name == "" {
			name = "this home's pix-memory MCP name"
		}
		fmt.Fprintf(d.Out, "  %s is already registered, and its endpoint could not be read on this host (`sbx mcp inspect %s` and `sbx mcp get %s` both failed); left untouched, nothing overwritten or removed\n", name, name, name)
		fmt.Fprintf(d.Out, "  Check it yourself, and if it is not this home's memory endpoint, remove it and rerun setup:\n    sbx mcp inspect %s\n    sbx mcp rm %s\n    pix setup\n", name, name)
	default:
		fmt.Fprintln(d.Out, "  pix-memory did not reach a ready state. For the full host report: pix doctor")
	}
}

// confirmContainerReplace is the exact prompt architecture §9.1 requires
// before a mismatched pix-memory container is stopped, removed, and
// recreated: say plainly that the memory service changed and that its data
// is preserved either way, ask, default to declining on anything that
// cannot ask (non-interactive, no confirmation requested with --yes).
// Normal output names NEITHER image reference NOR fingerprint — a raw
// digest dump reads as a full audit a human has to interpret before
// answering y/N, when the only fact that actually changes the answer is
// "the service changed, your data survives regardless". verbose restores
// the exact running/wanted image and fingerprint for anyone who wants to
// confirm precisely what changed, matching `pix env trust`'s own
// counts-by-default/--verbose-for-detail split.
func confirmContainerReplace(d *cli.Deps, verbose bool) func(current container.Info, want container.Spec) bool {
	return func(current container.Info, want container.Spec) bool {
		fmt.Fprintln(d.Err, "pix setup: the pix-memory service has changed and needs to be replaced. Its /data volume is preserved either way.")
		if verbose {
			fmt.Fprintf(d.Err, "  running: %s (fingerprint %s)\n", current.Image, current.Fingerprint())
			fmt.Fprintf(d.Err, "  wanted:  %s (fingerprint %s)\n", want.Image, want.Fingerprint())
		}
		if !d.Interactive {
			fmt.Fprintf(d.Err, "pix setup: refusing to replace it on a non-interactive terminal; rerun interactively or remove it yourself: docker rm -f %s\n", want.ContainerName)
			return false
		}
		fmt.Fprint(d.Err, "Replace it? [y/N] ")
		var line string
		fmt.Fscanln(d.In, &line)
		return line == "y" || line == "Y"
	}
}
