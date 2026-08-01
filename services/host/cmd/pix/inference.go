package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/routing"
)

const directInferenceProbeTimeout = 8 * time.Second

// Ollama probe budgets. They are vars, not consts, for ONE reason: a hermetic
// test has to be able to shrink them to exercise the budget branch without
// sitting through a five-minute wall clock. Nothing else writes them.
var (
	// ollamaCloudProbeTimeout bounds a cloud probe: a pure network round trip
	// that holds no local resource.
	ollamaCloudProbeTimeout = 20 * time.Second
	// ollamaLocalProbeTimeout bounds ONE cold local load, with nothing queued
	// ahead of it because the local set is serialized.
	ollamaLocalProbeTimeout = 90 * time.Second
	// ollamaLocalProbeBudget is the TOTAL wall clock the serialized local set may
	// spend. Four pulled rungs at 90s each is a pathological box, not a setup a
	// user should sit through.
	ollamaLocalProbeBudget = 300 * time.Second
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

// setupChooseInference owns the single ordinary-user inference question. It
// is skipped when a pack or prior setup already supplied a backend. Ollama is
// shown only after both binary and daemon probes succeed.
func setupChooseInference(cfg *config.Config, env shellEnv, in io.Reader, out io.Writer, interactive bool) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	if len(cfg.Inference.Backends) > 0 {
		if inferenceNeedsOnePassword(cfg) {
			return false, nil
		}
		if err := enableDeclaredInferenceBindings(cfg); err != nil {
			return false, err
		}
		return true, nil
	}
	if !interactive {
		return false, nil
	}
	ollamaReady := false
	if env.lookPath != nil {
		if _, err := env.lookPath("ollama"); err == nil {
			_, timedOut, runErr := probeRun(env, "ollama", "list")
			ollamaReady = runErr == nil && !timedOut
		}
	}
	fmt.Fprintln(out, "How should Pix run models? (choose one or more, comma-separated)")
	fmt.Fprintln(out, "  1. API key (default)     Anthropic / OpenAI / Google keys, resolved from 1Password")
	if ollamaReady {
		fmt.Fprintln(out, "  2. Ollama local          models that run on this machine")
	}
	fmt.Fprintln(out, "  3. Custom gateway        an OpenAI-compatible endpoint you host")
	if ollamaReady {
		fmt.Fprintln(out, "  4. Ollama Cloud          large models on your ollama.com subscription")
		// A HINT, not entitlement. A `:cloud` row in the listing appears on every
		// signed-in machine and proves nothing about what the plan may call — that
		// inference is exactly how a gated model got bound and 401'd at call time.
		if n := listedCloudTagCount(env); n > 0 {
			fmt.Fprintf(out, "  (this machine lists %d cloud model(s); Pix proves which ones your plan can call)\n", n)
		}
	}
	fmt.Fprint(out, "Choose [1]: ")
	choice, ok := readSetupLine(in)
	if !ok {
		return false, fmt.Errorf("no inference choice; setup cannot continue")
	}
	if strings.TrimSpace(choice) == "" {
		return false, nil
	}
	selected := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(choice), func(r rune) bool { return r == ',' || r == ' ' }) {
		switch raw {
		case "1", "api":
			selected["api"] = true
		case "2", "ollama", "ollama-local", "local":
			selected["ollama"] = true
		case "3", "gateway":
			selected["gateway"] = true
		case "4", "ollama-cloud", "cloud":
			selected["ollama-cloud"] = true
		default:
			return false, fmt.Errorf("unknown inference choice %q", raw)
		}
	}
	if selected["ollama"] || selected["ollama-cloud"] {
		if !ollamaReady {
			return false, fmt.Errorf("Ollama is not installed and healthy, so it is not an available inference choice")
		}
		sel := ollamaSelection{Local: selected["ollama"], Cloud: selected["ollama-cloud"]}
		if _, err := configureOllamaInference(cfg, env, sel, out); err != nil {
			return false, err
		}
	}
	if selected["gateway"] {
		if _, err := configureCustomGateway(cfg, in, out); err != nil {
			return false, err
		}
	}
	// false means the caller must continue through the direct-key transaction.
	return !selected["api"], nil
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

// ollamaSelection is what the user chose in the inference prompt. Local and
// Cloud are separate answers because they are separate products: a `:cloud`
// row in `ollama list` shows up on every signed-in machine and says nothing
// about what this machine can RUN, and a local model says nothing about what
// the subscription may CALL.
type ollamaSelection struct{ Local, Cloud bool }

// ollamaPlan is what configureOllamaInference decided, for the caller to render
// and for the models step to act on. It contains no success claims: every
// binding it created is a CANDIDATE (Verified: false) until a probe says
// otherwise.
type ollamaPlan struct {
	Endpoint   string     // resolved via effectiveOllamaEndpoint
	LocalBound []string   // catalog ids bound as candidates from the listing
	CloudBound []string   // ditto, cloud
	WantPull   string     // the RAM-appropriate rung handed to setupLocalModels
	SkippedRAM []string   // catalog local ids this machine cannot run
	Memory     hostMemory // the reading that sized the offer
}

// ollamaListedModels returns the tags `ollama list` reports. This is a LISTING,
// the weakest possible signal: it proves a name was printed, not that the model
// runs here or that the account may call it.
func ollamaListedModels(env shellEnv) (map[string]bool, error) {
	out, timedOut, err := probeRun(env, "ollama", "list")
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
func listedCloudTagCount(env shellEnv) int {
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

// configureOllamaInference binds CANDIDATES, never verified models. It splits
// the catalog on Model.Local so "Ollama local" and "Ollama Cloud" are the two
// separate answers they are, gates local rungs on the memory this machine
// actually has, and — crucially — NEVER hard-fails a user who has not pulled
// anything yet. The old error ("Ollama is healthy but none of its installed
// models match the Pix catalog") propagated out of a fatal mutation step, so
// the MOST COMMON local flow had the worst outcome in the whole setup. The
// replacement writes the RAM-appropriate rung to cfg.OllamaBridgeModel and lets
// it flow through the models step's EXISTING consent — there is no second
// consent mechanism, and a bare --yes still downloads nothing.
func configureOllamaInference(cfg *config.Config, env shellEnv, sel ollamaSelection, out io.Writer) (ollamaPlan, error) {
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
	endpoint := strings.TrimRight(effectiveOllamaEndpoint(cfg, env).URL, "/")
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
			// A listing is not evidence. verifyOllamaInference earns Verified with a
			// bounded, model-specific request through the resolved endpoint.
			Model: m.ID, Backend: "ollama", Upstream: ollamaTagFor(m.ID), Available: true,
		})
	}

	var rung, bestLocal routing.Model
	rungOK := false
	if sel.Local {
		plan.Memory = probeHostMemory(env)
		rung, rungOK = chooseLocalRung(reg, plan.Memory)
		fmt.Fprintln(out, localRungOfferLine(plan.Memory, rung, rungOK))
	}

	for _, m := range reg.Models {
		if m.Provider != "ollama" || !m.Available {
			continue
		}
		tag := ollamaTagFor(m.ID)
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
		cfg.OllamaBridgeModel = ollamaTagFor(bestLocal.ID)
		return plan, nil
	}
	if sel.Local && rungOK {
		// The rung is the local model Pix will call and the tag the bridge exposes.
		// Writing it BEFORE consent is deliberate and safe: setupLocalModels reads
		// this key to build its readiness axes, so the tag must exist in config
		// before the step that asks about it — and naming a tag is a declared
		// intent, not a claim (Verified stays false, the binding is not callable,
		// and doctor's bridge row reports it missing).
		cfg.OllamaBridgeModel = ollamaTagFor(rung.ID)
		bind(rung)
		plan.WantPull = ollamaTagFor(rung.ID)
	}

	// An Ollama selection that produced NOTHING must not be persisted. Deleting
	// the old hard error left this function returning nil with an empty plan, so
	// the keys step reached cfg.Save() and wrote a backend with no models — and
	// the NEXT `pix setup` early-returns into enableDeclaredInferenceBindings
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
func emptyOllamaSelectionMessage(sel ollamaSelection, plan ollamaPlan) string {
	var reasons []string
	if sel.Local {
		switch {
		case !plan.Memory.OK:
			reasons = append(reasons, "local: could not size this machine, so no local model was offered")
		case plan.Memory.TotalGB < localFloorTotalGB:
			reasons = append(reasons, fmt.Sprintf("local: %.0f GB RAM is below the %d GB a local model needs here", plan.Memory.TotalGB, localFloorTotalGB))
		default:
			reasons = append(reasons, "local: no catalog model fits this machine's usable memory")
		}
	}
	if sel.Cloud {
		reasons = append(reasons, "cloud: `ollama list` shows no cloud models — sign in with `ollama signin`, then re-run setup")
	}
	return "Ollama was selected but nothing is callable through it (" + strings.Join(reasons, "; ") + "). Nothing was saved; re-run `pix setup` and choose Ollama Cloud or an API key."
}

// verifyOllamaInference earns Verified for ollama bindings with an actual
// model-specific request through the RESOLVED endpoint. Every binding is
// checked independently. CLOUD probes run concurrently (they are network round
// trips and hold no local resource). LOCAL probes are SERIALIZED and unload
// after themselves: two concurrent generates make Ollama co-load two sets of
// weights, which either exhausts the memory budget readiness_hardware.go just
// computed or serializes the loads anyway behind timers that started at
// dispatch — so the second reports a timeout it never got a turn to spend, and
// un-binds a model that works. Mirrors verifyDirectInference in structure, not
// in concurrency.
//
// The local set runs LARGEST RUNG FIRST under its own wall budget, and a probe
// is never STARTED unless its full timeout fits what remains: the budget can
// therefore never manufacture a timeout. A candidate the budget never reached
// is `not probed` — a THIRD state that is neither verified nor failed, excluded
// from attempted, and never rendered as a rejection.
//
// Deviation from the design's signature: it takes an io.Writer (the design's
// own output spec prints a live line per local probe, which cannot be done
// without one) and returns the notProbed set (the third state has to be
// observable to be assertable).
func verifyOllamaInference(cfg *config.Config, env shellEnv, out io.Writer) (attempted, verified int, failures, notProbed []string) {
	if cfg == nil || env.ollamaInferenceProbe == nil {
		return 0, 0, nil, nil
	}
	if out == nil {
		out = io.Discard
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return 0, 0, nil, nil
	}
	endpoint := strings.TrimRight(effectiveOllamaEndpoint(cfg, env).URL, "/")
	type candidate struct {
		index  int
		label  string
		tag    string
		numCtx int
		minRAM float64
	}
	var local, cloud []candidate
	for i := range cfg.Inference.Models {
		binding := &cfg.Inference.Models[i]
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || backend.Driver != "ollama" || !binding.Available || !inferenceBindingAllowed(cfg, *binding) {
			continue
		}
		if binding.Source != "" {
			// A pack's authority is the sandbox smoke test; a host probe must not
			// demote what it cannot faithfully replay.
			continue
		}
		// Demote first: a stale claim (including a pre-provenance listing-derived
		// one) must never survive a run that could not re-earn it.
		binding.Verified, binding.VerifiedBy, binding.VerifiedAt = false, "", ""
		c := candidate{index: i, label: binding.Model, tag: binding.Upstream}
		m, found := reg.Get(binding.Model)
		if found && m.Local {
			// num_ctx is the rung's DECLARED context budget, so the probe allocates
			// the same KV cache the RAM gate priced. A rung that cannot hold its own
			// declared context fails here, which is exactly when we want to find out.
			c.numCtx, c.minRAM = m.ContextWindow, m.MinRAMGB
			local = append(local, c)
			continue
		}
		cloud = append(cloud, c)
	}

	promote := func(index int) {
		cfg.Inference.Models[index].Verified = true
		cfg.Inference.Models[index].VerifiedBy = config.VerifiedByProbe
		cfg.Inference.Models[index].VerifiedAt = time.Now().UTC().Format(time.RFC3339)
		verified++
	}

	// Cloud: concurrent. Nothing local is held, so N probes cost one timeout.
	type result struct {
		index int
		label string
		err   error
	}
	results := make(chan result, len(cloud))
	for _, c := range cloud {
		attempted++
		go func(c candidate) {
			results <- result{index: c.index, label: c.label, err: env.ollamaInferenceProbe(endpoint, c.tag, 0, ollamaCloudProbeTimeout)}
		}(c)
	}

	// Local: strictly serial, largest rung first, each unloading after itself.
	sort.Slice(local, func(i, j int) bool {
		if local[i].minRAM != local[j].minRAM {
			return local[i].minRAM > local[j].minRAM
		}
		return local[i].label < local[j].label
	})
	if len(local) > 0 {
		fmt.Fprintf(out, "  verifying %d local ollama model(s), one at a time (each is loaded and unloaded) ...\n", len(local))
	}
	remaining := ollamaLocalProbeBudget
	for _, c := range local {
		if remaining < ollamaLocalProbeTimeout {
			// NOT a failure: this candidate never got a turn. Reporting it as broken
			// would let a budget un-bind a healthy model.
			notProbed = append(notProbed, c.label)
			fmt.Fprintf(out, "    %-14s not probed — %.0fs left of the %.0fs local budget, less than one probe's %.0fs\n",
				c.tag, remaining.Seconds(), ollamaLocalProbeBudget.Seconds(), ollamaLocalProbeTimeout.Seconds())
			continue
		}
		attempted++
		start := time.Now()
		err := env.ollamaInferenceProbe(endpoint, c.tag, c.numCtx, ollamaLocalProbeTimeout)
		elapsed := time.Since(start)
		if remaining -= elapsed; remaining < 0 {
			remaining = 0
		}
		if err != nil {
			failures = append(failures, c.label+": "+err.Error())
			fmt.Fprintf(out, "    %-14s failed (%.0fs): %v\n", c.tag, elapsed.Seconds(), err)
			continue
		}
		promote(c.index)
		fmt.Fprintf(out, "    %-14s ok (%.0fs)\n", c.tag, elapsed.Seconds())
	}

	for range cloud {
		res := <-results
		if res.err != nil {
			failures = append(failures, res.label+": "+res.err.Error())
			continue
		}
		promote(res.index)
	}
	sort.Strings(failures)
	sort.Strings(notProbed)
	return attempted, verified, failures, notProbed
}

// liveOllamaInferenceProbe posts ONE minimal generate to endpoint/api/generate.
// endpoint is ALWAYS supplied by effectiveOllamaEndpoint; this function never
// spells an address of its own (scripts/check-endpoint-literals.sh). No auth
// header: the local daemon owns any cloud credential and Pix stores none.
//
// keep_alive:0 is load-bearing, not tidiness — it tells the daemon to unload
// the model as soon as the response is written, so probe n+1 starts against a
// free memory budget instead of stacking on probe n's resident weights.
func liveOllamaInferenceProbe(endpoint, model string, numCtx int, timeout time.Duration) error {
	options := map[string]any{"num_predict": 8}
	if numCtx > 0 {
		options["num_ctx"] = numCtx
	}
	body, err := json.Marshal(map[string]any{
		"model": model, "prompt": "Reply OK", "stream": false, "keep_alive": 0, "options": options,
	})
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("probe unavailable")
	}
	defer resp.Body.Close()
	// Drained, never echoed: an Ollama error body can quote request content.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint rejected the request (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// configureModelRoster turns the broad set of backend bindings into the small,
// explicit catalog-model surface agents may use. The router continues to pick
// by intent, but it can never escape this roster. A mandatory pack is already
// an explicit policy decision and therefore skips the personal roster prompt.
func configureModelRoster(cfg *config.Config, in io.Reader, out io.Writer, interactive bool, requested string) error {
	return configureModelRosterFrom(cfg, in, out, interactive, requested, nil)
}

// boundNativeProviders is the set of providers that already had a native
// binding. Callers capture it BEFORE configureDirectInference mutates the
// bindings; that pre-mutation snapshot is the whole mechanism behind widening.
func boundNativeProviders(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	if cfg == nil {
		return out
	}
	for _, b := range cfg.Inference.Models {
		if cfg.Inference.Backends[b.Backend].Driver == "native" {
			out[b.Backend] = true
		}
	}
	return out
}

// configureModelRosterFrom is configureModelRoster with the pre-mutation
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
		if !b.Available || !inferenceBindingTopologyAllowed(cfg, b) {
			continue
		}
		if inferenceBindingCallable(cfg, b) {
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
	// (see reconcileDirectInference): verifyDirectInference only probes bindings
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

type runtimeInferenceManifest struct {
	Version  int                       `json:"version"`
	Backends map[string]runtimeBackend `json:"backends"`
	Models   []runtimeModel            `json:"models"`
}

type runtimeBackend struct {
	Driver   string `json:"driver"`
	Protocol string `json:"protocol,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Auth     string `json:"auth"`
	KeyEnv   string `json:"key_env,omitempty"`
}

type runtimeModel struct {
	ID               string  `json:"id"`
	CatalogModel     string  `json:"catalog_model"`
	Backend          string  `json:"backend"`
	Name             string  `json:"name"`
	ContextWindow    int     `json:"context_window,omitempty"`
	MaxTokens        int     `json:"max_tokens,omitempty"`
	Reasoning        bool    `json:"reasoning,omitempty"`
	AdaptiveThinking bool    `json:"adaptive_thinking,omitempty"`
	InputCost        float64 `json:"input_cost,omitempty"`
	OutputCost       float64 `json:"output_cost,omitempty"`
}

func routingBindings(cfg *config.Config) []routing.Binding {
	if cfg == nil {
		return nil
	}
	out := make([]routing.Binding, 0, len(cfg.Inference.Models))
	for _, b := range cfg.Inference.Models {
		if !inferenceBindingCallable(cfg, b) {
			continue
		}
		out = append(out, routing.Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: true})
	}
	return out
}

func inferenceBindingTopologyAllowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || b.Backend == cfg.Inference.ExclusiveBackend
}

func inferenceBindingAllowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if !inferenceBindingTopologyAllowed(cfg, b) {
		return false
	}
	// A mandatory pack owns the whole inference surface while active. Preserve
	// the personal roster underneath so detaching the pack restores it.
	if cfg.Inference.ExclusiveSource != "" || len(cfg.Inference.AllowedModels) == 0 {
		return true
	}
	for _, id := range cfg.Inference.AllowedModels {
		if id == b.Model {
			return true
		}
	}
	return false
}

func inferenceBackendAllowed(cfg *config.Config, b config.InferenceBackend, name string) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || name == cfg.Inference.ExclusiveBackend
}

func inferenceNeedsOnePassword(cfg *config.Config) bool {
	if cfg == nil || len(cfg.Inference.Backends) == 0 {
		return true // default setup path is a direct API key
	}
	for _, binding := range cfg.Inference.Models {
		// Availability is probe evidence, not topology. Setup must still require
		// 1Password for an allowed direct binding before that first probe has
		// promoted it; exclusivity alone decides whether a backend is dormant.
		if !inferenceBindingAllowed(cfg, binding) {
			continue
		}
		b, ok := cfg.Inference.Backends[binding.Backend]
		if ok && inferenceBackendAllowed(cfg, b, binding.Backend) && b.Auth == "1password" {
			return true
		}
	}
	return false
}

func configuredKeylessInference() bool {
	cfg, err := config.Load()
	return err == nil && len(cfg.Inference.Models) > 0 && !inferenceNeedsOnePassword(cfg)
}

// enableDeclaredInferenceBindings promotes pack-declared bindings into the
// create-time candidate set. The final sandbox smoke test remains the success
// authority because sbx-session authentication exists only on the sandbox data
// plane and cannot be faithfully replayed by a host HTTP probe.
func enableDeclaredInferenceBindings(cfg *config.Config) error {
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

// configureDirectInference derives native backend bindings from the provider
// refs setup just validated and reconciled. The model catalog remains the one
// source of model metadata; adding a key never copies a second model list into
// setup code.
func configureDirectInference(cfg *config.Config, providers []string) error {
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
				// verifyDirectInference earns Verified with a bounded live request.
				Model: m.ID, Backend: m.Provider, Upstream: m.ID, Available: true,
			})
		}
	}
	return nil
}

// verifyDirectInference earns Verified with an actual model-specific inference
// request. Every binding is independently checked; probes run concurrently so
// the wall-clock bound is one probe timeout rather than N timeouts. Resolved
// key bytes stay in process memory and are never included in errors or persisted.
func verifyDirectInference(cfg *config.Config, env shellEnv) (attempted, verified int, failures []string) {
	if cfg == nil || env.directInferenceProbe == nil {
		return 0, 0, nil
	}
	type candidate struct {
		index           int
		provider, model string
	}
	var candidates []candidate
	for i := range cfg.Inference.Models {
		binding := &cfg.Inference.Models[i]
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || backend.Auth != "1password" || !binding.Available || !inferenceBindingAllowed(cfg, *binding) {
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
		ref, ok := currentOpRef(env, backend.KeyEnv)
		if !ok {
			failures = append(failures, provider+": credential ref missing")
			keyOK[provider] = false
			continue
		}
		key, ok := opReadNonEmpty(env, ref)
		if !ok {
			failures = append(failures, provider+": credential could not be resolved")
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
		attempted++
		go func(c candidate, key string) {
			results <- result{index: c.index, label: cfg.Inference.Models[c.index].Model, err: env.directInferenceProbe(c.provider, c.model, key)}
		}(c, keys[c.provider])
	}
	for i := 0; i < attempted; i++ {
		res := <-results
		if res.err != nil {
			failures = append(failures, res.label+": "+res.err.Error())
			continue
		}
		cfg.Inference.Models[res.index].Verified = true
		// Provenance is written in the SAME assignment as the claim, and cleared
		// with it above, so it can never outlive what it describes.
		cfg.Inference.Models[res.index].VerifiedBy = config.VerifiedByProbe
		cfg.Inference.Models[res.index].VerifiedAt = time.Now().UTC().Format(time.RFC3339)
		verified++
	}
	sort.Strings(failures)
	return attempted, verified, failures
}

// liveDirectInferenceProbe makes a minimal generation request through the
// provider's public API. The client has a hard wall-clock timeout and response
// bodies are never echoed, preventing provider errors from accidentally
// reflecting credential material into setup output.
func liveDirectInferenceProbe(provider, model, key string) error {
	var endpoint string
	var body []byte
	headers := map[string]string{"Content-Type": "application/json"}
	switch provider {
	case "openai":
		endpoint = "https://api.openai.com/v1/responses"
		body, _ = json.Marshal(map[string]any{"model": model, "input": "Reply OK", "max_output_tokens": 16})
		headers["Authorization"] = "Bearer " + key
	case "anthropic":
		endpoint = "https://api.anthropic.com/v1/messages"
		body, _ = json.Marshal(map[string]any{"model": model, "max_tokens": 8, "messages": []map[string]string{{"role": "user", "content": "Reply OK"}}})
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	case "google":
		endpoint = "https://generativelanguage.googleapis.com/v1beta/models/" + url.PathEscape(model) + ":generateContent"
		body, _ = json.Marshal(map[string]any{"contents": []map[string]any{{"parts": []map[string]string{{"text": "Reply OK"}}}}, "generationConfig": map[string]int{"maxOutputTokens": 8}})
		headers["x-goog-api-key"] = key
	default:
		return fmt.Errorf("unsupported provider")
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: directInferenceProbeTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("probe unavailable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider rejected model request (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// bindingNeedsHostProof reports whether Pix CAN — and therefore MUST — prove
// this binding from the host before calling it callable. It replaces an inline
// `backend.Auth != "1password"` shortcut that made honest Ollama verification
// cosmetic: the ollama backend is written with Auth "none", so an ollama
// binding used to be callable regardless of Verified. That is the hole the
// gated-cloud-model incident came through.
//
// Pack-declared bindings are exempt ONLY where the exemption is earned: a
// pack's authority is the sandbox smoke test (see
// enableDeclaredInferenceBindings) because sbx-session auth cannot be
// faithfully replayed by a host HTTP probe.
//
// That reasoning does NOT extend to a pack's 1Password-backed native backend,
// which packs may legally declare. Host proof for those is not merely possible,
// it already happens: verifyDirectInference probes every 1password binding with
// no Source check, and demotes the ones that fail. Exempting them by source
// would let a binding whose probe was DISPATCHED AND REFUSED stay callable, and
// flow on into the compiled manifest, the sandbox kit, and doctor's "N callable
// model(s)" — a success word behind a failed probe. So the exemption is scoped
// to the auth Pix cannot verify from here.
func bindingNeedsHostProof(cfg *config.Config, b config.InferenceModelBinding) bool {
	backend, ok := cfg.Inference.Backends[b.Backend]
	if !ok {
		return false
	}
	if b.Source != "" && backend.Auth != "1password" {
		return false
	}
	return backend.Auth == "1password" || backend.Driver == "ollama"
}

func inferenceBindingCallable(cfg *config.Config, binding config.InferenceModelBinding) bool {
	if cfg == nil || !binding.Available || !inferenceBindingAllowed(cfg, binding) {
		return false
	}
	if _, ok := cfg.Inference.Backends[binding.Backend]; !ok {
		return false
	}
	return !bindingNeedsHostProof(cfg, binding) || binding.Verified
}

func boundRuntimeID(b routing.Binding) string {
	if routing.IsQualifiedID(b.UpstreamID) && strings.HasPrefix(b.UpstreamID, b.Backend+"/") {
		return b.UpstreamID
	}
	return b.Backend + "/" + b.UpstreamID
}

func compileInferenceRuntime(cfg *config.Config, now time.Time) (routing.CompiledRouting, runtimeInferenceManifest, error) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		return routing.CompiledRouting{}, runtimeInferenceManifest{}, err
	}
	sc, err := routing.LoadScorecard()
	if err != nil {
		return routing.CompiledRouting{}, runtimeInferenceManifest{}, err
	}
	pol, err := routing.LoadPolicy()
	if err != nil {
		return routing.CompiledRouting{}, runtimeInferenceManifest{}, err
	}
	bindings := routingBindings(cfg)
	filtered := routing.RegistryForBindings(reg, bindings, "")
	compiled := routing.MaterializeBindings(routing.Compile(filtered, sc, pol, now), bindings, "")
	manifest := runtimeInferenceManifest{Version: 1, Backends: map[string]runtimeBackend{}}
	for name, b := range cfg.Inference.Backends {
		if !inferenceBackendAllowed(cfg, b, name) {
			continue
		}
		manifest.Backends[name] = runtimeBackend{Driver: b.Driver, Protocol: b.Protocol, BaseURL: b.BaseURL, Auth: b.Auth, KeyEnv: b.KeyEnv}
	}
	for _, configured := range cfg.Inference.Models {
		if !inferenceBindingCallable(cfg, configured) {
			continue
		}
		b := routing.Binding{Model: configured.Model, Backend: configured.Backend, UpstreamID: configured.Upstream, Available: true}
		m, ok := reg.Get(b.Model)
		if !ok {
			continue
		}
		manifest.Models = append(manifest.Models, runtimeModel{
			ID: boundRuntimeID(b), CatalogModel: m.ID, Backend: b.Backend, Name: m.Label,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxOutputTokens,
			Reasoning: true, AdaptiveThinking: m.AdaptiveThinking,
			InputCost: m.InputPerMTok, OutputCost: m.OutputPerMTok,
		})
	}
	sort.Slice(manifest.Models, func(i, j int) bool { return manifest.Models[i].ID < manifest.Models[j].ID })
	return compiled, manifest, nil
}

// synthesizeInferenceKit creates a create-time mixin containing only generated
// public metadata. It carries no credential values. The extension reads the
// manifest; subagents read the compiled routing file beside it.
func synthesizeInferenceKit(cfg *config.Config) (string, error) {
	if cfg == nil || len(cfg.Inference.Models) == 0 {
		return "", nil
	}
	compiled, manifest, err := compileInferenceRuntime(cfg, time.Now())
	if err != nil {
		return "", err
	}
	if len(manifest.Models) == 0 {
		// A dead-end refusal is the failure mode here: the user is told what is
		// wrong and not what to type. The usual cause is weights that were never
		// pulled, so name the pull.
		return "", fmt.Errorf("inference is configured but no model binding passed its probe; pull a local model with `pix setup --pull-models`, or re-run `pix setup` to re-verify")
	}
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(state, "inference-kits")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "runtime-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	agentDir := filepath.Join(dir, "files", "home", ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return "", err
	}
	if err := routing.WriteCompiled(filepath.Join(agentDir, "routing.json"), compiled); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(agentDir, "inference.json"), append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	spec, err := inferenceKitSpec(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o600); err != nil {
		return "", err
	}
	complete = true
	return dir, nil
}

func inferenceKitSpec(cfg *config.Config) (string, error) {
	var hosts []string
	type credential struct{ service, name, domain, header, format string }
	var credentials []credential
	seenHost, seenCredential := map[string]bool{}, map[string]bool{}
	referenced := map[string]bool{}
	for _, binding := range cfg.Inference.Models {
		if inferenceBindingCallable(cfg, binding) {
			referenced[binding.Backend] = true
		}
	}
	for name, backend := range cfg.Inference.Backends {
		if !referenced[name] || !inferenceBackendAllowed(cfg, backend, name) || backend.Driver == "ollama" || backend.BaseURL == "" {
			continue
		}
		u, err := url.Parse(backend.BaseURL)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("backend %q has invalid base_url %q", name, backend.BaseURL)
		}
		host := u.Host
		if !strings.Contains(host, ":") && u.Scheme == "https" {
			host += ":443"
		}
		if !seenHost[host] {
			seenHost[host], hosts = true, append(hosts, host)
		}
		if backend.Auth == "sbx-session" {
			header, format := backend.CredentialHeader, backend.CredentialFormat
			if header == "" {
				header = "Authorization"
			}
			if format == "" {
				format = "Bearer %s"
			}
			key := backend.CredentialService + "\x00" + backend.KeyEnv + "\x00" + u.Hostname()
			if !seenCredential[key] {
				seenCredential[key] = true
				credentials = append(credentials, credential{backend.CredentialService, backend.KeyEnv, u.Hostname(), header, format})
			}
		}
	}
	sort.Strings(hosts)
	var b strings.Builder
	b.WriteString("schemaVersion: \"2\"\nkind: mixin\nname: pix-inference\n")
	if len(hosts) > 0 {
		b.WriteString("permissions:\n  network:\n    allow:\n")
		for _, host := range hosts {
			fmt.Fprintf(&b, "      - %s\n", strconv.Quote(host))
		}
	}
	if len(credentials) > 0 {
		b.WriteString("credentials:\n")
		for _, c := range credentials {
			fmt.Fprintf(&b, "  - service: %s\n    apiKey:\n      name: %s\n      proxyManaged: true\n      inject:\n        - domain: %s\n          header: %s\n          format: %s\n", strconv.Quote(c.service), strconv.Quote(c.name), strconv.Quote(c.domain), strconv.Quote(c.header), strconv.Quote(c.format))
		}
	}
	return b.String(), nil
}

// callableRuntimeModels is the exact create-time model surface. Keeping this
// list beside the generated manifest prevents the baked image's broad default
// cycle from advertising models that this machine cannot call.
func callableRuntimeModels(cfg *config.Config) ([]string, error) {
	if cfg == nil || len(cfg.Inference.Models) == 0 {
		return nil, nil
	}
	_, manifest, err := compileInferenceRuntime(cfg, time.Now())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(manifest.Models))
	for _, model := range manifest.Models {
		ids = append(ids, model.ID)
	}
	return ids, nil
}

func inferenceAllowsModel(cfg *config.Config, id string) bool {
	if cfg == nil || len(cfg.Inference.Models) == 0 || strings.TrimSpace(id) == "" {
		return true
	}
	models, err := callableRuntimeModels(cfg)
	if err != nil {
		return false
	}
	for _, candidate := range models {
		if candidate == id {
			return true
		}
	}
	return false
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

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// errInferenceExclusive is returned when a mandatory pack owns the whole
// inference surface. It is a REFUSAL, not a failure: adding a key would write
// bindings that the topology filter then silently drops, so reporting success
// would be a success word with nothing behind it.
var errInferenceExclusive = fmt.Errorf("a mandatory pack owns inference on this host")

// reconcileResult is what a reconcile actually did and proved.
type reconcileResult struct {
	Providers []string // every provider with a resolvable key, sorted
	Added     []string // providers that had no native binding before this run
	Attempted int
	Verified  int
	Failures  []string
}

// reconcileDirectInference turns the provider keys that exist on this host into
// callable model bindings. It is the sequence that used to live only inside
// setup's keys step, which is why adding a key any other way left it inert:
// `pix secret set` wrote the ref, and nothing ever rebuilt the bindings, probed
// them, or widened the roster.
//
// Order is load-bearing and matches the step it was extracted from:
//
//		capture prior providers -> bind -> verify -> save -> judge -> roster
//
//	  - The prior set is captured BEFORE binding, or widening cannot see what is
//	    new (see rosterSeenProviders).
//	  - cfg.Save() happens BEFORE the verified==0 verdict, so a partial success is
//	    never thrown away by the error path.
//	  - verified == 0 with something attempted is a hard error; verified > 0 with
//	    some failures is not.
func reconcileDirectInference(cfg *config.Config, env shellEnv, in io.Reader, out io.Writer, interactive bool, requestedModels string) (reconcileResult, error) {
	var res reconcileResult
	if cfg == nil {
		return res, fmt.Errorf("no config")
	}
	if cfg.Inference.ExclusiveSource != "" {
		return res, errInferenceExclusive
	}
	if out == nil {
		out = io.Discard
	}
	prior := boundNativeProviders(cfg)

	providers, err := hostModeProviderKeys(env)
	if err != nil {
		return res, fmt.Errorf("reading configured providers: %w", err)
	}
	res.Providers = providers
	for _, p := range providers {
		if !prior[p] {
			res.Added = append(res.Added, p)
		}
	}
	sort.Strings(res.Added)

	if err := configureDirectInference(cfg, providers); err != nil {
		return res, fmt.Errorf("configuring direct inference: %w", err)
	}
	// Widen BEFORE probing. verifyDirectInference only probes bindings the roster
	// allows (inferenceBindingAllowed), so on a config with a non-empty roster
	// the newly added provider would otherwise never be probed, never become
	// callable, and be pruned straight back out of the roster for not being
	// callable — the key stays inert and the command still reports success.
	widenRosterForNewProviders(cfg, prior)
	res.Attempted, res.Verified, res.Failures = verifyDirectInference(cfg, env)
	if err := cfg.Save(); err != nil {
		return res, err
	}
	if res.Verified == 0 && (res.Attempted > 0 || len(res.Failures) > 0) {
		detail := strings.Join(res.Failures, "; ")
		if detail == "" {
			detail = "no provider accepted a model-specific request"
		}
		return res, fmt.Errorf("provider keys resolved, but live inference verification failed: %s", detail)
	}
	if callable, _ := configuredInferenceSummary(cfg); callable > 0 || strings.TrimSpace(requestedModels) != "" {
		if err := configureModelRosterFrom(cfg, in, out, interactive, requestedModels, prior); err != nil {
			return res, fmt.Errorf("choosing models: %w", err)
		}
	}
	return res, cfg.Save()
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
		if seen[b.Backend] || !b.Available || containsString(cfg.Inference.AllowedModels, b.Model) {
			continue
		}
		if m, ok := reg.Get(b.Model); ok && m.Available {
			cfg.Inference.AllowedModels = append(cfg.Inference.AllowedModels, b.Model)
		}
	}
}
