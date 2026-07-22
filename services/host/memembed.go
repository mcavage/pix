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
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
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
// default 15s), short, because a wedged Ollama should degrade recall to
// keyword search well within a turn, not hang it.
func embedTimeout() time.Duration { return envMs("MEMORY_EMBED_TIMEOUT_MS", 15000) }

// watcherTimeout bounds the capture-side /api/chat call (MEMORY_WATCHER_TIMEOUT_MS,
// default 90s), generous enough for a slow cold local model load, but bounded:
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

// modelProbeClient is the HTTP client for lightweight Ollama model-presence
// probes (/api/show). A 10-second timeout prevents goroutine leaks when Ollama
// accepts the connection but never sends a response. It is a package-level
// variable so tests can substitute a mock server without forking the process.
var modelProbeClient = &http.Client{Timeout: 10 * time.Second}

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
			log.Printf("memory embed: semantic recall DISABLED, embed model unavailable: %v; will retry automatically every %.0fs", err, embedProbeInterval.Seconds())
		}
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if !embedDisabled.Swap(true) {
			log.Printf("memory embed: semantic recall DISABLED, embed HTTP %d; will retry automatically every %.0fs", res.StatusCode, embedProbeInterval.Seconds())
		}
		return nil
	}
	// Success: if the flag was set (recovery path), announce the restoration.
	if embedDisabled.Swap(false) {
		log.Printf("memory embed: embed model available again, semantic recall RE-ENABLED")
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
	// A small, extraction-grade model dedicated to capture. `pi-stack serve`
	// normally passes the resolved config value (config.DefaultMemoryWatcherModel);
	// this fallback only applies when the daemon runs with MEMORY_WATCHER_MODEL
	// unset. Keep in sync with config.DefaultMemoryWatcherModel.
	return "qwen3.5:9b"
}

// memWatcherWarm forces the watcher model resident in Ollama at startup so the
// FIRST real capture doesn't pay the cold-load latency that caused watcher
// timeouts. Best-effort, background, generous cold-load budget, and it does NOT
// touch the capture availability/degraded flags (warming is priming, not a
// readiness verdict, memWatch owns that). No-op if the model isn't pulled.
func memWatcherWarm() {
	m := memWatcherModel()
	if !memOllamaHasModel(m) {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"model": m, "stream": false,
		"keep_alive": "10m", // stay resident so the first real capture isn't a cold reload
		"messages":   []map[string]any{{"role": "user", "content": "ok"}},
		"options":    map[string]any{"num_predict": 1},
	})
	client := &http.Client{Timeout: 5 * time.Minute} // generous: a cold load can be slow
	res, err := client.Post(ollamaHost()+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	log.Printf("memory watcher: warmed model %q", m)
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
// watcherCaptureAvailable() returns false WITHOUT re-probing /api/show, that
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
// /api/show, no inference), used by the startup probe and `make doctor` so a
// missing watcher model is loud, not a silent dropped capture. Uses
// modelProbeClient (10-second timeout) so a wedged Ollama cannot leak goroutines
// via an indefinite hang (H-2).
func memOllamaHasModel(model string) bool {
	body, _ := json.Marshal(map[string]any{"name": model})
	res, err := modelProbeClient.Post(ollamaHost()+"/api/show", "application/json", bytes.NewReader(body))
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
	setWatcherReason(fmt.Sprintf("model %q is not pulled (or Ollama is down), run `ollama pull %s`", m, m))
	log.Printf("memory watcher: model %q is not pulled (or Ollama is down), fact capture is DISABLED until you run `ollama pull %s` (recall still works). Set MEMORY_WATCHER_MODEL to override.", m, m)
}

// watcherProbeInterval throttles the live re-probe so a disabled watcher does not
// hammer Ollama /api/show on every captured turn.
const watcherProbeInterval = 30 * time.Second

// watcherLastProbe is the unix-nano time of the last live re-probe.
var watcherLastProbe atomic.Int64

// watcherCaptureAvailable reports whether fact capture can run RIGHT NOW, and
// breaks the startup latch. memWatcherProbe() runs once at boot; if Ollama was
// down or the model unpulled then, watcherUnavailable stayed true forever -
// observe() short-circuited before ever calling the watcher, so it could never
// reset (memWatch only clears the flag on a successful run it never reached).
// Here, when marked unavailable, we re-probe at most once per interval; the
// moment the model appears (the user ran `ollama pull`) capture recovers with no
// daemon restart. Available is the common path and returns immediately.
func watcherCaptureAvailable() bool {
	if time.Now().UnixNano() < watcherDegradedUntil.Load() {
		// Backing off after a real inference failure, do NOT let the metadata-only
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
		log.Printf("memory watcher: model %q is now available, fact capture re-enabled.", m)
		return true
	}
	setWatcherReason(fmt.Sprintf("model %q is not pulled (or Ollama is down), run `ollama pull %s`", m, m))
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
- NEVER infer preferences, interests, or tool usage from a question. Asking ABOUT a capability is not evidence the user has it, wants it, or is using it. A question about memory itself, in particular, asserts nothing about the user, never record "user is interested in memory" or "user is using memory" from it. Examples that must produce all-empty output: "so are you using my memories?", "why do we think those are the right things to inject?", "can I see what you remember?", "is memory working right now?", "how does recall pick what to show me?".
- NEVER record mood or feelings; that is what valence is for.
- Acknowledgments ("thanks", "great", "cool") => all empty.
- Code, file names, and one-off task details are not worth saving.
- When in doubt, leave it out. Empty arrays are the common, correct answer.
- "facts", "events", and "corrections" are always JSON arrays of strings, never objects, even when empty. An empty list is [], not {}.

Output only the JSON, compact, no prose, no code fence. Exact shape:
{"facts":[],"events":[],"corrections":[],"valence":0}`

// fencedCodeBlockRE matches a ``` ... ``` fenced code block (any language tag,
// any content, including newlines) so questionOnlyUserMessage can ignore code
// entirely rather than mis-splitting it into "sentences".
var fencedCodeBlockRE = regexp.MustCompile("(?s)```.*?```")

// hasWordChar reports whether s contains at least one letter or digit, used
// to tell a real clause ("i like cheese") apart from a stray fragment left
// over from splitting on punctuation (an empty string, or bare punctuation).
func hasWordChar(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// splitSentenceClauses splits s into sentence-ish clauses on runs of
// terminal punctuation (. ! ?), keeping the punctuation attached to the
// clause it ends. A run of punctuation ("...?", "?!") collapses to its LAST
// mark, so "explain this...?" still reads as a question. Any trailing text
// with no terminal punctuation becomes one final clause.
//
// A punctuation run only TERMINATES a clause when it is followed by
// whitespace or the end of the string. An inline mark immediately followed by
// more non-space text, "main.go", "v1.2", "example.com/path", is not
// sentence punctuation; it is absorbed into the running clause instead of
// splitting it, so a question that happens to contain a filename, a version
// number, or a URL isn't chopped into a bogus non-question fragment.
func splitSentenceClauses(s string) []string {
	var clauses []string
	var buf strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); {
		c := runes[i]
		buf.WriteRune(c)
		if c == '.' || c == '!' || c == '?' {
			last := c
			j := i + 1
			for j < len(runes) && (runes[j] == '.' || runes[j] == '!' || runes[j] == '?') {
				last = runes[j]
				j++
			}
			if j >= len(runes) || unicode.IsSpace(runes[j]) {
				text := strings.TrimRight(buf.String(), ".!?")
				clauses = append(clauses, text+string(last))
				buf.Reset()
				i = j
				continue
			}
			// Not a terminator: keep the whole punctuation run as ordinary text
			// in the current clause and keep going.
			for k := i + 1; k < j; k++ {
				buf.WriteRune(runes[k])
			}
			i = j
			continue
		}
		i++
	}
	if strings.TrimSpace(buf.String()) != "" {
		clauses = append(clauses, buf.String())
	}
	return clauses
}

// trailingWrapperChars are closing/markdown wrapper characters that can
// legitimately follow a question's terminal "?" without changing its
// meaning: closing quotes (straight and curly), closing parens/brackets/
// braces, markdown emphasis (`*`, `_`), and inline-code backticks. Trimmed
// (TrimRight handles a whole run, e.g. the two asterisks in "**...?**") before
// checking whether a clause is a question, so a message that is nothing but
// a wrapped question, `"Are you using my memories?"`, `**Are you using my
// memories?**`, `(Are you using my memories?)`, still reads as
// question-only instead of a false-negative fact extraction.
const trailingWrapperChars = "\"'\u201d\u2019)]}*_`"

// questionOnlyUserMessage reports whether a user message is, in substance,
// nothing but question(s), no assertions worth extracting as a fact. It
// strips fenced code (which isn't prose and shouldn't be sentence-split), then
// requires every non-trivial clause (one with an actual word or digit in it,
// after trimming trailing wrapper punctuation) to end in "?", and that there
// is at least one such clause.
//
// This gates FACT extraction only (see memCapture), a correction phrased as
// a polite question ("Can you stop using em dashes?") must still be
// capturable, so callers must never apply this to corrections.
func questionOnlyUserMessage(user string) bool {
	stripped := fencedCodeBlockRE.ReplaceAllString(user, "")
	nontrivial := 0
	for _, clause := range splitSentenceClauses(stripped) {
		trimmed := strings.TrimSpace(clause)
		trimmed = strings.TrimSpace(strings.TrimRight(trimmed, trailingWrapperChars))
		if !hasWordChar(trimmed) {
			continue
		}
		nontrivial++
		if !strings.HasSuffix(trimmed, "?") {
			return false
		}
	}
	return nontrivial > 0
}

// watcherNoisePrefixes are conservative, observed session-narration openers
// the watcher model sometimes emits in place of a real fact/event, text
// describing what happened in THIS session rather than a durable thing worth
// recalling later. Prefix-only (not substring) so a legitimate fact that
// happens to mention "the user" mid-sentence is never dropped.
//
// Deliberately excludes anything about what the user "expects": "the user
// expects tests on every PR" is a real, durable expectation/preference, not
// narration, so no "expects"/"expected" prefix belongs on this list.
var watcherNoisePrefixes = []string{
	"user requested", "the user requested",
	"user asked", "the user asked",
	"user is interested", "the user is interested",
	"user wants to know", "the user wants to know",
	"user ran", "the user ran",
	"user executed", "the user executed",
}

// watcherNoise reports whether content opens with one of watcherNoisePrefixes
// (case-insensitive, leading whitespace ignored), a session-narration
// artifact rather than a real fact/event. Callers apply it to the fact and
// event channels ONLY, see memCapture, never to corrections, since a
// legitimate correction can be phrased exactly like these openers ("The user
// requested the agent stop doing X").
func watcherNoise(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	for _, p := range watcherNoisePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// extractJSONObject returns the first balanced {...} JSON object substring in s,
// or "" if none, salvaging the object from a model that wraps it in prose or a
// ```json fence instead of returning bare JSON.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// flexibleStringList decodes a watcher list field (facts/events/corrections)
// that is supposed to be a JSON array of strings, but some models (observed:
// qwen) emit an empty object {} instead of [] when the list is empty. Accept:
// a string array, null/absent (=> empty), or an empty object (=> empty).
// Reject a non-empty object and any other shape (string, number, bool, array
// of non-strings) as an error so the caller can fall back or give up.
func flexibleStringList(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return []string{}, nil
	}
	var arr []string
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		return arr, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		if len(obj) == 0 {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list field is a non-empty object: %s", trimmed)
	}
	return nil, fmt.Errorf("list field has unexpected shape: %s", trimmed)
}

// parseWatchJSON decodes one candidate JSON object (either the model's raw
// content, or content salvaged by extractJSONObject) into a watchResult,
// tolerating the {} vs [] wrinkle in flexibleStringList for each list field.
//
// The top level is deliberately strict: a watcher model that returns `null`,
// `{}`, an array/string/number/bool, or an object missing one of the four
// required keys (facts/events/corrections/valence) is a malformed response,
// not "nothing captured", it must error so the caller falls back/salvages
// instead of silently recording an empty capture. Only the per-field list
// shape (facts/events/corrections) tolerates the {} vs [] vs null wrinkle,
// via flexibleStringList.
func parseWatchJSON(s string) (*watchResult, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &top); err != nil {
		return nil, fmt.Errorf("top-level value is not a JSON object: %w", err)
	}
	if top == nil {
		// json.Unmarshal("null", &map) succeeds with a nil map and no error -
		// must be rejected explicitly, not treated as an all-empty capture.
		return nil, fmt.Errorf("top-level value is null, not a JSON object")
	}
	for _, key := range []string{"facts", "events", "corrections", "valence"} {
		if _, ok := top[key]; !ok {
			return nil, fmt.Errorf("missing required key %q", key)
		}
	}
	facts, err := flexibleStringList(top["facts"])
	if err != nil {
		return nil, fmt.Errorf("facts: %w", err)
	}
	events, err := flexibleStringList(top["events"])
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	corrections, err := flexibleStringList(top["corrections"])
	if err != nil {
		return nil, fmt.Errorf("corrections: %w", err)
	}
	var valence float64
	if err := json.Unmarshal(top["valence"], &valence); err != nil {
		return nil, fmt.Errorf("valence: %w", err)
	}
	return &watchResult{
		Facts: facts, Events: events, Corrections: corrections,
		Valence: math.Max(-1, math.Min(1, valence)),
	}, nil
}

func parseWatchContent(content string) (*watchResult, error) {
	p, err := parseWatchJSON(content)
	// Salvage only a non-JSON envelope (prose or a code fence). If the model
	// returned valid JSON with the wrong top-level shape, such as [{...}], keep
	// that error instead of accepting a nested object and bypassing validation.
	if err != nil && !json.Valid([]byte(content)) {
		if salvaged := extractJSONObject(content); salvaged != "" {
			return parseWatchJSON(salvaged)
		}
	}
	return p, err
}

func memWatch(user string) *watchResult {
	model := memWatcherModel()
	// NO constrained `format` schema. ollama's grammar-constrained decode hung the
	// watcher (90s timeouts on qwen3.5:9b even for a 13-char input; nil on
	// gemma4:e4b-mlx). The system prompt already fully specifies the JSON shape
	// (facts/events/corrections/valence) and says "Output only the JSON", and we
	// parse it leniently (extractJSONObject). Unconstrained decode is fast and works
	// across models. num_predict caps a runaway; keep_alive avoids cold reloads
	// between captures (a cold 9B reload was part of the 90s).
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": false,
		"keep_alive": "10m",
		"think":      false, // qwen3.x et al default to thinking; the reasoning ate the
		// num_predict budget and left content empty. We want the JSON answer directly.
		"options": map[string]any{"temperature": 0, "num_predict": 1024},
		"messages": []map[string]any{
			{"role": "system", "content": memWatcherSystem},
			{"role": "user", "content": user},
		},
	})
	client := &http.Client{Timeout: watcherTimeout()}
	res, err := client.Post(ollamaHost()+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		// Surface it: a silent return here is why the capture half can look "dead"
		// (Ollama down, MEMORY_WATCHER_MODEL not pulled, or, the observed live
		// failure mode, Ollama accepting the connection but never finishing
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
		log.Printf("memory watcher: %s, capture skipped", reason)
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
		log.Printf("memory watcher: %s, capture skipped", reason)
		return nil
	}
	watcherUnavailable.Store(false)
	setWatcherReason("")
	watcherDegradedUntil.Store(0)
	rawBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var chat struct {
		Message struct {
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
	}
	if json.Unmarshal(rawBody, &chat) != nil || chat.Message.Content == "" {
		raw := string(rawBody)
		if len(raw) > 400 {
			raw = raw[:400] + "..."
		}
		log.Printf("memory watcher: empty/undecodable chat response from model %q, raw: %q", model, raw)
		return nil
	}
	// Primary: the whole content is the JSON object (structured-output models).
	// Fallback: some models wrap it in prose or a ```json fence, salvage the first
	// balanced {...} object rather than returning nil. Log the raw output on total
	// failure so "returned nil" is never a mystery (which model, what it said).
	content := chat.Message.Content
	p, err := parseWatchContent(content)
	if err != nil || p == nil {
		raw := content
		if len(raw) > 300 {
			raw = raw[:300] + "..."
		}
		log.Printf("memory watcher: model %q returned unparseable output (no JSON object), raw: %q", model, raw)
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
		Valence: p.Valence,
	}
}
