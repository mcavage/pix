// memory_plugin.go — adapts the built-in memory store (memory.go's *memStore) to
// the go-plugin `plugin.MemoryStore` interface, plus the self-exec serve entry
// point `pix-host plugin memory` runs. Every method maps 1:1 onto a *memStore
// method, or (observe/health) onto the SAME shared helper the JSON-RPC surface
// calls, so the two cannot drift.

package main

import (
	"log"

	"pix/host/plugin"

	goplugin "github.com/hashicorp/go-plugin"
)

// memoryStoreAdapter wraps the real *memStore and satisfies plugin.MemoryStore.
type memoryStoreAdapter struct {
	store *memStore
}

func newMemoryStoreAdapter(store *memStore) *memoryStoreAdapter {
	return &memoryStoreAdapter{store: store}
}

var _ plugin.MemoryStore = (*memoryStoreAdapter)(nil)

func (a *memoryStoreAdapter) Remember(req plugin.RememberReq) (plugin.RememberResp, error) {
	res, err := a.store.remember(rememberInput{
		content: req.Content, kind: req.Kind, source: req.Source,
		project: req.Project, profile: req.Profile, hasProject: req.HasProject,
		confidence: req.Confidence, tags: req.Tags,
		dedupe: req.Dedupe, hasDedupe: req.HasDedupe,
	})
	if err != nil {
		return plugin.RememberResp{}, err
	}
	id, _ := res["id"].(string)
	reaffirmed, _ := res["reaffirmed"].(bool)
	return plugin.RememberResp{ID: id, Reaffirmed: reaffirmed}, nil
}

func (a *memoryStoreAdapter) Recall(req plugin.RecallReq) (plugin.RecallResp, error) {
	hits, err := a.store.recall(req.Query, req.Limit, req.CharBudget, req.Kind, req.Project, req.Profile)
	if err != nil {
		return plugin.RecallResp{}, err
	}
	out := make([]plugin.Hit, 0, len(hits))
	for _, h := range hits {
		project := ""
		if h.project.Valid {
			project = h.project.String
		}
		out = append(out, plugin.Hit{
			CreatedAt: h.createdAt, ID: h.id, Content: h.content, Score: h.score,
			Kind: h.kind, Project: project, Source: h.source,
		})
	}
	return plugin.RecallResp{Hits: out}, nil
}

func (a *memoryStoreAdapter) Forget(req plugin.ForgetReq) (plugin.ForgetResp, error) {
	return plugin.ForgetResp{OK: a.store.forget(req.ID, req.Profile)}, nil
}

// Observe and Health call the shared helpers (memObserve, memWatcherStatus).
func (a *memoryStoreAdapter) Observe(req plugin.ObserveReq) (plugin.ObserveResp, error) {
	accepted, reason := memObserve(a.store, req.User, req.Project, req.HasProject, req.Profile)
	return plugin.ObserveResp{Accepted: accepted, Reason: reason}, nil
}

// Stats reports the requested profile's counts, so a plugin-backed :11435
// answers `stats {profile}` exactly as the in-process path does.
func (a *memoryStoreAdapter) Stats(profile string) (plugin.Stats, error) {
	s := a.store.stats(profile)
	get := func(k string) int { n, _ := s[k].(int); return n }
	return plugin.Stats{
		Active: get("active"), Facts: get("facts"), Learnings: get("learnings"), Deleted: get("deleted"),
	}, nil
}

// Health reports the CURRENT tri-state embed/capture health, both read live:
// Vector comes from embedHealthState (memembed.go), Capture from
// watcherHealthState (memory.go). Both are nil ("unknown") until a real
// attempt has actually happened — never a boot-time snapshot, and never a
// probe THIS call makes itself, though watcherHealthState's underlying check
// can trigger its own throttled live re-probe (see watcherCaptureAvailable).
func (a *memoryStoreAdapter) Health() (plugin.Health, error) {
	capture, reason := watcherHealthState()
	return plugin.Health{
		OK: true, Vector: embedHealthState(), Capture: capture,
		WatcherModel: memWatcherModel(), CaptureReason: reason,
	}, nil
}

// servePluginMemory builds the store (buildMemStore) and serves it as a
// go-plugin. Called by the `plugin memory` subcommand.
func servePluginMemory() {
	// Store lock BEFORE opening the db: this serves the LIVE store, so it must be
	// mutually exclusive with any other memory server and with `restore` (lock.go).
	// Held for the process lifetime; fails fast if another holder owns the db.
	release := lockMemoryStoreOrFatal(nil)
	defer release()
	store, err := buildMemStore()
	if err != nil {
		log.Fatalf("%v", err)
	}
	adapter := newMemoryStoreAdapter(store)
	plugin.Serve(map[string]goplugin.Plugin{"memory": &plugin.MemoryPlugin{Impl: adapter}})
}
