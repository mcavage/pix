package inference

// catalog.go is the shipped model catalog: LITERAL model facts and nothing
// else. It answers "what models does Pix know about, and what are they" —
// identity, provider/family, human label, context and output limits, local vs
// cloud, request-shape capability, and whether the row is still offered.
//
// What it deliberately does NOT answer is "which one should I use". There is
// no price, no benchmark, no accuracy, no score, no policy, no intent, no
// ranking and no resolver here, because there is no router anymore: the model
// is chosen by the user (--model, [models].main, a pack's binding), and this
// file exists only so the rest of the stack can validate that choice and
// describe it truthfully. catalog_shape_test.go pins that by reflection over
// the struct AND over the shipped JSON keys, so the scored fields the deleted
// router carried (input_per_mtok, scorecard accuracy, prefer_providers) cannot
// come back one field at a time.
//
// Local HARDWARE facts (usable RAM, download size, KV cost per token) are NOT
// here either: they live exactly once, in hardware.go's rung table (E4.3),
// which is canonical for them.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
)

//go:embed catalog/models.json
var catalogDefaults embed.FS

// Model is one model the shipped catalog knows about. Adding a model to the
// stack is a single entry in catalog/models.json; every consumer (binding,
// validation, the runtime manifest) picks it up with no code change.
//
// Available means "still offered by this catalog", NOT "callable here":
// callability comes only from a probed backend binding (see Callable).
type Model struct {
	ID               string   `json:"id"`                          // fully qualified provider/id
	Provider         string   `json:"provider"`                    // "anthropic", "openai", "ollama", ...
	Family           string   `json:"family,omitempty"`            // claude, gpt, gemini, glm, ...
	Label            string   `json:"label"`                       // human label
	ContextWindow    int      `json:"context_window"`              // maximum model context in tokens
	MaxOutputTokens  int      `json:"max_output_tokens"`           // maximum generated tokens per response
	Local            bool     `json:"local"`                       // runs on this machine (Ollama/DMR)
	Available        bool     `json:"available"`                   // still offered by this catalog
	AdaptiveThinking bool     `json:"adaptive_thinking,omitempty"` // Anthropic adaptive-thinking request shape
	Aliases          []string `json:"aliases,omitempty"`
	Notes            string   `json:"notes,omitempty"`
}

// Catalog is the set of known models.
type Catalog struct {
	Models []Model `json:"models"`
}

// Get returns the model with id (or one of its aliases) and whether it was found.
func (c *Catalog) Get(id string) (Model, bool) {
	if c == nil {
		return Model{}, false
	}
	for _, m := range c.Models {
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

// CatalogForBindings returns a copy whose availability reflects successful
// backend probes. A catalog model without a proven binding is unavailable.
// When exclusiveBackend is set, bindings through every other backend are
// ignored; this is how a private pack can enforce an internal gateway without
// putting any private endpoint or model alias in public Pix.
func CatalogForBindings(catalog *Catalog, bindings []Binding, exclusiveBackend string) *Catalog {
	if catalog == nil {
		return nil
	}
	available := map[string]bool{}
	for _, b := range bindings {
		if !b.Available || (exclusiveBackend != "" && b.Backend != exclusiveBackend) {
			continue
		}
		available[b.Model] = true
	}
	out := &Catalog{Models: make([]Model, len(catalog.Models))}
	copy(out.Models, catalog.Models)
	for i := range out.Models {
		out.Models[i].Available = out.Models[i].Available && available[out.Models[i].ID]
	}
	return out
}

// IsQualifiedID reports whether id is a fully qualified provider/id with a
// non-empty provider and a non-empty model part. A bare or half-formed id
// ("haiku", "/x", "openai/") can resolve to a keyless provider and hang, so it
// is rejected everywhere it matters.
func IsQualifiedID(id string) bool {
	i := strings.IndexByte(id, '/')
	return i > 0 && i < len(id)-1
}

// ValidateCatalog checks the catalog is complete and internally consistent
// before anything is bound or described from it: qualified unique ids and
// positive, self-consistent limits.
func ValidateCatalog(c *Catalog) error {
	if c == nil || len(c.Models) == 0 {
		return fmt.Errorf("model catalog is empty")
	}
	ids := map[string]bool{}
	for _, m := range c.Models {
		if !IsQualifiedID(m.ID) {
			return fmt.Errorf("model id %q is not fully qualified (provider/id)", m.ID)
		}
		if ids[m.ID] {
			return fmt.Errorf("duplicate model id %q", m.ID)
		}
		ids[m.ID] = true
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

// CatalogPath is the optional on-disk override of the shipped catalog:
// $PIX_MODEL_CATALOG if set, else <data dir>/models.json. Absent means "use
// the catalog built into this binary", so a user creates it only to override.
func CatalogPath() string {
	if p := strings.TrimSpace(os.Getenv("PIX_MODEL_CATALOG")); p != "" {
		return p
	}
	dir, err := config.DataDir()
	if err != nil {
		return "" // no resolvable data dir: there is no override, only the shipped catalog
	}
	return filepath.Join(dir, "models.json")
}

// LoadCatalog reads the catalog: the on-disk override if one exists, else the
// shipped default embedded in this binary. Either way it is validated, so a
// hand-edited override fails loudly instead of silently describing a model
// wrong.
func LoadCatalog() (*Catalog, error) {
	var b []byte
	var err error
	if path := CatalogPath(); path != "" {
		b, err = os.ReadFile(path)
	} else {
		err = os.ErrNotExist
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("load model catalog: %w", err)
		}
		if b, err = catalogDefaults.ReadFile("catalog/models.json"); err != nil {
			return nil, fmt.Errorf("load model catalog: %w", err)
		}
	}
	c := &Catalog{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("load model catalog: %w", err)
	}
	if err := ValidateCatalog(c); err != nil {
		return nil, fmt.Errorf("load model catalog: %w", err)
	}
	return c, nil
}
