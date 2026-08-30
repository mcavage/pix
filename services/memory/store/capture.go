// capture.go is the Ollama-backed embedder and watcher (opted-in automatic
// fact/correction extraction), plus the memory_capture admission gate and its
// daily watcher-inference budget. Ported from services/host/memembed.go and
// memory_capture_mode.go (pix-v2 U2): behavior is unchanged; config comes
// from plain env vars instead of pix/host/config, matching this module's
// standalone container contract.
package store

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
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

func ollamaHost() string { return envOr("OLLAMA_HOST", "http://127.0.0.1:11434") }

// embedTimeout bounds the recall-side /api/embed call (MEMORY_EMBED_TIMEOUT_MS,
// default 15s): a wedged Ollama should degrade recall to keyword search well
// within a turn, not hang it.
func embedTimeout() time.Duration { return envMs("MEMORY_EMBED_TIMEOUT_MS", 15000) }

// watcherTimeout bounds the capture-side /api/chat call
// (MEMORY_WATCHER_TIMEOUT_MS, default 90s): generous for a slow cold local
// model load, but bounded.
func watcherTimeout() time.Duration { return envMs("MEMORY_WATCHER_TIMEOUT_MS", 90000) }

// watcherBackoff is how long a real inference failure (timeout/non-200)
// keeps capture degraded, ignoring the (metadata-only) /api/show re-probe.
const watcherBackoff = 60 * time.Second

// embedProbeInterval throttles the automatic re-probe of the embed endpoint
// after a failure.
const embedProbeInterval = 60 * time.Second

var embedLastProbe atomic.Int64

var modelProbeClient = &http.Client{Timeout: 10 * time.Second}

var embedDisabled atomic.Bool

// embedExercised is set true the moment a REAL /api/embed attempt (success
// or failure) has actually happened; Status() must never call the embedder
// "healthy" before this.
var embedExercised atomic.Bool

func embedUnavailable(why string) []float64 {
	embedExercised.Store(true)
	if !embedDisabled.Swap(true) {
		log.Printf("memory embed: semantic recall DISABLED, %s; will retry automatically every %.0fs", why, embedProbeInterval.Seconds())
	}
	return nil
}

// EmbedHealthState reports the tri-state embed health: nil ("unknown") until
// the first real attempt, then whether it is currently healthy.
func EmbedHealthState() *bool {
	if !embedExercised.Load() {
		return nil
	}
	healthy := !embedDisabled.Load()
	return &healthy
}

// EmbedModel is the Ollama embedding model (MEMORY_EMBED_MODEL, default
// nomic-embed-text).
func EmbedModel() string { return envOr("MEMORY_EMBED_MODEL", "nomic-embed-text") }

// Embed is the Embedder this service passes to store.Open: a self-retrying
// call against the host/container's configured Ollama. It never blocks
// store construction on a boot-time probe; a real failure latches semantic
// recall off until the next probe window, and a recovered Ollama restores
// it with no restart.
func Embed(text string) []float64 {
	if embedDisabled.Load() {
		now := time.Now().UnixNano()
		last := embedLastProbe.Load()
		if now-last < int64(embedProbeInterval) {
			return nil
		}
		if !embedLastProbe.CompareAndSwap(last, now) {
			return nil
		}
		embedDisabled.Store(false)
	}
	model := EmbedModel()
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

// WatcherModel is the small, extraction-grade model dedicated to capture
// (MEMORY_WATCHER_MODEL, default qwen3.5:9b).
func WatcherModel() string { return envOr("MEMORY_WATCHER_MODEL", "qwen3.5:9b") }

var watcherUnavailable atomic.Bool
var watcherReason atomic.Pointer[string]
var watcherDegradedUntil atomic.Int64
var watcherExercised atomic.Bool

func setWatcherReason(s string) { watcherReason.Store(&s) }

func watcherDegrade(reason string) {
	watcherExercised.Store(true)
	watcherUnavailable.Store(true)
	setWatcherReason(reason)
	watcherDegradedUntil.Store(time.Now().Add(watcherBackoff).UnixNano())
	log.Printf("memory watcher: %s, capture skipped", reason)
}

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

func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func ollamaHasModel(model string) bool {
	body, _ := json.Marshal(map[string]any{"name": model})
	res, err := modelProbeClient.Post(ollamaHost()+"/api/show", "application/json", bytes.NewReader(body))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return res.StatusCode == 200
}

const watcherProbeInterval = 30 * time.Second

var watcherLastProbe atomic.Int64

// watcherCaptureAvailable reports whether fact capture can run RIGHT NOW,
// re-probing Ollama (throttled) when previously marked unavailable.
func watcherCaptureAvailable() bool {
	if time.Now().UnixNano() < watcherDegradedUntil.Load() {
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
	if !watcherLastProbe.CompareAndSwap(last, now) {
		return false
	}
	m := WatcherModel()
	if ollamaHasModel(m) {
		watcherHealthy()
		log.Printf("memory watcher: model %q is now available, fact capture re-enabled.", m)
		return true
	}
	setWatcherReason(fmt.Sprintf("model %q is not pulled (or Ollama is down), run `ollama pull %s`", m, m))
	return false
}

// WatcherHealthState is EmbedHealthState's twin for capture: nil ("unknown")
// until the first real memWatch attempt.
func WatcherHealthState() (state *bool, reason string) {
	if watcherCaptureAvailable() {
		if !watcherExercised.Load() {
			return nil, ""
		}
		t := true
		return &t, ""
	}
	reason = getWatcherReason()
	if !watcherExercised.Load() {
		return nil, ""
	}
	f := false
	return &f, reason
}

// --- memory_capture admission mode + daily watcher budget -------------------

const (
	CaptureExplicit         = "explicit"
	CaptureExperimentalAuto = "experimental-auto"
)

// CaptureMode reads MEMORY_CAPTURE_MODE. Anything but the opt-in value reads
// as explicit: unset/garbled must never silently turn automatic capture on.
func CaptureMode() string {
	if strings.TrimSpace(os.Getenv("MEMORY_CAPTURE_MODE")) == CaptureExperimentalAuto {
		return CaptureExperimentalAuto
	}
	return CaptureExplicit
}

// watcherDailyBudget: at most this many rows may be STORED per calendar day
// (UTC), counted from what's actually persisted, never from how many times
// the watcher was called.
const watcherDailyBudget = 10

const watcherUsedTodaySQL = "SELECT COUNT(*) FROM memories WHERE source = 'watcher' AND created_at >= ? AND created_at < ?"

// watcherUsedToday deliberately does NOT filter deleted_at or profile: see
// services/host/memory_capture_mode.go's doc comment for the full rationale
// (a forgotten watcher row still cost a real capture; the budget meters
// volume written on this host today, not per-profile visible inventory).
func (s *Store) watcherUsedToday() (int, error) {
	start, end := watcherDayBoundsUTC(time.Now())
	var used int
	err := s.db.QueryRow(watcherUsedTodaySQL, start, end).Scan(&used)
	return used, err
}

func watcherDayBoundsUTC(t time.Time) (start, end string) {
	t = t.UTC()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	const noZ = "2006-01-02T15:04:05"
	return dayStart.Format(noZ), dayEnd.Format(noZ)
}

// WatcherBudgetRemaining reports how many more rows may be stored today
// (UTC), never negative. Advisory only; the actual cap is enforced
// atomically at INSERT time in rememberSourced.
func (s *Store) WatcherBudgetRemaining() (int, error) {
	used, err := s.watcherUsedToday()
	if err != nil {
		return 0, err
	}
	remaining := watcherDailyBudget - used
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

// Observe is the ONE capture-admission path the memory_observe MCP tool
// calls. explicit (the default) refuses immediately, before any watcher
// state is consulted: zero inference, zero side-effect Ollama probe.
func (s *Store) Observe(user, project string, hasProject bool, profile string) (accepted bool, reason string) {
	if strings.TrimSpace(user) == "" {
		return false, ""
	}
	if CaptureMode() == CaptureExplicit {
		return false, "automatic capture is off (memory_capture=explicit); use explicit remember"
	}
	if remaining, err := s.WatcherBudgetRemaining(); err != nil {
		return false, "capture budget check failed; try again shortly"
	} else if remaining <= 0 {
		return false, fmt.Sprintf("daily watcher capture budget exhausted (max %d stored rows/day); recall still works", watcherDailyBudget)
	}
	if !watcherCaptureAvailable() {
		why := getWatcherReason()
		if why == "" {
			why = "watcher model unavailable, run `ollama pull " + WatcherModel() + "` (or set MEMORY_WATCHER_MODEL)"
		}
		return false, why + "; recall still works"
	}
	select {
	case captureSem <- struct{}{}:
		go capture(s, user, project, hasProject, profile)
		return true, ""
	default:
		return false, "capture busy (too many in flight); retry shortly, recall still works"
	}
}

// captureMaxConcurrency bounds in-flight background captures: each makes a
// watcher-model (Ollama) call, so an unbounded goroutine per observe call is
// a trivial local DoS.
const captureMaxConcurrency = 8

var captureSem = make(chan struct{}, captureMaxConcurrency)

func capture(s *Store, user, project string, hasProj bool, profile string) {
	defer func() { recover() }()
	defer func() { <-captureSem }()
	if containsSecretShape(user) {
		log.Printf("memory: capture input blocked by the secret filter (stage 1), watcher not invoked")
		return
	}
	user = truncate(user, 8000)
	remaining, err := s.WatcherBudgetRemaining()
	if err != nil {
		log.Printf("memory: watcher budget check failed, skipping this capture: %v", err)
		return
	}
	if remaining <= 0 {
		log.Printf("memory: daily watcher capture budget exhausted (max %d stored rows/day), watcher not invoked", watcherDailyBudget)
		return
	}
	log.Printf("memory: observe -> watcher (user %d chars, project %q, profile %q, budget remaining %d)", len(user), project, profile, remaining)
	w := watch(user)
	if w == nil {
		log.Printf("memory: watcher returned nil (no extraction), nothing captured")
		return
	}
	if len(w.Facts) > 0 && questionOnlyUserMessage(user) {
		log.Printf("memory: dropping %d fact(s), user message was question-only, no assertions to extract", len(w.Facts))
		w.Facts = nil
	}
	dropNoise := func(label string, in []string) []string {
		out := make([]string, 0, len(in))
		dropped := 0
		for _, s := range in {
			if watcherNoise(s) {
				dropped++
				continue
			}
			out = append(out, s)
		}
		if dropped > 0 {
			log.Printf("memory: dropped %d watcher %s item(s) as session-narration noise", dropped, label)
		}
		return out
	}
	w.Facts = dropNoise("fact", w.Facts)

	filterSecrets := func(label string, in []string) []string {
		out := make([]string, 0, len(in))
		dropped := 0
		for _, s := range in {
			if containsSecretShape(s) {
				dropped++
				continue
			}
			out = append(out, s)
		}
		if dropped > 0 {
			log.Printf("memory: dropped %d watcher %s item(s), secret-shaped content (stage 2)", dropped, label)
		}
		return out
	}
	w.Facts = filterSecrets("fact", w.Facts)
	w.Corrections = filterSecrets("correction", w.Corrections)

	type watchItem struct {
		content, kind string
		conf          float64
	}
	items := make([]watchItem, 0, len(w.Facts)+len(w.Corrections))
	for _, f := range w.Facts {
		items = append(items, watchItem{f, "fact", 0.65})
	}
	for _, c := range w.Corrections {
		items = append(items, watchItem{c, "learning", 0.75})
	}
	stored := 0
	for i, it := range items {
		res, err := s.rememberWatcherCapture(RememberInput{Content: it.content, Kind: it.kind,
			Confidence: it.conf, Project: project, HasProject: hasProj,
			Profile: profile, Dedupe: 0.9, HasDedupe: true})
		if err != nil {
			log.Printf("memory: watcher item failed to store, not counted against the daily budget: %v", err)
			continue
		}
		if res.BudgetExceeded {
			log.Printf("memory: daily watcher capture budget exhausted mid-capture, dropping %d item(s) (count only, no content logged)", len(items)-i)
			break
		}
		if res.Reaffirmed {
			continue
		}
		stored++
	}
	if len(items) > 0 {
		log.Printf("memory: capture considered %d item(s) (%d fact(s), %d correction(s)), stored %d new row(s)",
			len(items), len(w.Facts), len(w.Corrections), stored)
	} else {
		log.Printf("memory: watcher ran but extracted 0 items (nothing it judged worth keeping, or an empty result)")
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// --- watcher model call + response parsing -----------------------------------

type watchResult struct {
	Facts       []string
	Corrections []string
}

const watcherSystem = `You read ONE message a user sent to their coding agent and extract only what is worth remembering for future sessions. You see ONLY the user's message, never the agent's reply, so everything must come from what the user themselves said.

Be very conservative. Most messages contain nothing worth saving. Saving noise is worse than saving nothing.

Return JSON:
- "facts": DURABLE things, true until the user changes their mind: preferences, identity, conventions, how they like to work, settled decisions. Each self-contained and still useful months from now.
- "corrections": the user telling the agent to stop doing something or do it differently ("don't X", "always Y"). Phrase each as a durable rule.

Hard rules:
- Only what the USER asserts. A QUESTION states nothing ("which branch do I use?" => all empty).
- NEVER infer preferences, interests, or tool usage from a question. Asking ABOUT a capability is not evidence the user has it, wants it, or is using it.
- Acknowledgments ("thanks", "great", "cool") => all empty.
- Code, file names, and one-off task details are not worth saving.
- Time-bound status is not worth saving either.
- When in doubt, leave it out. Empty arrays are the common, correct answer.
- "facts" and "corrections" are always JSON arrays of strings, never objects, even when empty.

Output only the JSON, compact, no prose, no code fence. Exact shape:
{"facts":[],"corrections":[]}`

var fencedCodeBlockRE = regexp.MustCompile("(?s)```.*?```")

func hasWordChar(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

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

const trailingWrapperChars = "\"'\u201d\u2019)]}*_`"

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

var watcherNoisePrefixes = []string{
	"user requested", "the user requested",
	"user asked", "the user asked",
	"user is interested", "the user is interested",
	"user wants to know", "the user wants to know",
	"user ran", "the user ran",
	"user executed", "the user executed",
}

func watcherNoise(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	for _, p := range watcherNoisePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

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
		return nil, fmt.Errorf("list field is a non-empty object (%d byte(s))", len(trimmed))
	}
	return nil, fmt.Errorf("list field has unexpected shape (%d byte(s))", len(trimmed))
}

func parseWatchJSON(s string) (*watchResult, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &top); err != nil {
		return nil, fmt.Errorf("top-level value is not a JSON object: %w", err)
	}
	if top == nil {
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
	if err != nil && !json.Valid([]byte(content)) {
		if salvaged := extractJSONObject(content); salvaged != "" {
			return parseWatchJSON(salvaged)
		}
	}
	return p, err
}

func watch(user string) *watchResult {
	model := WatcherModel()
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": false,
		"keep_alive": "10m",
		"think":      false,
		"options":    map[string]any{"temperature": 0, "num_predict": 1024},
		"messages": []map[string]any{
			{"role": "system", "content": watcherSystem},
			{"role": "user", "content": user},
		},
	})
	client := &http.Client{Timeout: watcherTimeout()}
	res, err := client.Post(ollamaHost()+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
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
		log.Printf("memory watcher: empty/undecodable chat response from model %q (%d bytes)", model, len(rawBody))
		return nil
	}
	content := chat.Message.Content
	p, err := parseWatchContent(content)
	if err != nil || p == nil {
		log.Printf("memory watcher: model %q returned unparseable output (no JSON object), %d bytes, error: %v", model, len(content), err)
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
