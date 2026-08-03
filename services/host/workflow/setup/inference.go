package setup

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/readiness/axis"
	"pix/host/routing"
	"pix/host/secret"
)

// Ollama probe budgets. They are vars, not consts, for ONE reason: a hermetic
// test has to be able to shrink them to exercise the budget branch without
// sitting through a five-minute wall clock. Nothing else writes them.
var (
	// ollamaCloudProbeTimeout bounds a cloud probe: a pure network round trip
	// that holds no local resource.
	ollamaCloudProbeTimeout = 20 * time.Second
	// OllamaLocalProbeTimeout bounds ONE cold local load, with nothing queued
	// ahead of it because the local set is serialized.
	OllamaLocalProbeTimeout = 90 * time.Second
	// OllamaLocalProbeBudget is the TOTAL wall clock the serialized local set may
	// spend. Four pulled rungs at 90s each is a pathological box, not a setup a
	// user should sit through.
	OllamaLocalProbeBudget = 300 * time.Second
)

// readSetupLine consumes exactly one line without a buffered reader that could
// steal subsequent answers from setup's provider-ref scanner.
func readSetupLine(in io.Reader) (string, bool) {
	var b strings.Builder
	one := []byte{0}
	for {
		n, err := in.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return strings.TrimSpace(b.String()), true
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			if b.Len() == 0 {
				return "", false
			}
			return strings.TrimSpace(b.String()), true
		}
	}
}

func configureCustomGateway(cfg *config.Config, in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "Gateway base URL: ")
	baseURL, ok := readSetupLine(in)
	if !ok || (!strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://localhost") && !strings.HasPrefix(baseURL, "http://127.0.0.1")) {
		return false, fmt.Errorf("gateway URL must use https (or loopback http)")
	}
	fmt.Fprint(out, "Authentication [sbx-session] (sbx-session/none): ")
	auth, ok := readSetupLine(in)
	if !ok || auth == "" {
		auth = "sbx-session"
	}
	if auth != "sbx-session" && auth != "none" {
		return false, fmt.Errorf("unsupported gateway authentication %q", auth)
	}
	fmt.Fprintln(out, "Map catalog models to gateway IDs (comma-separated catalog=upstream):")
	fmt.Fprint(out, "Models: ")
	mappings, ok := readSetupLine(in)
	if !ok || strings.TrimSpace(mappings) == "" {
		return false, fmt.Errorf("a gateway needs at least one model mapping")
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return false, err
	}
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	backend := config.InferenceBackend{Driver: "openai-compatible", BaseURL: strings.TrimRight(baseURL, "/"), Auth: auth}
	if auth == "sbx-session" {
		// sbx-login is a reserved Docker Sandboxes credential service. The
		// sandbox proxy resolves it from the current `sbx login` session; it is
		// not a secret users seed into the sbx credential store.
		backend.KeyEnv = "DOCKER_TOKEN"
		backend.CredentialService = "sbx-login"
		backend.CredentialHeader = "Authorization"
		backend.CredentialFormat = "Bearer %s"
	}
	cfg.Inference.Backends["gateway"] = backend
	for _, raw := range strings.Split(mappings, ",") {
		parts := strings.SplitN(strings.TrimSpace(raw), "=", 2)
		if len(parts) != 2 {
			return false, fmt.Errorf("invalid model mapping %q (want catalog=upstream)", raw)
		}
		canonical, upstream := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if _, found := reg.Get(canonical); !found {
			return false, fmt.Errorf("model %q is not in the Pix catalog", canonical)
		}
		if upstream == "" || strings.ContainsAny(upstream, " \t\r\n") {
			return false, fmt.Errorf("invalid upstream model id %q", upstream)
		}
		cfg.Inference.Models = append(cfg.Inference.Models, config.InferenceModelBinding{
			Model: canonical, Backend: "gateway", Upstream: upstream, Available: true,
		})
	}
	return true, nil
}

// OllamaSelection is what the user chose in the inference prompt. Local and
// Cloud are separate answers because they are separate products: a `:cloud`
// row in `ollama list` shows up on every signed-in machine and says nothing
// about what this machine can RUN, and a local model says nothing about what
// the subscription may CALL.
type OllamaSelection struct{ Local, Cloud bool }

// ollamaPlan is what ConfigureOllamaInference decided, for the caller to render
// and for the models step to act on. It contains no success claims: every
// binding it created is a CANDIDATE (Verified: false) until a probe says
// otherwise.
type ollamaPlan struct {
	Endpoint   string          // resolved via axis.EffectiveOllamaEndpoint
	LocalBound []string        // catalog ids bound as candidates from the listing
	CloudBound []string        // ditto, cloud
	WantPull   string          // the RAM-appropriate rung handed to SetupLocalModels
	SkippedRAM []string        // catalog local ids this machine cannot run
	Memory     axis.HostMemory // the reading that sized the offer
	// BestFit is the largest local rung this machine can run, pulled or not. It
	// is NOT the same as WantPull: WantPull is only set when nothing local is on
	// disk yet. Without BestFit the offer line ("offering qwen3.5:35b") is printed
	// and then silently abandoned whenever a smaller rung is already pulled,
	// which reads as a promise the command did not keep.
	BestFit string
}

// LocalBoundTags is LocalBound as ollama TAGS rather than catalog ids, for
// comparing against WantPull/BestFit (which are tags). The two spellings differ
// — "ollama/qwen3.5:9b" vs "qwen3.5:9b" — and comparing across them silently
// never matches.
func (p ollamaPlan) LocalBoundTags() []string {
	out := make([]string, 0, len(p.LocalBound))
	for _, id := range p.LocalBound {
		out = append(out, axis.OllamaTagFor(id))
	}
	return out
}

// ollamaListedModels returns the tags `ollama list` reports. This is a LISTING,
// the weakest possible signal: it proves a name was printed, not that the model
// runs here or that the account may call it.
func ollamaListedModels(env hostenv.Env) (map[string]bool, error) {
	out, timedOut, err := env.RunTimed("ollama", "list")
	if err != nil || timedOut {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(out, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			seen[fields[0]] = true
		}
	}
	return seen, nil
}

// listedCloudTagCount counts `:cloud`-tagged rows in the listing, for the
// prompt's hint line only. It is never used to bind or to claim entitlement.
func listedCloudTagCount(env hostenv.Env) int {
	listed, err := ollamaListedModels(env)
	if err != nil {
		return 0
	}
	n := 0
	for tag := range listed {
		if strings.Contains(tag, "cloud") {
			n++
		}
	}
	return n
}

// ConfigureOllamaInference binds CANDIDATES, never verified models. It splits
// the catalog on Model.Local so "Ollama local" and "Ollama Cloud" are the two
// separate answers they are, gates local rungs on the memory this machine
// actually has, and — crucially — NEVER hard-fails a user who has not pulled
// anything yet. The old error ("Ollama is healthy but none of its installed
// models match the Pix catalog") propagated out of a fatal mutation step, so
// the MOST COMMON local flow had the worst outcome in the whole setup. The
// replacement writes the RAM-appropriate rung to cfg.OllamaBridgeModel and lets
// it flow through the models step's EXISTING consent — there is no second
// consent mechanism, and a bare --yes still downloads nothing.
func ConfigureOllamaInference(cfg *config.Config, env hostenv.Env, sel OllamaSelection, out io.Writer) (ollamaPlan, error) {
	if out == nil {
		out = io.Discard
	}
	listed, err := ollamaListedModels(env)
	if err != nil {
		return ollamaPlan{}, err
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return ollamaPlan{}, err
	}
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	_, backendPreexisted := cfg.Inference.Backends["ollama"]
	endpoint := strings.TrimRight(axis.EffectiveOllamaEndpoint(cfg, env).URL, "/")
	cfg.Inference.Backends["ollama"] = config.InferenceBackend{Driver: "ollama", BaseURL: endpoint + "/v1", Auth: "none"}

	plan := ollamaPlan{Endpoint: endpoint}
	bound := map[string]bool{}
	for _, b := range cfg.Inference.Models {
		bound[b.Model] = true
	}
	bind := func(m routing.Model) {
		if bound[m.ID] {
			return
		}
		bound[m.ID] = true
		cfg.Inference.Models = append(cfg.Inference.Models, config.InferenceModelBinding{
			// A listing is not evidence. VerifyOllamaInference earns Verified with a
			// bounded, model-specific request through the resolved endpoint.
			Model: m.ID, Backend: "ollama", Upstream: axis.OllamaTagFor(m.ID), Available: true,
		})
	}

	var rung, bestLocal routing.Model
	rungOK := false
	if sel.Local {
		plan.Memory = axis.ProbeHostMemory(env)
		rung, rungOK = axis.ChooseLocalRung(reg, plan.Memory)
		if rungOK {
			plan.BestFit = axis.OllamaTagFor(rung.ID)
		}
		fmt.Fprintln(out, axis.LocalRungOfferLine(plan.Memory, rung, rungOK))
	}

	for _, m := range reg.Models {
		if m.Provider != "ollama" || !m.Available {
			continue
		}
		tag := axis.OllamaTagFor(m.ID)
		switch {
		case m.Local && sel.Local:
			// The gate decides what to OFFER TO PULL. A rung the user ALREADY pulled
			// costs nothing to bind as a candidate, and the probe — which loads it at
			// its declared context — is a better judge of whether it runs here than
			// the gate is. So the gate only skips a listed model when we positively
			// measured the machine and it does not fit.
			if plan.Memory.OK && !m.FitsMemory(plan.Memory.UsableGB) {
				plan.SkippedRAM = append(plan.SkippedRAM, m.ID)
				continue
			}
			if listed[tag] {
				bind(m)
				plan.LocalBound = append(plan.LocalBound, m.ID)
				if m.MinRAMGB >= bestLocal.MinRAMGB {
					bestLocal = m
				}
			}
		case !m.Local && sel.Cloud:
			if listed[tag] {
				bind(m)
				plan.CloudBound = append(plan.CloudBound, m.ID)
			}
		}
	}

	if sel.Local && bestLocal.ID != "" {
		// Something local is already on disk: the bridge and the router's local
		// option point at the largest one that fits, and nothing needs pulling.
		cfg.OllamaBridgeModel = axis.OllamaTagFor(bestLocal.ID)
		return plan, nil
	}
	if sel.Local && rungOK {
		// The rung is the local model Pix will call and the tag the bridge exposes.
		// Writing it BEFORE consent is deliberate and safe: SetupLocalModels reads
		// this key to build its readiness axes, so the tag must exist in config
		// before the step that asks about it — and naming a tag is a declared
		// intent, not a claim (Verified stays false, the binding is not callable,
		// and doctor's bridge row reports it missing).
		cfg.OllamaBridgeModel = axis.OllamaTagFor(rung.ID)
		bind(rung)
		plan.WantPull = axis.OllamaTagFor(rung.ID)
	}

	// An Ollama selection that produced NOTHING must not be persisted. Deleting
	// the old hard error left this function returning nil with an empty plan, so
	// the keys step reached cfg.Save() and wrote a backend with no models — and
	// the NEXT `pix setup` early-returns into EnableDeclaredInferenceBindings
	// ("configured but declares no models"), which is fatal. That bricks setup
	// until `pix state reset`, with config.toml hand-editing forbidden. Reachable
	// two ways: picking Ollama Cloud while signed out (no :cloud rows listed), and
	// picking local on a machine under the floor.
	//
	// So: roll the backend back and fail with the actionable reason. This is not a
	// return to the old hard error — that one fired at a user who simply had not
	// pulled anything yet, which is now the WantPull path above.
	if len(plan.LocalBound) == 0 && len(plan.CloudBound) == 0 && plan.WantPull == "" {
		if !backendPreexisted {
			delete(cfg.Inference.Backends, "ollama")
		}
		return ollamaPlan{}, fmt.Errorf("%s", emptyOllamaSelectionMessage(sel, plan))
	}
	return plan, nil
}

// emptyOllamaSelectionMessage names the ONE thing that would change the answer,
// per selected flow, instead of a generic "nothing matched". Nothing has been
// persisted by the time this is rendered.
func emptyOllamaSelectionMessage(sel OllamaSelection, plan ollamaPlan) string {
	var reasons []string
	if sel.Local {
		switch {
		case !plan.Memory.OK:
			reasons = append(reasons, "local: could not size this machine, so no local model was offered")
		case plan.Memory.TotalGB < axis.LocalFloorTotalGB:
			reasons = append(reasons, fmt.Sprintf("local: %.0f GB RAM is below the %d GB a local model needs here", plan.Memory.TotalGB, axis.LocalFloorTotalGB))
		default:
			reasons = append(reasons, "local: no catalog model fits this machine's usable memory")
		}
	}
	if sel.Cloud {
		reasons = append(reasons, "cloud: `ollama list` shows no cloud models — sign in with `ollama signin`, then re-run setup")
	}
	return "Ollama was selected but nothing is callable through it (" + strings.Join(reasons, "; ") + "). Nothing was saved; re-run `pix setup` and choose Ollama Cloud or an API key."
}

// configureModelRosterFrom is ConfigureModelRoster with the pre-mutation
// provider set injected, which is what makes "a provider the user has not been
// offered yet" answerable at all.
//
// prior == nil means "no reconcile happened, do not widen" (plain setup).
func configureModelRosterFrom(cfg *config.Config, in io.Reader, out io.Writer, interactive bool, requested string, prior map[string]bool) error {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" {
		return nil
	}
	_ = prior // widening moved ahead of the probe; see widenRosterForNewProviders
	reg, err := routing.LoadRegistry()
	if err != nil {
		return err
	}
	// Callable, not merely bound: the roster must not offer a model that has not
	// answered a request, or the user picks something that 401s at call time.
	// This changes the non-interactive `--models X` contract on purpose — see the
	// probe-specific error below.
	bound := map[string]bool{}
	candidateOnly := map[string]bool{}
	for _, b := range cfg.Inference.Models {
		if !b.Available || !inference.TopologyAllowed(cfg, b) {
			continue
		}
		if inference.Callable(cfg, b) {
			bound[b.Model] = true
			continue
		}
		candidateOnly[b.Model] = true
	}
	var candidates []routing.Model
	for _, m := range reg.Models {
		if m.Available && bound[m.ID] {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("the selected inference runtime exposes no models from the Pix catalog")
	}
	canonicalize := func(raw string) ([]string, error) {
		seen := map[string]bool{}
		var selected []string
		for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if token == "" {
				continue
			}
			if token == "all" {
				for _, m := range candidates {
					if !seen[m.ID] {
						seen[m.ID] = true
						selected = append(selected, m.ID)
					}
				}
				continue
			}
			if n, convErr := strconv.Atoi(token); convErr == nil {
				if n < 1 || n > len(candidates) {
					return nil, fmt.Errorf("model choice %d is out of range", n)
				}
				token = candidates[n-1].ID
			}
			m, ok := reg.Get(token)
			if ok && !bound[m.ID] && candidateOnly[m.ID] {
				return nil, fmt.Errorf("model %q is bound but has not passed a probe: pix setup --pull-models", token)
			}
			if !ok || !bound[m.ID] {
				return nil, fmt.Errorf("model %q is not available through the selected runtime", token)
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				selected = append(selected, m.ID)
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("choose at least one model")
		}
		return selected, nil
	}

	if strings.TrimSpace(requested) != "" {
		selected, err := canonicalize(requested)
		if err != nil {
			return err
		}
		cfg.Inference.AllowedModels = selected
		return nil
	}

	// Preserve an existing choice, dropping stale models that no longer have a
	// binding, and WIDEN it for any provider the roster has never been offered
	// for. Pruning alone froze the roster at first run: a user who set up with one
	// provider and later added a second got a key whose models could never enter
	// the roster, so they were never callable, so the key was inert — the dead end
	// this whole path exists to close.
	//
	// The widening itself happens in widenRosterForNewProviders, BEFORE the probe
	// (see ReconcileDirectInference): VerifyDirectInference only probes bindings
	// the roster already allows, so a roster widened after verification would
	// name models that were never probed, and therefore are not callable, and so
	// get pruned right back out here. Roster and probe are mutually gating; the
	// widen must come first and this pass prunes whatever failed.
	if len(cfg.Inference.AllowedModels) > 0 {
		var kept []string
		for _, id := range cfg.Inference.AllowedModels {
			if bound[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) > 0 {
			cfg.Inference.AllowedModels = kept
			recordRosterProviders(cfg, candidates)
			return nil
		}
	}
	defer func() { recordRosterProviders(cfg, candidates) }()

	if len(candidates) == 1 {
		cfg.Inference.AllowedModels = []string{candidates[0].ID}
		return nil
	}
	if !interactive {
		for _, m := range candidates {
			cfg.Inference.AllowedModels = append(cfg.Inference.AllowedModels, m.ID)
		}
		return nil
	}

	fmt.Fprintln(out, "Which models may Pix agents use?")
	fmt.Fprintln(out, "The router stays inside this roster; choose one model to use it everywhere.")
	for i, m := range candidates {
		fmt.Fprintf(out, "  %d. %s (%s)\n", i+1, m.Label, m.ID)
	}
	fmt.Fprint(out, "Choose models [all]: ")
	choice, ok := readSetupLine(in)
	if !ok || strings.TrimSpace(choice) == "" {
		choice = "all"
	}
	selected, err := canonicalize(choice)
	if err != nil {
		return err
	}
	cfg.Inference.AllowedModels = selected
	return nil
}

// EnableDeclaredInferenceBindings promotes pack-declared bindings into the
// create-time candidate set. The final sandbox smoke test remains the success
// authority because sbx-session authentication exists only on the sandbox data
// plane and cannot be faithfully replayed by a host HTTP probe.
func EnableDeclaredInferenceBindings(cfg *config.Config) error {
	if cfg == nil || len(cfg.Inference.Backends) == 0 || len(cfg.Inference.Models) == 0 {
		return fmt.Errorf("inference backend is configured but declares no models")
	}
	for i := range cfg.Inference.Models {
		b, ok := cfg.Inference.Backends[cfg.Inference.Models[i].Backend]
		if !ok {
			return fmt.Errorf("model %q references unknown backend %q", cfg.Inference.Models[i].Model, cfg.Inference.Models[i].Backend)
		}
		if b.Driver != "native" && strings.TrimSpace(b.BaseURL) == "" {
			return fmt.Errorf("backend %q has no base_url", cfg.Inference.Models[i].Backend)
		}
		cfg.Inference.Models[i].Available = true
	}
	return nil
}

// ConfigureDirectInference derives native backend bindings from the provider
// refs setup just validated and reconciled. The model catalog remains the one
// source of model metadata; adding a key never copies a second model list into
// setup code.
func ConfigureDirectInference(cfg *config.Config, providers []string) error {
	reg, err := routing.LoadRegistry()
	if err != nil {
		return err
	}
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	providerSet := map[string]bool{}
	for _, p := range providers {
		providerSet[p] = true
		keyEnv := map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY", "google": "GEMINI_API_KEY"}[p]
		if keyEnv != "" {
			cfg.Inference.Backends[p] = config.InferenceBackend{Driver: "native", Auth: "1password", KeyEnv: keyEnv}
		}
	}
	// Rebuild direct bindings deterministically while retaining bindings from
	// non-native pack/gateway backends.
	kept := cfg.Inference.Models[:0]
	for _, b := range cfg.Inference.Models {
		if cfg.Inference.Backends[b.Backend].Driver != "native" {
			kept = append(kept, b)
		}
	}
	cfg.Inference.Models = kept
	for _, m := range reg.Models {
		if m.Available && providerSet[m.Provider] {
			cfg.Inference.Models = append(cfg.Inference.Models, config.InferenceModelBinding{
				// A present credential makes this binding a candidate; it does not
				// prove that the account is entitled to this particular model.
				// VerifyDirectInference earns Verified with a bounded live request.
				Model: m.ID, Backend: m.Provider, Upstream: m.ID, Available: true,
			})
		}
	}
	return nil
}

// VerifyDirectInference earns Verified with an actual model-specific inference
// request. Every binding is independently checked; probes run concurrently so
// the wall-clock bound is one probe timeout rather than N timeouts. Resolved
// key bytes stay in process memory and are never included in errors or persisted.
func VerifyDirectInference(cfg *config.Config, env hostenv.Env) (res probeOutcome, err error) {
	if cfg == nil {
		return res, fmt.Errorf("verify direct inference: no config")
	}
	if env.DirectInference == nil {
		return res, ErrNoProbeSeam
	}
	type candidate struct {
		index           int
		provider, model string
	}
	var candidates []candidate
	for i := range cfg.Inference.Models {
		binding := &cfg.Inference.Models[i]
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || backend.Auth != "1password" || !binding.Available || !inference.Allowed(cfg, *binding) {
			continue
		}
		binding.Verified, binding.VerifiedBy, binding.VerifiedAt = false, "", ""
		candidates = append(candidates, candidate{index: i, provider: binding.Backend, model: strings.TrimPrefix(binding.Upstream, binding.Backend+"/")})
	}
	keys := map[string]string{}
	keyOK := map[string]bool{}
	for _, c := range candidates {
		if _, seen := keyOK[c.provider]; seen {
			continue
		}
		provider := c.provider
		backend := cfg.Inference.Backends[provider]
		ref, ok := secret.CurrentOpRef(env, backend.KeyEnv)
		if !ok {
			res.Failures = append(res.Failures, provider+": credential ref missing")
			keyOK[provider] = false
			continue
		}
		key, ok := secret.OpReadNonEmpty(env, ref)
		if !ok {
			res.Failures = append(res.Failures, provider+": credential could not be resolved")
			keyOK[provider] = false
			continue
		}
		keys[provider], keyOK[provider] = key, true
	}
	type result struct {
		index int
		label string
		err   error
	}
	results := make(chan result, len(candidates))
	for _, c := range candidates {
		if !keyOK[c.provider] {
			continue
		}
		res.Attempted++
		go func(c candidate, key string) {
			results <- result{index: c.index, label: cfg.Inference.Models[c.index].Model, err: env.DirectInference(c.provider, c.model, key)}
		}(c, keys[c.provider])
	}
	for i := 0; i < res.Attempted; i++ {
		r := <-results
		if r.err != nil {
			res.Failures = append(res.Failures, r.label+": "+r.err.Error())
			continue
		}
		cfg.Inference.Models[r.index].Verified = true
		// Provenance is written in the SAME assignment as the claim, and cleared
		// with it above, so it can never outlive what it describes.
		cfg.Inference.Models[r.index].VerifiedBy = config.VerifiedByProbe
		cfg.Inference.Models[r.index].VerifiedAt = time.Now().UTC().Format(time.RFC3339)
		res.Verified++
	}
	sort.Strings(res.Failures)
	return res, nil
}

// rosterSeenProviders answers "which providers has the roster already been
// offered for", which decides what widening may touch.
//
// A config written before roster_providers existed has an empty list, and the
// honest reading of that is the PRE-mutation bound set: those are the providers
// whose models the user had a chance to include when they last chose. Reading
// it as the CURRENT bound set instead is the bug this function exists to avoid
// — the provider being added is already bound by then, so it would count as
// seen and widening would silently do nothing on exactly the upgrade path that
// motivated it.
func rosterSeenProviders(cfg *config.Config, prior map[string]bool) map[string]bool {
	seen := map[string]bool{}
	for _, p := range cfg.Inference.RosterProviders {
		seen[p] = true
	}
	if len(seen) > 0 {
		return seen
	}
	if prior == nil {
		// No reconcile in flight (a plain `pix setup` re-run): nothing is new, so
		// treat every current provider as seen and widen nothing. Upgrading an
		// existing install must not silently change the roster.
		for _, id := range cfg.Inference.AllowedModels {
			if provider, _, ok := strings.Cut(id, "/"); ok {
				seen[provider] = true
			}
		}
		for _, b := range cfg.Inference.Models {
			seen[b.Backend] = true
		}
		return seen
	}
	for p := range prior {
		seen[p] = true
	}
	// A legacy config may name providers in AllowedModels that the pre-mutation
	// scan missed (a binding dropped from the catalog since). Union them in: the
	// user was plainly offered those.
	for _, id := range cfg.Inference.AllowedModels {
		if provider, _, ok := strings.Cut(id, "/"); ok {
			seen[provider] = true
		}
	}
	return seen
}

// recordRosterProviders stamps the providers this roster decision covered, so
// the next reconcile can tell a new provider from one the user already declined
// models from. Sorted for a stable, diffable config.
func recordRosterProviders(cfg *config.Config, candidates []routing.Model) {
	seen := map[string]bool{}
	for _, p := range cfg.Inference.RosterProviders {
		seen[p] = true
	}
	for _, m := range candidates {
		seen[m.Provider] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	cfg.Inference.RosterProviders = out
}

func ContainsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ErrInferenceExclusive is returned when a mandatory pack owns the whole
// inference surface. It is a REFUSAL, not a failure: adding a key would write
// bindings that the topology filter then silently drops, so reporting success
// would be a success word with nothing behind it.
var ErrInferenceExclusive = fmt.Errorf("a mandatory pack owns inference on this host")

// probeOutcome is what a verification pass ESTABLISHED, as one value instead of
// four positional returns. Attempted/Verified/Failures were already a tuple
// every caller destructured; NotProbed (ollama's third state — a candidate the
// time budget never reached, neither verified nor failed) made it four, and
// `_, verified, _, _ :=` at a dozen call sites is how a field gets silently
// dropped.
type probeOutcome struct {
	Attempted int
	Verified  int
	Failures  []string
	// NotProbed is neither verified nor failed: the local probe budget ran out
	// before this candidate got a turn. Reporting it as a failure would blame a
	// model for a clock.
	NotProbed []string
}

// ErrNoProbeSeam is returned when a verify function is handed a hostenv.Env with no
// probe function. That is a PROGRAMMING error, not a runtime condition, and it
// used to be returned as `0 attempted, 0 verified, no failures` — a value
// indistinguishable from a clean pass that found nothing to do. A caller then
// printed "0 model(s) answered a live request" and exited zero.
//
// It cost a real debugging cycle, and worse: it made the hard-error branch in
// RunSetupInferenceStep unreachable from its own test, hiding a bug where
// declining a model download exited non-zero. Absence rendered as a benign
// value is the same shape as the availability bug this package spent a week on.
var ErrNoProbeSeam = fmt.Errorf("no inference probe is configured on this hostenv.Env (use defaultShellEnv, or inject a probe in tests)")

// reconcileResult is what a reconcile actually did and proved.
type reconcileResult struct {
	Providers []string // every provider with a resolvable key, sorted
	Added     []string // providers that had no native binding before this run
	probeOutcome
}

// WidenRosterForProvider adds every bound catalog model of ONE provider to the
// roster, whether or not the roster has been offered that provider before.
//
// This is what makes `pix models add <provider>` mean what it says.
// widenRosterForNewProviders deliberately skips a provider recorded in
// RosterProviders, so that a considered narrowing ("I was shown Google's five
// models and picked two") is not silently undone by an unrelated reconcile. But
// a user TYPING the provider's name is asking to be offered it again, and
// honoring the stamp there re-creates the exact inertness this command exists to
// fix, one level up: on the second add, models bind and probe and then sit
// outside the roster while the command reports success.
//
// Matching is on the CATALOG model's provider, not the backend key, because
// those need not be the same word (a gateway backend can serve anthropic models).
func WidenRosterForProvider(cfg *config.Config, provider string) {
	if cfg == nil || provider == "" || cfg.Inference.ExclusiveSource != "" || len(cfg.Inference.AllowedModels) == 0 {
		return // no explicit request, or an empty roster (already "no restriction")
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return
	}
	for _, b := range cfg.Inference.Models {
		if !b.Available || ContainsString(cfg.Inference.AllowedModels, b.Model) {
			continue
		}
		if m, ok := reg.Get(b.Model); ok && m.Available && m.Provider == provider {
			cfg.Inference.AllowedModels = append(cfg.Inference.AllowedModels, b.Model)
		}
	}
}

// ReconcileOllamaInference is ReconcileDirectInference's counterpart for the one
// backend that has no key to store: Ollama. Same shape, same order, same
// honesty rules — bind candidates, widen, probe, save, judge — but the evidence
// comes from `ollama list` plus a model-specific generate through the resolved
// endpoint rather than from a 1Password ref plus a provider API call.
//
// It exists because `pix models add` was built entirely around
// secret.ProviderKeyRefOrder (anthropic/openai/google), so the ONE backend a user can
// wire without a credential was the one backend with no post-setup path at all:
// pulling a new local model or gaining a cloud entitlement left you re-running
// `pix setup` to make Pix notice.
//
// Downloads nothing. ConfigureOllamaInference may name a rung worth pulling; we
// report that tag and let the user decide, because `models add` is a wiring
// command and a multi-gigabyte download is not something to infer from it.
func ReconcileOllamaInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, sel OllamaSelection) (reconcileResult, ollamaPlan, error) {
	var res reconcileResult
	if cfg == nil {
		return res, ollamaPlan{}, fmt.Errorf("no config")
	}
	if cfg.Inference.ExclusiveSource != "" {
		return res, ollamaPlan{}, ErrInferenceExclusive
	}
	if out == nil {
		out = io.Discard
	}
	if err := RequireOllamaReady(env); err != nil {
		return res, ollamaPlan{}, err
	}
	res.Providers = []string{"ollama"}
	if _, existed := cfg.Inference.Backends["ollama"]; !existed {
		res.Added = []string{"ollama"}
	}

	plan, err := ConfigureOllamaInference(cfg, env, sel, out)
	if err != nil {
		return res, plan, err
	}
	// Widen BEFORE probing, for the same reason the direct path does: probes only
	// run on bindings the roster admits, so a newly bound model outside the roster
	// would never be probed, never become callable, and be pruned right back out
	// for not being callable.
	WidenRosterForProvider(cfg, "ollama")

	outcome, verr := VerifyOllamaInference(cfg, env, out)
	if verr != nil {
		return res, plan, fmt.Errorf("verifying ollama models: %w", verr)
	}
	res.probeOutcome = outcome
	// Save BEFORE the verdict so a partial success is never thrown away by the
	// error path below.
	if err := cfg.Save(); err != nil {
		return res, plan, err
	}
	if res.Verified == 0 && res.Attempted > 0 {
		detail := strings.Join(res.Failures, "; ")
		if detail == "" {
			detail = "no Ollama model answered a generate request"
		}
		return res, plan, fmt.Errorf("Ollama is reachable, but no model proved callable: %s", detail)
	}
	if len(res.NotProbed) > 0 {
		fmt.Fprintf(out, "%d candidate(s) were not probed within the time budget: %s\n", len(res.NotProbed), strings.Join(res.NotProbed, ", "))
	}
	if callable, _ := axis.ConfiguredInferenceSummary(cfg); callable > 0 {
		if err := configureModelRosterFrom(cfg, in, out, interactive, "", inference.BoundNativeProviders(cfg)); err != nil {
			return res, plan, fmt.Errorf("choosing models: %w", err)
		}
	}
	return res, plan, cfg.Save()
}

// RequireOllamaReady refuses early with the ONE thing that would change the
// answer. Both probes matter and they fail differently: no binary means Ollama
// is not installed, while a binary whose `list` hangs or errors means the daemon
// is not running — and telling a user to install software they already have is
// its own kind of wrong.
func RequireOllamaReady(env hostenv.Env) error {

	if _, err := env.LookPath("ollama"); err != nil {
		return fmt.Errorf("ollama is not installed or not on PATH — see https://ollama.com, then re-run")
	}
	if _, timedOut, err := env.RunTimed("ollama", "list"); err != nil || timedOut {
		return fmt.Errorf("the ollama binary is installed but the daemon did not answer `ollama list` — start Ollama, then re-run")
	}
	return nil
}

// widenRosterForNewProviders adds every catalog model of a provider the roster
// has never been offered for. It runs after binding and BEFORE verification, so
// the new provider's models are probed; configureModelRosterFrom then prunes
// whichever of them failed.
//
// A roster that is empty means "no user restriction" and needs no widening. A
// non-empty one is the case that froze: it named only the first provider's
// models, so every later key was permanently outside it.
func widenRosterForNewProviders(cfg *config.Config, prior map[string]bool) {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" || len(cfg.Inference.AllowedModels) == 0 {
		return
	}
	seen := rosterSeenProviders(cfg, prior)
	reg, err := routing.LoadRegistry()
	if err != nil {
		return
	}
	for _, b := range cfg.Inference.Models {
		if seen[b.Backend] || !b.Available || ContainsString(cfg.Inference.AllowedModels, b.Model) {
			continue
		}
		if m, ok := reg.Get(b.Model); ok && m.Available {
			cfg.Inference.AllowedModels = append(cfg.Inference.AllowedModels, b.Model)
		}
	}
}
