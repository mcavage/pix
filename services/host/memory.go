// pix memory service (host side). JSON-RPC 2.0 over HTTP; pure-Go sqlite
// (modernc) with FTS5 + a JSON-stored embedding per row; embeddings and the
// capture "watcher" run against the host's Ollama. One global store every
// sandbox talks to over host.docker.internal.
//
// Trust model: this service is UNAUTHENTICATED by design. It binds loopback
// (MEMORY_BIND=127.0.0.1) and is reached by sandboxes via the Docker Desktop
// host.docker.internal proxy. The deliberate assumption is single-user: your
// machine, your disposable VMs, your memory store, so any sandbox you launch may
// read/write it. Do not bind it to a routable interface or run it on a shared
// host without putting an auth proxy in front.
//
// Env: MEMORY_PORT (11435), MEMORY_BIND (127.0.0.1), MEMORY_DB
// (~/.local/share/pix/memory/memory.db), OLLAMA_HOST, MEMORY_EMBED_MODEL,
// MEMORY_WATCHER_MODEL.

package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

	"pix/host/config"
	"pix/host/plugin"
)

const (
	memRecencyHalflifeDays = 90.0
	memMinRelevance        = 0.15
	memVecFloor            = 0.45
	memVecCeil             = 0.8
	memProjectMatchBoost   = 1.5
	memProjectOtherFactor  = 0.5
)

// access_count/last_accessed/reward (used elsewhere in this file) are
// RESERVED/INERT, kept for additive/legacy compatibility; nothing writes or
// reads them any more. Same posture applies to any column below no longer
// written by every code path.
const memSchema = `
CREATE TABLE IF NOT EXISTS memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT, profile TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content);
`

// memSchemaVersion is the memory schema (PRAGMA user_version) THIS binary
// understands and stamps on open; newMemStore refuses a db claiming a newer
// one rather than silently downgrading it. Bumped from 1 to 2 by U5: a
// one-time v2 DATA sweep (migrateMemorySchema, below), not a column change
// (the table shape itself hasn't moved since v1). Shared with
// memory_snapshot.go's verifyMemoryDB via memSnapshotSchemaVersion so the two
// never drift.
const memSchemaVersion = 2

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

// migrateMemorySchema brings a store whose PRAGMA user_version is below
// memSchemaVersion up to it. This is a ONE-TIME DATA sweep, not a column
// migration: v2 does not add or rename any column. Two things happen, both
// inside a SINGLE transaction, so a crash or error anywhere leaves the store
// fully at its old version or fully v2, never a hybrid — completion is
// judged by user_version alone on the next open, never a partial column
// probe (see TestSchemaV2_CrashMidTransactionStaysAtOldVersion):
//
//  1. The legacy profile column is added if a pre-profile-scoping db still
//     lacks it (memColumnExists decides).
//  2. Every LIVE row whose historical source is neither 'user' nor 'cli' —
//     the watcher's past captures, or a source this binary has never seen —
//     is SOFT-DELETED (deleted_at, the store's existing forget() mechanism,
//     paired with dropping its FTS entry). This is an ADVISORY, one-time
//     reading of that pre-v2 free-text history, never a verified trust
//     boundary: pre-v2 `source` was operator-set text with no enforcement
//     behind it at all. A row already soft-deleted is left exactly alone.
//
// Reversibility is the SAME soft-delete semantics as an ordinary forget():
// clearing deleted_at for a specific id (directly in the db file, with the
// service stopped) restores that row exactly as recall() reads it — nothing
// new to learn. An operator who wants a point-in-time copy before upgrading
// should run `pix-host memory snapshot` first (see docs/memory.md); this
// migration does not take one automatically.
//
// A row written by the watcher (or anything else) AFTER this migration has
// stamped user_version=2 is never touched by it again: the sweep is
// keyed off curVersion alone and runs at most once per store.
func migrateMemorySchema(db *sql.DB, curVersion int) error {
	if curVersion >= memSchemaVersion {
		return nil
	}
	hasProfile, err := memColumnExists(db, "memories", "profile")
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
		// CREATE TABLE IF NOT EXISTS never alters an existing table, so a db
		// created before profile-scoping lacks this column. Legacy rows get
		// profile NULL = the default bucket.
		if _, err := tx.Exec("ALTER TABLE memories ADD COLUMN profile TEXT"); err != nil {
			return fmt.Errorf("schema migration (add profile column): %w", err)
		}
	}

	// Collect rowids before the UPDATE so the paired FTS delete below doesn't
	// need a second source-based scan (and stays exact even if source is
	// later changed by the UPDATE... it isn't, but this is the same
	// query-then-mutate shape softDelete/retireLegacyWatcherPerishableRows use).
	rows, err := tx.Query("SELECT rowid FROM memories WHERE deleted_at IS NULL AND source NOT IN ('user','cli')")
	if err != nil {
		return fmt.Errorf("schema migration (select non-explicit rows): %w", err)
	}
	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			rows.Close()
			return fmt.Errorf("schema migration (scan non-explicit rowid): %w", err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("schema migration (iterate non-explicit rows): %w", err)
	}
	rows.Close()

	if len(rowids) > 0 {
		if _, err := tx.Exec("UPDATE memories SET deleted_at = ? WHERE deleted_at IS NULL AND source NOT IN ('user','cli')", memNowIso()); err != nil {
			return fmt.Errorf("schema migration (soft-delete non-explicit rows): %w", err)
		}
		for _, rowid := range rowids {
			if _, err := tx.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
				return fmt.Errorf("schema migration (drop fts entry for rowid %d): %w", rowid, err)
			}
		}
	}

	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", memSchemaVersion)); err != nil {
		return fmt.Errorf("schema migration (stamp user_version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("schema migration: commit: %w", err)
	}
	committed = true
	log.Printf("memory: schema migration to v%d complete (%d row(s) soft-deleted, source not user/cli)", memSchemaVersion, len(rowids))
	return nil
}

// memKnownSources is the CLOSED vocabulary for the free-text `source` column:
// memNormSource maps anything outside it to "unknown" rather than storing
// arbitrary caller-supplied text verbatim. `source` is descriptive metadata
// only, but an unbounded free-text column is still worth pinning down: a
// typo or a new sandbox extension inventing its own label would otherwise
// silently fork the exact vocabulary migrateMemorySchema's (advisory)
// historical classification reads.
var memKnownSources = map[string]bool{"user": true, "cli": true, "watcher": true}

// memNormSource maps an incoming source string to the closed vocabulary:
// empty passes through (the caller applies its own default — "user" for an
// ordinary remember), a known value passes through unchanged, anything else
// normalizes to "unknown".
func memNormSource(s string) string {
	if s == "" || memKnownSources[s] {
		return s
	}
	return "unknown"
}

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

// memProfileVisible is the READ-time SQL WHERE fragment (one bound arg): the rows
// an active profile may see, its own UNION the default bucket. Bind the
// normalized active profile.
const memProfileVisible = "(profile IS NULL OR profile = '' OR profile = 'default' OR profile = ?)"

// memProfileStorage matches a row's EXACT storage bucket — the WRITE-time half,
// used for dedupe/reaffirm/synthesis/ownership, so `work` remembering text
// `personal` already has creates a NEW work row instead of reaffirming
// personal's. Bind the normalized profile.
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
	// unconditionally creates the table). A db written by a NEWER binary (version
	// > memSchemaVersion) must be refused loudly, never silently downgraded to
	// this binary's marker, that would corrupt a forward-incompatible schema.
	// Only proceed when the current version is already <= it.
	var curVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&curVersion); err != nil {
		return nil, err
	}
	if curVersion > memSchemaVersion {
		return nil, fmt.Errorf("database schema v%d is newer than this binary supports (%d), upgrade pix", curVersion, memSchemaVersion)
	}
	if _, err := db.Exec(memSchema); err != nil {
		return nil, err
	}
	// migrateMemorySchema is the ENTIRE story from here: it decides (from
	// curVersion) whether the one-time v2 sweep still needs to run, and if so
	// commits every ALTER, the classification UPDATE, and the user_version
	// stamp in ONE transaction. Completion is judged by user_version alone on
	// the NEXT open, never a column probe: see its doc comment.
	if err := migrateMemorySchema(db, curVersion); err != nil {
		return nil, err
	}
	if err := retireLegacyWatcherPerishableRows(db); err != nil {
		return nil, err
	}
	return &memStore{db: db, embedder: embedder}, nil
}

// retireLegacyWatcherPerishableRows is a one-time, idempotent startup state
// retirement (no schema change, no periodic sweep): the watcher's perishable
// event channel and the TTL-expiry sweep that used to garbage-collect it were
// both deleted, so a db written by an older binary can be left holding live,
// still-recallable perishable rows that were meant to eventually expire and
// now never will (nothing sweeps them any more — without this, they would
// become immortal instead of going away). It soft-deletes every remaining
// live perishable row, once: idempotent because a row already soft-deleted
// (deleted_at set) never matches the WHERE clause again, so every startup
// after the first is a no-op SELECT.
//
// This is a SOFT delete (deleted_at, matching softDelete's own convention),
// not a row purge, so it is reversible: clearing deleted_at for a specific id
// directly in the db file restores that row exactly as recall() reads it.
// Run before the store is handed out (no s.mu to take yet), operating on the
// raw *sql.DB like the schema/profile migrations above it.
func retireLegacyWatcherPerishableRows(db *sql.DB) error {
	rows, err := db.Query("SELECT rowid FROM memories WHERE durability = 'perishable' AND deleted_at IS NULL")
	if err != nil {
		return err
	}
	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			rows.Close()
			return err
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(rowids) == 0 {
		return nil
	}
	if _, err := db.Exec("UPDATE memories SET deleted_at = ? WHERE durability = 'perishable' AND deleted_at IS NULL", memNowIso()); err != nil {
		return err
	}
	// Drop each retired row's FTS entry too, matching softDelete's own pairing:
	// a soft-deleted row left in the index still answers keyword searches on an
	// unfiltered scan, even though every read path here also joins deleted_at.
	for _, rowid := range rowids {
		if _, err := db.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
			return err
		}
	}
	return nil
}

type memRow struct {
	id, kind, content, durability, source string
	confidence                            float64
	frequency                             int
	createdAt                             string
	project                               sql.NullString
	embedding                             sql.NullString
}

// bump reaffirms a row on a reaffirm/dedupe hit: bump its frequency (fed into
// recall's freqBoost) and confidence. It intentionally does NOT touch
// last_accessed: that column has no reader (see recall's comment) and
// reaffirm/dedupe is a WRITE-time event, unrelated to what last_accessed would
// even mean (a READ/access timestamp) if something ever did read it.
func (s *memStore) bump(id string, confidence float64) {
	s.db.Exec("UPDATE memories SET frequency = frequency + 1, confidence = ? WHERE id = ?",
		math.Min(1, confidence+0.05), id)
}

func (s *memStore) reaffirm(hash, profile string) string {
	var id string
	var conf float64
	// Same storage bucket only: a cross-profile hash collision must not reaffirm
	// (i.e. merge into) a sibling's row.
	if s.db.QueryRow("SELECT id, confidence FROM memories WHERE content_hash = ? AND deleted_at IS NULL AND "+memProfileStorage, hash, memNormProfile(profile)).Scan(&id, &conf) == nil {
		s.bump(id, conf)
		return id
	}
	return ""
}

func (s *memStore) findSimilar(vec []float64, threshold float64, profile string) (string, bool) {
	// Same storage bucket only (see reaffirm): dedupe must not collapse a new row
	// into a sibling profile's.
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
	content, kind, source, project string
	profile                        string
	hasProject                     bool
	confidence                     float64
	tags                           []string
	dedupe                         float64
	hasDedupe                      bool
}

// remember is the ONLY entry point reachable from an external caller — the
// JSON-RPC "remember" method and the go-plugin adapter's Remember both land
// here (see rememberFromParams). The `source` a caller sends is closed-
// vocabulary metadata (memNormSource), with one extra rule: a caller
// claiming source="watcher" is spoofing the internal capture path's own
// label (see rememberWatcherCapture, the ONLY other caller of
// rememberSourced and the only place "watcher" may ever be written), so it
// normalizes to "unknown" instead of being stored verbatim.
func (s *memStore) remember(in rememberInput) (jsonObj, error) {
	source := memNormSource(orDefault(in.source, "user"))
	if source == "watcher" {
		source = "unknown"
	}
	return s.rememberSourced(in, source)
}

// rememberWatcherCapture is the watcher's own internal capture path
// (memCapture), an internal Go call with no externally reachable parameter
// anywhere upstream of it — memCapture is reached from memObserve, which
// itself takes only free-text user input, never a source value. Unexported:
// nothing outside this package (and so nothing across the JSON-RPC or plugin
// boundary) can call it, and it is the ONLY path that ever writes
// source="watcher" to the store.
func (s *memStore) rememberWatcherCapture(in rememberInput) (jsonObj, error) {
	return s.rememberSourced(in, "watcher")
}

// rememberSourced is remember()'s (and rememberWatcherCapture's) shared
// body, parameterized on a source value the CALLER (not the request)
// supplies — see remember()'s doc comment for why an external caller cannot
// simply put "watcher" in the request and get the same value.
func (s *memStore) rememberSourced(in rememberInput, source string) (jsonObj, error) {
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
	// durability is no longer caller-configurable: every row written by this
	// binary is "durable" (the perishable/TTL behavior it used to gate was
	// removed along with the watcher's event channel). The column itself stays
	// in the schema, and legacy perishable rows already on disk are untouched,
	// pending the schema work that retires it.
	const durability = "durable"
	confidence := in.confidence
	if confidence == 0 {
		confidence = 0.8
	}
	tagsJSON, _ := json.Marshal(in.tags)
	if in.tags == nil {
		tagsJSON = []byte("[]")
	}
	created := memNowIso()

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
	// reward and expires_at are omitted here (not written at all): reward has no
	// write-path input any more (the column defaults to 0, its schema default),
	// and expiry was the perishable behavior removed above — every row written
	// now lives until explicitly forgotten, so there is never a value to bind.
	res, err := s.db.Exec(`INSERT INTO memories
		(id, kind, content, content_hash, durability, confidence, source, tags, project, created_at, embedding, profile)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, kind, content, hash, durability, confidence, source, string(tagsJSON), project, created, embJSON, profile)
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
	id, content, kind, durability, source string
	project                               sql.NullString
	score                                 float64
	createdAt                             string
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

	// The literal query "*" is "list everything" (what `pix memory recall '*'` and
	// a blank sandbox /recall send): relevance is pinned to 1 for every visible row
	// and neither FTS nor the embedder is consulted, so it still answers with
	// Ollama down. Everything downstream is the SAME path; only relevance and the
	// ordering differ.
	star := strings.TrimSpace(query) == "*"

	// FTS candidates → normalized [0,1] per id.
	ftsScore := map[string]float64{}
	if match := memFtsQuery(query); !star && match != "" {
		type fh struct {
			id  string
			val float64
		}
		hits := []fh{}
		// The visible-profile filter applies BEFORE the ORDER BY rank LIMIT 50:
		// ranking across ALL profiles first would let a flood of invisible sibling
		// rows evict the visible matches from the top 50.
		rows, err := s.db.Query("SELECT m.id, f.rank FROM memories_fts f JOIN memories m ON m.rowid = f.rowid WHERE f.content MATCH ? AND m.deleted_at IS NULL AND "+memProfileVisible+" ORDER BY f.rank LIMIT 50", match, memNormProfile(profile))
		if err == nil {
			for rows.Next() {
				var id string
				var bm float64
				if rows.Scan(&id, &bm) == nil {
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
	if s.embedder != nil && !star {
		queryVec = s.embedder(query)
	}

	// Candidates are the visible set for the active profile: its own rows UNION
	// the default bucket. An FTS-only hit on an invisible row is harmless — it
	// lands in ftsScore but this query never scans the row, so it cannot become
	// a candidate.
	// source is exposed so a caller (recall JSON, the TS extension's rendered
	// line) can tell an auto-captured row (source=watcher) apart from an
	// explicit one — the only feedback/undo mechanism is the existing
	// `/forget <id>`, so a user has to be able to SEE which rows are auto.
	where := "SELECT id, kind, content, durability, confidence, frequency, created_at, project, embedding, source FROM memories WHERE deleted_at IS NULL"
	args := []any{}
	if kind != "" {
		where += " AND kind = ?"
		args = append(args, kind)
	}
	where += " AND " + memProfileVisible
	args = append(args, memNormProfile(profile))
	if star {
		// Newest first. rowid, not created_at, is the authoritative insertion
		// sequence: created_at is wall-clock and can regress or tie (coarse
		// clock resolution, NTP step-back, two inserts in the same millisecond),
		// which would misorder "newest" if it were the sort key. rowid is an
		// autoincrementing counter that only ever reflects insertion order, so
		// it is used alone; created_at is still returned in the row for display.
		// Push the bounded caller limit into SQLite too: list-all must not scan
		// and materialize the entire visible store only to truncate it in Go.
		// Scored queries are ordered by score below instead.
		where += " ORDER BY rowid DESC LIMIT ?"
		args = append(args, limit)
	}
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
		if err := rows.Scan(&r.id, &r.kind, &r.content, &r.durability, &r.confidence, &r.frequency, &r.createdAt, &r.project, &r.embedding, &r.source); err != nil {
			continue
		}
		relevance := 1.0 // star: every visible row is equally "relevant"
		relVec, haveVec := 0.0, false
		if queryVec != nil && r.embedding.Valid {
			var v []float64
			if json.Unmarshal([]byte(r.embedding.String), &v) == nil {
				if len(v) != len(queryVec) {
					// The embedding model changed: memCosine would return 0 (silent
					// degradation), so count it and warn after the loop.
					dimMismatch++
				} else {
					c := memCosine(queryVec, v)
					relVec = math.Max(0, math.Min(1, (c-memVecFloor)/(memVecCeil-memVecFloor)))
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
			if relevance < memMinRelevance {
				continue
			}
		}
		ageDays := now.Sub(parseTime(r.createdAt)).Hours() / 24
		recency := math.Pow(2, -ageDays/memRecencyHalflifeDays)
		freqBoost := 1 + math.Log(float64(r.frequency))
		projectFactor := 1.0
		if project != "" {
			if r.project.Valid && r.project.String == project {
				projectFactor = memProjectMatchBoost
			} else if r.project.Valid && r.project.String != "" {
				projectFactor = memProjectOtherFactor
			}
		}
		// reward no longer factors into score: it was only ever seeded from the
		// watcher's removed valence signal, and the column stays inert (still
		// written by remember, never read here) until the schema work retires it.
		score := relevance * r.confidence * recency * freqBoost * projectFactor
		cands = append(cands, cand{scoredHit{r.id, r.content, r.kind, r.durability, r.source, r.project, score, r.createdAt}, score})
	}
	if dimMismatch > 0 {
		log.Printf("memory: %d stored embeddings have a different dimension than the current model (%d dims), they degrade to keyword-only. The embedding model likely changed; re-embed to restore semantic recall.", dimMismatch, len(queryVec))
	}

	// Scored queries rank by score; star keeps the SQL's newest-first order.
	if !star {
		sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	}

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
	// recall performs NO access_count/last_accessed writes, star or scored: no
	// reader anywhere consults either column (they never fed scoring — see the
	// dead `reward` note above for the same shape of leftover), so a write here
	// bought nothing but WAL churn on every read. The columns stay in the schema
	// as RESERVED/INERT for additive compatibility (an older binary's row, or a
	// future reader, still finds them present, just permanently 0/NULL from any
	// binary built after this change).
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
	s.softDelete(id, rowid, "forget")
	return true
}

// softDelete retires one row: stamp deleted_at and drop its FTS entry — the
// pair that must always happen together, since a soft-deleted row left in the
// index still answers keyword searches. rowid < 1 means "look it up"; a failed
// lookup skips the index delete rather than issue a silent no-op DELETE WHERE
// rowid=0 (0 is never a real rowid). Caller holds s.mu; failures are logged,
// not returned, because both callers are best-effort sweeps.
func (s *memStore) softDelete(id string, rowid int64, who string) {
	if rowid < 1 {
		if err := s.db.QueryRow("SELECT rowid FROM memories WHERE id = ?", id).Scan(&rowid); err != nil {
			log.Printf("%s: rowid lookup failed for %s: %v", who, id, err)
			rowid = -1
		}
	}
	if _, err := s.db.Exec("UPDATE memories SET deleted_at = ? WHERE id = ?", memNowIso(), id); err != nil {
		log.Printf("%s: soft-delete failed for %s: %v", who, id, err)
	}
	if rowid > 0 {
		if _, err := s.db.Exec("DELETE FROM memories_fts WHERE rowid = ?", rowid); err != nil {
			log.Printf("%s: FTS delete failed for rowid %d: %v", who, rowid, err)
		}
	}
}

// synthesize is the on-demand near-duplicate merge (JSON-RPC "synthesize"):
// no longer run on a periodic ticker (removed background sweep), and no
// longer sweeps TTL-expired rows (removed along with perishable/TTL
// behavior; there is no more background deletion). The response used to also
// report an "expired" count from that sweep; it had no caller left once the
// sweep was deleted, so it was removed from the response shape rather than
// kept around pinned at 0.
func (s *memStore) synthesize(threshold float64) jsonObj {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threshold == 0 {
		threshold = 0.93
	}
	// Merge WITHIN each storage bucket only: a merge must never compare or collapse
	// rows across profiles, and frequency must never move between buckets.
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
	return jsonObj{"merged": merged}
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
				s.softDelete(recs[j].id, 0, "synthesizeBucket") // merged into i
				dead[recs[j].id] = true
				merged++
			}
		}
	}
	return merged
}

// stats reports counts for the active profile's visible rows.
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
// on failure.
func memoryMux() http.Handler {
	store, err := buildMemStore()
	if err != nil {
		log.Fatalf("%v", err)
	}
	return newMemoryMux(store)
}

// newMemoryMux serves :11435 over an already-constructed IN-PROCESS store: it is
// memoryStoreMux (serve_plugin.go) over the same typed adapter the go-plugin
// unit serves. There is ONE JSON-RPC surface, and both the bare daemon and the
// supervised unit answer through it, so the two cannot drift.
func newMemoryMux(store *memStore) http.Handler {
	adapter := newMemoryStoreAdapter(store)
	return memoryStoreMux(func(fn func(plugin.MemoryStore) error) error { return fn(adapter) })
}

func runMemory() {
	// Store lock BEFORE opening the db, so the bare daemon is mutually exclusive
	// with `serve`, the memory plugin, and `restore` (see lock.go). Held for the
	// process lifetime; fails fast if another holder owns the db.
	release := lockMemoryStoreOrFatal(nil)
	defer release()
	// Apply config->env the SAME way `serve` does (applyMemoryModelEnv,
	// serve.go): the standalone daemon used to read model/capture-mode env vars
	// with no config.toml fallback at all, so `pix-host memory` silently
	// ignored memory_watcher_model/memory_embed_model/memory_capture unless the
	// caller set the env vars by hand. An explicit env override still wins; a
	// config load failure just logs and falls back to env-only, it never
	// blocks the daemon from starting.
	if cfg, err := config.Load(); err == nil {
		applyMemoryModelEnv(cfg)
	} else {
		log.Printf("memory: could not load config (%v); using env-only model/capture settings", err)
	}
	addr := env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")
	mux := memoryMux()
	log.Printf("memory service (json-rpc) on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// buildMemStore constructs the memory store. It returns an error rather than
// log.Fatalf-ing: called after a plugin subprocess has launched, a bare os.Exit
// would skip supervisor cleanup and orphan it. The caller routes the error
// through its cleanup-aware fatal; standalone callers may still fatal on it.
//
// The embedder is always the live, self-retrying memEmbed: there is no
// synchronous probe of Ollama here, so store construction (and therefore
// listener/watcher startup) never waits on a network round-trip. memEmbed
// itself latches semantic recall off on a real failure and re-probes on its
// own schedule (embedProbeInterval), so a recovered Ollama restores it with no
// daemon restart — see embedDisabled in memembed.go, which Health()/identity
// read live for the truth, instead of a boot-time snapshot.
func buildMemStore() (*memStore, error) {
	dbPath := config.MemoryDBPath()
	store, err := newMemStore(dbPath, memEmbed)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	return store, nil
}

// --- helpers ---------------------------------------------------------------

// memWatcherStatus reports whether capture is live and, when not, why.
// watcherCaptureAvailable re-probes (throttled) so a live recovery after
// `ollama pull` shows up without a daemon restart, and `pix doctor` reads the
// truth. Deliberately optimistic (see watcherCaptureAvailable): this is the
// ADMISSION check memObserve gates on, so a never-exercised watcher still
// gets to try its first real capture. For a HEALTH READING that must not lie
// about a fresh, unconfirmed watcher, use watcherHealthState instead.
func memWatcherStatus() (capture bool, reason string) {
	if watcherCaptureAvailable() {
		return true, ""
	}
	return false, getWatcherReason()
}

// watcherHealthState is memWatcherStatus's tri-state twin for `health`/
// `identity`: nil ("unknown") until watcherExercised flips true on the first
// real memWatch() attempt, so a store that has never actually captured
// anything reports "we don't know", never "healthy". Once exercised it
// reflects the SAME live watcherCaptureAvailable() check memWatcherStatus
// uses — including that check's own throttled live re-probe side effect
// (see watcherCaptureAvailable's doc comment).
func watcherHealthState() (state *bool, reason string) {
	capture, reason := memWatcherStatus()
	if !watcherExercised.Load() {
		return nil, ""
	}
	return &capture, reason
}

// memObserve is the ONE capture-admission path BOTH front ends use. explicit
// (the default) refuses immediately, before memWatcherStatus is ever
// consulted: zero watcher inference, zero side-effect Ollama probe.
// experimental-auto peeks the daily budget (watcherBudgetRemaining) BEFORE
// the watcher-availability gate, so an exhausted day is accepted:false at
// zero inference cost; memCapture peeks it again right before the watcher
// call. UX policy on an experimental feature, not a security boundary.
func memObserve(store *memStore, user, project string, hasProject bool, profile string) (accepted bool, reason string) {
	user = truncate(user, 8000)
	if strings.TrimSpace(user) == "" {
		return false, ""
	}
	if memCaptureMode() == config.MemoryCaptureExplicit {
		return false, "automatic capture is off (memory_capture=explicit); use explicit remember"
	}
	if remaining, err := store.watcherBudgetRemaining(); err != nil {
		return false, "capture budget check failed; try again shortly"
	} else if remaining <= 0 {
		return false, fmt.Sprintf("daily watcher capture budget exhausted (max %d stored rows/day); recall still works", memWatcherDailyBudget)
	}
	if capture, why := memWatcherStatus(); !capture {
		if why == "" {
			why = "watcher model unavailable, run `ollama pull " + memWatcherModel() + "` (or set MEMORY_WATCHER_MODEL)"
		}
		return false, why + "; recall still works"
	}
	select {
	case memCaptureSem <- struct{}{}:
		go memCapture(store, user, project, hasProject, profile)
		return true, ""
	default:
		return false, "capture busy (too many in flight); retry shortly, recall still works"
	}
}

func rememberFromParams(p jsonObj) rememberInput {
	// durability/ttlDays/reward are no longer read from an incoming request:
	// durability/ttlDays configured the perishable/TTL behavior that was
	// removed, and reward was never read back into recall's score even before
	// this, so all three are silently ignored now (every row is durable, with
	// the reward column defaulting to 0).
	in := rememberInput{
		content: getStr(p, "content"), kind: getStr(p, "kind"),
		source: getStr(p, "source"), confidence: numOr(p["confidence"], 0),
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

// memCaptureMaxConcurrency bounds in-flight background captures. Each makes a
// watcher-model (Ollama) call, so an unbounded `go memCapture` per observe entry
// lets one sandbox-issued JSON-RPC BATCH (thousands of observe calls inside the
// 1 MiB body cap) spawn thousands of goroutines and concurrent Ollama requests —
// a trivial local DoS. observe acquires the sem non-blockingly and applies
// honest backpressure (accepted:false) when saturated.
const memCaptureMaxConcurrency = 8

var memCaptureSem = make(chan struct{}, memCaptureMaxConcurrency)

func memCapture(store *memStore, user, project string, hasProj bool, profile string) {
	defer func() { recover() }()
	defer func() { <-memCaptureSem }()
	// Stage 1 (before the watcher): a secret-shaped message never reaches the
	// watcher model. Fail closed, never logs the matched text.
	if containsSecretShape(user) {
		log.Printf("memory: capture input blocked by the secret filter (stage 1), watcher not invoked")
		return
	}
	// Peek the daily budget again, fresh (memObserve already peeked it once at
	// admission time; this catches a concurrent capture landing in between).
	remaining, err := store.watcherBudgetRemaining()
	if err != nil {
		log.Printf("memory: watcher budget check failed, skipping this capture: %v", err)
		return
	}
	if remaining <= 0 {
		log.Printf("memory: daily watcher capture budget exhausted (max %d stored rows/day), watcher not invoked", memWatcherDailyBudget)
		return
	}
	// Make every capture attempt visible: memWatch logs its own errors, but a 200
	// with unparseable/empty content returns nil silently — the exact "capture on
	// but 0 facts" black box.
	log.Printf("memory: observe -> watcher (user %d chars, project %q, profile %q, budget remaining %d)", len(user), project, profile, remaining)
	w := memWatch(user)
	if w == nil {
		log.Printf("memory: watcher returned nil (no extraction), nothing captured")
		return
	}
	// A question-only user message asserts nothing, so any "facts" the watcher
	// extracted from it are a false positive (observed: "so are you using my
	// memories?" -> "user is using memory"). Dropped deterministically rather than
	// trusting the model. Corrections are exempt: a correction phrased as a polite
	// question ("Can you stop using em dashes?") is still a capturable rule.
	if len(w.Facts) > 0 && questionOnlyUserMessage(user) {
		log.Printf("memory: dropping %d fact(s), user message was question-only, no assertions to extract", len(w.Facts))
		w.Facts = nil
	}
	// Noise filter: facts are session narration about the user's activity ("user
	// asked...", "user ran...") often enough that a conservative prefix match is
	// worth the rare false drop. Corrections NEVER run through it: a legitimate
	// correction can be phrased exactly like the noise patterns.
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

	// Stage 2 (before storing): a watcher that echoes a secret back into an
	// extracted item must not get it stored either. Same fail-closed rule.
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

	// rememberWatcherCapture is the ONE call site allowed to write
	// source="watcher". One watcher result may store only the remaining budget
	// rows: stop storing, don't re-invoke the watcher, once used up. `stored`
	// tracks the daily budget ONLY against a row rememberWatcherCapture actually
	// INSERTED: a hash reaffirm or a vector-dedupe collapse (either way,
	// reaffirmed:true, see rememberSourced) touches an existing row, not a new
	// one, and watcherBudgetRemaining's own COUNT(*) never sees it either — so
	// counting it here would burn budget the persisted count agrees was never
	// spent. A call that errors (nothing landed at all) is likewise not counted.
	// This is what makes a batch that happens to include reaffirmed/semantic-
	// duplicate/failed items store every genuinely NEW item the remaining
	// budget allows, instead of stopping early on attempts that cost nothing.
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
		if stored >= remaining {
			log.Printf("memory: daily watcher capture budget exhausted mid-capture, dropping %d item(s) (count only, no content logged)", len(items)-i)
			break
		}
		res, err := store.rememberWatcherCapture(rememberInput{content: it.content, kind: it.kind,
			confidence: it.conf, project: project, hasProject: hasProj,
			profile: profile, dedupe: 0.9, hasDedupe: true})
		if err != nil {
			log.Printf("memory: watcher item failed to store, not counted against the daily budget: %v", err)
			continue
		}
		if reaffirmed, _ := res["reaffirmed"].(bool); reaffirmed {
			continue // hash or semantic dedupe collapsed into an existing row; no new row, no budget spent
		}
		stored++
	}
	// Truthful logging: "considered" (what the watcher extracted, post-filter)
	// is not the same as "stored" (what actually landed a NEW row and consumed
	// the daily budget — reaffirmed/deduped/failed items are excluded, matching
	// what watcherBudgetRemaining's own COUNT(*) would report).
	if len(items) > 0 {
		log.Printf("memory: capture considered %d item(s) (%d fact(s), %d correction(s)), stored %d new row(s)",
			len(items), len(w.Facts), len(w.Corrections), stored)
	} else {
		log.Printf("memory: watcher ran but extracted 0 items (nothing it judged worth keeping, or an empty result)")
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
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func parseTime(s string) time.Time {
	if t, ok := parseTimeStrict(s); ok {
		return t
	}
	return time.Now()
}

// parseTimeStrict is parseTime without the time.Now() fallback: a migration
// that decides whether to rewrite a row must never treat an unparseable
// timestamp as "now", which would silently shorten a row it should have left
// alone. Callers get an explicit ok=false instead.
func parseTimeStrict(s string) (time.Time, bool) {
	for _, f := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
