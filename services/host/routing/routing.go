// Package routing is the model router: it turns a declared INTENT (a hard
// cost/latency/accuracy constraint) into a concrete model id, using a registry
// of callable models, their prices, and a scorecard of measured accuracy. It is
// deliberately dependency-light (JSON + stdlib only — no sqlite, no go-plugin)
// so BOTH the host binary and the tiny launcher can import it, exactly like the
// config package.
//
// Truth lives on the host: models.json (registry+price), scorecard.json
// (measured), policy.json (intents). The resolver runs here (Go, tested), and
// `route compile` writes the resolved intent->model map to routing.json, which
// the sandbox reads offline (no live host call that could hang a subagent). See
// docs/design/routing.md.
package routing

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed defaults/models.json defaults/scorecard.json defaults/policy.json
var defaults embed.FS

// Model is one callable model, its limits, and its economics. Adding a model to
// the stack is a single entry in models.json; everything downstream (resolver,
// runtime provider generation, compile) picks it up with no code change.
type Model struct {
	ID               string   `json:"id"`                // fully qualified provider/id
	Provider         string   `json:"provider"`          // "anthropic", "openai", "ollama", ...
	Family           string   `json:"family,omitempty"`  // claude, gpt, gemini, glm, ...
	Label            string   `json:"label"`             // human label
	ContextWindow    int      `json:"context_window"`    // maximum model context in tokens
	MaxOutputTokens  int      `json:"max_output_tokens"` // maximum generated tokens per response
	InputPerMTok     float64  `json:"input_per_mtok"`    // USD per 1M input tokens
	OutputPerMTok    float64  `json:"output_per_mtok"`
	Local            bool     `json:"local"`                       // Ollama/DMR: unmetered
	Available        bool     `json:"available"`                   // wired/callable in this stack now
	AdaptiveThinking bool     `json:"adaptive_thinking,omitempty"` // Anthropic adaptive-thinking request shape
	Aliases          []string `json:"aliases,omitempty"`
	Notes            string   `json:"notes,omitempty"`

	// MinRAMGB is the minimum USABLE RAM a local rung needs, priced as
	// ceil(DownloadGB*1.15 + ContextWindow*KVGBPerTok + 1). It is compared
	// against the probed USABLE budget (a fraction of total RAM), never against
	// total RAM, and it is a SETUP-TIME OFFER FILTER only: availability comes
	// from bindings, so `pix route show` on an unbound box still describes the
	// catalog truthfully.
	MinRAMGB float64 `json:"min_ram_gb,omitempty"`
	// DownloadGB is the on-disk weight size, for the honest size warning in the
	// pull prompt and as the auditable first term of MinRAMGB.
	DownloadGB float64 `json:"download_gb,omitempty"`
	// KVGBPerTok is the fp16 KV-cache cost per token — the term MinRAMGB was
	// computed with, carried so the gate is auditable rather than a magic
	// constant. ContextWindow does double duty as the rung's declared context
	// budget: what MinRAMGB was priced for, what the setup probe sends as
	// num_ctx, and what the bridge sends. One number, three readers.
	KVGBPerTok float64 `json:"kv_gb_per_token,omitempty"`
}

// FitsMemory reports whether m fits a USABLE-memory budget in GB. A model with
// no declared MinRAMGB never fits: an undeclared requirement is not a small one
// (a user's ~/.pix/routing/models.json override predating the gate degrades
// conservatively rather than offering an unsized model).
func (m Model) FitsMemory(usableGB float64) bool {
	if m.MinRAMGB <= 0 {
		return false
	}
	return m.MinRAMGB <= usableGB
}

// LocalRungs returns the registry's available local models, LARGEST FIRST
// (by MinRAMGB, id as the deterministic tiebreak). Both binaries share this
// ordering: setup offers the largest rung that fits, and the setup probe walks
// the same order so a budget that runs out never runs out on the model the
// roster will actually use.
func LocalRungs(reg *Registry) []Model {
	if reg == nil {
		return nil
	}
	var out []Model
	for _, m := range reg.Models {
		if m.Local && m.Available {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MinRAMGB != out[j].MinRAMGB {
			return out[i].MinRAMGB > out[j].MinRAMGB
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Binding is the availability boundary between the shipped catalog and a
// concrete inference backend. The catalog says what a model IS; a binding says
// where it can be called on this host. UpstreamID may differ from Model when a
// gateway uses private aliases.
type Binding struct {
	Model      string `json:"model"`
	Backend    string `json:"backend"`
	UpstreamID string `json:"upstream_id"`
	Available  bool   `json:"available"`
}

// RegistryForBindings returns a copy whose availability reflects successful
// backend probes. A catalog model without a proven binding is unavailable.
// When exclusiveBackend is set, bindings through every other backend are
// ignored; this is how a private pack can enforce an internal gateway without
// putting any private endpoint or model alias in public Pix.
func RegistryForBindings(catalog *Registry, bindings []Binding, exclusiveBackend string) *Registry {
	available := map[string]bool{}
	for _, b := range bindings {
		if !b.Available || (exclusiveBackend != "" && b.Backend != exclusiveBackend) {
			continue
		}
		available[b.Model] = true
	}
	out := &Registry{Models: make([]Model, len(catalog.Models))}
	copy(out.Models, catalog.Models)
	for i := range out.Models {
		out.Models[i].Available = out.Models[i].Available && available[out.Models[i].ID]
	}
	return out
}

// CostFor returns the USD cost of a run given input/output token counts.
func (m Model) CostFor(inTok, outTok int) float64 {
	if m.Local {
		return 0
	}
	return float64(inTok)/1e6*m.InputPerMTok + float64(outTok)/1e6*m.OutputPerMTok
}

// Registry is the set of callable models.
type Registry struct {
	Models []Model `json:"models"`
}

// Get returns the model with id (or one of its aliases) and whether it was found.
func (r *Registry) Get(id string) (Model, bool) {
	for _, m := range r.Models {
		if m.ID == id {
			return m, true
		}
		for _, a := range m.Aliases {
			if a == id {
				return m, true
			}
		}
	}
	return Model{}, false
}

// Score is one measured (or seeded) (model, task_type) data point.
type Score struct {
	Model        string  `json:"model"`
	TaskType     string  `json:"task_type"`
	Accuracy     float64 `json:"accuracy"`       // 0..1
	LatencyMsP50 float64 `json:"latency_ms_p50"` // median wall time, ms
	CostUSD      float64 `json:"cost_usd"`       // mean cost per task, USD
	N            int     `json:"n"`              // sample count
	Source       string  `json:"source"`         // "seed" | "eval"
	Updated      string  `json:"updated,omitempty"`
}

// Scorecard is the collection of scores, keyed logically by (model, task_type).
type Scorecard struct {
	Scores []Score `json:"scores"`
}

// Lookup returns the score for (model, taskType) and whether it exists.
func (s *Scorecard) Lookup(model, taskType string) (Score, bool) {
	for _, sc := range s.Scores {
		if sc.Model == model && sc.TaskType == taskType {
			return sc, true
		}
	}
	return Score{}, false
}

// Upsert inserts or replaces the score for its (model, task_type) key.
func (s *Scorecard) Upsert(in Score) {
	for i, sc := range s.Scores {
		if sc.Model == in.Model && sc.TaskType == in.TaskType {
			s.Scores[i] = in
			return
		}
	}
	s.Scores = append(s.Scores, in)
}

// Intent is a declared need: the hard-constraint form of "pick me a model". The
// resolver filters models by the ceilings/floor + provider allowlist, then
// optimizes Objective among the survivors.
type Intent struct {
	Name         string   `json:"name"`
	TaskType     string   `json:"task_type"`
	Objective    string   `json:"objective,omitempty"`      // accuracy|cost|latency|balanced (default accuracy)
	MaxCostUSD   float64  `json:"max_cost_usd,omitempty"`   // hard ceiling, 0 = none
	MaxLatencyMs float64  `json:"max_latency_ms,omitempty"` // hard ceiling, 0 = none
	MinAccuracy  float64  `json:"min_accuracy,omitempty"`   // floor, 0 = none
	Providers    []string `json:"providers,omitempty"`      // allowlist, empty = any
	Fallback     string   `json:"fallback,omitempty"`       // model id when nothing is feasible
	About        string   `json:"about,omitempty"`
}

// Policy is the set of intents plus a global fallback model.
type Policy struct {
	DefaultFallback string   `json:"default_fallback"`
	Intents         []Intent `json:"intents"`
}

// Intent returns the named intent and whether it exists.
func (p *Policy) Intent(name string) (Intent, bool) {
	for _, in := range p.Intents {
		if in.Name == name {
			return in, true
		}
	}
	return Intent{}, false
}

// IsQualifiedID reports whether id is a fully qualified provider/id with a
// non-empty provider and a non-empty model part. A bare or half-formed id
// ("haiku", "/x", "openai/") can resolve to a keyless provider and hang, so it
// is rejected everywhere it matters.
func IsQualifiedID(id string) bool {
	i := strings.IndexByte(id, '/')
	return i > 0 && i < len(id)-1
}

// validObjectives are the objectives the resolver understands. An unknown one
// silently degrades to balanced in rankBy; Validate rejects it up front so a
// typo ("accuarcy") is caught at compile time, not by mystery routing.
var validObjectives = map[string]bool{"accuracy": true, "cost": true, "latency": true, "balanced": true, "": true}

// ValidateRegistry checks the model catalog independently so every consumer,
// including runtime provider generation, gets complete and safe metadata.
func ValidateRegistry(reg *Registry) error {
	if len(reg.Models) == 0 {
		return fmt.Errorf("registry is empty")
	}
	ids := map[string]bool{}
	for _, m := range reg.Models {
		if !IsQualifiedID(m.ID) {
			return fmt.Errorf("model id %q is not fully qualified (provider/id)", m.ID)
		}
		if ids[m.ID] {
			return fmt.Errorf("duplicate model id %q", m.ID)
		}
		ids[m.ID] = true
		if m.InputPerMTok < 0 || m.OutputPerMTok < 0 {
			return fmt.Errorf("model %q has a negative price", m.ID)
		}
		if m.ContextWindow <= 0 {
			return fmt.Errorf("model %q has no positive context_window", m.ID)
		}
		if m.MaxOutputTokens <= 0 {
			return fmt.Errorf("model %q has no positive max_output_tokens", m.ID)
		}
		if m.MaxOutputTokens > m.ContextWindow {
			return fmt.Errorf("model %q max_output_tokens exceeds context_window", m.ID)
		}
	}
	return nil
}

// Validate checks the three truth sources for internal consistency BEFORE they
// are used to compile routes: catalog metadata and prices sane, scores in range
// and pointing at real models, intents using a known objective and a resolvable
// fallback, and a resolvable default fallback. Returns the first problem, or nil.
func Validate(reg *Registry, sc *Scorecard, pol *Policy) error {
	if err := ValidateRegistry(reg); err != nil {
		return err
	}
	for _, s := range sc.Scores {
		if _, ok := reg.Get(s.Model); !ok {
			return fmt.Errorf("scorecard references unknown model %q", s.Model)
		}
		if s.Accuracy < 0 || s.Accuracy > 1 {
			return fmt.Errorf("scorecard accuracy for %q/%q out of range: %v", s.Model, s.TaskType, s.Accuracy)
		}
	}
	if pol.DefaultFallback == "" {
		return fmt.Errorf("policy has no default_fallback")
	}
	if _, ok := reg.Get(pol.DefaultFallback); !ok {
		return fmt.Errorf("policy default_fallback %q not in registry", pol.DefaultFallback)
	}
	names := map[string]bool{}
	for _, in := range pol.Intents {
		if in.Name == "" {
			return fmt.Errorf("an intent has an empty name")
		}
		if names[in.Name] {
			return fmt.Errorf("duplicate intent %q", in.Name)
		}
		names[in.Name] = true
		if !validObjectives[in.Objective] {
			return fmt.Errorf("intent %q has unknown objective %q", in.Name, in.Objective)
		}
		if in.Fallback != "" {
			if _, ok := reg.Get(in.Fallback); !ok {
				return fmt.Errorf("intent %q fallback %q not in registry", in.Name, in.Fallback)
			}
		}
	}
	return nil
}

// ── paths ────────────────────────────────────────────────────────────────────

// Dir is the on-disk routing dir: $ROUTING_DIR, else
// $XDG_DATA_HOME/pix/routing, else ~/.local/share/pix/routing.
// Overrides for models/scorecard/policy live here; hand-edit scorecard.json here.
// (This mirrors config.DataDir()'s resolution but stays inlined so the routing
// subpackage keeps its dependency-light, config-free layering.)
func Dir() string {
	if d := strings.TrimSpace(os.Getenv("ROUTING_DIR")); d != "" {
		return d
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "pix", "routing")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "pix", "routing")
}

func ModelsPath() string    { return filepath.Join(Dir(), "models.json") }
func ScorecardPath() string { return filepath.Join(Dir(), "scorecard.json") }
func PolicyPath() string    { return filepath.Join(Dir(), "policy.json") }

// CompiledRoutingPath is the default output of `route compile`: the resolved
// intent->model map the sandbox reads. On the host it defaults under Dir();
// pass --out to target the repo's baked routing.json (baked via `make load`).
func CompiledRoutingPath() string { return filepath.Join(Dir(), "routing.json") }

// ── loaders (disk override, else embedded default) ───────────────────────────

func loadOrDefault(path, embedded string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		b, err = defaults.ReadFile(embedded)
		if err != nil {
			return err
		}
	}
	return json.Unmarshal(b, v)
}

// LoadRegistry reads models.json (disk override, else embedded default).
func LoadRegistry() (*Registry, error) {
	r := &Registry{}
	if err := loadOrDefault(ModelsPath(), "defaults/models.json", r); err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	if err := ValidateRegistry(r); err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	return r, nil
}

// LoadScorecard reads scorecard.json (disk override, else embedded seed).
func LoadScorecard() (*Scorecard, error) {
	s := &Scorecard{}
	if err := loadOrDefault(ScorecardPath(), "defaults/scorecard.json", s); err != nil {
		return nil, fmt.Errorf("load scorecard: %w", err)
	}
	return s, nil
}

// LoadPolicy reads policy.json (disk override, else embedded default).
func LoadPolicy() (*Policy, error) {
	p := &Policy{}
	if err := loadOrDefault(PolicyPath(), "defaults/policy.json", p); err != nil {
		return nil, fmt.Errorf("load policy: %w", err)
	}
	return p, nil
}

// SaveScorecard writes the scorecard back to disk (tests, and hand edits via
// tooling). Scores are sorted (task_type, then descending accuracy) for a
// stable, diffable file.
func (s *Scorecard) Save() error {
	sort.SliceStable(s.Scores, func(i, j int) bool {
		if s.Scores[i].TaskType != s.Scores[j].TaskType {
			return s.Scores[i].TaskType < s.Scores[j].TaskType
		}
		return s.Scores[i].Accuracy > s.Scores[j].Accuracy
	})
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ScorecardPath(), append(b, '\n'), 0o600)
}
