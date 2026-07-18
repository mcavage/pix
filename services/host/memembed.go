// Memory embeddings + watcher, both against the host's Ollama. Ports
// mcp/memory/embeddings.ts and watcher.ts.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func ollamaHost() string {
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		return v
	}
	return "http://127.0.0.1:11434"
}

// envMs parses a millisecond duration from an env var, defensively: an absent,
// empty, unparsable, or non-positive value falls back to def rather than ever
// disabling the timeout (a bare http.Post with no timeout is the bug this whole
// file guards against).
func envMs(key string, defMs int) time.Duration {
	ms := defMs
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			ms = p
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// embedTimeout bounds the recall-side /api/embed call (MEMORY_EMBED_TIMEOUT_MS,
// default 15s) — short, because a wedged Ollama should degrade recall to
// keyword search well within a turn, not hang it.
func embedTimeout() time.Duration { return envMs("MEMORY_EMBED_TIMEOUT_MS", 15000) }

// watcherTimeout bounds the capture-side /api/chat call (MEMORY_WATCHER_TIMEOUT_MS,
// default 90s) — generous enough for a slow cold local model load, but bounded:
// Ollama can accept the connection and never finish inference, and an
// unbounded http.Post there is exactly how captures silently pile up and vanish.
func watcherTimeout() time.Duration { return envMs("MEMORY_WATCHER_TIMEOUT_MS", 90000) }

// watcherBackoff is how long a real inference failure (timeout/non-200) keeps
// capture degraded, ignoring the (metadata-only, and therefore misleading)
// /api/show re-probe. See watcherCaptureAvailable().
const watcherBackoff = 60 * time.Second

// embedProbeInterval throttles the automatic re-probe of the embed endpoint
// after a failure, mirroring the watcher re-probe pattern. Once embedDisabled
// is latched, we re-attempt at most once per interval so a recovered Ollama
// restores semantic recall without a daemon restart.
const embedProbeInterval = 60 * time.Second

// embedLastProbe is the unix-nano time of the most recent embed re-probe
// attempt. Used to throttle recovery probes when embedDisabled is true.
var embedLastProbe atomic.Int64

// embedProbeClient is the HTTP client for lightweight Ollama model-presence
// probes (/api/show). A 10-second timeout prevents goroutine leaks when Ollama
// accepts the connection but never sends a response. It is a package-level
// variable so tests can substitute a mock server without forking the process.
var embedProbeClient = &http.Client{Timeout: 10 * time.Second}

var embedDisabled atomic.Bool

func memEmbed(text string) []float64 {
	if embedDisabled.Load() {
		// Re-probe: if the backoff interval has elapsed, tentatively clear the
		// flag and let the call through. On continued failure we re-latch below;
		// on success the flag stays clear and semantic recall is restored without
		// a daemon restart (H-3).
		now := time.Now().UnixNano()
		last := embedLastProbe.Load()
		if now-last < int64(embedProbeInterval) {
			return nil // still in backoff
		}
		if !embedLastProbe.CompareAndSwap(last, now) {
			return nil // another goroutine is already re-probing
		}
		embedDisabled.Store(false) // tentatively allow; re-latched below on failure
	}
	model := os.Getenv("MEMORY_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": text})
	client := &http.Client{Timeout: embedTimeout()}
	res, err := client.Post(ollamaHost()+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		if !embedDisabled.Swap(true) {
			// First time latching: log so users know why semantic recall degraded.
			log.Printf("memory embed: semantic recall DISABLED — embed model unavailable: %v; will retry automatically every %.0fs", err, embedProbeInterval.Seconds())
		}
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if !embedDisabled.Swap(true) {
			log.Printf("memory embed: semantic recall DISABLED — embed HTTP %d; will retry automatically every %.0fs", res.StatusCode, embedProbeInterval.Seconds())
		}
		return nil
	}
	// Success: if the flag was set (recovery path), announce the restoration.
	if embedDisabled.Swap(false) {
		log.Printf("memory embed: embed model available again — semantic recall RE-ENABLED")
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
	// Defaults to the same local model the ollama-bridge/router uses (qwen3.5:9b)
	// so Ollama keeps ONE model resident for both capture and local inference,
	// instead of a second watcher-only model. `pi-stack serve` normally passes the
	// resolved config value (config.DefaultMemoryWatcherModel); this fallback only
	// applies when the daemon runs with MEMORY_WATCHER_MODEL unset. Override for a
	// smaller model via MEMORY_WATCHER_MODEL / `pi-stack config set`.
	return "qwen3.5:9b"
}

// watcherUnavailable is set true once a capture attempt fails because the
// watcher model isn't reachable/pulled, so `observe` can tell the caller the
// truth instead of always claiming {accepted:true}. memOllamaHasModel() seeds it
// at startup; memWatch() keeps it current.
var watcherUnavailable atomic.Bool

// watcherReason is the current human-readable explanation for why capture is
// off (empty when it's on). Set alongside watcherUnavailable everywhere that
// flag is set, so observe()/health() can surface WHY, not just a bool.
var watcherReason atomic.Pointer[string]

// watcherDegradedUntil is the unix-nano deadline of a backoff started by a real
// memWatch() failure (timeout/transport error/non-200). While in this window,
// watcherCaptureAvailable() returns false WITHOUT re-probing /api/show — that
// probe only checks the model is present, which succeeds even while inference
// itself is wedged, so it must not be allowed to flip capture back to available
// mid-wedge.
var watcherDegradedUntil atomic.Int64

func setWatcherReason(s string) { watcherReason.Store(&s) }

func getWatcherReason() string {
	if p := watcherReason.Load(); p != nil {
		return *p
	}
	return ""
}

// isTimeoutErr reports whether err is a network timeout (what an http.Client
// with a Timeout returns once the deadline fires), as opposed to a plain
// connection error (Ollama not listening at all).
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// memOllamaHasModel asks Ollama whether a model is present locally (POST
// /api/show, no inference) — used by the startup probe and `make doctor` so a
// missing watcher model is loud, not a silent dropped capture. Uses
// embedProbeClient (10-second timeout) so a wedged Ollama cannot leak goroutines
// via an indefinite hang (H-2).
func memOllamaHasModel(model string) bool {
	body, _ := json.Marshal(map[string]any{"name": model})
	res, err := embedProbeClient.Post(ollamaHost()+"/api/show", "application/json", bytes.NewReader(body))
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
		setWatcherReason("")
		return
	}
	watcherUnavailable.Store(true)
	setWatcherReason(fmt.Sprintf("model %q is not pulled (or Ollama is down) — run `ollama pull %s`", m, m))
	log.Printf("memory watcher: model %q is not pulled (or Ollama is down) — fact capture is DISABLED until you run `ollama pull %s` (recall still works). Set MEMORY_WATCHER_MODEL to override.", m, m)
}

// watcherProbeInterval throttles the live re-probe so a disabled watcher does not
// hammer Ollama /api/show on every captured turn.
const watcherProbeInterval = 30 * time.Second

// watcherLastProbe is the unix-nano time of the last live re-probe.
var watcherLastProbe atomic.Int64

// watcherCaptureAvailable reports whether fact capture can run RIGHT NOW, and
// breaks the startup latch. memWatcherProbe() runs once at boot; if Ollama was
// down or the model unpulled then, watcherUnavailable stayed true forever —
// observe() short-circuited before ever calling the watcher, so it could never
// reset (memWatch only clears the flag on a successful run it never reached).
// Here, when marked unavailable, we re-probe at most once per interval; the
// moment the model appears (the user ran `ollama pull`) capture recovers with no
// daemon restart. Available is the common path and returns immediately.
func watcherCaptureAvailable() bool {
	if time.Now().UnixNano() < watcherDegradedUntil.Load() {
		// Backing off after a real inference failure — do NOT let the metadata-only
		// /api/show probe below flip this back to available mid-wedge (it lies:
		// /api/show succeeds instantly even while /api/chat is hung).
		return false
	}
	if !watcherUnavailable.Load() {
		return true
	}
	now := time.Now().UnixNano()
	last := watcherLastProbe.Load()
	if now-last < int64(watcherProbeInterval) {
		return false
	}
	// CAS so exactly one goroutine performs the (network) probe per interval.
	if !watcherLastProbe.CompareAndSwap(last, now) {
		return false
	}
	m := memWatcherModel()
	if memOllamaHasModel(m) {
		watcherUnavailable.Store(false)
		setWatcherReason("")
		log.Printf("memory watcher: model %q is now available — fact capture re-enabled.", m)
		return true
	}
	setWatcherReason(fmt.Sprintf("model %q is not pulled (or Ollama is down) — run `ollama pull %s`", m, m))
	return false
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
	client := &http.Client{Timeout: watcherTimeout()}
	res, err := client.Post(ollamaHost()+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		// Surface it: a silent return here is why the capture half can look "dead"
		// (Ollama down, MEMORY_WATCHER_MODEL not pulled, or — the observed live
		// failure mode — Ollama accepting the connection but never finishing
		// inference). Recall still works.
		watcherUnavailable.Store(true)
		var reason string
		if isTimeoutErr(err) {
			reason = fmt.Sprintf("Ollama not responding: watcher inference timed out after %ds (is ollama healthy? try restarting it)", int(watcherTimeout().Seconds()))
		} else {
			reason = fmt.Sprintf("Ollama /api/chat unreachable at %s: %v", ollamaHost(), err)
		}
		setWatcherReason(reason)
		watcherDegradedUntil.Store(time.Now().Add(watcherBackoff).UnixNano())
		log.Printf("memory watcher: %s — capture skipped", reason)
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		watcherUnavailable.Store(true)
		reason := fmt.Sprintf("Ollama /api/chat HTTP %d (is model %q pulled? `ollama pull %s`): %s",
			res.StatusCode, model, model, strings.TrimSpace(string(b)))
		setWatcherReason(reason)
		watcherDegradedUntil.Store(time.Now().Add(watcherBackoff).UnixNano())
		log.Printf("memory watcher: %s — capture skipped", reason)
		return nil
	}
	watcherUnavailable.Store(false)
	setWatcherReason("")
	watcherDegradedUntil.Store(0)
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
