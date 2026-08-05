package models

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/routing"
)

// inference.go — what this host BINDS: the backends, the candidate bindings
// they imply, and the personal roster over them. Nothing here proves anything;
// setupinference.go owns the probe path that makes a candidate callable.

// Ollama probe budgets. Vars, not consts, so a hermetic test can shrink them.
// Cloud is a network round trip; local is ONE cold load, serialized, under a
// total wall budget (four rungs at 90s is a pathological box, not a setup to
// sit through).
var (
	ollamaCloudProbeTimeout = 20 * time.Second
	OllamaLocalProbeTimeout = 90 * time.Second
	OllamaLocalProbeBudget  = 300 * time.Second
)

// ErrInferenceExclusive is a REFUSAL, not a failure: under a mandatory pack the
// topology filter drops every binding written here, so "added" would be a
// success word with nothing behind it.
var ErrInferenceExclusive = fmt.Errorf("a mandatory pack owns inference on this host")

// readSetupLine consumes exactly one line without a buffered reader that could
// steal subsequent answers from the provider-ref scanner.
func readSetupLine(in io.Reader) (string, bool) {
	var b strings.Builder
	one := []byte{0}
	for {
		n, err := in.Read(one)
		if n == 1 && one[0] == '\n' {
			return strings.TrimSpace(b.String()), true
		}
		if n == 1 {
			b.WriteByte(one[0])
		}
		if err != nil {
			return strings.TrimSpace(b.String()), b.Len() > 0
		}
	}
}

// backends lazily creates the backend map, so no caller has to.
func backends(cfg *config.Config) map[string]config.InferenceBackend {
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	return cfg.Inference.Backends
}

// bind is the ONE place a candidate binding is written. Available means "worth
// probing", never "proven": Verified stays false until a probe earns it, which
// is why neither a listing nor a resolvable key makes a model callable.
func bind(cfg *config.Config, model, backend, upstream string) {
	cfg.Inference.Models = append(cfg.Inference.Models, config.InferenceModelBinding{
		Model: model, Backend: backend, Upstream: upstream, Available: true,
	})
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
	backend := config.InferenceBackend{Driver: "openai-compatible", BaseURL: strings.TrimRight(baseURL, "/"), Auth: auth}
	if auth == "sbx-session" {
		// sbx-login is a reserved Docker Sandboxes service resolved from the
		// current `sbx login` session, not a secret users seed into sbx.
		backend.KeyEnv, backend.CredentialService = "DOCKER_TOKEN", "sbx-login"
		backend.CredentialHeader, backend.CredentialFormat = "Authorization", "Bearer %s"
	}
	backends(cfg)["gateway"] = backend
	for _, raw := range strings.Split(mappings, ",") {
		canonical, upstream, found := strings.Cut(strings.TrimSpace(raw), "=")
		canonical, upstream = strings.TrimSpace(canonical), strings.TrimSpace(upstream)
		switch {
		case !found:
			return false, fmt.Errorf("invalid model mapping %q (want catalog=upstream)", raw)
		case !inCatalog(reg, canonical):
			return false, fmt.Errorf("model %q is not in the Pix catalog", canonical)
		case upstream == "" || strings.ContainsAny(upstream, " \t\r\n"):
			return false, fmt.Errorf("invalid upstream model id %q", upstream)
		}
		bind(cfg, canonical, "gateway", upstream)
	}
	return true, nil
}

func inCatalog(reg *routing.Registry, id string) bool {
	_, ok := reg.Get(id)
	return ok
}

// OllamaSelection is what the user chose. Local and Cloud are separate answers
// because they are separate products: a `:cloud` row appears on every signed-in
// machine and says nothing about what this machine can RUN, and a local model
// says nothing about what the subscription may CALL.
type OllamaSelection struct{ Local, Cloud bool }

// ollamaPlan is what ConfigureOllamaInference decided, for the caller to render
// and the models step to act on. It contains no success claims.
type ollamaPlan struct {
	Endpoint   string               // resolved via inference.OllamaEndpointFor
	LocalBound []string             // catalog ids bound as candidates from the listing
	CloudBound []string             // ditto, cloud
	WantPull   string               // the RAM-appropriate rung, only when nothing local is on disk
	SkippedRAM []string             // catalog local ids this machine cannot run
	Memory     inference.HostMemory // the reading that sized the offer
	// BestFit is the largest rung this machine can run, pulled or not. Without
	// it the offer line is printed and then silently abandoned whenever a
	// smaller rung is already pulled — a promise the command did not keep.
	BestFit string
}

// LocalBoundTags is LocalBound as ollama TAGS, for comparing against
// WantPull/BestFit: comparing across the two spellings never matches.
func (p ollamaPlan) LocalBoundTags() []string {
	out := make([]string, 0, len(p.LocalBound))
	for _, id := range p.LocalBound {
		out = append(out, inference.OllamaTagFor(id))
	}
	return out
}

// ollamaListing returns the tags `ollama list` reports — the weakest possible
// signal: a name was printed, not that the model runs here or that the account
// may call it. It doubles as the daemon readiness probe, so "is Ollama up" has
// exactly one spelling.
func ollamaListing(env hostenv.Env) (map[string]bool, error) {
	out, timedOut, err := env.RunTimed("ollama", "list")
	if err != nil || timedOut {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); i > 0 && len(fields) > 0 {
			seen[fields[0]] = true
		}
	}
	return seen, nil
}

// RequireOllamaReady refuses with the ONE thing that would change the answer.
// The two checks fail differently: no binary means not installed, a binary
// whose `list` hangs means the daemon is down — and telling a user to install
// software they already have is its own kind of wrong.
func RequireOllamaReady(env hostenv.Env) error {
	if _, err := env.LookPath("ollama"); err != nil {
		return fmt.Errorf("ollama is not installed or not on PATH — see https://ollama.com, then re-run")
	}
	if _, err := ollamaListing(env); err != nil {
		return fmt.Errorf("the ollama binary is installed but the daemon did not answer `ollama list` — start Ollama, then re-run")
	}
	return nil
}

// ConfigureOllamaInference binds CANDIDATES, never verified models. It splits
// the catalog on Model.Local, gates local rungs on measured memory, and never
// hard-fails a user who has simply pulled nothing yet: the RAM-appropriate rung
// goes to cfg.OllamaBridgeModel and flows through the models step's EXISTING
// consent, so a bare --yes still downloads nothing.
func ConfigureOllamaInference(cfg *config.Config, env hostenv.Env, sel OllamaSelection, out io.Writer) (ollamaPlan, error) {
	out = orDiscard(out)
	listed, err := ollamaListing(env)
	if err != nil {
		return ollamaPlan{}, err
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return ollamaPlan{}, err
	}
	_, backendPreexisted := cfg.Inference.Backends["ollama"]
	endpoint := strings.TrimRight(inference.OllamaEndpointFor(env).URL, "/")
	backends(cfg)["ollama"] = config.InferenceBackend{Driver: "ollama", BaseURL: endpoint + "/v1", Auth: "none"}

	plan := ollamaPlan{Endpoint: endpoint}
	bound := map[string]bool{}
	for _, b := range cfg.Inference.Models {
		bound[b.Model] = true
	}
	bindOnce := func(m routing.Model) {
		if !bound[m.ID] {
			bound[m.ID] = true
			bind(cfg, m.ID, "ollama", inference.OllamaTagFor(m.ID))
		}
	}

	var rung, bestLocal routing.Model
	rungOK := false
	if sel.Local {
		plan.Memory = inference.ProbeHostMemory(env)
		if rung, rungOK = inference.ChooseLocalRung(reg, plan.Memory); rungOK {
			plan.BestFit = inference.OllamaTagFor(rung.ID)
		}
		fmt.Fprintln(out, inference.LocalRungOfferLine(plan.Memory, rung, rungOK))
	}
	for _, m := range reg.Models {
		if m.Provider != "ollama" || !m.Available {
			continue
		}
		switch {
		case m.Local && sel.Local:
			// The RAM gate decides what to OFFER TO PULL. A rung already on disk
			// costs nothing to bind and the probe judges it better, so the gate only
			// skips a listed model on a machine we measured and it does not fit.
			if plan.Memory.OK && !m.FitsMemory(plan.Memory.UsableGB) {
				plan.SkippedRAM = append(plan.SkippedRAM, m.ID)
			} else if listed[inference.OllamaTagFor(m.ID)] {
				bindOnce(m)
				plan.LocalBound = append(plan.LocalBound, m.ID)
				if m.MinRAMGB >= bestLocal.MinRAMGB {
					bestLocal = m
				}
			}
		case !m.Local && sel.Cloud && listed[inference.OllamaTagFor(m.ID)]:
			bindOnce(m)
			plan.CloudBound = append(plan.CloudBound, m.ID)
		}
	}

	switch {
	case sel.Local && bestLocal.ID != "":
		// Something local is already on disk: the bridge and the router's local
		// option point at the largest one that fits, and nothing needs pulling.
		cfg.OllamaBridgeModel = inference.OllamaTagFor(bestLocal.ID)
	case sel.Local && rungOK:
		// Writing the tag BEFORE consent is deliberate: the models step reads this
		// key to build its readiness axes. Naming a tag is a declared intent, not
		// a claim — Verified stays false and doctor reports the weights missing.
		cfg.OllamaBridgeModel = inference.OllamaTagFor(rung.ID)
		bindOnce(rung)
		plan.WantPull = inference.OllamaTagFor(rung.ID)
	case len(plan.LocalBound) == 0 && len(plan.CloudBound) == 0:
		// A selection that produced NOTHING must not be persisted: a backend with
		// no models makes the NEXT `pix setup` fatal in
		// EnableDeclaredInferenceBindings, bricking it until `pix state reset`.
		// Reachable via Cloud-while-signed-out and local-under-the-floor, so roll
		// the backend back and name the fix.
		if !backendPreexisted {
			delete(cfg.Inference.Backends, "ollama")
		}
		return ollamaPlan{}, fmt.Errorf("%s", emptyOllamaSelectionMessage(sel, plan))
	}
	return plan, nil
}

// emptyOllamaSelectionMessage names the ONE thing that would change the answer
// per selected flow, instead of a generic "nothing matched".
func emptyOllamaSelectionMessage(sel OllamaSelection, plan ollamaPlan) string {
	var reasons []string
	if sel.Local {
		switch {
		case !plan.Memory.OK:
			reasons = append(reasons, "local: could not size this machine, so no local model was offered")
		case plan.Memory.TotalGB < inference.LocalFloorTotalGB:
			reasons = append(reasons, fmt.Sprintf("local: %.0f GB RAM is below the %d GB a local model needs here", plan.Memory.TotalGB, inference.LocalFloorTotalGB))
		default:
			reasons = append(reasons, "local: no catalog model fits this machine's usable memory")
		}
	}
	if sel.Cloud {
		reasons = append(reasons, "cloud: `ollama list` shows no cloud models — sign in with `ollama signin`, then re-run setup")
	}
	return "Ollama was selected but nothing is callable through it (" + strings.Join(reasons, "; ") + "). Nothing was saved; re-run `pix setup` and choose Ollama Cloud or an API key."
}

// EnableDeclaredInferenceBindings promotes pack-declared bindings into the
// create-time candidate set. The sandbox smoke test stays the success
// authority: sbx-session auth cannot be replayed by a host HTTP probe.
func EnableDeclaredInferenceBindings(cfg *config.Config) error {
	if cfg == nil || len(cfg.Inference.Backends) == 0 || len(cfg.Inference.Models) == 0 {
		return fmt.Errorf("inference backend is configured but declares no models")
	}
	for i := range cfg.Inference.Models {
		m := &cfg.Inference.Models[i]
		b, ok := cfg.Inference.Backends[m.Backend]
		switch {
		case !ok:
			return fmt.Errorf("model %q references unknown backend %q", m.Model, m.Backend)
		case b.Driver != "native" && strings.TrimSpace(b.BaseURL) == "":
			return fmt.Errorf("backend %q has no base_url", m.Backend)
		}
		m.Available = true
	}
	return nil
}

// ConfigureDirectInference derives native backend bindings from the provider
// refs that resolved. The catalog stays the one source of model metadata.
func ConfigureDirectInference(cfg *config.Config, providers []string) error {
	reg, err := routing.LoadRegistry()
	if err != nil {
		return err
	}
	keyEnvs := map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY", "google": "GEMINI_API_KEY"}
	wanted := map[string]bool{}
	for _, p := range providers {
		wanted[p] = true
		if keyEnv := keyEnvs[p]; keyEnv != "" {
			backends(cfg)[p] = config.InferenceBackend{Driver: "native", Auth: "1password", KeyEnv: keyEnv}
		}
	}
	// Rebuild direct bindings deterministically, retaining bindings from
	// non-native pack/gateway backends.
	kept := cfg.Inference.Models[:0]
	for _, b := range cfg.Inference.Models {
		if cfg.Inference.Backends[b.Backend].Driver != "native" {
			kept = append(kept, b)
		}
	}
	cfg.Inference.Models = kept
	for _, m := range reg.Models {
		if m.Available && wanted[m.Provider] {
			bind(cfg, m.ID, m.Provider, m.ID)
		}
	}
	return nil
}

// ConfigureModelRoster turns the broad set of bindings into the small, explicit
// catalog-model surface agents may use; the router picks by intent but can
// never escape it. Candidates are CALLABLE models, not merely bound ones —
// offering one that never answered means the user picks something that 401s at
// call time — so a bound-but-unprobed model gets its own error.
func ConfigureModelRoster(cfg *config.Config, in io.Reader, out io.Writer, interactive bool, requested string) error {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" {
		return nil
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return err
	}
	callable, unproven := map[string]bool{}, map[string]bool{}
	for _, b := range cfg.Inference.Models {
		switch {
		case !b.Available || !inference.TopologyAllowed(cfg, b):
		case inference.Callable(cfg, b):
			callable[b.Model] = true
		default:
			unproven[b.Model] = true
		}
	}
	var candidates []routing.Model
	for _, m := range reg.Models {
		if m.Available && callable[m.ID] {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("the selected inference runtime exposes no models from the Pix catalog")
	}

	canonicalize := func(raw string) ([]string, error) {
		var selected []string
		add := func(id string) {
			if !slices.Contains(selected, id) {
				selected = append(selected, id)
			}
		}
		for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if token == "all" {
				for _, m := range candidates {
					add(m.ID)
				}
				continue
			}
			if n, convErr := strconv.Atoi(token); convErr == nil {
				if n < 1 || n > len(candidates) {
					return nil, fmt.Errorf("model choice %d is out of range", n)
				}
				token = candidates[n-1].ID
			}
			m, known := reg.Get(token)
			switch {
			case known && unproven[m.ID] && !callable[m.ID]:
				return nil, fmt.Errorf("model %q is bound but has not passed a probe: pix setup --pull-models", token)
			case !known || !callable[m.ID]:
				return nil, fmt.Errorf("model %q is not available through the selected runtime", token)
			}
			add(m.ID)
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
	defer func() { recordRosterProviders(cfg, candidates) }()

	// Preserve an existing choice, dropping models with no callable binding.
	// Widening happens BEFORE the probe (see widenRoster): a roster widened
	// afterwards names models that were never probed, so they are not callable,
	// so they get pruned right back out on this very line.
	var kept []string
	for _, id := range cfg.Inference.AllowedModels {
		if callable[id] {
			kept = append(kept, id)
		}
	}
	if len(kept) > 0 {
		cfg.Inference.AllowedModels = kept
		return nil
	}
	if len(candidates) == 1 || !interactive {
		cfg.Inference.AllowedModels = nil
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

// WidenRosterForProvider adds every bound catalog model of ONE provider,
// offered before or not — this is what makes `pix models add <provider>` mean
// what it says. New-provider widening honors the RosterProviders stamp so a
// considered narrowing survives an unrelated reconcile; a user TYPING the
// provider's name is asking to be offered it again. Matching is on the CATALOG
// model's provider: a gateway backend can serve anthropic models.
func WidenRosterForProvider(cfg *config.Config, provider string) {
	if provider != "" {
		widenRoster(cfg, func(_ config.InferenceModelBinding, m routing.Model) bool { return m.Provider == provider })
	}
}

// widenRosterForNewProviders adds every catalog model of a provider the roster
// has never been offered. It runs after binding and BEFORE verification, so the
// new models are probed; ConfigureModelRoster prunes whichever failed.
func widenRosterForNewProviders(cfg *config.Config, prior map[string]bool) {
	if cfg == nil {
		return
	}
	seen := rosterSeenProviders(cfg, prior)
	widenRoster(cfg, func(b config.InferenceModelBinding, _ routing.Model) bool { return !seen[b.Backend] })
}

// widenRoster is the one widening mechanism. An EMPTY roster already means "no
// restriction" and is never widened: turning an absence of policy into an
// explicit list is how the roster froze in the first place.
func widenRoster(cfg *config.Config, admit func(config.InferenceModelBinding, routing.Model) bool) {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" || len(cfg.Inference.AllowedModels) == 0 {
		return
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return
	}
	for _, b := range cfg.Inference.Models {
		if !b.Available || slices.Contains(cfg.Inference.AllowedModels, b.Model) {
			continue
		}
		if m, ok := reg.Get(b.Model); ok && m.Available && admit(b, m) {
			cfg.Inference.AllowedModels = append(cfg.Inference.AllowedModels, b.Model)
		}
	}
}

// rosterSeenProviders answers "which providers has the roster already been
// offered", deciding what widening may touch. A pre-roster_providers config has
// an empty list, and the honest reading of that is the PRE-mutation bound set
// (prior): reading the CURRENT set counts the provider being added as seen, so
// widening does nothing on exactly the upgrade path that motivated it. prior ==
// nil means no reconcile is in flight, where nothing is new and nothing widens.
func rosterSeenProviders(cfg *config.Config, prior map[string]bool) map[string]bool {
	seen := map[string]bool{}
	for _, p := range cfg.Inference.RosterProviders {
		seen[p] = true
	}
	if len(seen) > 0 {
		return seen
	}
	for p := range prior {
		seen[p] = true
	}
	if prior == nil {
		for _, b := range cfg.Inference.Models {
			seen[b.Backend] = true
		}
	}
	// A legacy config may name providers the pre-mutation scan missed (a binding
	// dropped from the catalog since); the user was plainly offered those.
	for _, id := range cfg.Inference.AllowedModels {
		if provider, _, ok := strings.Cut(id, "/"); ok {
			seen[provider] = true
		}
	}
	return seen
}

// recordRosterProviders stamps the providers this decision covered, so the next
// reconcile can tell a new provider from one the user declined models from.
func recordRosterProviders(cfg *config.Config, candidates []routing.Model) {
	seen := append([]string{}, cfg.Inference.RosterProviders...)
	for _, m := range candidates {
		if !slices.Contains(seen, m.Provider) {
			seen = append(seen, m.Provider)
		}
	}
	sort.Strings(seen)
	cfg.Inference.RosterProviders = seen
}

// ContainsString is the roster's membership test.
func ContainsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
