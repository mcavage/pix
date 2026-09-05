// store.go is the schema, migration, and core remember/recall/forget/stats
// logic. Ported from services/host/memory.go (pix-v2 U2): behavior,
// on-disk schema, and migration semantics are unchanged, only the package
// name, the exported surface (typed structs instead of map[string]any
// JSON-RPC params), and config wiring differ.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
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
	recencyHalflifeDays = 90.0
	minRelevance        = 0.15
	vecFloor            = 0.45
	vecCeil             = 0.8
	projectMatchBoost   = 1.5
	projectOtherFactor  = 0.5
)

// access_count/last_accessed/reward/durability are RESERVED/INERT, kept for
// additive/legacy/on-disk compatibility only: nothing writes or reads them
// for any behavioral purpose any more.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT, profile TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content);
CREATE INDEX IF NOT EXISTS idx_memories_source_created_at ON memories(source, created_at);
`

// schemaVersion is the memory schema (PRAGMA user_version) this binary
// understands and stamps on open; Open refuses a db claiming a newer one
// rather than silently downgrading it. Shared with snapshot.go's
// verifyMemoryDB so the two never drift.
const schemaVersion = 2

func columnExists(db *sql.DB, table, col string) (bool, error) {
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

// migrateSchema brings a store whose PRAGMA user_version is below
// schemaVersion up to it. This is a ONE-TIME DATA sweep, not a column
// migration: v2 does not add or rename any column beyond the legacy
// pre-profile-scoping `profile` backfill. Everything happens inside a
// SINGLE transaction, so a crash or error anywhere leaves the store fully
// at its old version or fully v2, never a hybrid.
func migrateSchema(db *sql.DB, curVersion int) error {
	if curVersion >= schemaVersion {
		return nil
	}
	hasProfile, err := columnExists(db, "memories", "profile")
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("schema migration: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
				log.Printf("memory: schema migration rollback failed: %v", rbErr)
			}
		}
	}()

	if !hasProfile {
		if _, err := tx.Exec("ALTER TABLE memories ADD COLUMN profile TEXT"); err != nil {
			return fmt.Errorf("schema migration (add profile column): %w", err)
		}
	}

	nonExplicit, err := softDeleteMatchingTx(tx, "deleted_at IS NULL AND source NOT IN ('user','cli')", "non-explicit rows")
	if err != nil {
		return err
	}
	perishable, err := softDeleteMatchingTx(tx, "deleted_at IS NULL AND durability = 'perishable'", "legacy perishable rows")
	if err != nil {
		return err
	}

	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("schema migration (stamp user_version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("schema migration: commit: %w", err)
	}
	committed = true
	log.Printf("memory: schema migration to v%d complete (%d row(s) soft-deleted, source not user/cli; %d row(s) soft-deleted, legacy perishable)", schemaVersion, nonExplicit, perishable)
	return nil
}

// softDeleteMatchingTx collects the rowids matching where (which must itself
// scope to "deleted_at IS NULL"), soft-deletes them, and drops each one's
// FTS entry, all inside the caller's transaction.
func softDeleteMatchingTx(tx *sql.Tx, where, label string) (int, error) {
	rows, err := tx.Query("SELECT rowid FROM memories WHERE " + where)
	if err != nil {
		return 0, fmt.Errorf("schema migration (select %s): %w", label, err)
	}
	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			rows.Close()
			return 0, fmt.Errorf("schema migration (scan %s): %w", label, err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("schema migration (iterate %s): %w", label, err)
	}
	rows.Close()
	if len(rowids) == 0 {
		return 0, nil
	}
	if _, err := tx.Exec("UPDATE memories SET deleted_at = ? WHERE "+where, nowISO()); err != nil {
		return 0, fmt.Errorf("schema migration (soft-delete %s): %w", label, err)
	}
	for _, rowid := range rowids {
		if _, err := tx.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
			return 0, fmt.Errorf("schema migration (drop fts entry for rowid %d, %s): %w", rowid, label, err)
		}
	}
	return len(rowids), nil
}

// knownSources is the CLOSED vocabulary for the free-text `source` column.
var knownSources = map[string]bool{"user": true, "cli": true, "mcp": true, "watcher": true}

// normSource maps an incoming source string to the closed vocabulary: empty
// passes through (the caller applies its own default), a known value passes
// through unchanged, anything else normalizes to "unknown".
func normSource(s string) string {
	if s == "" || knownSources[s] {
		return s
	}
	return "unknown"
}

// DefaultProfile is the shared base bucket. A memory with a NULL/empty/
// "default" profile lives here and is visible under every profile; a named
// profile only additionally sees its own rows.
const DefaultProfile = "default"

// NormProfile canonicalizes a profile identifier: NULL/empty/"default" all
// collapse to the default bucket.
func NormProfile(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return DefaultProfile
	}
	return p
}

// profileVisible is the READ-time SQL WHERE fragment (one bound arg): the
// rows an active profile may see, its own UNION the default bucket.
const profileVisible = "(profile IS NULL OR profile = '' OR profile = 'default' OR profile = ?)"

// profileStorage matches a row's EXACT storage bucket, the WRITE-time half.
const profileStorage = "COALESCE(NULLIF(profile,''),'default') = ?"

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func hashContent(s string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(h[:])
}

var wordRe = regexp.MustCompile(`[^a-z0-9]+`)

func ftsQuery(q string) string {
	parts := wordRe.Split(strings.ToLower(q), -1)
	terms := []string{}
	for _, t := range parts {
		if len(t) > 1 {
			terms = append(terms, `"`+t+`"`)
		}
	}
	return strings.Join(terms, " OR ")
}

func cosine(a, b []float64) float64 {
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

// Embedder returns a vector for text, or nil if embedding is unavailable.
type Embedder func(string) []float64

// Store is the memory sqlite store: schema, migration, and every
// remember/recall/forget/stats operation. One Store per process; the
// standalone pix-memory server holds exactly one, opened once at startup.
type Store struct {
	db       *sql.DB
	path     string
	mu       sync.Mutex
	embedder Embedder // nil if no embedder
}

// Open opens (creating if absent) the sqlite store at path, applies the
// schema and any pending migration, and returns a ready Store. embedder may
// be nil (keyword-only recall).
func Open(path string, embedder Embedder) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, path: path, embedder: embedder}, nil
}

// openDB does the actual sqlite open + pragmas + schema + migration; shared
// by Open and Restore (which reopens at the same path after swapping the
// underlying file).
func openDB(path string) (*sql.DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; keeps WAL + FTS simple
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, err
	}
	var curVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&curVersion); err != nil {
		db.Close()
		return nil, err
	}
	if curVersion > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("database schema v%d is newer than this binary supports (%d), upgrade pix-memory", curVersion, schemaVersion)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateSchema(db, curVersion); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the underlying sqlite handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// Path is the sqlite file this Store was opened against.
func (s *Store) Path() string { return s.path }

// Ping reports whether the underlying sqlite handle is reachable, the
// readiness half of /healthz.
func (s *Store) Ping() error {
	return s.db.Ping()
}

// SchemaVersion is the on-disk PRAGMA user_version this store is stamped at
// (always schemaVersion once Open has returned successfully).
func (s *Store) SchemaVersion() int { return schemaVersion }

type memRow struct {
	id, kind, content, source string
	confidence                float64
	frequency                 int
	createdAt                 string
	project                   sql.NullString
	embedding                 sql.NullString
}

// bump reaffirms a row on a reaffirm/dedupe hit: bump its frequency (fed
// into recall's freqBoost) and confidence.
func (s *Store) bump(id string, confidence float64) {
	s.db.Exec("UPDATE memories SET frequency = frequency + 1, confidence = ? WHERE id = ?",
		math.Min(1, confidence+0.05), id)
}

func (s *Store) reaffirm(hash, profile string) string {
	var id string
	var conf float64
	if s.db.QueryRow("SELECT id, confidence FROM memories WHERE content_hash = ? AND deleted_at IS NULL AND "+profileStorage, hash, NormProfile(profile)).Scan(&id, &conf) == nil {
		s.bump(id, conf)
		return id
	}
	return ""
}

func (s *Store) findSimilar(vec []float64, threshold float64, profile string) (string, bool) {
	rows, err := s.db.Query("SELECT id, confidence, embedding FROM memories WHERE deleted_at IS NULL AND embedding IS NOT NULL AND "+profileStorage, NormProfile(profile))
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
		if c := cosine(vec, v); c >= best {
			best = c
			hit = id
		}
	}
	return hit, hit != ""
}

// RememberInput is the remember/reaffirm request shape.
type RememberInput struct {
	Content, Kind, Source, Project string
	Profile                        string
	HasProject                     bool
	Confidence                     float64
	Tags                           []string
	Dedupe                         float64
	HasDedupe                      bool
}

// RememberResult reports what Remember actually did.
type RememberResult struct {
	ID             string
	Reaffirmed     bool
	BudgetExceeded bool
}

// Remember is the ONLY externally reachable insertion path (the
// memory_remember MCP tool). A caller claiming source="watcher" is spoofing
// the internal capture path's own label, so it normalizes to "unknown"
// instead of being stored verbatim; only rememberWatcherCapture (capture.go)
// may write source="watcher".
func (s *Store) Remember(in RememberInput) (RememberResult, error) {
	source := normSource(orDefault(in.Source, "user"))
	if source == "watcher" {
		source = "unknown"
	}
	return s.rememberSourced(in, source)
}

// rememberWatcherCapture is the watcher's own internal capture path
// (capture.go); unexported so nothing outside this package can call it.
func (s *Store) rememberWatcherCapture(in RememberInput) (RememberResult, error) {
	return s.rememberSourced(in, "watcher")
}

// rememberSourced is Remember's (and rememberWatcherCapture's) shared body.
// Deliberately THREE phases: the embed is a NETWORK call to Ollama that can
// take seconds, while s.mu is the one lock every recall also serializes
// through.
//
//  1. locked: exact-hash reaffirm, so "same fact again" never pays for an embed.
//  2. UNLOCKED: the embed.
//  3. locked: REVALIDATE the hash, then dedupe, budget, and INSERT atomically.
func (s *Store) rememberSourced(in RememberInput, source string) (RememberResult, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return RememberResult{}, nil
	}
	hash := hashContent(content)

	s.mu.Lock()
	if id := s.reaffirm(hash, in.Profile); id != "" {
		s.mu.Unlock()
		return RememberResult{ID: id, Reaffirmed: true}, nil
	}
	s.mu.Unlock()

	var embJSON any
	var vec []float64
	if s.embedder != nil {
		vec = s.embedder(content)
		if vec != nil {
			b, _ := json.Marshal(vec)
			embJSON = string(b)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.reaffirm(hash, in.Profile); id != "" {
		return RememberResult{ID: id, Reaffirmed: true}, nil
	}

	kind := orDefault(in.Kind, "fact")
	const durability = "durable"
	confidence := in.Confidence
	if confidence == 0 {
		confidence = 0.8
	}
	tagsJSON, _ := json.Marshal(in.Tags)
	if in.Tags == nil {
		tagsJSON = []byte("[]")
	}
	created := nowISO()

	if in.HasDedupe && vec != nil {
		if id, ok := s.findSimilar(vec, in.Dedupe, in.Profile); ok {
			var conf float64
			s.db.QueryRow("SELECT confidence FROM memories WHERE id = ?", id).Scan(&conf)
			s.bump(id, conf)
			return RememberResult{ID: id, Reaffirmed: true}, nil
		}
	}

	if source == "watcher" {
		used, err := s.watcherUsedToday()
		if err != nil {
			return RememberResult{}, err
		}
		if used >= watcherDailyBudget {
			return RememberResult{BudgetExceeded: true}, nil
		}
	}

	var project any
	if in.HasProject && in.Project != "" {
		project = in.Project
	}
	profile := NormProfile(in.Profile)
	id := uuid.NewString()
	res, err := s.db.Exec(`INSERT INTO memories
		(id, kind, content, content_hash, durability, confidence, source, tags, project, created_at, embedding, profile)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, kind, content, hash, durability, confidence, source, string(tagsJSON), project, created, embJSON, profile)
	if err != nil {
		return RememberResult{}, err
	}
	rowid, _ := res.LastInsertId()
	if _, ferr := s.db.Exec("INSERT INTO memories_fts (rowid, content) VALUES (?, ?)", rowid, content); ferr != nil {
		log.Printf("memory: FTS index insert failed for %s (row kept, searchable by vector only): %v", id, ferr)
	}
	return RememberResult{ID: id}, nil
}

// ScoredHit is one recall result row.
type ScoredHit struct {
	ID, Content, Kind, Source, Project string
	Score                              float64
	CreatedAt                          string
}

// Recall searches stored facts. query == "*" is "list everything": relevance
// is pinned to 1 for every visible row, newest first, and neither FTS nor
// the embedder is consulted, so it still answers with Ollama down.
func (s *Store) Recall(query string, limit, charBudget int, kind, project, profile string) ([]ScoredHit, error) {
	if limit == 0 {
		limit = 8
	}
	if charBudget == 0 {
		charBudget = 1200
	}
	now := time.Now()
	star := strings.TrimSpace(query) == "*"

	var queryVec []float64
	if s.embedder != nil && !star {
		queryVec = s.embedder(query)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ftsScore := map[string]float64{}
	if match := ftsQuery(query); !star && match != "" {
		type fh struct {
			id  string
			val float64
		}
		hits := []fh{}
		rows, err := s.db.Query("SELECT m.id, f.rank FROM memories_fts f JOIN memories m ON m.rowid = f.rowid WHERE f.content MATCH ? AND m.deleted_at IS NULL AND "+profileVisible+" ORDER BY f.rank LIMIT 50", match, NormProfile(profile))
		if err == nil {
			for rows.Next() {
				var id string
				var bm float64
				if rows.Scan(&id, &bm) == nil {
					hits = append(hits, fh{id, -bm})
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

	where := "SELECT id, kind, content, confidence, frequency, created_at, project, embedding, source FROM memories WHERE deleted_at IS NULL"
	args := []any{}
	if kind != "" {
		where += " AND kind = ?"
		args = append(args, kind)
	}
	where += " AND " + profileVisible
	args = append(args, NormProfile(profile))
	if star {
		where += " ORDER BY rowid DESC LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type cand struct {
		hit   ScoredHit
		score float64
	}
	cands := []cand{}
	dimMismatch := 0
	for rows.Next() {
		var r memRow
		if err := rows.Scan(&r.id, &r.kind, &r.content, &r.confidence, &r.frequency, &r.createdAt, &r.project, &r.embedding, &r.source); err != nil {
			continue
		}
		relevance := 1.0
		relVec, haveVec := 0.0, false
		if queryVec != nil && r.embedding.Valid {
			var v []float64
			if json.Unmarshal([]byte(r.embedding.String), &v) == nil {
				if len(v) != len(queryVec) {
					dimMismatch++
				} else {
					c := cosine(queryVec, v)
					relVec = math.Max(0, math.Min(1, (c-vecFloor)/(vecCeil-vecFloor)))
					haveVec = true
				}
			}
		}
		relFts, haveFts := ftsScore[r.id]
		if !star {
			if !haveFts && !haveVec {
				continue
			}
			switch {
			case haveFts && haveVec:
				relevance = 0.5*relFts + 0.5*relVec
			case haveFts:
				relevance = relFts
			default:
				relevance = relVec
			}
			if relevance < minRelevance {
				continue
			}
		}
		ageDays := now.Sub(parseTime(r.createdAt)).Hours() / 24
		recency := math.Pow(2, -ageDays/recencyHalflifeDays)
		freqBoost := 1 + math.Log(float64(r.frequency))
		projectFactor := 1.0
		if project != "" {
			if r.project.Valid && r.project.String == project {
				projectFactor = projectMatchBoost
			} else if r.project.Valid && r.project.String != "" {
				projectFactor = projectOtherFactor
			}
		}
		score := relevance * r.confidence * recency * freqBoost * projectFactor
		cands = append(cands, cand{ScoredHit{r.id, r.content, r.kind, r.source, "", score, r.createdAt}, score})
		if r.project.Valid {
			cands[len(cands)-1].hit.Project = r.project.String
		}
	}
	if dimMismatch > 0 {
		log.Printf("memory: %d stored embeddings have a different dimension than the current model (%d dims), they degrade to keyword-only. The embedding model likely changed; re-embed to restore semantic recall.", dimMismatch, len(queryVec))
	}

	if !star {
		sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	}

	out := []ScoredHit{}
	used := 0
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		if used+len(c.hit.Content) > charBudget && len(out) > 0 {
			break
		}
		out = append(out, c.hit)
		used += len(c.hit.Content)
	}
	return out, nil
}

// Forget soft-deletes one fact by exact id or unambiguous prefix, restricted
// to rows visible in profile. Reports whether anything was deleted.
func (s *Store) Forget(idOrPrefix, profile string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := NormProfile(profile)
	var rowid int64
	var id string
	err := s.db.QueryRow("SELECT rowid, id FROM memories WHERE id = ? AND deleted_at IS NULL AND "+profileVisible, idOrPrefix, active).Scan(&rowid, &id)
	if err != nil {
		rows, _ := s.db.Query("SELECT rowid, id FROM memories WHERE id LIKE ? AND deleted_at IS NULL AND "+profileVisible, idOrPrefix+"%", active)
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
	s.softDelete(id, rowid, "forget")
	return true
}

func (s *Store) softDelete(id string, rowid int64, who string) {
	if rowid < 1 {
		if err := s.db.QueryRow("SELECT rowid FROM memories WHERE id = ?", id).Scan(&rowid); err != nil {
			log.Printf("%s: rowid lookup failed for %s: %v", who, id, err)
			rowid = -1
		}
	}
	if _, err := s.db.Exec("UPDATE memories SET deleted_at = ? WHERE id = ?", nowISO(), id); err != nil {
		log.Printf("%s: soft-delete failed for %s: %v", who, id, err)
	}
	if rowid > 0 {
		if _, err := s.db.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
			log.Printf("%s: FTS delete failed for rowid %d: %v", who, rowid, err)
		}
	}
}

// Stats reports counts for the active profile's visible rows.
type Stats struct {
	Active, Facts, Learnings, Deleted int
}

func (s *Store) Stats(profile string) Stats {
	active := NormProfile(profile)
	get := func(cond string) int {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM memories WHERE "+cond+" AND "+profileVisible, active).Scan(&n); err != nil {
			log.Printf("stats: query failed (%s): %v", cond, err)
		}
		return n
	}
	return Stats{
		Active:    get("deleted_at IS NULL"),
		Facts:     get("deleted_at IS NULL AND kind='fact'"),
		Learnings: get("deleted_at IS NULL AND kind='learning'"),
		Deleted:   get("deleted_at IS NOT NULL"),
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func parseTime(s string) time.Time {
	if t, ok := parseTimeStrict(s); ok {
		return t
	}
	return time.Now()
}

func parseTimeStrict(s string) (time.Time, bool) {
	for _, f := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
