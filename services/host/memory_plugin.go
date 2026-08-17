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
// hasVector is the `hasEmb` value health() reports, so the typed surface and the
// JSON-RPC surface agree.
type memoryStoreAdapter struct {
	store     *memStore
	hasVector bool
}

func newMemoryStoreAdapter(store *memStore, hasVector bool) *memoryStoreAdapter {
	return &memoryStoreAdapter{store: store, hasVector: hasVector}
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
			Kind: h.kind, Durability: h.durability, Project: project,
		})
	}
	return plugin.RecallResp{Hits: out}, nil
}

func (a *memoryStoreAdapter) Forget(req plugin.ForgetReq) (plugin.ForgetResp, error) {
	return plugin.ForgetResp{OK: a.store.forget(req.ID, req.Profile)}, nil
}

func (a *memoryStoreAdapter) Synthesize(req plugin.SynthesizeReq) (plugin.SynthesizeResp, error) {
	res := a.store.synthesize(req.Threshold)
	merged, _ := res["merged"].(int)
	return plugin.SynthesizeResp{Merged: merged}, nil
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
		Active: get("active"), Durable: get("durable"), Perishable: get("perishable"),
		Facts: get("facts"), Learnings: get("learnings"), Deleted: get("deleted"),
	}, nil
}

func (a *memoryStoreAdapter) Health() (plugin.Health, error) {
	capture, reason := memWatcherStatus()
	return plugin.Health{
		OK: true, Vector: a.hasVector, Capture: capture,
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
	store, hasEmb, err := buildMemStore()
	if err != nil {
		log.Fatalf("%v", err)
	}
	adapter := newMemoryStoreAdapter(store, hasEmb)
	plugin.Serve(map[string]goplugin.Plugin{"memory": &plugin.MemoryPlugin{Impl: adapter}})
}
