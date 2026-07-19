// pi-stack knowledge store (host side). A BUILT-IN retrieval index over OKF
// knowledge bundles, reusing the memory service's sqlite + FTS5 + embedding
// infrastructure (memFtsQuery / memEmbed / memCosine, all in the same package).
//
// The index is a DISPOSABLE cache of a git-managed bundle: it lives in its OWN
// sqlite file (knowledge.db), separate from memory.db, and can be rebuilt from
// the source bundles at any time via reindex(). Like memory, the vector half is
// optional — if the host Ollama / embed model is unavailable, embeddings are
// skipped and query() degrades to keyword-only FTS ranking.
//
// Env: KNOWLEDGE_DB (~/.local/share/pi-stack/knowledge/knowledge.db). Embeddings reuse the
// memory service's OLLAMA_HOST / MEMORY_EMBED_MODEL knobs via memEmbed.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"pi-stack/host/config"
	"pi-stack/host/okf"
	"pi-stack/host/plugin"
)

const knowledgeSchema = `
CREATE TABLE IF NOT EXISTS concepts (
  rowid INTEGER PRIMARY KEY, id TEXT NOT NULL, type TEXT, title TEXT, description TEXT,
  path TEXT, body TEXT, citations TEXT NOT NULL DEFAULT '[]', tags TEXT NOT NULL DEFAULT '[]',
  bundle TEXT NOT NULL, embedding TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS concepts_fts USING fts5(title, description, body);
`

// knowledgeSnippetWindow is the target length (chars) of the body slice returned
// with each hit.
const knowledgeSnippetWindow = 240

type knowledgeStore struct {
	db       *sql.DB
	mu       sync.Mutex
	embedder func(string) []float64 // nil if no embedder (keyword-only)
}

// newKnowledgeStore opens/creates the knowledge index at path. Mirrors
// newMemStore: single writer, WAL, idempotent schema.
func newKnowledgeStore(path string, embedder func(string) []float64) (*knowledgeStore, error) {
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
	if _, err := db.Exec(knowledgeSchema); err != nil {
		return nil, err
	}
	return &knowledgeStore{db: db, embedder: embedder}, nil
}

// reindex reads each bundle path with okf.ReadBundle and upserts every concept.
// A bundle is (re)indexed wholesale: its existing rows are dropped first, so a
// re-run neither duplicates concepts nor leaves behind ones deleted from the
// source. Returns the total concepts indexed and the bundle paths touched.
func (s *knowledgeStore) reindex(bundlePaths []string) (int, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	indexed := 0
	bundles := make([]string, 0, len(bundlePaths))
	for _, bp := range bundlePaths {
		// F5: validate the bundle root UP FRONT so a typo'd, missing, non-dir, or
		// unreadable path is a HARD error instead of "indexed 0 concept(s)" +
		// health ok:true + silent empty recall.
		if err := validateBundleRoot(bp); err != nil {
			return indexed, bundles, err
		}
		b, err := okf.ReadBundle(bp)
		if err != nil {
			return indexed, bundles, err
		}
		// F5: surface OKF warnings. Walk/read/rel failures under the root mean recall
		// is silently incomplete, so treat them as a hard error rather than reporting
		// success. Benign warnings (skipped symlinks, unparseable files) are logged
		// loudly but don't fail the reindex.
		if err := bundleWarningError(bp, b.Warnings); err != nil {
			return indexed, bundles, err
		}

		// Canonicalize the bundle path (abs + symlink-free + cleaned) so the
		// stored `bundle` id is independent of how the path was spelled — a query
		// filter canonicalizes the same way and always matches (design risk #1).
		canon := canonicalizeBundle(bp)

		// Drop the old rows for this bundle (concepts + their FTS entries) so the
		// re-index is idempotent and reflects deletions in the source.
		rows, qerr := s.db.Query("SELECT rowid FROM concepts WHERE bundle = ?", canon)
		if qerr == nil {
			var rids []int64
			for rows.Next() {
				var rid int64
				if rows.Scan(&rid) == nil {
					rids = append(rids, rid)
				}
			}
			rows.Close()
			for _, rid := range rids {
				s.db.Exec("DELETE FROM concepts_fts WHERE rowid = ?", rid)
			}
		}
		s.db.Exec("DELETE FROM concepts WHERE bundle = ?", canon)

		for _, c := range b.Concepts() {
			citationsJSON := marshalStrings(c.Citations)
			tagsJSON := marshalStrings(c.Tags)

			var embJSON any
			if s.embedder != nil {
				// Embed the human-meaningful text of the concept. Skip gracefully
				// (leave embedding NULL) if the model/Ollama is unavailable.
				if vec := s.embedder(embedText(c)); vec != nil {
					if raw, merr := json.Marshal(vec); merr == nil {
						embJSON = string(raw)
					}
				}
			}

			res, ierr := s.db.Exec(`INSERT INTO concepts
				(id, type, title, description, path, body, citations, tags, bundle, embedding)
				VALUES (?,?,?,?,?,?,?,?,?,?)`,
				c.ID, c.Type, c.Title, c.Description, c.Path, c.Body, citationsJSON, tagsJSON, canon, embJSON)
			if ierr != nil {
				return indexed, bundles, ierr
			}
			rowid, _ := res.LastInsertId()
			if _, ferr := s.db.Exec("INSERT INTO concepts_fts (rowid, title, description, body) VALUES (?, ?, ?, ?)",
				rowid, c.Title, c.Description, c.Body); ferr != nil {
				// Row is stored but not keyword-searchable. Surface it rather than
				// silently losing FTS recall (mirrors memory.go's remember()).
				log.Printf("knowledge: FTS index insert failed for %s (row kept, searchable by vector only): %v", c.ID, ferr)
			}
			indexed++
		}
		bundles = append(bundles, canon)
	}
	return indexed, bundles, nil
}

// query runs a hybrid keyword (FTS5) + vector (cosine, when embeddings are
// present) search and returns ranked, cited concepts. An empty bundles set
// searches all bundles; a non-empty set scopes the search to those bundles (so
// recall can restrict to e.g. {global, this-project}). Each filter is
// canonicalized the same way reindex stores it, so ids match regardless of path
// spelling (design risk #1). Scoring mirrors memory.go's recall(): FTS and
// vector relevances are each normalized to [0,1] and averaged when both present.
func (s *knowledgeStore) query(q string, bundles []string, limit int) []plugin.CitedConcept {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 8
	}

	// FTS candidates → normalized [0,1] per rowid (higher = better).
	ftsScore := map[int64]float64{}
	if match := memFtsQuery(q); match != "" {
		type fh struct {
			rid int64
			val float64
		}
		hits := []fh{}
		rows, err := s.db.Query("SELECT rowid, rank FROM concepts_fts WHERE concepts_fts MATCH ? ORDER BY rank LIMIT 50", match)
		if err == nil {
			for rows.Next() {
				var rid int64
				var bm float64
				if rows.Scan(&rid, &bm) == nil {
					hits = append(hits, fh{rid, -bm}) // negate: FTS5 rank is lower=better
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
				ftsScore[h.rid] = norm
			}
		}
	}

	var queryVec []float64
	if s.embedder != nil {
		queryVec = s.embedder(q)
	}

	where := "SELECT rowid, id, type, title, description, path, body, citations, bundle, embedding FROM concepts"
	args := []any{}
	if len(bundles) > 0 {
		placeholders := make([]string, len(bundles))
		for i, b := range bundles {
			placeholders[i] = "?"
			args = append(args, canonicalizeBundle(b))
		}
		where += " WHERE bundle IN (" + strings.Join(placeholders, ", ") + ")"
	}
	rows, err := s.db.Query(where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type cand struct {
		cc    plugin.CitedConcept
		score float64
	}
	cands := []cand{}
	dimMismatch := 0
	for rows.Next() {
		var rid int64
		var id, typ, title, desc, path, body, citationsJSON, bnd string
		var emb sql.NullString
		if rows.Scan(&rid, &id, &typ, &title, &desc, &path, &body, &citationsJSON, &bnd, &emb) != nil {
			continue
		}

		relVec, haveVec := 0.0, false
		if queryVec != nil && emb.Valid {
			var v []float64
			if json.Unmarshal([]byte(emb.String), &v) == nil {
				if len(v) != len(queryVec) {
					dimMismatch++
				} else {
					c := memCosine(queryVec, v)
					relVec = math.Max(0, math.Min(1, (c-memVecFloor)/(memVecCeil-memVecFloor)))
					haveVec = true
				}
			}
		}
		relFts, haveFts := ftsScore[rid]
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

		var citations []string
		if json.Unmarshal([]byte(citationsJSON), &citations) != nil {
			citations = nil
		}
		cands = append(cands, cand{plugin.CitedConcept{
			ID:          id,
			Type:        typ,
			Title:       title,
			Description: desc,
			Path:        path,
			Snippet:     knowledgeSnippet(body, q),
			Score:       relevance,
			Citations:   citations,
			Bundle:      bnd,
		}, relevance})
	}
	if dimMismatch > 0 {
		log.Printf("knowledge: %d stored embeddings have a different dimension than the current model (%d dims) — they degrade to keyword-only. The embedding model likely changed; reindex to restore semantic ranking.", dimMismatch, len(queryVec))
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	out := []plugin.CitedConcept{}
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		out = append(out, c.cc)
	}
	return out
}

// health reports index status: ok, whether the vector half is available, the
// distinct bundles indexed, and the total concept count.
func (s *knowledgeStore) health() plugin.KnowledgeHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	s.db.QueryRow("SELECT count(*) FROM concepts").Scan(&count)
	rows, _ := s.db.Query("SELECT DISTINCT bundle FROM concepts ORDER BY bundle")
	bundles := []string{}
	if rows != nil {
		for rows.Next() {
			var b string
			if rows.Scan(&b) == nil {
				bundles = append(bundles, b)
			}
		}
		rows.Close()
	}
	return plugin.KnowledgeHealth{
		OK:       true,
		Vector:   s.embedder != nil,
		Bundles:  bundles,
		Concepts: count,
	}
}

// buildKnowledgeStore constructs the store the way servePluginKnowledge uses it,
// mirroring buildMemStore: resolve the DB path, probe the embedder, wire it in
// only when available. Returns the store, whether the vector half is live, and
// any error.
//
// It returns the error rather than log.Fatalf-ing (F3): when called from runServe
// AFTER plugin subprocesses have already launched, a bare os.Exit would skip
// supervisor cleanup and orphan those subprocesses (which may hold the bearer).
// The caller routes the error through its cleanup-aware fatal path; standalone
// callers (servePluginKnowledge self-exec) may still fatal on it.
func buildKnowledgeStore() (*knowledgeStore, bool, error) {
	dbPath := config.KnowledgeDBPath()
	hasEmb := memEmbedderAvailable()
	var embedder func(string) []float64
	if hasEmb {
		embedder = memEmbed
	}
	store, err := newKnowledgeStore(dbPath, embedder)
	if err != nil {
		return nil, false, fmt.Errorf("knowledge: %w", err)
	}
	return store, hasEmb, nil
}

// --- helpers ---------------------------------------------------------------

// canonicalizeBundle normalizes a bundle path so its stored id is independent of
// how the path was spelled — relative vs absolute, through a symlink, or with
// redundant separators all collapse to one form (design risk #1). It resolves to
// an absolute, symlink-free, cleaned path; if the path does not exist (so
// EvalSymlinks fails) it falls back to the cleaned absolute form, keeping a
// filter for a missing bundle deterministic. An empty path stays empty.
func canonicalizeBundle(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// validateBundleRoot fail-loud checks a bundle root path before ReadBundle (F5):
// it must exist, be a directory, and be readable. A bad root returns a hard
// error so `knowledge use /typo` surfaces the problem instead of silently
// serving an empty index.
func validateBundleRoot(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("knowledge: empty bundle path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("knowledge: bundle root %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("knowledge: bundle root %q is not a directory", path)
	}
	if _, err := os.ReadDir(path); err != nil {
		return fmt.Errorf("knowledge: bundle root %q not readable: %w", path, err)
	}
	return nil
}

// bundleWarningError classifies the non-fatal warnings ReadBundle records (F5).
// Walk/read/rel failures mean files under the root couldn't be read, so recall
// is silently incomplete — those are promoted to a hard error. Benign warnings
// (skipped symlinks, unparseable concept files) are logged loudly but tolerated.
func bundleWarningError(path string, warnings []string) error {
	if len(warnings) == 0 {
		return nil
	}
	var hard []string
	for _, w := range warnings {
		if strings.HasPrefix(w, "walk ") || strings.HasPrefix(w, "read ") || strings.HasPrefix(w, "rel ") {
			hard = append(hard, w)
		} else {
			log.Printf("knowledge: bundle %q warning: %s", path, w)
		}
	}
	if len(hard) > 0 {
		return fmt.Errorf("knowledge: bundle %q had %d unrecoverable read error(s): %s", path, len(hard), strings.Join(hard, "; "))
	}
	return nil
}

// embedText is the concept text fed to the embedder: title + description + body.
func embedText(c *okf.Concept) string {
	return strings.TrimSpace(c.Title + "\n" + c.Description + "\n" + c.Body)
}

// marshalStrings JSON-encodes a string slice, normalizing nil to "[]".
func marshalStrings(in []string) string {
	if in == nil {
		return "[]"
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// knowledgeSnippet returns a relevant slice of body: a window centered on the
// earliest matching query term, else the head of the body. Ellipses mark
// truncation on either side.
func knowledgeSnippet(body, query string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	best := -1
	for _, t := range memWordRe.Split(strings.ToLower(query), -1) {
		if len(t) <= 1 {
			continue
		}
		if i := strings.Index(lower, t); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		if len(body) > knowledgeSnippetWindow {
			return strings.TrimSpace(body[:knowledgeSnippetWindow]) + "…"
		}
		return body
	}
	start := best - knowledgeSnippetWindow/2
	if start < 0 {
		start = 0
	}
	end := start + knowledgeSnippetWindow
	if end > len(body) {
		end = len(body)
	}
	snip := strings.TrimSpace(body[start:end])
	if start > 0 {
		snip = "…" + snip
	}
	if end < len(body) {
		snip = snip + "…"
	}
	return snip
}
