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
	"fmt"
	"sort"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/container"
	"pix/host/envsetup"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/secret"
	"pix/host/sys"
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
	// Env is the host seam machineSetup's Ollama detection reads (the SAME
	// integration `pix doctor`'s ollama row and any environment-declared
	// backend use). The zero value (System nil) is a deliberate, guarded
	// no-op — existing tests that build a bare setupSeams{} still run with no
	// network access and no panic; they simply get no memory-embedding wiring.
	Env hostenv.Env
}

func productionSetupSeams() setupSeams {
	return setupSeams{
		DiscoverBundle:  func() (*release.Bundle, error) { return release.DiscoverBundle(release.DefaultLocator) },
		Prereqs:         execChecker{},
		ContainerRunner: container.DefaultRunner,
		Prober:          startupProber{Inner: container.HTTPProber{}},
		MCP:             sbxMemoryRegistrar{},
		Env:             defaultShellEnv(),
	}
}

func (c *setupCmd) Help() string { return provision.Description }

// setupCmd provisions PIX_HOME. It takes no workspace argument and performs
// no agent handoff: `pix run` is the only thing that starts a sandbox.
type setupCmd struct {
	Verbose bool   `help:"Show the pix-memory container and MCP registration detail, not just the summary."`
	Env     string `help:"Also set up one existing environment: validate its declared requirements (docs/design/pix-v2-surface.md §3.6 step 7) and, if untrusted, run the same trust review pix env trust NAME does. On success it OFFERS (never assumes) to make it the machine default, so a bare pix launches it."`
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
	cfg, _ := config.Load()
	spec.Env, spec.ExtraHosts = memoryContainerEnv(cfg, s.Env)
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
	// Ollama comes FIRST, before the container is reconciled: the
	// pix-memory container's OLLAMA_HOST/MEMORY_EMBED_MODEL environment —
	// and its fingerprint, which decides whether the container is replaced
	// — is composed from exactly this detection (memoryContainerEnv). Ask
	// after the reconcile and a model pulled in this run lands in the NEXT
	// run's container, which is how a freshly pulled embedding model still
	// produced keyword-only recall until the user ran `pix setup` twice.
	setupMemoryEmbeddings(d, s.Env)

	res, err := machineSetup(home, s, *bundle, confirmContainerReplace(d, c.Verbose))
	if err != nil {
		return err
	}
	renderSetupResult(d, home, res, c.Verbose)
	// A named --env setup is not a base-install interview: the environment
	// declares its own roster ([models].main, [agents]) and often its own
	// authenticated backends, so EVERY base personal-provider step (the
	// 1Password offer, the provider-key report, the default-model picker,
	// the Parallel offer) is noise here; what it still asks for is that
	// environment's own declared values. All of it stays in full for a bare
	// `pix setup` (baseSetup == true).
	baseSetup := c.Env == ""
	setupCredentials(d, baseSetup)
	if baseSetup {
		setupModelSelection(d, home, defaultShellEnv(), res.DefaultEnvCreated)
	}
	if c.Env != "" {
		if eerr := setupSelectedEnvironment(d, home, c.Env); eerr != nil {
			return eerr
		}
		offerDefaultEnvironment(d, home, c.Env)
	}
	if !res.Ready() {
		return cli.SilentError{Code: 1}
	}
	return nil
}

// setupMemoryEmbeddings is setup's Ollama/embedding step: the SAME
// inference.DetectOllama integration the memory container's own OLLAMA_HOST
// (memoryContainerEnv) and `pix doctor`'s ollama row use, so all three can
// never disagree about what this host's Ollama looks like. It is never
// fatal — memory embeddings are an optional enhancement that degrades to
// keyword recall — and it follows the task's own rule literally: a LOCAL
// endpoint missing the embedding model gets an interactive, default-No
// offer to pull it; a REMOTE endpoint (a shared team daemon, or a proxied
// Ollama Cloud account) only ever gets reported, never offered a pull.
func setupMemoryEmbeddings(d *cli.Deps, env hostenv.Env) {
	if env.System == nil {
		return
	}
	cfg, _ := config.Load()
	embed := config.DefaultMemoryEmbedModel
	if cfg != nil && cfg.MemoryEmbedModel != "" {
		embed = cfg.MemoryEmbedModel
	}
	st := inference.DetectOllama(env)
	switch {
	case !st.CLIPresent && !st.Reachable:
		fmt.Fprintln(d.Out, "pix setup: ollama: not installed; memory embeddings degrade to keyword recall (optional).")
		return
	case !st.Reachable:
		fmt.Fprintf(d.Out, "pix setup: ollama: endpoint %s did not answer; memory embeddings degrade to keyword recall.\n", st.Endpoint.String())
		return
	}
	modeWord := "local"
	if st.Mode == inference.OllamaModeRemote {
		modeWord = "remote"
	}
	if st.HasModel(embed) {
		fmt.Fprintf(d.Out, "pix setup: ollama: %s endpoint %s, embedding model %q present.\n", modeWord, st.Endpoint.String(), embed)
		return
	}
	if !st.CanPull() {
		// Remote/cloud: report only, never pull — this is not this host's disk.
		fmt.Fprintf(d.Out, "pix setup: ollama: %s endpoint %s reachable, %d model(s) listed, embedding model %q not listed; memory embeddings degrade to keyword recall.\n", modeWord, st.Endpoint.String(), len(st.Models), embed)
		return
	}
	if !d.Interactive {
		fmt.Fprintf(d.Out, "pix setup: ollama: local endpoint %s, embedding model %q not pulled. Pull it yourself: ollama pull %s\n", st.Endpoint.String(), embed, embed)
		return
	}
	fmt.Fprintf(d.Out, "pix setup: ollama: local endpoint %s is missing the embedding model %q; semantic memory recall degrades to keyword search without it.\n", st.Endpoint.String(), embed)
	if !d.AskYN(fmt.Sprintf("Pull %s now? Downloading it can take a few minutes and several hundred MB of disk. [y/N] ", embed), false) {
		fmt.Fprintf(d.Out, "skipping; pull it yourself later: ollama pull %s\n", embed)
		return
	}
	if err := env.RunInteractive("ollama", "pull", embed); err != nil {
		fmt.Fprintf(d.Err, "pix setup: ollama pull %s failed: %v\n", embed, err)
		return
	}
	// A pull that exited 0 is not proof the model is callable: REPROBE the
	// same endpoint and report what it now lists. This is the only claim
	// this step is entitled to make, and it is why the whole step runs
	// before the container reconcile — the reprobed listing is what
	// memoryContainerEnv reads next.
	after := inference.DetectOllama(env)
	if !after.HasModel(embed) {
		fmt.Fprintf(d.Err, "pix setup: ollama pull %s reported success, but %s still does not list it; memory embeddings stay on keyword recall.\n", embed, after.Endpoint.String())
		return
	}
	listed, _ := after.ResolveModel(embed)
	fmt.Fprintf(d.Out, "pix setup: ollama: pulled %s; %s now lists it. Semantic memory recall is wired.\n", listed, after.Endpoint.String())
}

// setupCredentials is setup's credential step, and it is deliberately small:
// establish THIS PIX_HOME's refs file, and — on a BASE install only — offer
// to fill it with personal provider keys when there is someone to ask. It
// never inspects, writes or repairs a host-global sbx secret — a global
// belongs to whoever pushed it, Pix reads only its own refs, and the values
// themselves are resolved per sandbox at launch. So a host covered in globals
// still gets its own refs file and still gets offered the 1Password prompt:
// inheriting another stack's credentials is not setup finishing early, it is
// setup never having run.
//
// It claims nothing about a model being ready. Nothing here resolved a ref,
// so the honest close is the command that configures one.
func setupCredentials(d *cli.Deps, baseSetup bool) {
	path, _, err := config.SeedOpRefs()
	if err != nil {
		fmt.Fprintf(d.Err, "pix setup: could not create the secrets file (%s): %v\n", path, err)
		return
	}
	// A named `--env NAME` run stops here: the refs file had to exist (its
	// own declared values land in it moments later), but everything below is
	// the base personal-provider interview, and an environment with its own
	// authenticated backends can neither use a public-vendor key nor be told
	// it has none.
	if !baseSetup {
		return
	}
	env := defaultShellEnv()
	if d.Interactive {
		// Fires only when op is installed AND no provider ref is configured
		// yet (OfferOnePasswordKeys' own gate) — never a nag, never a claim.
		secret.OfferOnePasswordKeys(env, d.Line(), d.Out, true)
	}
	if secret.ProviderKeyRefsPresent(env) {
		fmt.Fprintln(d.Out, "model keys are configured as 1Password refs; each run resolves them into that run's own sandbox.")
	} else {
		fmt.Fprintln(d.Out, "no model provider key is configured yet. Next:")
		fmt.Fprintln(d.Out, "  pix secret set ANTHROPIC_API_KEY op://vault/item/field   (repeat per provider)")
		fmt.Fprintln(d.Out, "  pix secret check                                          (resolve every ref through op; no values printed)")
	}
	setupParallelSearch(d, env)
}

// setupParallelSearch is setup's explain step for the OPTIONAL Parallel
// web-search tool key: it never blocks a launch and it is never required,
// so this only ever offers (TTY, default-No) and reports, matching
// ToolKeyRefOrder's own contract (secret/sync.go). The offer runs BEFORE
// the report so a ref entered just now is reflected accurately, exactly
// like the model-key block above. Base install only: setupCredentials
// returns before this on a named `--env NAME` run.
func setupParallelSearch(d *cli.Deps, env hostenv.Env) {
	if d.Interactive {
		secret.OfferParallelSearchKey(env, d.Line(), d.Out, true)
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

// offerDefaultEnvironment closes a SUCCESSFUL `pix setup --env NAME` with
// the one question the user is otherwise left to discover: a bare `pix`
// launches the MACHINE DEFAULT environment, so an environment that was just
// set up but never selected silently keeps launching the old one — the
// failure mode being fixed here is a fully working `work` environment whose
// gateway inference never entered a launch because `default` was still
// selected.
//
// It OFFERS, it never assumes: an interactive default-Yes prompt (this is
// the environment the user just named on the command line, so Yes is the
// answer that matches the request, and No leaves everything untouched), and
// on a non-interactive terminal no write at all — just the exact command,
// because a script that ran setup never asked for its machine default to
// move. An environment that is ALREADY the default is confirmed, not
// re-asked and not re-written.
//
// The write goes through config.SetDefaultEnvironmentAt, the same single
// primitive `pix env default NAME` owns (invariant 2: one named writer per
// config.toml field — this is that writer, reached from the verb the user
// is already in, not a second one). A failed write is reported and never
// fatal: the environment IS set up, which is what this command promised.
func offerDefaultEnvironment(d *cli.Deps, home pixhome.Paths, name string) {
	cfg, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		fmt.Fprintf(d.Err, "pix setup: could not read the machine default environment: %v\n", err)
		fmt.Fprintf(d.Out, "To launch %q with a bare pix: pix env default %s\n", name, sys.ShellQuote(name))
		return
	}
	if strings.TrimSpace(cfg.DefaultEnvironment) == name {
		fmt.Fprintf(d.Out, "environment %q is already the default; run pix to start.\n", name)
		return
	}
	if !d.Interactive {
		fmt.Fprintf(d.Out, "environment %q is set up but is not the default (a bare pix launches %s). To change that: pix env default %s\n",
			name, defaultEnvironmentWord(cfg.DefaultEnvironment), sys.ShellQuote(name))
		return
	}
	if !d.AskYN(fmt.Sprintf("Use %s as the default environment for future pix runs? [Y/n] ", name), true) {
		fmt.Fprintf(d.Out, "keeping %s as the default; launch this one explicitly: pix run --env %s\n",
			defaultEnvironmentWord(cfg.DefaultEnvironment), sys.ShellQuote(name))
		return
	}
	if err := config.SetDefaultEnvironmentAt(home.Home, name); err != nil {
		fmt.Fprintf(d.Err, "pix setup: could not record the default environment: %v\n", err)
		fmt.Fprintf(d.Out, "Set it yourself: pix env default %s\n", sys.ShellQuote(name))
		return
	}
	fmt.Fprintf(d.Out, "Default environment: %s. Run pix to start.\n", name)
}

// defaultEnvironmentWord renders the CURRENT default for a sentence: its
// name, or an honest phrase when no default is recorded at all (where
// "launches \"\"" would read as an environment named the empty string).
func defaultEnvironmentWord(current string) string {
	if strings.TrimSpace(current) == "" {
		return "no environment"
	}
	return current
}

// validateDeclaredEnvironmentValues is the declared-requirements check
// surface.md §3.6 step 7 promises: prove every credential and every plain
// value this environment's `[host.mcp.<name>]` entries declare
// (env_keys/plain_keys) is actually recorded, BEFORE any `[[setup]]` hook
// runs — so an environment never has to ship its own hook whose whole job
// is failing on purpose to print `pix secret set` commands (the pattern
// this replaces), and a user is never told to stop, run a SEPARATE `pix
// secret set` per missing name, then rerun this exact command to find the
// next one. On a TTY, BOTH kinds are collected right here, in one screen,
// in one pass: an env_keys name through the exact primitive `pix secret
// set` itself uses (secret.SetRef — same op:// validation, same locked
// atomic write), and a plain_keys name through secret.SetPlainValue,
// recorded as a literal, never as an op:// reference. Neither loop invents
// a new persistence path or a new validation rule. A blank answer, or a
// value that primitive rejects, is not re-prompted — it falls through to
// the exact remedy printed below, same as an entirely non-interactive run.
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

	// One requirements screen: every missing name, secret and plain alike,
	// collected in this single pass — never a partial collection that
	// still sends the user off to run something else and come back. Each
	// prompt's Label/Example are the environment's own [host.values.<NAME>]
	// metadata when it declared any (valueLabel/valueExample), so a
	// well-authored environment asks for "Google Workspace account email"
	// instead of a bare "GOG_ACCOUNT".
	if d.Interactive && (len(missingSecrets)+len(missingPlain)) > 0 {
		fmt.Fprintf(d.Out, "this environment declares %d value(s) not yet recorded. Enter each now (blank to skip and record it later):\n", len(missingSecrets)+len(missingPlain))

		var stillMissingSecrets []string
		for _, k := range missingSecrets {
			// cli.Deps.Ask is the shared prompt: ONE stdin buffer for the
			// whole command, and a rejected answer is re-asked in place
			// instead of falling through to "go run three other commands
			// and start over". secret.SetRef is the SAME validated, locked
			// write `pix secret set` performs (op:// prefix, no control
			// characters, one atomic upsert), so the retry loop validates
			// against the real persistence path, never a copy of its rules.
			if _, ok := d.Ask(cli.Question{
				Label:   valueLabel(bom, k),
				Detail:  describeDeclaredValue(bom, k, true),
				Example: valueExample(bom, k, true),
				Accept: func(v string) error {
					if serr := secret.SetRef(home, k, v); serr != nil {
						return fmt.Errorf("could not record %s: %v", k, serr)
					}
					fmt.Fprintf(d.Out, "    recorded %s in %s\n", k, home.SecretsEnv)
					return nil
				},
			}); !ok {
				stillMissingSecrets = append(stillMissingSecrets, k)
			}
		}
		missingSecrets = stillMissingSecrets

		var stillMissingPlain []string
		for _, k := range missingPlain {
			if _, ok := d.Ask(cli.Question{
				Label:   valueLabel(bom, k),
				Detail:  describeDeclaredValue(bom, k, false),
				Example: valueExample(bom, k, false),
				Accept: func(v string) error {
					if serr := secret.SetPlainValue(home, k, v); serr != nil {
						return fmt.Errorf("could not record %s: %v", k, serr)
					}
					fmt.Fprintf(d.Out, "    recorded %s in %s\n", k, home.SecretsEnv)
					return nil
				},
			}); !ok {
				stillMissingPlain = append(stillMissingPlain, k)
			}
		}
		missingPlain = stillMissingPlain
	}

	// A value's own [host.values.NAME] metadata can mark it `required =
	// false`: still asked above, but never a reason to refuse setup when
	// it stays blank. Split AFTER the prompt loop, not before, so a
	// declined optional prompt still reaches this classification instead
	// of being dropped from missingSecrets/missingPlain earlier and
	// silently skipping its own "optional, continuing without it" line.
	requiredMissingSecrets, optionalMissingSecrets := splitByRequired(bom, missingSecrets)
	requiredMissingPlain, optionalMissingPlain := splitByRequired(bom, missingPlain)
	for _, k := range optionalMissingSecrets {
		fmt.Fprintf(d.Out, "  %s is optional and not recorded; continuing without it.\n", k)
	}
	for _, k := range optionalMissingPlain {
		fmt.Fprintf(d.Out, "  %s is optional and not recorded; continuing without it.\n", k)
	}

	if len(requiredMissingSecrets) == 0 && len(requiredMissingPlain) == 0 {
		return nil
	}

	fmt.Fprintln(d.Out, "this environment's declared requirements are not all recorded yet:")
	for _, k := range requiredMissingSecrets {
		fmt.Fprintf(d.Out, "  pix secret set %s op://<vault>/<item>/<field>\n", k)
	}
	for _, k := range requiredMissingPlain {
		fmt.Fprintf(d.Out, "  %s is a non-secret value; re-run `pix setup --env NAME` on a terminal to be prompted for it, or record it directly: printf '%s=<value>\\n' >> %s\n", k, k, home.SecretsEnv)
	}
	return fmt.Errorf("%d requirement(s) not recorded; see the exact commands above", len(requiredMissingSecrets)+len(requiredMissingPlain))
}

// splitByRequired partitions keys by each name's own [host.values.NAME]
// Required bit (default true when the environment declared no metadata
// for it at all — see envinfo.HostValueMeta.EffectiveRequired), preserving
// keys' relative order in both output slices.
func splitByRequired(bom nativeenv.BillOfMaterials, keys []string) (required, optional []string) {
	for _, k := range keys {
		if valueRequired(bom, k) {
			required = append(required, k)
		} else {
			optional = append(optional, k)
		}
	}
	return required, optional
}

// valueRequired reports whether key must be recorded before setup succeeds:
// its environment-declared [host.values.NAME].required when authored, true
// otherwise — every env_keys/plain_keys name is required by default.
func valueRequired(bom nativeenv.BillOfMaterials, key string) bool {
	if meta, ok := bom.ValueMeta(key); ok {
		return meta.Required
	}
	return true
}

// valueLabel is the prompt line's Label: the environment's own
// [host.values.NAME].label alongside the literal name (so the env var a
// value ultimately lands under is never hidden), or the bare name alone
// when the environment declared no friendlier one.
func valueLabel(bom nativeenv.BillOfMaterials, key string) string {
	if meta, ok := bom.ValueMeta(key); ok && meta.Label != "" {
		return fmt.Sprintf("%s (%s)", meta.Label, key)
	}
	return key
}

// valueExample is the prompt's Example, shown only after a rejected
// answer (cli.Question.Example's own placement): the environment's own
// [host.values.NAME].example when authored, else a well-formed op://
// sample for a secret prompt, else nothing — a plain value has no generic
// well-formed shape to demonstrate.
func valueExample(bom nativeenv.BillOfMaterials, key string, isSecret bool) string {
	if meta, ok := bom.ValueMeta(key); ok && meta.Example != "" {
		return meta.Example
	}
	if isSecret {
		return "op://Private/Anthropic API Key/credential"
	}
	return ""
}

// describeDeclaredValue is the one line of context a value prompt shows:
// WHICH declared integration needs this name, and what kind of answer it
// takes. An environment's own [host.values.<NAME>].help, when authored,
// is used verbatim (still suffixed with the same "Blank to skip." every
// prompt shows) — that is exactly what host.values metadata is FOR: a
// concise, meaningful sentence the environment author wrote, in place of
// the generic one derived below from bare env_keys/plain_keys membership.
// The derived fallback still exists for every environment that declares no
// metadata at all, from the bill of materials, never from a hardcoded
// table of key names, so an environment that declares a new key still gets
// a truthful prompt without a launcher change.
func describeDeclaredValue(bom nativeenv.BillOfMaterials, key string, isSecret bool) string {
	if meta, ok := bom.ValueMeta(key); ok && meta.Help != "" {
		return meta.Help + " Blank to skip."
	}
	var servers []string
	seen := map[string]bool{}
	for _, m := range bom.HostMCP {
		keys := m.PlainKeys
		if isSecret {
			keys = m.EnvKeys
		}
		for _, k := range keys {
			if k == key && !seen[m.Name] {
				seen[m.Name] = true
				servers = append(servers, m.Name)
			}
		}
	}
	sort.Strings(servers)
	kind := "a non-secret value, recorded literally"
	if isSecret {
		kind = "a 1Password reference (op://vault/item/field), never the secret itself"
	}
	if len(servers) == 0 {
		return fmt.Sprintf("%s needs %s. Blank to skip.", key, kind)
	}
	return fmt.Sprintf("%s is needed by %s; %s. Blank to skip.", key, strings.Join(servers, ", "), kind)
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
