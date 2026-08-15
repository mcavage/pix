package models

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// ollamaTagsTimeout bounds the /api/tags request so a wedged daemon can never
// hang setup. A var, not a const, so a hermetic test can shrink it (an
// unreachable-daemon test must fail fast, not wait out a production budget).
var ollamaTagsTimeout = 5 * time.Second

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

// OllamaSelection is what the user chose. Local and Cloud are separate answers
// because they are separate products: a `:cloud` row appears on every signed-in
// machine and says nothing about what this machine can RUN, and a local model
// says nothing about what the subscription may CALL.
type OllamaSelection struct{ Local, Cloud bool }

// ollamaTagInfo is what ONE row of the daemon's /api/tags response tells us
// about a pulled tag that the shipped catalog does not know. RemoteHost is the
// field docs.ollama.com/api/tags documents as "URL of the upstream Ollama host,
// if the model is remote" — non-empty is Ollama's OWN cloud/local answer, on
// any daemon new enough to report it. Size is the on-disk byte count: a real
// Ollama Cloud row reports the size of its (tiny) local MANIFEST stub, a few
// hundred bytes, verified against a live daemon (deepseek-v4-flash:cloud: 316,
// glm-5.2:cloud: 290, kimi-k3:cloud: 308) versus a real local model's actual
// weights (qwen3.5:9b: 6.6GB, the smallest embedding model on the same host:
// 274MB) — see the classification note below.
type ollamaTagInfo struct {
	RemoteHost string
	Size       int64
}

// ollamaLocalSizeFloor is the fallback size classifier: comfortably above any
// Ollama Cloud manifest stub (hundreds of bytes) and comfortably below the
// smallest real local model on disk (tens of MB at minimum). It exists to
// classify a tag with NEITHER remote_host nor a ":cloud"/"-cloud" name — a
// daemon old enough to omit both — without being fooled by a manifest-sized
// cloud row into calling it local.
const ollamaLocalSizeFloor = 1 << 20 // 1 MiB

// ollamaTagsResponse is /api/tags' documented shape. It is intentionally a
// narrow slice of the real schema (digest/details/etc. carry nothing this
// package classifies on).
type ollamaTagsResponse struct {
	Models []struct {
		Name       string `json:"name"`
		RemoteHost string `json:"remote_host"`
		Size       int64  `json:"size"`
	} `json:"models"`
}

// classifyOllamaTag decides local vs cloud for a tag the daemon listed that the
// shipped catalog does not know. The distinction is load-bearing: a cloud model
// wrongly bound as local would be charged against the RAM gate and offered as
// free; a local model wrongly bound as cloud would lose the RAM protection
// meant for it. So this never guesses — it reads exactly the signals the
// listing itself provides, largest-to-weakest:
//
//  1. info.RemoteHost non-empty: Ollama's own /api/tags answer. Authoritative.
//  2. The tag's own naming convention (":cloud" or "-cloud"), the SAME rule
//     every ollama/*:cloud row in defaults/models.json already encodes — not a
//     guess, a documented product convention (e.g. "gpt-oss:120b-cloud").
//  3. An on-disk size at or above ollamaLocalSizeFloor: a real local weight
//     file is always tens of MB at minimum, while a cloud row's own "local"
//     footprint is just its manifest stub (verified: a few hundred bytes) — so
//     a large size is real local evidence and a tiny one is not.
//
// classified=false means NONE of the three fired — a daemon too old to set
// remote_host, reporting a non-"cloud"-suffixed tag with a manifest-sized (or
// absent) footprint — which is exactly the shape a cloud row would have on
// that daemon. Local is the WRONG default there: it is the direction that
// would present a metered call as free. So an unclassified tag is refused, not
// bound, and the caller must say so.
func classifyOllamaTag(tag string, info ollamaTagInfo) (cloud, classified bool) {
	switch {
	case tag == "":
		return false, false
	case info.RemoteHost != "":
		return true, true
	case strings.HasSuffix(tag, ":cloud") || strings.HasSuffix(tag, "-cloud"):
		return true, true
	case info.Size >= ollamaLocalSizeFloor:
		return false, true
	default:
		return false, false
	}
}

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

	// UnknownLocal / UnknownCloud are pulled tags with NO shipped catalog entry,
	// bound anyway (that absence is the bug this plan exists to fix), classified
	// local or cloud from the listing itself — never guessed. UnknownUnclassified
	// is a tag the listing could not classify either way: reported, never bound.
	// None of the three ever earns a routing intent (see routing.Resolve: an
	// unscored model is never a candidate) or a bestLocal/rung role — those stay
	// scoped to the catalog, exactly as before this pass existed.
	UnknownLocal        []string
	UnknownCloud        []string
	UnknownUnclassified []string
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

// ollamaListing calls the daemon's /api/tags on the RESOLVED endpoint —
// PREFERRED over parsing `ollama list` text, which carries none of the
// daemon's own remote/local metadata (no remote_host, no size), forcing every
// cloud/local decision onto a naming guess alone. It doubles as the daemon
// readiness probe, so "is Ollama up" has exactly one spelling. Bounded by
// ollamaTagsTimeout so a wedged or absent daemon can never hang setup — an
// unreachable daemon reports the SAME "could not list" error a text-parsing
// failure would, so a user who has simply not started Ollama yet sees the one
// message that tells them what to do, and nothing upstream (RequireOllamaReady,
// ConfigureOllamaInference) has to know the transport changed.
func ollamaListing(env hostenv.Env) (map[string]ollamaTagInfo, error) {
	endpoint := strings.TrimRight(inference.OllamaEndpointFor(env).URL, "/")
	req, err := http.NewRequest(http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	resp, err := (&http.Client{Timeout: ollamaTagsTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("could not list Ollama models (HTTP %d)", resp.StatusCode)
	}
	var body ollamaTagsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	seen := map[string]ollamaTagInfo{}
	for _, m := range body.Models {
		if m.Name != "" {
			seen[m.Name] = ollamaTagInfo{RemoteHost: m.RemoteHost, Size: m.Size}
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
		return fmt.Errorf("the ollama binary is installed but the daemon did not answer at /api/tags — start Ollama, then re-run")
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
	knownTags := map[string]bool{}
	for _, m := range reg.Models {
		if m.Provider == "ollama" {
			// Unconditional on m.Available: a retired row (e.g. kimi-k3:cloud, gated
			// to extra-usage-only) is a considered decision, and its catalog id is
			// exactly the synthetic id the unknown-tag pass below would otherwise
			// re-mint — excluding it here is what keeps that pass from quietly
			// reviving it through the back door.
			knownTags[inference.OllamaTagFor(m.ID)] = true
		}
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
			} else if _, ok := listed[inference.OllamaTagFor(m.ID)]; ok {
				bindOnce(m)
				plan.LocalBound = append(plan.LocalBound, m.ID)
				if m.MinRAMGB >= bestLocal.MinRAMGB {
					bestLocal = m
				}
			}
		case !m.Local && sel.Cloud:
			if _, ok := listed[inference.OllamaTagFor(m.ID)]; ok {
				bindOnce(m)
				plan.CloudBound = append(plan.CloudBound, m.ID)
			}
		}
	}

	// Second pass: every tag the daemon listed that the shipped catalog does not
	// know at all. THIS is the fix — without it, a user's `ollama pull` of
	// anything not already in models.json was installed, listed, and completely
	// invisible to Pix. Binding is NOT routing: bindOnce writes a candidate with
	// no scorecard entry, and routing.Resolve already skips a model with none
	// (see routing/overlord_fallback_test.go for the cost-0-wins-everything
	// precedent this must never repeat), so an unknown tag can be called by
	// name (`pix run --model ollama/<tag>`) but never wins an intent.
	tags := make([]string, 0, len(listed))
	for tag := range listed {
		if !knownTags[tag] {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	for _, tag := range tags {
		cloud, classified := classifyOllamaTag(tag, listed[tag])
		catalogID := "ollama/" + tag
		switch {
		case !classified:
			plan.UnknownUnclassified = append(plan.UnknownUnclassified, tag)
			fmt.Fprintf(out, "  %s was pulled but could not be classified local vs Ollama Cloud (no remote_host, no :cloud/-cloud tag, no on-disk size) — not bound\n", tag)
		case cloud && sel.Cloud:
			if !bound[catalogID] {
				bound[catalogID] = true
				bind(cfg, catalogID, "ollama", tag)
			}
			plan.UnknownCloud = append(plan.UnknownCloud, tag)
			fmt.Fprintf(out, "  bound %s as Ollama CLOUD (not in the shipped catalog yet; metered by your Ollama subscription, not counted against local RAM)\n", catalogID)
		case !cloud && sel.Local:
			if !bound[catalogID] {
				bound[catalogID] = true
				bind(cfg, catalogID, "ollama", tag)
			}
			plan.UnknownLocal = append(plan.UnknownLocal, tag)
			fmt.Fprintf(out, "  bound %s as LOCAL (not in the shipped catalog yet; free and private, bounded by this machine's RAM)\n", catalogID)
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
	case len(plan.LocalBound)+len(plan.UnknownLocal) == 0 && len(plan.CloudBound)+len(plan.UnknownCloud) == 0:
		// A selection that produced NOTHING must not be persisted: a backend with
		// no models is an inert half-state no later reconcile can widen out of.
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
		reasons = append(reasons, "cloud: /api/tags shows no cloud models — sign in with `ollama signin`, then re-run setup")
	}
	return "Ollama was selected but nothing is callable through it (" + strings.Join(reasons, "; ") + "). Nothing was saved; re-run `pix setup` and choose Ollama Cloud or an API key."
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
