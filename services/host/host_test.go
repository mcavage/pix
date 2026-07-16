package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemFtsQuery(t *testing.T) {
	cases := map[string]string{
		"Hello World":        `"hello" OR "world"`,
		"a, b, c!":           `""`, // single-char tokens dropped
		"Go for host-svc 99": `"go" OR "for" OR "host" OR "svc" OR "99"`,
		"":                   "",
	}
	for in, want := range cases {
		if in == "a, b, c!" {
			want = "" // all tokens len<=1 -> empty
		}
		if got := memFtsQuery(in); got != want {
			t.Errorf("memFtsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMemCosine(t *testing.T) {
	if got := memCosine([]float64{1, 0}, []float64{1, 0}); got < 0.999 {
		t.Errorf("identical vectors cosine = %v, want ~1", got)
	}
	if got := memCosine([]float64{1, 0}, []float64{0, 1}); got != 0 {
		t.Errorf("orthogonal cosine = %v, want 0", got)
	}
	if got := memCosine([]float64{1, 2, 3}, []float64{1, 2}); got != 0 {
		t.Errorf("dimension mismatch cosine = %v, want 0 (safe)", got)
	}
	if got := memCosine([]float64{0, 0}, []float64{1, 1}); got != 0 {
		t.Errorf("zero vector cosine = %v, want 0", got)
	}
}

func TestClampInt(t *testing.T) {
	if clampInt(float64(5), 0, 1, 10) != 5 {
		t.Error("float64 in-range")
	}
	if clampInt(float64(99), 0, 1, 10) != 10 {
		t.Error("clamp hi")
	}
	if clampInt(nil, 7, 1, 10) != 7 {
		t.Error("nil -> default")
	}
	if clampInt("3", 0, 1, 10) != 3 {
		t.Error("string numeric")
	}
}

func TestMcpDispatcher(t *testing.T) {
	tools := []mcpTool{{Name: "echo", Description: "echo", Properties: jsonObj{}, Required: nil}}
	handlers := map[string]func(jsonObj) (any, error){
		"echo": func(a jsonObj) (any, error) { return jsonObj{"said": a["msg"]}, nil },
	}
	h := mcpDispatcher("test", tools, handlers)

	// initialize
	rep, ok := h(jsonObj{"jsonrpc": "2.0", "id": float64(1), "method": "initialize"})
	if !ok || rep["result"].(jsonObj)["serverInfo"].(jsonObj)["name"] != "test" {
		t.Fatalf("initialize bad reply: %v", rep)
	}
	// tools/list
	rep, ok = h(jsonObj{"id": float64(2), "method": "tools/list"})
	if !ok || len(rep["result"].(jsonObj)["tools"].([]jsonObj)) != 1 {
		t.Fatalf("tools/list bad reply: %v", rep)
	}
	// tools/call
	rep, _ = h(jsonObj{"id": float64(3), "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{"msg": "hi"}}})
	txt := rep["result"].(jsonObj)["content"].([]jsonObj)[0]["text"].(string)
	if !strings.Contains(txt, "hi") {
		t.Fatalf("tools/call result missing payload: %q", txt)
	}
	// notification -> no reply
	if _, ok := h(jsonObj{"method": "notifications/initialized"}); ok {
		t.Fatal("notification should produce no reply")
	}
	// unknown tool -> error
	rep, _ = h(jsonObj{"id": float64(4), "method": "tools/call", "params": map[string]any{"name": "nope"}})
	if _, isErr := rep["error"]; !isErr {
		t.Fatal("unknown tool should error")
	}
}

func TestMemStoreRememberRecall(t *testing.T) {
	st, err := newMemStore(":memory:", nil) // nil embedder -> FTS-only, no Ollama needed
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.remember(rememberInput{content: "The user prefers Go for host services and TypeScript in the sandbox."})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := r["reaffirmed"].(bool); b {
		t.Fatal("first remember should not be a reaffirm")
	}

	hits, err := st.recall("what language for host services", 8, 1200, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].content, "Go for host services") {
		t.Fatalf("recall did not surface the fact: %+v", hits)
	}

	// exact-duplicate remember -> reaffirmed, no new row
	r2, _ := st.remember(rememberInput{content: "The user prefers Go for host services and TypeScript in the sandbox."})
	if b, _ := r2["reaffirmed"].(bool); !b {
		t.Fatal("duplicate remember should reaffirm")
	}
	if st.stats("")["active"].(int) != 1 {
		t.Fatalf("expected 1 active memory, got %v", st.stats("")["active"])
	}

	// forget by id prefix
	id := r["id"].(string)
	if !st.forget(id[:8], "") {
		t.Fatal("forget by 8-char prefix should succeed")
	}
	if st.stats("")["active"].(int) != 0 {
		t.Fatal("expected 0 active after forget")
	}
}

// Exercises the recall scoring/ordering — specifically the project-match branch,
// which the single-row test never hit. Two equally-relevant facts (same query
// terms, same length) must order by project: the current-project one first.
func TestRecallOrdering(t *testing.T) {
	st, err := newMemStore(":memory:", nil) // FTS-only, deterministic
	if err != nil {
		t.Fatal(err)
	}
	mk := func(content, project string) {
		if _, err := st.remember(rememberInput{content: content, project: project, hasProject: true}); err != nil {
			t.Fatal(err)
		}
	}
	mk("alpha beta gamma", "proj")  // current project
	mk("alpha beta delta", "other") // different project — same relevance, lower factor

	hits, err := st.recall("alpha beta", 8, 100000, "", "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d: %+v", len(hits), hits)
	}
	if !strings.Contains(hits[0].content, "gamma") {
		t.Errorf("project-match memory should rank first; got order: %q then %q", hits[0].content, hits[1].content)
	}
	if hits[0].score <= hits[1].score {
		t.Errorf("scores not strictly ordered: %v <= %v", hits[0].score, hits[1].score)
	}
}

// TestMemProfileIsolation proves the default/opt-in visibility model: a named
// profile sees its own rows UNION the default bucket, the default profile sees
// only default rows, and a named profile never leaks into a sibling profile.
func TestMemProfileIsolation(t *testing.T) {
	st, err := newMemStore(":memory:", nil) // FTS-only, deterministic
	if err != nil {
		t.Fatal(err)
	}
	mk := func(content, profile string) {
		if _, err := st.remember(rememberInput{content: content, profile: profile}); err != nil {
			t.Fatal(err)
		}
	}
	mk("shared widget baseline fact", "default") // default bucket
	mk("work widget confidential roadmap", "work")
	mk("personal widget vacation note", "personal")

	// helper: the set of contents recall surfaces under a given profile.
	recalled := func(profile string) map[string]bool {
		hits, err := st.recall("widget", 8, 100000, "", "", profile)
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, h := range hits {
			set[h.content] = true
		}
		return set
	}

	work := recalled("work")
	if !work["work widget confidential roadmap"] || !work["shared widget baseline fact"] {
		t.Errorf("work recall must see work + default: %v", work)
	}
	if work["personal widget vacation note"] {
		t.Errorf("work recall must NOT see personal rows: %v", work)
	}

	personal := recalled("personal")
	if !personal["personal widget vacation note"] || !personal["shared widget baseline fact"] {
		t.Errorf("personal recall must see personal + default: %v", personal)
	}
	if personal["work widget confidential roadmap"] {
		t.Errorf("personal recall must NOT see work rows: %v", personal)
	}

	def := recalled("default")
	if !def["shared widget baseline fact"] {
		t.Errorf("default recall must see the default row: %v", def)
	}
	if def["work widget confidential roadmap"] || def["personal widget vacation note"] {
		t.Errorf("default recall must see ONLY default rows: %v", def)
	}

	// An absent/empty profile normalizes to default — backward-compatible.
	if blank := recalled(""); len(blank) != 1 || !blank["shared widget baseline fact"] {
		t.Errorf("empty profile must behave as default: %v", blank)
	}

	// Stats are profile-consistent too: work sees its own row + the default one.
	if got := st.stats("work")["active"].(int); got != 2 {
		t.Errorf("work stats active = %d, want 2 (work + default)", got)
	}
	if got := st.stats("default")["active"].(int); got != 1 {
		t.Errorf("default stats active = %d, want 1", got)
	}
}

// TestMemProfileMigration opens a store over a DB created WITHOUT the profile
// column (a legacy store) and confirms the idempotent ALTER runs, that legacy
// rows are treated as the default bucket (visible under every profile), and that
// new writes still work.
func TestMemProfileMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build a legacy DB: memories table WITHOUT the profile column.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const legacySchema = `
CREATE TABLE memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content);`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	const legacyContent = "legacy migration widget fact"
	res, err := db.Exec(`INSERT INTO memories
		(id, kind, content, content_hash, durability, confidence, reward, source, tags, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"legacy-id", "fact", legacyContent, memHash(legacyContent), "durable", 0.8, 0.0, "user", "[]", memNowIso())
	if err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	rowid, _ := res.LastInsertId()
	if _, err := db.Exec("INSERT INTO memories_fts (rowid, content) VALUES (?, ?)", rowid, legacyContent); err != nil {
		t.Fatalf("legacy fts insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen through newMemStore — the ALTER migration must run cleanly.
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore on legacy DB: %v", err)
	}

	// The legacy row (profile NULL) is the default bucket: visible under default
	// AND under any named profile.
	for _, profile := range []string{"default", "work"} {
		hits, err := st.recall("legacy widget", 8, 100000, "", "", profile)
		if err != nil {
			t.Fatalf("recall(%q): %v", profile, err)
		}
		if len(hits) != 1 || hits[0].content != legacyContent {
			t.Fatalf("recall(%q) did not surface the legacy row: %+v", profile, hits)
		}
	}

	// New writes still work post-migration.
	if _, err := st.remember(rememberInput{content: "new fact after migration", profile: "work"}); err != nil {
		t.Fatalf("remember after migration: %v", err)
	}
}

// fakeKeywordEmbedder returns a one-hot vector indexed by a string's FIRST
// token, so two strings that share a first token are cosine-identical (1.0)
// while keeping distinct content hashes. Deterministic; no Ollama needed. Used
// to exercise findSimilar/synthesize (the vector paths) in isolation tests.
func fakeKeywordEmbedder(dims int) func(string) []float64 {
	idx := map[string]int{}
	next := 0
	return func(s string) []float64 {
		fields := strings.Fields(strings.ToLower(s))
		key := ""
		if len(fields) > 0 {
			key = fields[0]
		}
		i, ok := idx[key]
		if !ok {
			i = next % dims
			idx[key] = i
			next++
		}
		v := make([]float64, dims)
		v[i] = 1
		return v
	}
}

// bucketActive counts active (non-deleted) rows in ONE exact storage bucket
// (normalized profile), reaching into the store's db so tests can assert per-
// bucket effects that recall's visible-union would hide.
func bucketActive(t *testing.T, st *memStore, profile string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow("SELECT count(*) FROM memories WHERE deleted_at IS NULL AND "+memProfileStorage, profile).Scan(&n); err != nil {
		t.Fatalf("bucketActive(%q): %v", profile, err)
	}
	return n
}

// profileOf returns the raw stored profile column for a row id.
func profileOf(t *testing.T, st *memStore, id string) string {
	t.Helper()
	var p sql.NullString
	if err := st.db.QueryRow("SELECT profile FROM memories WHERE id = ?", id).Scan(&p); err != nil {
		t.Fatalf("profileOf(%q): %v", id, err)
	}
	if p.Valid {
		return p.String
	}
	return ""
}

// bucketSurvivor returns the id and frequency of the single active row in one
// exact storage bucket. It fails the test if the bucket does not hold exactly
// one active row, so callers get a clear error rather than a silent first-row
// pick. Used to assert per-bucket survivor frequency after synthesize.
func bucketSurvivor(t *testing.T, st *memStore, profile string) (string, int) {
	t.Helper()
	if got := bucketActive(t, st, profile); got != 1 {
		t.Fatalf("bucketSurvivor(%q): want exactly 1 active row, got %d", profile, got)
	}
	var id string
	var freq int
	if err := st.db.QueryRow("SELECT id, frequency FROM memories WHERE deleted_at IS NULL AND "+memProfileStorage, profile).Scan(&id, &freq); err != nil {
		t.Fatalf("bucketSurvivor(%q): %v", profile, err)
	}
	return id, freq
}

// TestMemDedupeIsolation proves reaffirm is scoped to the STORAGE bucket: the
// same text remembered under `work` and `personal` must create TWO distinct
// rows (each stamped its own profile), never collapse across profiles. The same
// text re-remembered in the SAME bucket still reaffirms (dedup within a bucket).
func TestMemDedupeIsolation(t *testing.T) {
	st, err := newMemStore(":memory:", nil) // hash reaffirm path (no embedder)
	if err != nil {
		t.Fatal(err)
	}
	const text = "the release ships on friday at noon"
	rw, err := st.remember(rememberInput{content: text, profile: "work"})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := st.remember(rememberInput{content: text, profile: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := rw["reaffirmed"].(bool); b {
		t.Fatal("first work remember must be a new row, not a reaffirm")
	}
	if b, _ := rp["reaffirmed"].(bool); b {
		t.Fatal("personal remember must NOT reaffirm work's row across profiles")
	}
	wID, pID := rw["id"].(string), rp["id"].(string)
	if wID == pID {
		t.Fatalf("work and personal must be DISTINCT rows, got same id %q", wID)
	}
	if got := profileOf(t, st, wID); got != "work" {
		t.Errorf("work row profile = %q, want work", got)
	}
	if got := profileOf(t, st, pID); got != "personal" {
		t.Errorf("personal row profile = %q, want personal", got)
	}
	// Re-remember the same text under work -> reaffirm the SAME work row.
	rw2, err := st.remember(rememberInput{content: text, profile: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := rw2["reaffirmed"].(bool); !b {
		t.Fatal("same text in same bucket should reaffirm")
	}
	if rw2["id"].(string) != wID {
		t.Errorf("reaffirm returned %q, want work row %q", rw2["id"], wID)
	}
	if bucketActive(t, st, "work") != 1 || bucketActive(t, st, "personal") != 1 {
		t.Errorf("expected 1 row per bucket, got work=%d personal=%d",
			bucketActive(t, st, "work"), bucketActive(t, st, "personal"))
	}

	// Discriminating case for the visible-union bug: a DEFAULT row, then the
	// SAME text under `work`. Unlike the work/personal pair above, `work` CAN
	// see the default bucket (its visible union includes default), so a reaffirm
	// scoped to memProfileVisible would wrongly collapse the work write into the
	// default row. Correct storage-bucket scoping must create a NEW work row.
	const shared = "the all-hands is on monday morning"
	rd, err := st.remember(rememberInput{content: shared, profile: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := rd["reaffirmed"].(bool); b {
		t.Fatal("first default remember must be a new row, not a reaffirm")
	}
	rdw, err := st.remember(rememberInput{content: shared, profile: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := rdw["reaffirmed"].(bool); b {
		t.Fatal("work remember of a default row's text must NOT reaffirm the default row (visible-union bug)")
	}
	dID, dwID := rd["id"].(string), rdw["id"].(string)
	if dID == dwID {
		t.Fatalf("default and work must be DISTINCT rows, got same id %q", dID)
	}
	if got := profileOf(t, st, dID); got != "default" {
		t.Errorf("default row profile = %q, want default", got)
	}
	if got := profileOf(t, st, dwID); got != "work" {
		t.Errorf("new work row profile = %q, want work", got)
	}
	if got := bucketActive(t, st, "default"); got != 1 {
		t.Errorf("default bucket active = %d, want 1 (the shared row)", got)
	}
}

// TestMemFindSimilarIsolation proves near-duplicate dedupe (findSimilar) is
// scoped to the storage bucket: a vector-similar row in a SIBLING profile must
// not be collapsed into; it must create a new row in its own bucket.
func TestMemFindSimilarIsolation(t *testing.T) {
	st, err := newMemStore(":memory:", fakeKeywordEmbedder(16))
	if err != nil {
		t.Fatal(err)
	}
	// "alpha *" all share a first token -> cosine 1.0 between them.
	r1, err := st.remember(rememberInput{content: "alpha one", profile: "work", dedupe: 0.9, hasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	// Same bucket, vector-similar, different hash -> collapses (reaffirm).
	r2, err := st.remember(rememberInput{content: "alpha two", profile: "work", dedupe: 0.9, hasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := r2["reaffirmed"].(bool); !b {
		t.Fatal("same-bucket near-duplicate should collapse via findSimilar")
	}
	if r2["id"].(string) != r1["id"].(string) {
		t.Error("same-bucket near-duplicate should collapse into the existing row")
	}
	// Sibling profile, vector-similar -> must NOT collapse into work's row.
	r3, err := st.remember(rememberInput{content: "alpha three", profile: "personal", dedupe: 0.9, hasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := r3["reaffirmed"].(bool); b {
		t.Fatal("cross-profile near-duplicate must create a NEW row, not collapse")
	}
	if bucketActive(t, st, "work") != 1 || bucketActive(t, st, "personal") != 1 {
		t.Errorf("expected 1 row per bucket, got work=%d personal=%d",
			bucketActive(t, st, "work"), bucketActive(t, st, "personal"))
	}

	// Discriminating case for the visible-union bug on the vector path: a
	// DEFAULT row, then a near-duplicate under `work`. `work` CAN see the
	// default bucket via the visible union, so findSimilar scoped to
	// memProfileVisible would collapse the work write into the default row;
	// storage-bucket scoping must create a NEW work row instead. Use a fresh
	// first token ("beta") so it is vector-similar only within this case.
	rd, err := st.remember(rememberInput{content: "beta one", profile: "default", dedupe: 0.9, hasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	rdw, err := st.remember(rememberInput{content: "beta two", profile: "work", dedupe: 0.9, hasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := rdw["reaffirmed"].(bool); b {
		t.Fatal("work near-duplicate of a DEFAULT row must create a NEW row, not collapse (visible-union bug)")
	}
	if rdw["id"].(string) == rd["id"].(string) {
		t.Error("work write must not collapse into the default row")
	}
	if got := profileOf(t, st, rd["id"].(string)); got != "default" {
		t.Errorf("default row profile = %q, want default", got)
	}
	if got := profileOf(t, st, rdw["id"].(string)); got != "work" {
		t.Errorf("new work row profile = %q, want work", got)
	}
	if got := bucketActive(t, st, "default"); got != 1 {
		t.Errorf("default bucket active = %d, want 1 (the beta row)", got)
	}
}

// TestMemSynthesizeIsolation proves synthesize merges only WITHIN a storage
// bucket. Seed a vector-identical pair in each of work/personal plus a lone
// default row; after synthesis each bucket keeps exactly one row (its own pair
// merged), no row changed profile, and merged==2 (never a cross-profile merge
// that would have collapsed everything into one bucket).
func TestMemSynthesizeIsolation(t *testing.T) {
	st, err := newMemStore(":memory:", fakeKeywordEmbedder(16))
	if err != nil {
		t.Fatal(err)
	}
	// mkN remembers content in a bucket `times` times. The first insert creates
	// the row (frequency 1); each subsequent same-bucket remember reaffirms it
	// (frequency +1). Reaffirm returns the same id, so the id is stable. This
	// lets us give each seed row a KNOWN, distinct starting frequency.
	mkN := func(content, profile string, times int) string {
		var id string
		for i := 0; i < times; i++ {
			r, err := st.remember(rememberInput{content: content, profile: profile})
			if err != nil {
				t.Fatal(err)
			}
			id = r["id"].(string)
		}
		return id
	}
	// All "alpha *" share a vector (cosine 1.0), so global (broken) synthesis
	// would merge all five into one bucket; scoped synthesis merges per bucket.
	// Frequencies are chosen so the survivor of each within-bucket merge has a
	// KNOWN total, and so any cross-bucket frequency transfer (the bug) would
	// change it: synthesizeBucket bumps the survivor by frequency += dead.freq,
	// and the survivor is the highest-frequency row (ORDER BY frequency DESC).
	//   work:     alpha one freq 3 (survivor) + alpha two freq 1  -> survivor 4
	//   personal: alpha four freq 2 (survivor) + alpha three freq 1 -> survivor 3
	//   default:  alpha five freq 1 (lone, nothing to merge)      -> stays 1
	mkN("alpha one", "work", 3)
	mkN("alpha two", "work", 1)
	mkN("alpha three", "personal", 1)
	mkN("alpha four", "personal", 2)
	defID := mkN("alpha five", "default", 1)

	res := st.synthesize(0.9)
	merged, _ := res["merged"].(int)
	if merged != 2 {
		t.Errorf("merged = %d, want 2 (one within work, one within personal; never across)", merged)
	}
	if got := bucketActive(t, st, "work"); got != 1 {
		t.Errorf("work bucket active = %d, want 1", got)
	}
	if got := bucketActive(t, st, "personal"); got != 1 {
		t.Errorf("personal bucket active = %d, want 1", got)
	}
	if got := bucketActive(t, st, "default"); got != 1 {
		t.Errorf("default bucket active = %d, want 1 (lone row, nothing to merge)", got)
	}
	if got := profileOf(t, st, defID); got != "default" {
		t.Errorf("default row profile = %q after synthesize, want default (unchanged)", got)
	}

	// Survivor frequencies must reflect ONLY same-bucket merges. Cross-profile
	// frequency transfer (a synthesizeBucket scoped to memProfileVisible, which
	// would pull the default row's frequency into the work/personal survivor)
	// would push these totals above the within-bucket expectation.
	if _, freq := bucketSurvivor(t, st, "work"); freq != 4 {
		t.Errorf("work survivor frequency = %d, want 4 (3 + 1, same bucket only)", freq)
	}
	if _, freq := bucketSurvivor(t, st, "personal"); freq != 3 {
		t.Errorf("personal survivor frequency = %d, want 3 (2 + 1, same bucket only)", freq)
	}
	if _, freq := bucketSurvivor(t, st, "default"); freq != 1 {
		t.Errorf("default survivor frequency = %d, want 1 (lone row, no merge, none transferred out)", freq)
	}
}

// TestMemForgetIsolation proves forget is scoped to the VISIBLE set: a profile
// cannot delete a SIBLING's row by its id/prefix, but can forget its own and
// default rows.
func TestMemForgetIsolation(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(content, profile string) string {
		r, err := st.remember(rememberInput{content: content, profile: profile})
		if err != nil {
			t.Fatal(err)
		}
		return r["id"].(string)
	}
	workID := mk("work confidential roadmap", "work")
	persID := mk("personal vacation plan", "personal")
	defID := mk("shared baseline note", "default")

	// personal cannot forget the work row by full id...
	if st.forget(workID, "personal") {
		t.Error("personal must NOT forget a work row by id")
	}
	// ...nor by prefix.
	if st.forget(workID[:8], "personal") {
		t.Error("personal must NOT forget a work row by prefix")
	}
	if bucketActive(t, st, "work") != 1 {
		t.Error("work row must still be present after a sibling's forget attempt")
	}
	// Reverse direction: work cannot forget the personal row by full id...
	if st.forget(persID, "work") {
		t.Error("work must NOT forget a personal row by id")
	}
	// ...nor by prefix.
	if st.forget(persID[:8], "work") {
		t.Error("work must NOT forget a personal row by prefix")
	}
	if bucketActive(t, st, "personal") != 1 {
		t.Error("personal row must still be present after work's forget attempt")
	}
	// A profile CAN forget its own row and the shared default row.
	if !st.forget(persID, "personal") {
		t.Error("personal should forget its own row")
	}
	if !st.forget(defID, "personal") {
		t.Error("personal should forget a default (shared) row")
	}
	// The owner can still forget its own work row.
	if !st.forget(workID, "work") {
		t.Error("work should forget its own row")
	}
}

// TestMemFtsSaturationIsolation proves the FTS pre-limit is applied AFTER the
// visible-profile filter: a flood of matching sibling rows must not evict a
// visible match from the top-50 FTS pre-limit. Seed many strong `personal`
// matches + a lone weak `work` match; recall under `work` must still surface
// the work row (a global pre-limit would rank the 50 strong personal rows first
// and drop the weak work row before the visible filter ran).
func TestMemFtsSaturationIsolation(t *testing.T) {
	st, err := newMemStore(":memory:", nil) // FTS-only
	if err != nil {
		t.Fatal(err)
	}
	// 60 strong personal matches (many "widget" occurrences => best bm25 rank).
	for i := 0; i < 60; i++ {
		c := fmt.Sprintf("widget widget widget widget widget personal filler %d", i)
		if _, err := st.remember(rememberInput{content: c, profile: "personal"}); err != nil {
			t.Fatal(err)
		}
	}
	// A lone, weak work match (single "widget").
	workContent := "quarterly widget target note"
	if _, err := st.remember(rememberInput{content: workContent, profile: "work"}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.recall("widget", 8, 100000, "", "", "work")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.content == workContent {
			found = true
		}
		if strings.Contains(h.content, "personal filler") {
			t.Errorf("work recall leaked a personal row: %q", h.content)
		}
	}
	if !found {
		t.Errorf("work row evicted by sibling FTS saturation; hits=%d", len(hits))
	}

	// Reverse direction: the invisible flood lives in `work` (a sibling that
	// `personal` cannot see) plus a few weak, visible `default` rows, and one
	// lone weak `personal` match. Correct code applies the LIMIT 50 pre-limit
	// AFTER the visible filter, so `personal`'s handful of visible rows all
	// survive and the personal row is returned. A global pre-limit would let the
	// 60 strong (invisible) work rows fill the top 50 and evict the weak
	// personal row before the visible filter ran.
	st2, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 60 strong work matches (invisible to personal) — the saturating flood.
	for i := 0; i < 60; i++ {
		c := fmt.Sprintf("gadget gadget gadget gadget gadget work filler %d", i)
		if _, err := st2.remember(rememberInput{content: c, profile: "work"}); err != nil {
			t.Fatal(err)
		}
	}
	// A few weak, VISIBLE default matches (single "gadget").
	for i := 0; i < 3; i++ {
		c := fmt.Sprintf("gadget default filler %d", i)
		if _, err := st2.remember(rememberInput{content: c, profile: "default"}); err != nil {
			t.Fatal(err)
		}
	}
	// A lone, weak personal match (single "gadget").
	persContent := "annual gadget review note"
	if _, err := st2.remember(rememberInput{content: persContent, profile: "personal"}); err != nil {
		t.Fatal(err)
	}
	phits, err := st2.recall("gadget", 8, 100000, "", "", "personal")
	if err != nil {
		t.Fatal(err)
	}
	pfound := false
	for _, h := range phits {
		if h.content == persContent {
			pfound = true
		}
		if strings.Contains(h.content, "work filler") {
			t.Errorf("personal recall leaked a work row: %q", h.content)
		}
	}
	if !pfound {
		t.Errorf("personal row evicted by work FTS saturation; hits=%d", len(phits))
	}
}

// guards the FTS query never produces a syntactically broken MATCH (would panic
// the recall path); ensures special chars are stripped, not passed through.
func TestMemFtsQuerySafe(t *testing.T) {
	for _, in := range []string{`a "b" c`, `drop; table--`, `*`, `()`} {
		q := memFtsQuery(in)
		if strings.ContainsAny(q, `;*()`) {
			t.Errorf("memFtsQuery(%q) = %q leaked an unsafe char", in, q)
		}
	}
}
