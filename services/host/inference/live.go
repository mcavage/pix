package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"pix/host/config"
)

// live.go — the REAL provider probes (call the endpoint, report whether a
// (provider, model, key) triple answers) and the create-time artifacts built
// from what those probes proved.

const directInferenceProbeTimeout = 8 * time.Second

// inferenceManifestFilename names the generated manifest inside the mixin's
// agent dir. extensions/inference.ts and
// extensions/ollama-bridge.ts hardcode the same literal on the TS side;
// tests/inference-manifest-filename.test.mjs cross-checks all three so a
// rename on one side can never drift from the others silently.
const inferenceManifestFilename = "inference.json"

// LiveOllamaInferenceProbe posts ONE minimal generate. endpoint always comes
// from OllamaEndpointFor; this function never spells an address of its own. No
// auth header: the local daemon owns any cloud credential. keep_alive:0 is
// load-bearing — the daemon unloads on response, so probe n+1 starts against a
// free memory budget instead of stacking on probe n's resident weights.
func LiveOllamaInferenceProbe(endpoint, model string, numCtx int, timeout time.Duration) error {
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
	return postProbe(strings.TrimRight(endpoint, "/")+"/api/generate", body,
		map[string]string{"Content-Type": "application/json"}, timeout, "endpoint rejected the request")
}

// LiveDirectInferenceProbe makes a minimal generation request through the
// provider's public API.
func LiveDirectInferenceProbe(provider, model, key string) error {
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
	return postProbe(endpoint, body, headers, directInferenceProbeTimeout, "provider rejected model request")
}

// postProbe is the one HTTP shape both probes share: hard wall-clock timeout,
// body drained and NEVER echoed (an error body can quote request content),
// status is the whole verdict.
func postProbe(endpoint string, body []byte, headers map[string]string, timeout time.Duration, reject string) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("probe unavailable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s (HTTP %d)", reject, resp.StatusCode)
	}
	return nil
}

// InferenceNeedsOnePassword reports whether this host's inference depends on a
// 1Password credential. Availability is probe evidence, not topology: an
// ALLOWED direct binding still needs 1Password before its first probe.
func InferenceNeedsOnePassword(cfg *config.Config) bool {
	if cfg == nil || len(cfg.Inference.Backends) == 0 {
		return true // default setup path is a direct API key
	}
	for _, binding := range cfg.Inference.Models {
		if !Allowed(cfg, binding) {
			continue
		}
		b, ok := cfg.Inference.Backends[binding.Backend]
		if ok && BackendAllowed(cfg, b, binding.Backend) && b.Auth == "1password" {
			return true
		}
	}
	return false
}

// KeylessInference reports whether cfg reaches every model it allows
// WITHOUT a 1Password-held provider key. It takes the config from its
// caller rather than loading machine config.toml itself, because the config
// that decides this is per-RUN: `pix run` hands it the EFFECTIVE inference
// config — machine config merged with the selected environment's own
// [inference.*] declarations (workflow/launch.EffectiveInferenceConfig) —
// so an environment reaching its models through an sbx-session gateway is
// never asked for a personal API key it was never going to use.
func KeylessInference(cfg *config.Config) bool {
	return Configured(cfg) && !InferenceNeedsOnePassword(cfg)
}

// SynthesizeInferenceKit creates a create-time mixin containing only generated
// public metadata. It carries no credential values. The extension reads the
// manifest; there is no second generated file beside it to disagree with.
func SynthesizeInferenceKit(cfg *config.Config, roster RosterInput) (string, error) {
	if !Configured(cfg) {
		return "", nil
	}
	manifest, err := RuntimeManifest(cfg, roster)
	if err != nil {
		return "", err
	}
	if len(manifest.Models) == 0 {
		// Name the fix, not just the fault; the usual cause is unpulled weights.
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
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(agentDir, inferenceManifestFilename), append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	spec, err := InferenceKitSpec(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o600); err != nil {
		return "", err
	}
	complete = true
	return dir, nil
}

// CallableRuntimeModels is the exact create-time model surface and the list
// `pix run` passes as --models, so the image's broad default cycle can never
// advertise a model this machine cannot call.
func CallableRuntimeModels(cfg *config.Config) ([]string, error) {
	if !Configured(cfg) {
		return nil, nil
	}
	cat, err := LoadCatalog()
	if err != nil {
		return nil, err
	}
	models := manifestModels(cfg, cat)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func AllowsModel(cfg *config.Config, id string) bool {
	if !Configured(cfg) || strings.TrimSpace(id) == "" {
		return true
	}
	models, err := CallableRuntimeModels(cfg)
	if err != nil {
		return false
	}
	return slices.Contains(models, id)
}

// RuntimeManifest builds the create-time inference manifest: the backends this
// host may use and the models a probed binding makes callable, with the limits
// and request shape the catalog declares for each. It resolves nothing and
// ranks nothing — the model a session runs is the user's choice, checked
// against this set.
func RuntimeManifest(cfg *config.Config, roster RosterInput) (runtimeInferenceManifest, error) {
	cat, err := LoadCatalog()
	if err != nil {
		return runtimeInferenceManifest{}, err
	}
	manifest := runtimeInferenceManifest{Version: 1, Backends: map[string]runtimeBackend{}, Models: manifestModels(cfg, cat)}
	for name, b := range cfg.Inference.Backends {
		if !BackendAllowed(cfg, b, name) {
			continue
		}
		manifest.Backends[name] = runtimeBackend{Driver: b.Driver, Protocol: b.Protocol, BaseURL: b.BaseURL, Auth: b.Auth, KeyEnv: b.KeyEnv}
	}
	// roster.go's buildRoster validates against manifest.Models — the exact
	// set this manifest ships — never a separate resolution path; a
	// zero-value RosterInput (every caller not yet taught to resolve one)
	// builds no roster at all, so the additive field stays fully absent.
	r, err := buildRoster(roster, manifest.Models)
	if err != nil {
		return runtimeInferenceManifest{}, err
	}
	manifest.Roster = r
	return manifest, nil
}

// manifestModels is the ONE place a callable binding becomes runtime metadata,
// so --models and the manifest the bridge registers from can never disagree
// about which ids exist or how they are spelled. A binding whose catalog row
// is MISSING (an Ollama tag the user pulled that models.json has never heard
// of — see workflow/models.ConfigureOllamaInference's second pass) is still
// emitted, not dropped: dropping it here is exactly the dead-on-arrival bug —
// the binding would sit in config.toml forever, provably bound, and never
// reach CallableRuntimeModels or the bridge, so `pix run --model ollama/<tag>`
// fails at the AllowsModel gate no matter how the binding got there. There is
// no catalog row to draw a label or context window from, and inventing one
// would be a lie (a wrong number is worse than an absent one), so the entry
// carries only what IS known — id, backend, upstream name — and leaves the
// limits at their zero value; CatalogModel stays "" so this can
// never be mistaken for a catalog row downstream. The bridge's own fallback
// (extensions/ollama-bridge.ts modelsFromManifest: context/maxTokens default
// when unset) covers the gap on the runtime side.
// Critically, a synthesized entry stays synthesized: CatalogForBindings only
// ever flips Available on a catalog row that ALREADY exists by ID, so an id
// with no catalog row is never added to the catalog by being bound.
func manifestModels(cfg *config.Config, cat *Catalog) []runtimeModel {
	var out []runtimeModel
	for _, b := range Bindings(cfg) {
		m, ok := cat.Get(b.Model)
		if !ok {
			out = append(out, runtimeModel{ID: RuntimeID(b), Backend: b.Backend, Name: b.UpstreamID})
			continue
		}
		out = append(out, runtimeModel{
			ID: RuntimeID(b), CatalogModel: m.ID, Backend: b.Backend, Name: m.Label,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxOutputTokens,
			Reasoning: true, AdaptiveThinking: m.AdaptiveThinking,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// InferenceKitSpec is the create-time mixin: egress hosts for the callable
// backends, plus the proxy-managed credentials to inject into them.
//
// A credential's IDENTITY is service+name (that pair is the `credentials[]`
// entry the kit schema keys on): two backends sharing one identity must
// collapse into ONE entry, never two entries with the same identity (a
// duplicate identity is exactly the shape the upstream kit schema rejects).
// Within that one entry, each distinct (domain, header, format) the identity
// is actually used with becomes its own `inject[]` rule — same shape the
// hand-authored anthropic block in pi-kit/spec.yaml already uses for three
// domains sharing one header/format. So two backends with the same
// service+name but a different header, format, or domain must never silently
// drop one of them: both survive as separate inject rules under the shared
// entry. Identical (domain, header, format) rules dedupe to one. Ordering is
// sorted, never insertion order, so re-running against an unordered
// cfg.Inference.Backends map (Go map iteration is unordered) always emits
// byte-identical YAML.
func InferenceKitSpec(cfg *config.Config) (string, error) {
	type injectRule struct{ domain, header, format string }
	type credIdentity struct{ service, name string }
	var hosts []string
	seenHost := map[string]bool{}
	var credOrder []credIdentity
	seenIdentity := map[credIdentity]bool{}
	credRules := map[credIdentity]map[injectRule]bool{}
	referenced := map[string]bool{}
	for _, b := range Bindings(cfg) {
		referenced[b.Backend] = true
	}
	for name, backend := range cfg.Inference.Backends {
		if !referenced[name] || !BackendAllowed(cfg, backend, name) || backend.Driver == "ollama" || backend.BaseURL == "" {
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
		if backend.Auth != "sbx-session" {
			continue
		}
		// The pack loader (packinfo) rejects a sbx-session backend missing
		// either field before it ever reaches disk, but hand-edited
		// ~/.config/pix/config.toml never goes through that loader. Catch it
		// here too: an incomplete identity has nowhere legitimate to inject,
		// so fail loudly instead of emitting a kit the proxy can't wire.
		if strings.TrimSpace(backend.CredentialService) == "" || strings.TrimSpace(backend.KeyEnv) == "" {
			return "", fmt.Errorf("backend %q uses sbx-session auth but has no credential_service/key_env; set both, or switch auth to 1password or none", name)
		}
		header, format := backend.CredentialHeader, backend.CredentialFormat
		if header == "" {
			header = "Authorization"
		}
		if format == "" {
			format = "Bearer %s"
		}
		id := credIdentity{backend.CredentialService, backend.KeyEnv}
		if !seenIdentity[id] {
			seenIdentity[id] = true
			credOrder = append(credOrder, id)
			credRules[id] = map[injectRule]bool{}
		}
		credRules[id][injectRule{u.Hostname(), header, format}] = true
	}
	sort.Strings(hosts)
	sort.Slice(credOrder, func(i, j int) bool {
		if credOrder[i].service != credOrder[j].service {
			return credOrder[i].service < credOrder[j].service
		}
		return credOrder[i].name < credOrder[j].name
	})
	var b strings.Builder
	b.WriteString("schemaVersion: \"2\"\nkind: mixin\nname: pix-inference\n")
	if len(hosts) > 0 {
		b.WriteString("permissions:\n  network:\n    allow:\n")
		for _, host := range hosts {
			fmt.Fprintf(&b, "      - %s\n", strconv.Quote(host))
		}
	}
	if len(credOrder) > 0 {
		b.WriteString("credentials:\n")
		for _, id := range credOrder {
			rules := make([]injectRule, 0, len(credRules[id]))
			for r := range credRules[id] {
				rules = append(rules, r)
			}
			sort.Slice(rules, func(i, j int) bool {
				if rules[i].domain != rules[j].domain {
					return rules[i].domain < rules[j].domain
				}
				if rules[i].header != rules[j].header {
					return rules[i].header < rules[j].header
				}
				return rules[i].format < rules[j].format
			})
			fmt.Fprintf(&b, "  - service: %s\n    apiKey:\n      name: %s\n      proxyManaged: true\n      inject:\n", strconv.Quote(id.service), strconv.Quote(id.name))
			for _, r := range rules {
				fmt.Fprintf(&b, "        - domain: %s\n          header: %s\n          format: %s\n", strconv.Quote(r.domain), strconv.Quote(r.header), strconv.Quote(r.format))
			}
		}
	}
	return b.String(), nil
}

type runtimeInferenceManifest struct {
	Version  int                       `json:"version"`
	Backends map[string]runtimeBackend `json:"backends"`
	Models   []runtimeModel            `json:"models"`
	// Roster is additive: nil omits the key entirely (omitempty on the
	// pointer), so an existing v1 reader that only ever checked
	// version/backends/models sees byte-for-byte the same shape it always
	// has. See roster.go.
	Roster *runtimeRoster `json:"roster,omitempty"`
}

type runtimeBackend struct {
	Driver   string `json:"driver"`
	Protocol string `json:"protocol,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Auth     string `json:"auth"`
	KeyEnv   string `json:"key_env,omitempty"`
}

type runtimeModel struct {
	ID               string `json:"id"`
	CatalogModel     string `json:"catalog_model"`
	Backend          string `json:"backend"`
	Name             string `json:"name"`
	ContextWindow    int    `json:"context_window,omitempty"`
	MaxTokens        int    `json:"max_tokens,omitempty"`
	Reasoning        bool   `json:"reasoning,omitempty"`
	AdaptiveThinking bool   `json:"adaptive_thinking,omitempty"`
}
