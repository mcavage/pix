package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	fmt.Fprintln(out, "How should Pix run models? (choose one or more)")
	fmt.Fprintln(out, "  1. API key (default)")
	if ollamaReady {
		fmt.Fprintln(out, "  2. Ollama")
	}
	fmt.Fprintln(out, "  3. Custom gateway")
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
		case "2", "ollama":
			selected["ollama"] = true
		case "3", "gateway":
			selected["gateway"] = true
		default:
			return false, fmt.Errorf("unknown inference choice %q", raw)
		}
	}
	if selected["ollama"] {
		if !ollamaReady {
			return false, fmt.Errorf("Ollama is not installed and healthy, so it is not an available inference choice")
		}
		if _, err := configureOllamaInference(cfg, env); err != nil {
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

func configureOllamaInference(cfg *config.Config, env shellEnv) (bool, error) {
	out, timedOut, err := probeRun(env, "ollama", "list")
	if err != nil || timedOut {
		return false, fmt.Errorf("could not list Ollama models")
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return false, err
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
	if cfg.Inference.Backends == nil {
		cfg.Inference.Backends = map[string]config.InferenceBackend{}
	}
	endpoint := strings.TrimRight(effectiveOllamaEndpoint(cfg, env).URL, "/")
	cfg.Inference.Backends["ollama"] = config.InferenceBackend{Driver: "ollama", BaseURL: endpoint + "/v1", Auth: "none"}
	before := len(cfg.Inference.Models)
	for _, m := range reg.Models {
		if m.Provider != "ollama" || !m.Available {
			continue
		}
		upstream := strings.TrimPrefix(m.ID, "ollama/")
		if seen[upstream] {
			cfg.Inference.Models = append(cfg.Inference.Models, config.InferenceModelBinding{
				Model: m.ID, Backend: "ollama", Upstream: upstream, Available: true, Verified: true,
			})
		}
	}
	if len(cfg.Inference.Models) == before {
		return false, fmt.Errorf("Ollama is healthy but none of its installed models match the Pix catalog")
	}
	return true, nil
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
	ID            string  `json:"id"`
	CatalogModel  string  `json:"catalog_model"`
	Backend       string  `json:"backend"`
	Name          string  `json:"name"`
	ContextWindow int     `json:"context_window,omitempty"`
	MaxTokens     int     `json:"max_tokens,omitempty"`
	Reasoning     bool    `json:"reasoning,omitempty"`
	InputCost     float64 `json:"input_cost,omitempty"`
	OutputCost    float64 `json:"output_cost,omitempty"`
}

func routingBindings(cfg *config.Config) []routing.Binding {
	if cfg == nil {
		return nil
	}
	out := make([]routing.Binding, 0, len(cfg.Inference.Models))
	for _, b := range cfg.Inference.Models {
		if !inferenceBindingAllowed(cfg, b) {
			continue
		}
		out = append(out, routing.Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: b.Available})
	}
	return out
}

func inferenceBindingAllowed(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg.Inference.ExclusiveSource != "" {
		return b.Source == cfg.Inference.ExclusiveSource
	}
	return cfg.Inference.ExclusiveBackend == "" || b.Backend == cfg.Inference.ExclusiveBackend
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
		if !binding.Available || !inferenceBindingAllowed(cfg, binding) {
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
				// prove that the account is entitled to this particular model. The
				// first sandbox request is the honest live verification point.
				Model: m.ID, Backend: m.Provider, Upstream: m.ID, Available: true,
			})
		}
	}
	return nil
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
		if !configured.Available || !inferenceBindingAllowed(cfg, configured) {
			continue
		}
		b := routing.Binding{Model: configured.Model, Backend: configured.Backend, UpstreamID: configured.Upstream, Available: configured.Available}
		m, ok := reg.Get(b.Model)
		if !ok {
			continue
		}
		manifest.Models = append(manifest.Models, runtimeModel{
			ID: boundRuntimeID(b), CatalogModel: m.ID, Backend: b.Backend, Name: m.Label,
			Reasoning: true, InputCost: m.InputPerMTok, OutputCost: m.OutputPerMTok,
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
		return "", fmt.Errorf("inference is configured but no model binding passed its probe")
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
	return dir, nil
}

func inferenceKitSpec(cfg *config.Config) (string, error) {
	var hosts []string
	type credential struct{ service, name, domain, header, format string }
	var credentials []credential
	seenHost, seenCredential := map[string]bool{}, map[string]bool{}
	referenced := map[string]bool{}
	for _, binding := range cfg.Inference.Models {
		if binding.Available && inferenceBindingAllowed(cfg, binding) {
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
