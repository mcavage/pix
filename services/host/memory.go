// pi-stack memory service (host side). Port of mcp/memory/{server,store,embeddings,
// watcher}.ts. JSON-RPC 2.0 over HTTP; pure-Go sqlite (modernc) with FTS5 + a
// JSON-stored embedding per row; embeddings + the capture "watcher" run against
// the host's Ollama. One global store every sandbox talks to over
// host.docker.internal.
//
// Trust model: this service is UNAUTHENTICATED by design. It binds loopback
// (MEMORY_BIND=127.0.0.1) and is reached by sandboxes via the Docker Desktop
// host.docker.internal proxy. The deliberate assumption is single-user: it's
// your machine, your disposable VMs, and your own memory store, so any sandbox
// you launch may read/write it. Do not bind it to a routable interface or run it
// on a shared host without putting an auth proxy in front.
//
// Env: MEMORY_PORT (11435), MEMORY_BIND (127.0.0.1), MEMORY_DB
// (~/.pi-stack/memory/memory.db), OLLAMA_HOST, MEMORY_EMBED_MODEL,
// MEMORY_WATCHER_MODEL, MEMORY_SYNTH_MS.

package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const (
	memRecencyHalflifeDays = 90.0
	memMinRelevance        = 0.15
	memVecFloor            = 0.45
	memVecCeil             = 0.8
	memProjectMatchBoost   = 1.5
	memProjectOtherFactor  = 0.5
)

const memSchema = `
CREATE TABLE IF NOT EXISTS memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT, profile TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content);
PRAGMA user_version = 1;
`

// memDefaultProfile is the shared base bucket. A memory with a NULL/empty/
// "default" profile lives here and is visible under every profile; a named
// profile only additionally sees its own rows. This mirrors the knowledge union
// model (default = shared base, a named profile = base UNION its own).
const memDefaultProfile = "default"

// memNormProfile canonicalizes a profile identifier: NULL/empty/"default" all
// collapse to the default bucket. Callers pass it through everywhere a profile
// crosses the store boundary so an absent param is always backward-compatible.
func memNormProfile(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return memDefaultProfile
	}
	return p
}

// memProfileVisible is the SQL WHERE fragment (with one bound arg) selecting the
// rows a given active profile may see: its own rows UNION the default bucket
// (NULL/”/'default'). Bind the normalized active profile as the single arg.
const memProfileVisible = "(profile IS NULL OR profile = '' OR profile = 'default' OR profile = ?)"

// memProfileStorage is the SQL WHERE fragment (with one bound arg) matching the
// EXACT storage bucket of a row: its normalized profile equals the bound value.
// Unlike memProfileVisible (the read-time union), this is the write-time bucket
// used for dedupe/reaffirm/synthesis/ownership — so `work` remembering text that
// `personal` already has creates a NEW work row rather than reaffirming
// personal's. Bind the normalized profile (memNormProfile) as the single arg.
const memProfileStorage = "COALESCE(NULLIF(profile,''),'default') = ?"

func memNowIso() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func memHash(s string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(h[:])
}

var memWordRe = regexp.MustCompile(`[^a-z0-9]+`)

func memFtsQuery(q string) string {
	parts := memWordRe.Split(strings.ToLower(q), -1)
	terms := []string{}
	for _, t := range parts {
		if len(t) > 1 {
			terms = append(terms, `"`+t+`"`)
		}
	}
	return strings.Join(terms, " OR ")
}

func memCosine(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type memStore struct {
	db       *sql.DB
	mu       sync.Mutex
	embedder func(string) []float64 // nil if no embedder
}

func newMemStore(path string, embedder func(string) []float64) (*memStore, error) {
	if path != ":memory:" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; keeps WAL + FTS simple
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, err
	}
	// A busy timeout so a concurrent legacy open (or the periodic synthesis tick)
	// waits briefly for the lock instead of returning SQLITE_BUSY immediately.
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		return nil, err
	}
	// Schema-version guard: read the CURRENT user_version BEFORE memSchema (which
	// unconditionally stamps 1). A db written by a NEWER binary (version > 1) must
	// be refused loudly, never silently downgraded to the 1 marker — that would
	// corrupt a forward-incompatible schema. Only proceed (and stamp 1) when the
	// current version is <= 1.
	var curVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&curVersion); err != nil {
		return nil, err
	}
	if curVersion > 1 {
		return nil, fmt.Errorf("database schema v%d is newer than this binary supports (1) — upgrade pi-stack", curVersion)
	}
	if _, err := db.Exec(memSchema); err != nil {
		return nil, err
	}
	// Idempotent migration: CREATE TABLE IF NOT EXISTS never alters an existing
	// table, so a DB created before profile-scoping lacks the column. Probe the
	// schema with PRAGMA table_info and ALTER only when the column is absent (more
	// robust than string-matching a "duplicate column name" driver error). Legacy
	// rows get profile NULL, which memNormProfile treats as the default bucket.
	hasProfile, err := memColumnExists(db, "memories", "profile")
	if err != nil {
		return nil, err
	}
	if !hasProfile {
		if _, err := db.Exec("ALTER TABLE memories ADD COLUMN profile TEXT"); err != nil {
			return nil, err
		}
	}
	// Stamp the current schema version. memSchema already sets it for a fresh DB,
	// but a migrated legacy DB predates the pragma — set it explicitly so
	// backup/restore can detect version drift later.
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		return nil, err
	}
	return &memStore{db: db, embedder: embedder}, nil
}

// memColumnExists reports whether table has a column named col, via
// PRAGMA table_info. Used to gate the idempotent profile-column migration.
func memColumnExists(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

type memRow struct {
	id, kind, content, durability, source, tags string
	confidence, reward                          float64
	frequency, accessCount                      int
	createdAt                                   string
	project                                     sql.NullString
	embedding                                   sql.NullString
}

func (s *memStore) bump(id string, confidence float64) {
	s.db.Exec("UPDATE memories SET frequency = frequency + 1, confidence = ?, last_accessed = ? WHERE id = ?",
		math.Min(1, confidence+0.05), memNowIso(), id)
}

func (s *memStore) reaffirm(hash, profile string) string {
	var id string
	var conf float64
	// Scope to the SAME storage bucket: a hash collision across profiles must NOT
	// reaffirm a sibling's row (that would leak/merge across profiles).
	if s.db.QueryRow("SELECT id, confidence FROM memories WHERE content_hash = ? AND deleted_at IS NULL AND "+memProfileStorage, hash, memNormProfile(profile)).Scan(&id, &conf) == nil {
		s.bump(id, conf)
		return id
	}
	return ""
}

func (s *memStore) findSimilar(vec []float64, threshold float64, profile string) (string, bool) {
	// Only scan rows in the same storage bucket: near-duplicate dedupe must never
	// collapse a new row into a sibling profile's existing row.
	rows, err := s.db.Query("SELECT id, confidence, embedding FROM memories WHERE deleted_at IS NULL AND embedding IS NOT NULL AND "+memProfileStorage, memNormProfile(profile))
	if err != nil {
		return "", false
	}
	defer rows.Close()
	best := threshold
	hit := ""
	for rows.Next() {
		var id, emb string
		var conf float64
		if rows.Scan(&id, &conf, &emb) != nil {
			continue
		}
		var v []float64
		if json.Unmarshal([]byte(emb), &v) != nil {
			continue
		}
		if c := memCosine(vec, v); c >= best {
			best = c
			hit = id
		}
	}
	return hit, hit != ""
}

type rememberInput struct {
	content, kind, durability, source, project string
	profile                                    string
	hasProject                                 bool
	ttlDays                                    int
	confidence, reward                         float64
	tags                                       []string
	dedupe                                     float64
	hasDedupe                                  bool
}

func (s *memStore) remember(in rememberInput) (jsonObj, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	content := strings.TrimSpace(in.content)
	if content == "" {
		return jsonObj{"id": "", "reaffirmed": false}, nil
	}
	hash := memHash(content)
	if id := s.reaffirm(hash, in.profile); id != "" {
		return jsonObj{"id": id, "reaffirmed": true}, nil
	}

	kind := orDefault(in.kind, "fact")
	durability := orDefault(in.durability, "durable")
	confidence := in.confidence
	if confidence == 0 {
		confidence = 0.8
	}
	source := orDefault(in.source, "user")
	tagsJSON, _ := json.Marshal(in.tags)
	if in.tags == nil {
		tagsJSON = []byte("[]")
	}
	reward := math.Max(-1, math.Min(1, in.reward))
	created := memNowIso()

	var expiresAt any
	if durability == "perishable" {
		ttl := in.ttlDays
		if ttl == 0 {
			ttl = 14
		}
		expiresAt = time.Now().UTC().Add(time.Duration(ttl) * 24 * time.Hour).Format(time.RFC3339Nano)
	}

	var embJSON any
	var vec []float64
	if s.embedder != nil {
		vec = s.embedder(content)
		if vec != nil {
			b, _ := json.Marshal(vec)
			embJSON = string(b)
		}
	}
	if in.hasDedupe && vec != nil {
		if id, ok := s.findSimilar(vec, in.dedupe, in.profile); ok {
			var conf float64
			s.db.QueryRow("SELECT confidence FROM memories WHERE id = ?", id).Scan(&conf)
			s.bump(id, conf)
			return jsonObj{"id": id, "reaffirmed": true}, nil
		}
	}

	var project any
	if in.hasProject && in.project != "" {
		project = in.project
	}
	profile := memNormProfile(in.profile)
	id := uuid.NewString()
	res, err := s.db.Exec(`INSERT INTO memories
		(id, kind, content, content_hash, durability, confidence, reward, source, tags, project, created_at, expires_at, embedding, profile)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, kind, content, hash, durability, confidence, reward, source, string(tagsJSON), project, created, expiresAt, embJSON, profile)
	if err != nil {
		return nil, err
	}
	rowid, _ := res.LastInsertId()
	if _, ferr := s.db.Exec("INSERT INTO memories_fts (rowid, content) VALUES (?, ?)", rowid, content); ferr != nil {
		// The row exists but won't be keyword-searchable. Surface it rather than
		// silently losing FTS recall (would otherwise only bite when Ollama is down
		// and vectors are unavailable).
		log.Printf("memory: FTS index insert failed for %s (row kept, searchable by vector only): %v", id, ferr)
	}
	return jsonObj{"id": id, "reaffirmed": false}, nil
}

type scoredHit struct {
	id, content, kind, durability string
	project                       sql.NullString
	score                         float64
}

func (s *memStore) recall(query string, limit, charBudget int, kind, project, profile string) ([]scoredHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit == 0 {
		limit = 8
	}
	if charBudget == 0 {
		charBudget = 1200
	}
	now := time.Now()

	s.db.Exec("UPDATE memories SET deleted_at = ? WHERE expires_at IS NOT NULL AND expires_at < ? AND deleted_at IS NULL", memNowIso(), memNowIso())

	// FTS candidates → normalized [0,1] per id.
	ftsScore := map[string]float64{}
	if match := memFtsQuery(query); match != "" {
		rowidToID := map[int64]string{}
		idRows, _ := s.db.Query("SELECT rowid, id FROM memories WHERE deleted_at IS NULL")
		for idRows.Next() {
			var rid int64
			var id string
			idRows.Scan(&rid, &id)
			rowidToID[rid] = id
		}
		idRows.Close()
		type fh struct {
			id  string
			val float64
		}
		hits := []fh{}
		// Join FTS to memories and apply the visible-profile filter BEFORE the
		// ORDER BY rank LIMIT 50: ranking across ALL profiles first would let a
		// flood of invisible sibling rows evict the visible matches from the top 50.
		rows, err := s.db.Query("SELECT f.rowid, f.rank FROM memories_fts f JOIN memories m ON m.rowid = f.rowid WHERE f.content MATCH ? AND m.deleted_at IS NULL AND "+memProfileVisible+" ORDER BY f.rank LIMIT 50", match, memNormProfile(profile))
		if err == nil {
			for rows.Next() {
				var rid int64
				var bm float64
				rows.Scan(&rid, &bm)
				if id, ok := rowidToID[rid]; ok {
					hits = append(hits, fh{id, -bm}) // higher = better
				}
			}
			rows.Close()
		}
		if len(hits) > 0 {
			min, max := hits[0].val, hits[0].val
			for _, h := range hits {
				if h.val < min {
					min = h.val
				}
				if h.val > max {
					max = h.val
				}
			}
			for _, h := range hits {
				norm := 1.0
				if max != min {
					norm = (h.val - min) / (max - min)
				}
				ftsScore[h.id] = norm
			}
		}
	}

	var queryVec []float64
	if s.embedder != nil {
		queryVec = s.embedder(query)
	}

	// Restrict candidates to the visible set for the active profile: its own rows
	// UNION the default bucket. An FTS-only hit on an invisible row is harmless —
	// it lands in ftsScore but the row is never scanned by this query, so it can
	// never become a candidate.
	where := "SELECT id, kind, content, durability, confidence, frequency, reward, created_at, project, embedding FROM memories WHERE deleted_at IS NULL"
	args := []any{}
	if kind != "" {
		where += " AND kind = ?"
		args = append(args, kind)
	}
	where += " AND " + memProfileVisible
	args = append(args, memNormProfile(profile))
	rows, err := s.db.Query(where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type cand struct {
		hit   scoredHit
		score float64
	}
	cands := []cand{}
	dimMismatch := 0
	for rows.Next() {
		var r memRow
		if err := rows.Scan(&r.id, &r.kind, &r.content, &r.durability, &r.confidence, &r.frequency, &r.reward, &r.createdAt, &r.project, &r.embedding); err != nil {
			continue
		}
		relVec, haveVec := 0.0, false
		if queryVec != nil && r.embedding.Valid {
			var v []float64
			if json.Unmarshal([]byte(r.embedding.String), &v) == nil {
				if len(v) != len(queryVec) {
					// Stored embedding has a different dimension than the current
					// model — the embedding model changed. memCosine would return 0
					// (silent degradation); count it and warn after the loop.
					dimMismatch++
				} else {
					c := memCosine(queryVec, v)
					relVec = math.Max(0, math.Min(1, (c-memVecFloor)/(memVecCeil-memVecFloor)))
					haveVec = true
				}
			}
		}
		relFts, haveFts := ftsScore[r.id]
		if !haveFts && !haveVec {
			continue
		}
		var relevance float64
		switch {
		case haveFts && haveVec:
			relevance = 0.5*relFts + 0.5*relVec
		case haveFts:
			relevance = relFts
		default:
			relevance = relVec
		}
		if relevance < memMinRelevance {
			continue
		}
		ageDays := now.Sub(parseTime(r.createdAt)).Hours() / 24
		recency := math.Pow(2, -ageDays/memRecencyHalflifeDays)
		freqBoost := 1 + math.Log(float64(r.frequency))
		rewardBoost := 1 + r.reward
		projectFactor := 1.0
		if project != "" {
			if r.project.Valid && r.project.String == project {
				projectFactor = memProjectMatchBoost
			} else if r.project.Valid && r.project.String != "" {
				projectFactor = memProjectOtherFactor
			}
		}
		score := relevance * r.confidence * recency * freqBoost * rewardBoost * projectFactor
		cands = append(cands, cand{scoredHit{r.id, r.content, r.kind, r.durability, r.project, score}, score})
	}
	if dimMismatch > 0 {
		log.Printf("memory: %d stored embeddings have a different dimension than the current model (%d dims) — they degrade to keyword-only. The embedding model likely changed; re-embed to restore semantic recall.", dimMismatch, len(queryVec))
	}

	// sort desc by score
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	out := []scoredHit{}
	used := 0
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		if used+len(c.hit.content) > charBudget && len(out) > 0 {
			break
		}
		out = append(out, c.hit)
		used += len(c.hit.content)
	}
	ts := memNowIso()
	for _, h := range out {
		s.db.Exec("UPDATE memories SET access_count = access_count + 1, last_accessed = ? WHERE id = ?", ts, h.id)
	}
	return out, nil
}

func (s *memStore) forget(idOrPrefix, profile string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Restrict id/prefix matching to the VISIBLE set for this profile so a profile
	// can't delete a sibling's row by guessing (or colliding on) its id/prefix.
	active := memNormProfile(profile)
	var rowid int64
	var id string
	err := s.db.QueryRow("SELECT rowid, id FROM memories WHERE id = ? AND deleted_at IS NULL AND "+memProfileVisible, idOrPrefix, active).Scan(&rowid, &id)
	if err != nil {
		rows, _ := s.db.Query("SELECT rowid, id FROM memories WHERE id LIKE ? AND deleted_at IS NULL AND "+memProfileVisible, idOrPrefix+"%", active)
		found := [][2]any{}
		for rows.Next() {
			var rid int64
			var i string
			rows.Scan(&rid, &i)
			found = append(found, [2]any{rid, i})
		}
		rows.Close()
		if len(found) != 1 {
			return false
		}
		rowid, id = found[0][0].(int64), found[0][1].(string)
	}
	if _, err := s.db.Exec("UPDATE memories SET deleted_at = ? WHERE id = ?", memNowIso(), id); err != nil {
		log.Printf("forget: soft-delete failed for %s: %v", id, err)
	}
	if _, err := s.db.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
		log.Printf("forget: FTS delete failed for rowid %d: %v", rowid, err)
	}
	return true
}

func (s *memStore) synthesize(threshold float64) jsonObj {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threshold == 0 {
		threshold = 0.93
	}
	res, _ := s.db.Exec("UPDATE memories SET deleted_at = ? WHERE expires_at IS NOT NULL AND expires_at < ? AND deleted_at IS NULL", memNowIso(), memNowIso())
	expired := int64(0)
	if res != nil {
		expired, _ = res.RowsAffected()
	}
	// Partition by storage bucket and merge WITHIN each bucket only: a merge must
	// never compare or collapse rows across profiles, and frequency must never
	// move between buckets. Fetch the distinct normalized profiles, then run the
	// existing pairwise merge per bucket.
	buckets := []string{}
	prows, _ := s.db.Query("SELECT DISTINCT COALESCE(NULLIF(profile,''),'default') FROM memories WHERE deleted_at IS NULL AND embedding IS NOT NULL")
	for prows.Next() {
		var p string
		prows.Scan(&p)
		buckets = append(buckets, p)
	}
	prows.Close()
	merged := 0
	for _, bucket := range buckets {
		merged += s.synthesizeBucket(bucket, threshold)
	}
	return jsonObj{"merged": merged, "expired": expired}
}

// synthesizeBucket runs the pairwise near-duplicate merge over a SINGLE storage
// bucket (normalized profile). The caller (synthesize) already holds s.mu.
func (s *memStore) synthesizeBucket(profile string, threshold float64) int {
	rows, _ := s.db.Query("SELECT id, confidence, frequency, embedding FROM memories WHERE deleted_at IS NULL AND embedding IS NOT NULL AND "+memProfileStorage+" ORDER BY frequency DESC, confidence DESC", profile)
	type rec struct {
		id         string
		confidence float64
		frequency  int
		vec        []float64
	}
	recs := []rec{}
	for rows.Next() {
		var r rec
		var emb string
		rows.Scan(&r.id, &r.confidence, &r.frequency, &emb)
		json.Unmarshal([]byte(emb), &r.vec)
		recs = append(recs, r)
	}
	rows.Close()
	dead := map[string]bool{}
	merged := 0
	for i := range recs {
		if dead[recs[i].id] || recs[i].vec == nil {
			continue
		}
		for j := i + 1; j < len(recs); j++ {
			if dead[recs[j].id] || recs[j].vec == nil {
				continue
			}
			if memCosine(recs[i].vec, recs[j].vec) >= threshold {
				if _, err := s.db.Exec("UPDATE memories SET frequency = frequency + ?, confidence = ? WHERE id = ?",
					recs[j].frequency, math.Min(1, recs[i].confidence+0.05), recs[i].id); err != nil {
					log.Printf("synthesizeBucket: update survivor frequency failed for %s: %v", recs[i].id, err)
				}
				// forget j (inline; we already hold the lock)
				var rowid int64
				if err := s.db.QueryRow("SELECT rowid FROM memories WHERE id = ?", recs[j].id).Scan(&rowid); err != nil {
					log.Printf("synthesizeBucket: rowid lookup failed for %s: %v", recs[j].id, err)
				}
				if _, err := s.db.Exec("UPDATE memories SET deleted_at = ? WHERE id = ?", memNowIso(), recs[j].id); err != nil {
					log.Printf("synthesizeBucket: soft-delete failed for %s: %v", recs[j].id, err)
				}
				if _, err := s.db.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
					log.Printf("synthesizeBucket: FTS delete failed for rowid %d: %v", rowid, err)
				}
				dead[recs[j].id] = true
				merged++
			}
		}
	}
	return merged
}

func (s *memStore) promotable(minFreq int, profile string) []jsonObj {
	if minFreq == 0 {
		minFreq = 3
	}
	rows, _ := s.db.Query("SELECT id, content, frequency, project FROM memories WHERE deleted_at IS NULL AND kind='learning' AND frequency >= ? AND "+memProfileVisible+" ORDER BY frequency DESC", minFreq, memNormProfile(profile))
	out := []jsonObj{}
	for rows.Next() {
		var id, content string
		var freq int
		var proj sql.NullString
		rows.Scan(&id, &content, &freq, &proj)
		out = append(out, jsonObj{"id": id, "content": content, "frequency": freq, "project": nullStr(proj)})
	}
	rows.Close()
	return out
}

func (s *memStore) stats(profile string) jsonObj {
	active := memNormProfile(profile)
	get := func(cond string) int {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM memories WHERE "+cond+" AND "+memProfileVisible, active).Scan(&n); err != nil {
			log.Printf("stats: query failed (%s): %v", cond, err)
		}
		return n
	}
	return jsonObj{
		"active":     get("deleted_at IS NULL"),
		"durable":    get("deleted_at IS NULL AND durability='durable'"),
		"perishable": get("deleted_at IS NULL AND durability='perishable'"),
		"facts":      get("deleted_at IS NULL AND kind='fact'"),
		"learnings":  get("deleted_at IS NULL AND kind='learning'"),
		"deleted":    get("deleted_at IS NOT NULL"),
	}
}

// --- JSON-RPC server ---------------------------------------------------------

// memoryMux is the standalone entry (runMemory): it builds the store and fatals
// on failure. runServe does NOT use this path — it calls buildMemStore directly
// and routes a failure through its cleanup-aware fatal (F3), then newMemoryMux.
func memoryMux() http.Handler {
	store, hasEmb, err := buildMemStore()
	if err != nil {
		log.Fatalf("%v", err)
	}
	return newMemoryMux(store, hasEmb)
}

// newMemoryMux builds the JSON-RPC handler over an already-constructed store, so
// runServe can construct the store with error handling and still share this
// surface byte-for-byte with the standalone path.
func newMemoryMux(store *memStore, hasEmb bool) http.Handler {
	methods := map[string]func(jsonObj) (any, error){
		"health": func(jsonObj) (any, error) {
			// watcherCaptureAvailable re-probes (throttled) so health reflects a live
			// recovery after `ollama pull`, and so `pi-stack doctor` reads the truth.
			capture := watcherCaptureAvailable()
			reason := ""
			if !capture {
				reason = getWatcherReason()
			}
			return jsonObj{"ok": true, "vector": hasEmb, "capture": capture, "captureReason": reason, "watcherModel": memWatcherModel()}, nil
		},
		"stats": func(p jsonObj) (any, error) { return store.stats(profileFromParams(p)), nil },
		"recall": func(p jsonObj) (any, error) {
			hits, err := store.recall(getStr(p, "query"), clampInt(p["limit"], 0, 0, 1000),
				clampInt(p["charBudget"], 0, 0, 1000000), getStr(p, "kind"), getStr(p, "project"), profileFromParams(p))
			if err != nil {
				return nil, err
			}
			list := []jsonObj{}
			for _, h := range hits {
				list = append(list, jsonObj{"id": h.id, "content": h.content, "score": h.score,
					"kind": h.kind, "durability": h.durability, "project": nullStr(h.project)})
			}
			return jsonObj{"hits": list}, nil
		},
		"remember": func(p jsonObj) (any, error) { return store.remember(rememberFromParams(p)) },
		"forget": func(p jsonObj) (any, error) {
			return jsonObj{"ok": store.forget(getStr(p, "id"), profileFromParams(p))}, nil
		},
		"synthesize": func(jsonObj) (any, error) { return store.synthesize(0), nil },
		"promotable": func(p jsonObj) (any, error) {
			return jsonObj{"candidates": store.promotable(clampInt(p["minFrequency"], 3, 1, 1000000), profileFromParams(p))}, nil
		},
		"observe": func(p jsonObj) (any, error) {
			user := truncate(getStr(p, "user"), 8000)
			project, hasProj := projectFromParams(p)
			profile := profileFromParams(p)
			if strings.TrimSpace(user) == "" {
				return jsonObj{"accepted": false}, nil
			}
			// Don't claim success when the watcher model can't run — the capture
			// would be silently dropped. Tell the caller why (recall still works).
			// watcherCaptureAvailable re-probes (throttled) so capture recovers the
			// moment the user pulls the model, without a daemon restart.
			if !watcherCaptureAvailable() {
				reason := getWatcherReason()
				if reason == "" {
					reason = "watcher model unavailable — run `ollama pull " + memWatcherModel() + "` (or set MEMORY_WATCHER_MODEL)"
				}
				return jsonObj{"accepted": false, "reason": reason + "; recall still works"}, nil
			}
			go memCapture(store, user, project, hasProj, profile)
			return jsonObj{"accepted": true}, nil
		},
	}

	handleOne := func(msg jsonObj) jsonObj {
		id := msg["id"]
		method, _ := msg["method"].(string)
		fn := methods[method]
		if fn == nil {
			return jsonObj{"jsonrpc": "2.0", "id": id, "error": jsonObj{"code": -32601, "message": "method not found"}}
		}
		params, _ := msg["params"].(map[string]any)
		if params == nil {
			params = jsonObj{}
		}
		res, err := fn(params)
		if err != nil {
			return jsonObj{"jsonrpc": "2.0", "id": id, "error": jsonObj{"code": -32603, "message": err.Error()}}
		}
		return jsonObj{"jsonrpc": "2.0", "id": id, "result": res}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, jsonObj{"error": "POST JSON-RPC only"})
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			writeJSON(w, http.StatusOK, jsonObj{"jsonrpc": "2.0", "id": nil, "error": jsonObj{"code": -32700, "message": "parse error"}})
			return
		}
		switch v := parsed.(type) {
		case []any:
			out := []jsonObj{}
			for _, mm := range v {
				if m, ok := mm.(map[string]any); ok {
					out = append(out, handleOne(m))
				}
			}
			writeJSON(w, http.StatusOK, out)
		case map[string]any:
			writeJSON(w, http.StatusOK, handleOne(v))
		}
	})
	return mux
}

func runMemory() {
	// Take the shared store lock BEFORE opening the db (memoryMux -> buildMemStore),
	// exactly as serve.go's built-in branch does, so the bare daemon is mutually
	// exclusive with `serve`, the memory plugin, and `restore`. Held for the
	// process lifetime; dropped when ListenAndServe returns (the OS also releases
	// the flock on process exit / signal). Fails fast (log.Fatalf) if another
	// holder owns the db instead of opening it anyway.
	release := lockMemoryStoreOrFatal(nil)
	defer release()
	addr := env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")
	mux := memoryMux()
	log.Printf("memory service (json-rpc) on http://%s", addr)
	// periodic synthesis is started inside buildMemStore via a goroutine
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// buildMemStore constructs the memory store. It returns an error rather than
// log.Fatalf-ing (F3): when called from runServe AFTER a plugin subprocess has
// launched, a bare os.Exit would skip supervisor cleanup and orphan it. The
// caller routes the error through its cleanup-aware fatal; standalone callers
// (memoryMux/runMemory, servePluginMemory self-exec) may still fatal on it.
func buildMemStore() (*memStore, bool, error) {
	dbPath := strings.TrimSpace(os.Getenv("MEMORY_DB"))
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".pi-stack", "memory", "memory.db")
	}
	hasEmb := memEmbedderAvailable()
	var embedder func(string) []float64
	if hasEmb {
		embedder = memEmbed
	}
	// Probe the capture-side watcher model so a missing/unpulled model is loud at
	// startup (and reflected in `observe`/`health`) instead of silently dropping
	// every captured fact. Async: don't block store init on an Ollama round-trip.
	go memWatcherProbe()
	store, err := newMemStore(dbPath, embedder)
	if err != nil {
		return nil, false, fmt.Errorf("memory: %w", err)
	}
	// periodic self-synthesis
	synthMs := 6 * 3600 * 1000
	if v := strings.TrimSpace(os.Getenv("MEMORY_SYNTH_MS")); v != "" {
		fmt.Sscanf(v, "%d", &synthMs)
	}
	go func() {
		t := time.NewTicker(time.Duration(synthMs) * time.Millisecond)
		for range t.C {
			r := store.synthesize(0)
			if m, _ := r["merged"].(int); m > 0 {
				log.Printf("synthesis: merged %v, expired %v", r["merged"], r["expired"])
			}
		}
	}()
	return store, hasEmb, nil
}

// --- helpers ---------------------------------------------------------------

func rememberFromParams(p jsonObj) rememberInput {
	in := rememberInput{
		content: getStr(p, "content"), kind: getStr(p, "kind"), durability: getStr(p, "durability"),
		source: getStr(p, "source"), confidence: numOr(p["confidence"], 0), reward: numOr(p["reward"], 0),
		ttlDays: clampInt(p["ttlDays"], 0, 0, 100000),
	}
	in.project, in.hasProject = projectFromParams(p)
	in.profile = profileFromParams(p)
	if d, ok := p["dedupe"]; ok {
		if f, ok2 := d.(float64); ok2 {
			in.dedupe, in.hasDedupe = f, true
		}
	}
	if t, ok := p["tags"].([]any); ok {
		for _, x := range t {
			if s, ok := x.(string); ok {
				in.tags = append(in.tags, s)
			}
		}
	}
	return in
}

func projectFromParams(p jsonObj) (string, bool) {
	v, ok := p["project"]
	if !ok || v == nil {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// profileFromParams reads the active profile from RPC params, normalizing an
// absent/empty value to the default bucket (so an un-scoped caller keeps the
// backward-compatible default-only behavior).
func profileFromParams(p jsonObj) string {
	return memNormProfile(getStr(p, "profile"))
}

func memCapture(store *memStore, user, project string, hasProj bool, profile string) {
	defer func() { recover() }()
	w := memWatch(user)
	if w == nil {
		return
	}
	rewardSeed := w.Valence * 0.3
	rem := func(content, kind, durability string, ttl int, conf float64) {
		store.remember(rememberInput{content: content, kind: kind, durability: durability, ttlDays: ttl,
			confidence: conf, reward: rewardSeed, source: "watcher", project: project, hasProject: hasProj,
			profile: profile, dedupe: 0.9, hasDedupe: true})
	}
	for _, f := range w.Facts {
		rem(f, "fact", "durable", 0, 0.65)
	}
	for _, e := range w.Events {
		rem(e, "fact", "perishable", 21, 0.6)
	}
	for _, c := range w.Corrections {
		rem(c, "learning", "durable", 0, 0.75)
	}
	if len(w.Facts)+len(w.Events)+len(w.Corrections) > 0 {
		log.Printf("captured %d fact(s), %d event(s), %d correction(s) (valence %v)", len(w.Facts), len(w.Events), len(w.Corrections), w.Valence)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
func numOr(v any, def float64) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return def
}
func nullStr(n sql.NullString) any {
	if n.Valid {
		return n.String
	}
	return nil
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func parseTime(s string) time.Time {
	for _, f := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Now()
}
