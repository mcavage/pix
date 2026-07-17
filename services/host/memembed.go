// Memory embeddings + watcher, both against the host's Ollama. Ports
// mcp/memory/embeddings.ts and watcher.ts.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

func ollamaHost() string {
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		return v
	}
	return "http://127.0.0.1:11434"
}

var embedDisabled atomic.Bool

func memEmbed(text string) []float64 {
	if embedDisabled.Load() {
		return nil
	}
	model := os.Getenv("MEMORY_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": text})
	res, err := http.Post(ollamaHost()+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		embedDisabled.Store(true)
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		embedDisabled.Store(true)
		return nil
	}
	var parsed struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if json.NewDecoder(res.Body).Decode(&parsed) != nil {
		return nil
	}
	if len(parsed.Embeddings) == 0 || len(parsed.Embeddings[0]) == 0 {
		return nil
	}
	return parsed.Embeddings[0]
}

func memEmbedderAvailable() bool { return memEmbed("probe") != nil }

func memWatcherModel() string {
	if v := os.Getenv("MEMORY_WATCHER_MODEL"); v != "" {
		return v
	}
	// Small default: the watcher runs resident on the host, so a big model OOMs a
	// 16GB laptop. Bump via MEMORY_WATCHER_MODEL / `pi-stack config set`.
	return "gemma3:4b"
}

// watcherUnavailable is set true once a capture attempt fails because the
// watcher model isn't reachable/pulled, so `observe` can tell the caller the
// truth instead of always claiming {accepted:true}. memOllamaHasModel() seeds it
// at startup; memWatch() keeps it current.
var watcherUnavailable atomic.Bool

// memOllamaHasModel asks Ollama whether a model is present locally (POST
// /api/show, no inference) — used by the startup probe and `make doctor` so a
// missing watcher model is loud, not a silent dropped capture.
func memOllamaHasModel(model string) bool {
	body, _ := json.Marshal(map[string]any{"name": model})
	res, err := http.Post(ollamaHost()+"/api/show", "application/json", bytes.NewReader(body))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return res.StatusCode == 200
}

// memWatcherProbe runs once at memory startup: is the configured watcher model
// actually pulled? If not, log the exact fix and flip watcherUnavailable so the
// self-learning capture half fails loudly instead of silently.
func memWatcherProbe() {
	m := memWatcherModel()
	if memOllamaHasModel(m) {
		watcherUnavailable.Store(false)
		return
	}
	watcherUnavailable.Store(true)
	log.Printf("memory watcher: model %q is not pulled (or Ollama is down) — fact capture is DISABLED until you run `ollama pull %s` (recall still works). Set MEMORY_WATCHER_MODEL to override.", m, m)
}

// --- watcher ---------------------------------------------------------------

type watchResult struct {
	Facts       []string
	Events      []string
	Corrections []string
	Valence     float64
}

const memWatcherSystem = `You read ONE message a user sent to their coding agent and extract only what is worth remembering for future sessions, plus the user's sentiment. You see ONLY the user's message, never the agent's reply, so everything must come from what the user themselves said.

Be very conservative. Most messages contain nothing worth saving. Saving noise is worse than saving nothing.

Return JSON:
- "facts": DURABLE things, true until the user changes their mind: preferences, identity, conventions, how they like to work, settled decisions. Each self-contained and still useful months from now.
- "events": TIME-BOUND status that will go stale on its own: what they are doing right now, a current activity or transition ("migrating to X", "working on Y this week"), what is installed/pulled today, anything dated. Saved but short-lived.
- "corrections": the user telling the agent to stop doing something or do it differently ("don't X", "always Y"). Phrase each as a durable rule.
- "valence": -1 (frustrated) to 1 (pleased) reading the user's tone. 0 if neutral.

The fact-vs-event test: if a statement could become false without the user changing their mind (a status, a current task, an installed thing, a date), it is an EVENT, not a fact. When a sentence mixes both, split it: keep the durable half as a fact, the perishable half as an event.

Hard rules:
- Only what the USER asserts. A QUESTION states nothing ("which branch do I use?" => all empty).
- NEVER record mood or feelings; that is what valence is for.
- Acknowledgments ("thanks", "great", "cool") => all empty.
- Code, file names, and one-off task details are not worth saving.
- When in doubt, leave it out. Empty arrays are the common, correct answer.

Output only the JSON.`

func memWatch(user string) *watchResult {
	model := memWatcherModel()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"facts":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"events":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"corrections": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"valence":     map[string]any{"type": "number"},
		},
		"required": []string{"facts", "events", "corrections", "valence"},
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": false, "format": schema,
		"options": map[string]any{"temperature": 0},
		"messages": []map[string]any{
			{"role": "system", "content": memWatcherSystem},
			{"role": "user", "content": user},
		},
	})
	res, err := http.Post(ollamaHost()+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		// Surface it: a silent return here is why the capture half can look "dead"
		// (Ollama down, or MEMORY_WATCHER_MODEL not pulled). Recall still works.
		watcherUnavailable.Store(true)
		log.Printf("memory watcher: Ollama /api/chat unreachable at %s — capture skipped: %v", ollamaHost(), err)
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		watcherUnavailable.Store(true)
		log.Printf("memory watcher: Ollama /api/chat HTTP %d (is model %q pulled? `ollama pull %s`) — capture skipped: %s",
			res.StatusCode, model, model, strings.TrimSpace(string(b)))
		return nil
	}
	watcherUnavailable.Store(false)
	var chat struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.NewDecoder(res.Body).Decode(&chat) != nil || chat.Message.Content == "" {
		return nil
	}
	var p struct {
		Facts       []string `json:"facts"`
		Events      []string `json:"events"`
		Corrections []string `json:"corrections"`
		Valence     float64  `json:"valence"`
	}
	if json.Unmarshal([]byte(chat.Message.Content), &p) != nil {
		return nil
	}
	clean := func(in []string) []string {
		out := []string{}
		for _, s := range in {
			if t := strings.TrimSpace(s); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return &watchResult{
		Facts: clean(p.Facts), Events: clean(p.Events), Corrections: clean(p.Corrections),
		Valence: math.Max(-1, math.Min(1, p.Valence)),
	}
}
