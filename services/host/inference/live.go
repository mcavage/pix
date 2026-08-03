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
	"pix/host/config"
	"pix/host/routing"
	"sort"
	"strconv"
	"strings"
	"time"
)

// live.go — the REAL provider probes: actually call the endpoint and report
// whether a (provider, model, key) triple answers. They lived in cmd/pix
// because doctor was the first caller; asking "does this model actually work"
// is the inference capability's own question.

// LiveOllamaInferenceProbe posts ONE minimal generate to endpoint/api/generate.
// endpoint is ALWAYS supplied by axis.EffectiveOllamaEndpoint; this function never
// spells an address of its own (scripts/check-endpoint-literals.sh). No auth
// header: the local daemon owns any cloud credential and Pix stores none.
//
// keep_alive:0 is load-bearing, not tidiness — it tells the daemon to unload
// the model as soon as the response is written, so probe n+1 starts against a
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

func InferenceNeedsOnePassword(cfg *config.Config) bool {
	if cfg == nil || len(cfg.Inference.Backends) == 0 {
		return true // default setup path is a direct API key
	}
	for _, binding := range cfg.Inference.Models {
		// Availability is probe evidence, not topology. Setup must still require
		// 1Password for an allowed direct binding before that first probe has
		// promoted it; exclusivity alone decides whether a backend is dormant.
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

// LiveDirectInferenceProbe makes a minimal generation request through the
// provider's public API. The client has a hard wall-clock timeout and response
// bodies are never echoed, preventing provider errors from accidentally
// reflecting credential material into setup output.
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

const directInferenceProbeTimeout = 8 * time.Second

func ConfiguredKeylessInference() bool {
	cfg, err := config.Load()
	return err == nil && len(cfg.Inference.Models) > 0 && !InferenceNeedsOnePassword(cfg)
}

// SynthesizeInferenceKit creates a create-time mixin containing only generated
// public metadata. It carries no credential values. The extension reads the
// manifest; subagents read the compiled routing file beside it.
func SynthesizeInferenceKit(cfg *config.Config) (string, error) {
	if cfg == nil || len(cfg.Inference.Models) == 0 {
		return "", nil
	}
	compiled, manifest, err := CompileInferenceRuntime(cfg, time.Now())
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
	if err := os.WriteFile(filepath.Join(agentDir, "json"), append(b, '\n'), 0o600); err != nil {
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

// CallableRuntimeModels is the exact create-time model surface. Keeping this
// list beside the generated manifest prevents the baked image's broad default
// cycle from advertising models that this machine cannot call.
func CallableRuntimeModels(cfg *config.Config) ([]string, error) {
	if cfg == nil || len(cfg.Inference.Models) == 0 {
		return nil, nil
	}
	_, manifest, err := CompileInferenceRuntime(cfg, time.Now())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(manifest.Models))
	for _, model := range manifest.Models {
		ids = append(ids, model.ID)
	}
	return ids, nil
}

func AllowsModel(cfg *config.Config, id string) bool {
	if cfg == nil || len(cfg.Inference.Models) == 0 || strings.TrimSpace(id) == "" {
		return true
	}
	models, err := CallableRuntimeModels(cfg)
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

func CompileInferenceRuntime(cfg *config.Config, now time.Time) (routing.CompiledRouting, runtimeInferenceManifest, error) {
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
	bindings := Bindings(cfg)
	filtered := routing.RegistryForBindings(reg, bindings, "")
	compiled := routing.MaterializeBindings(routing.Compile(filtered, sc, pol, now), bindings, "")
	manifest := runtimeInferenceManifest{Version: 1, Backends: map[string]runtimeBackend{}}
	for name, b := range cfg.Inference.Backends {
		if !BackendAllowed(cfg, b, name) {
			continue
		}
		manifest.Backends[name] = runtimeBackend{Driver: b.Driver, Protocol: b.Protocol, BaseURL: b.BaseURL, Auth: b.Auth, KeyEnv: b.KeyEnv}
	}
	for _, configured := range cfg.Inference.Models {
		if !Callable(cfg, configured) {
			continue
		}
		b := routing.Binding{Model: configured.Model, Backend: configured.Backend, UpstreamID: configured.Upstream, Available: true}
		m, ok := reg.Get(b.Model)
		if !ok {
			continue
		}
		manifest.Models = append(manifest.Models, runtimeModel{
			ID: RuntimeID(b), CatalogModel: m.ID, Backend: b.Backend, Name: m.Label,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxOutputTokens,
			Reasoning: true, AdaptiveThinking: m.AdaptiveThinking,
			InputCost: m.InputPerMTok, OutputCost: m.OutputPerMTok,
		})
	}
	sort.Slice(manifest.Models, func(i, j int) bool { return manifest.Models[i].ID < manifest.Models[j].ID })
	return compiled, manifest, nil
}

func InferenceKitSpec(cfg *config.Config) (string, error) {
	var hosts []string
	type credential struct{ service, name, domain, header, format string }
	var credentials []credential
	seenHost, seenCredential := map[string]bool{}, map[string]bool{}
	referenced := map[string]bool{}
	for _, binding := range cfg.Inference.Models {
		if Callable(cfg, binding) {
			referenced[binding.Backend] = true
		}
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
