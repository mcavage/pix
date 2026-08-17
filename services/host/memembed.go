// Memory embeddings + watcher, both against the host's Ollama.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
// empty, unparsable or non-positive value falls back to def rather than ever
// disabling the timeout (an unbounded http.Post is the bug this file guards).
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
// after a failure: once embedDisabled is latched we re-attempt at most once per
// interval, so a recovered Ollama restores semantic recall with no restart.
const embedProbeInterval = 60 * time.Second

// embedLastProbe is the unix-nano time of the most recent embed re-probe.
var embedLastProbe atomic.Int64

// modelProbeClient is the HTTP client for lightweight Ollama model-presence
// probes (/api/show). The timeout prevents goroutine leaks when Ollama accepts
// the connection but never answers; a package var so a test can redirect it.
var modelProbeClient = &http.Client{Timeout: 10 * time.Second}

var embedDisabled atomic.Bool

// embedExercised is set true the moment a REAL /api/embed attempt (success or
// failure) has actually happened. Health reporting (embedHealthState) must
// never call the embedder "healthy" before this: buildMemStore attaches
// memEmbed with no boot-time probe, so a fresh store has made zero network
// calls and its true state is simply not known yet, not good. embedDisabled
// alone can't tell unknown from healthy — both read as "false" — which is
// exactly the false-healthy gap this flag closes.
var embedExercised atomic.Bool

// embedUnavailable latches semantic recall off and returns the nil vector
// memEmbed answers with. It logs only on the FIRST latch (a wedged Ollama cannot
// flood the log) and names the retry interval, so the degradation reads as
// temporary.
func embedUnavailable(why string) []float64 {
	embedExercised.Store(true)
	if !embedDisabled.Swap(true) {
		log.Printf("memory embed: semantic recall DISABLED, %s; will retry automatically every %.0fs", why, embedProbeInterval.Seconds())
	}
	return nil
}

// embedHealthState reports the tri-state EMBED health `health`/`identity`
// surface: nil ("unknown") until embedExercised flips true on the first real
// attempt, then the SAME live embedDisabled latch this file has always
// tracked. A pointer, not a bool, so nil is representable on the wire as
// JSON null rather than a value that could be mistaken for "confirmed good"
// or "confirmed bad".
func embedHealthState() *bool {
	if !embedExercised.Load() {
		return nil
	}
	healthy := !embedDisabled.Load()
	return &healthy
}

func memEmbed(text string) []float64 {
	if embedDisabled.Load() {
		// Re-probe: once the backoff has elapsed, tentatively clear the flag and let
		// the call through — re-latched below on continued failure.
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
		return embedUnavailable(fmt.Sprintf("embed model unavailable: %v", err))
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return embedUnavailable(fmt.Sprintf("embed HTTP %d", res.StatusCode))
	}
	// Success: mark exercised (a real call just completed) and, if the flag was
	// set (recovery path), announce the restoration.
	embedExercised.Store(true)
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

func memWatcherModel() string {
	if v := os.Getenv("MEMORY_WATCHER_MODEL"); v != "" {
		return v
	}
	// A small, extraction-grade model dedicated to capture. `pix serve` normally
	// passes the resolved config value; this fallback applies only when the daemon
	// runs with MEMORY_WATCHER_MODEL unset. Keep in sync with
	// config.DefaultMemoryWatcherModel.
	return "qwen3.5:9b"
}

// memWatcherWarm (the boot-time "force the watcher model resident" call) was
// deleted along with the boot-time probe (see watcherCaptureAvailable's doc
// comment): warming at startup made every listener wait on Ollama again,
// the exact cost this unit removed. That is a deliberate simplification, not
// a verdict that priming is unwanted forever — an opt-in capture mode still
// wants to avoid paying a cold-model-load penalty on its FIRST real capture.
// The honest place for that is lazy: warm once whatever actually activates
// capture (the listener starting, an opt-in mode flipping on) fires, not
// unconditionally at process boot. Tracked for U7; not implemented here.

// watcherUnavailable is set true once a capture attempt fails because the watcher
// model isn't reachable/pulled, so `observe` tells the caller the truth instead
// of always claiming {accepted:true}.
var watcherUnavailable atomic.Bool

// watcherReason is why capture is off (empty when it is on), set alongside
// watcherUnavailable so observe()/health() surface WHY, not just a bool.
var watcherReason atomic.Pointer[string]

// watcherDegradedUntil is the unix-nano deadline of the backoff watcherDegrade
// opens after a real memWatch() failure (timeout/transport error/non-200).
var watcherDegradedUntil atomic.Int64

// watcherExercised is embedExercised's twin for capture: set true the moment a
// real memWatch() attempt (success or failure) has happened, so health
// reporting (watcherHealthState) can tell "never actually tried" apart from
// "tried and it's fine" — watcherUnavailable defaults to false either way, so
// on its own it cannot distinguish unknown from healthy.
var watcherExercised atomic.Bool

func setWatcherReason(s string) { watcherReason.Store(&s) }

// watcherDegrade is the ONE way capture goes off: record why, open the backoff
// window (inside it watcherCaptureAvailable must not consult the metadata-only
// /api/show probe, which succeeds while inference is wedged), and log once so a
// dead capture half stays diagnosable. Recall works throughout.
func watcherDegrade(reason string) {
	watcherExercised.Store(true)
	watcherUnavailable.Store(true)
	setWatcherReason(reason)
	watcherDegradedUntil.Store(time.Now().Add(watcherBackoff).UnixNano())
	log.Printf("memory watcher: %s, capture skipped", reason)
}

// watcherHealthy is its inverse: capture is on, with no reason and no backoff
// left over from an earlier failure. Also reached on the FIRST ever successful
// memWatch() call (no prior watcherDegrade), so it marks exercised too —
// otherwise a store whose very first capture attempt succeeds would still
// report "unknown" forever.
func watcherHealthy() {
	watcherExercised.Store(true)
	watcherUnavailable.Store(false)
	setWatcherReason("")
	watcherDegradedUntil.Store(0)
}

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
// /api/show, no inference), so a missing watcher model is loud rather than a
// silently dropped capture. Bounded by modelProbeClient's timeout.
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

// watcherProbeInterval throttles the live re-probe so a disabled watcher does not
// hammer Ollama /api/show on every captured turn.
const watcherProbeInterval = 30 * time.Second

// watcherLastProbe is the unix-nano time of the last live re-probe.
var watcherLastProbe atomic.Int64

// watcherCaptureAvailable reports whether fact capture can run RIGHT NOW. There
// is no boot-time probe: watcherUnavailable defaults to false (optimistic,
// capture assumed live) and only latches true from a REAL memWatch() failure,
// so a store never waits on Ollama at startup and a wrong initial guess simply
// self-corrects on the first actual capture attempt. When marked unavailable it
// re-probes at most once per interval. Available is the common path and returns
// at once.
//
// SIDE EFFECT WORTH NAMING: when unavailable and the throttle window has
// elapsed, this makes a REAL network call (memOllamaHasModel's /api/show).
// memWatcherStatus/watcherHealthState both route through this, which means
// something that reads as a passive health check (`health`, `identity`, even
// `pix doctor`) can trigger live Ollama traffic as a side effect — bounded to
// once per watcherProbeInterval, never more, but real. Not a bug: it's how
// capture recovers with no daemon restart. Just don't assume a health read is
// free of network I/O.
func watcherCaptureAvailable() bool {
	if time.Now().UnixNano() < watcherDegradedUntil.Load() {
		// Backing off after a real inference failure: the metadata-only /api/show
		// probe below lies here (it succeeds instantly while /api/chat is hung).
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
		watcherHealthy()
		log.Printf("memory watcher: model %q is now available, fact capture re-enabled.", m)
		return true
	}
	setWatcherReason(fmt.Sprintf("model %q is not pulled (or Ollama is down), run `ollama pull %s`", m, m))
	return false
}

// --- watcher ---------------------------------------------------------------

type watchResult struct {
	Facts       []string
	Corrections []string
}

const memWatcherSystem = `You read ONE message a user sent to their coding agent and extract only what is worth remembering for future sessions. You see ONLY the user's message, never the agent's reply, so everything must come from what the user themselves said.

Be very conservative. Most messages contain nothing worth saving. Saving noise is worse than saving nothing.

Return JSON:
- "facts": DURABLE things, true until the user changes their mind: preferences, identity, conventions, how they like to work, settled decisions. Each self-contained and still useful months from now.
- "corrections": the user telling the agent to stop doing something or do it differently ("don't X", "always Y"). Phrase each as a durable rule.

Hard rules:
- Only what the USER asserts. A QUESTION states nothing ("which branch do I use?" => all empty).
- NEVER infer preferences, interests, or tool usage from a question. Asking ABOUT a capability is not evidence the user has it, wants it, or is using it. A question about memory itself, in particular, asserts nothing about the user, never record "user is interested in memory" or "user is using memory" from it. Examples that must produce all-empty output: "so are you using my memories?", "why do we think those are the right things to inject?", "can I see what you remember?", "is memory working right now?", "how does recall pick what to show me?".
- Acknowledgments ("thanks", "great", "cool") => all empty.
- Code, file names, and one-off task details are not worth saving.
- Time-bound status (what the user is doing right now, a current activity or transition, what's installed/pulled today) is not worth saving either: it goes stale on its own and this watcher does not track it.
- When in doubt, leave it out. Empty arrays are the common, correct answer.
- "facts" and "corrections" are always JSON arrays of strings, never objects, even when empty. An empty list is [], not {}.

Output only the JSON, compact, no prose, no code fence. Exact shape:
{"facts":[],"corrections":[]}`

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

// splitSentenceClauses splits s into sentence-ish clauses on runs of terminal
// punctuation (. ! ?), keeping the punctuation attached to the clause it ends. A
// run ("...?", "?!") collapses to its LAST mark, so "explain this...?" still
// reads as a question; trailing text with no terminal mark is a final clause.
//
// A run only TERMINATES a clause when whitespace or the end of the string
// follows it: an inline mark followed by more non-space text ("main.go", "v1.2",
// "example.com/path") is absorbed into the running clause, so a question
// containing a filename or URL isn't chopped into a bogus non-question fragment.
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

// trailingWrapperChars are closing/markdown wrappers that can legitimately
// follow a question's terminal "?" without changing its meaning: closing quotes,
// parens/brackets/braces, emphasis and backticks. Trimmed as a whole run before
// the question check, so `**Are you using my memories?**` still reads as
// question-only instead of a false-negative fact extraction.
const trailingWrapperChars = "\"'\u201d\u2019)]}*_`"

// questionOnlyUserMessage reports whether a user message is, in substance,
// nothing but question(s) — no assertions worth extracting as a fact. It strips
// fenced code, then requires at least one non-trivial clause and every one of
// them to end in "?".
//
// This gates FACT extraction ONLY (see memCapture): a correction phrased as a
// polite question must still be capturable, so never apply it to corrections.
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

// watcherNoisePrefixes are conservative, observed session-narration openers the
// watcher model sometimes emits in place of a real fact. Prefix-only (not
// substring) so a legitimate fact mentioning "the user" mid-sentence survives.
//
// It deliberately excludes what the user "expects": "the user expects tests on
// every PR" is a durable preference, not narration.
var watcherNoisePrefixes = []string{
	"user requested", "the user requested",
	"user asked", "the user asked",
	"user is interested", "the user is interested",
	"user wants to know", "the user wants to know",
	"user ran", "the user ran",
	"user executed", "the user executed",
}

// watcherNoise reports whether content opens with one of watcherNoisePrefixes
// (case-insensitive). Apply it to the fact channel ONLY, never to corrections:
// a real correction can be phrased exactly like these openers.
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

// flexibleStringList decodes a watcher list field (facts/corrections),
// which should be a JSON array of strings — but some models (observed: qwen) emit
// an empty object {} for an empty list. Accepts a string array, null/absent, or
// an empty object; any other shape is an error so the caller can fall back.
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

// parseWatchJSON decodes one candidate JSON object (the model's raw content, or
// content salvaged by extractJSONObject) into a watchResult.
//
// The top level is deliberately strict: `null`, `{}`, a non-object, or a missing
// required key is a MALFORMED response, not "nothing captured", and must error so
// the caller salvages instead of silently recording an empty capture. Only the
// per-field list shape tolerates the {} vs [] wrinkle (flexibleStringList).
func parseWatchJSON(s string) (*watchResult, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &top); err != nil {
		return nil, fmt.Errorf("top-level value is not a JSON object: %w", err)
	}
	if top == nil {
		// json.Unmarshal("null", &map) succeeds with a nil map and no error, so it
		// must be rejected explicitly rather than read as an all-empty capture.
		return nil, fmt.Errorf("top-level value is null, not a JSON object")
	}
	for _, key := range []string{"facts", "corrections"} {
		if _, ok := top[key]; !ok {
			return nil, fmt.Errorf("missing required key %q", key)
		}
	}
	facts, err := flexibleStringList(top["facts"])
	if err != nil {
		return nil, fmt.Errorf("facts: %w", err)
	}
	corrections, err := flexibleStringList(top["corrections"])
	if err != nil {
		return nil, fmt.Errorf("corrections: %w", err)
	}
	return &watchResult{Facts: facts, Corrections: corrections}, nil
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
	// NO constrained `format` schema: ollama's grammar-constrained decode hung the
	// watcher (90s timeouts for a 13-char input). The system prompt already pins the
	// JSON shape and we parse it leniently (extractJSONObject). num_predict caps a
	// runaway; keep_alive avoids a cold reload between captures.
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
		// A timeout is the observed failure mode: Ollama accepts the connection and
		// never finishes inference.
		reason := fmt.Sprintf("Ollama /api/chat unreachable at %s: %v", ollamaHost(), err)
		if isTimeoutErr(err) {
			reason = fmt.Sprintf("Ollama not responding: watcher inference timed out after %ds (is ollama healthy? try restarting it)", int(watcherTimeout().Seconds()))
		}
		watcherDegrade(reason)
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		watcherDegrade(fmt.Sprintf("Ollama /api/chat HTTP %d (is model %q pulled? `ollama pull %s`): %s",
			res.StatusCode, model, model, strings.TrimSpace(string(b))))
		return nil
	}
	watcherHealthy()
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
	// Primary: the content IS the JSON object. Fallback: a model that wraps it in
	// prose or a fence gets salvaged. On total failure log the raw output, so
	// "returned nil" is never a mystery about which model said what.
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
	return &watchResult{Facts: clean(p.Facts), Corrections: clean(p.Corrections)}
}
