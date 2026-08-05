// memory_plugin.go — adapts the built-in memory store (memory.go's *memStore)
// to the go-plugin `plugin.MemoryStore` interface, and provides a self-exec
// serve entry point.
//
// This is the "same process, typed RPC surface" bridge: the JSON-RPC front-end
// in memoryMux() stays as-is, and this adapter re-expresses the same eight
// store operations (remember, recall, forget, synthesize, promotable, observe,
// stats, health) as the typed calls the plugin transport speaks. Every method
// maps 1:1 onto an existing *memStore method or, for observe/health, mirrors the
// exact branch logic in memoryMux() so behaviour does not drift.
//
// The later `plugin` subcommand unit calls servePluginMemory(); nothing is
// registered in main.go here.

package main

import (
	"log"

	"pix/host/plugin"

	goplugin "github.com/hashicorp/go-plugin"
)

// memoryStoreAdapter wraps the real *memStore and satisfies plugin.MemoryStore.
// hasVector mirrors the `hasEmb` value memoryMux() reports in health() so the
// typed surface and the JSON-RPC surface agree.
type memoryStoreAdapter struct {
	store     *memStore
	hasVector bool
}

// newMemoryStoreAdapter builds an adapter around an existing store.
func newMemoryStoreAdapter(store *memStore, hasVector bool) *memoryStoreAdapter {
	return &memoryStoreAdapter{store: store, hasVector: hasVector}
}

var _ plugin.MemoryStore = (*memoryStoreAdapter)(nil)

func (a *memoryStoreAdapter) Remember(req plugin.RememberReq) (plugin.RememberResp, error) {
	in := rememberInput{
		content:    req.Content,
		kind:       req.Kind,
		durability: req.Durability,
		source:     req.Source,
		project:    req.Project,
		profile:    req.Profile,
		hasProject: req.HasProject,
		ttlDays:    req.TTLDays,
		confidence: req.Confidence,
		reward:     req.Reward,
		tags:       req.Tags,
		dedupe:     req.Dedupe,
		hasDedupe:  req.HasDedupe,
	}
	res, err := a.store.remember(in)
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
			CreatedAt:  h.createdAt,
			ID:         h.id,
			Content:    h.content,
			Score:      h.score,
			Kind:       h.kind,
			Durability: h.durability,
			Project:    project,
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
	expired, _ := res["expired"].(int64)
	return plugin.SynthesizeResp{Merged: merged, Expired: expired}, nil
}

func (a *memoryStoreAdapter) Promotable(req plugin.PromotableReq) (plugin.PromotableResp, error) {
	cands := a.store.promotable(req.MinFrequency, req.Profile)
	out := make([]plugin.Candidate, 0, len(cands))
	for _, c := range cands {
		id, _ := c["id"].(string)
		content, _ := c["content"].(string)
		freq, _ := c["frequency"].(int)
		project, _ := c["project"].(string) // nil (no project) -> ""
		out = append(out, plugin.Candidate{
			ID:        id,
			Content:   content,
			Frequency: freq,
			Project:   project,
		})
	}
	return plugin.PromotableResp{Candidates: out}, nil
}

// Observe and Health call the SAME helpers memoryMux() does (memObserve,
// memWatcherStatus), so the typed surface cannot drift from the JSON-RPC one.
func (a *memoryStoreAdapter) Observe(req plugin.ObserveReq) (plugin.ObserveResp, error) {
	accepted, reason := memObserve(a.store, req.User, req.Project, req.HasProject, req.Profile)
	return plugin.ObserveResp{Accepted: accepted, Reason: reason}, nil
}

// Stats reports the requested profile's counts (protocol v2), so the
// plugin-backed :11435 answers `stats {profile}` like the in-process path did.
func (a *memoryStoreAdapter) Stats(profile string) (plugin.Stats, error) {
	s := a.store.stats(profile)
	get := func(k string) int { n, _ := s[k].(int); return n }
	return plugin.Stats{
		Active:     get("active"),
		Durable:    get("durable"),
		Perishable: get("perishable"),
		Facts:      get("facts"),
		Learnings:  get("learnings"),
		Deleted:    get("deleted"),
	}, nil
}

func (a *memoryStoreAdapter) Health() (plugin.Health, error) {
	capture, reason := memWatcherStatus()
	return plugin.Health{
		OK:            true,
		Vector:        a.hasVector,
		Capture:       capture,
		WatcherModel:  memWatcherModel(),
		CaptureReason: reason,
	}, nil
}

// servePluginMemory constructs the store the same way memoryMux() does
// (buildMemStore) and serves it as a go-plugin. Called by the `plugin`
// subcommand (wired in a later unit); intentionally not registered in main.go.
func servePluginMemory() {
	// Take the shared store lock BEFORE opening the db, like serve.go's built-in
	// branch and runMemory: the self-exec plugin serves the LIVE store, so it must
	// be mutually exclusive with any other memory server and with `restore`. Held
	// for the process lifetime; dropped when Serve returns (the OS also releases the
	// flock on exit). Fails fast if another holder owns the db.
	release := lockMemoryStoreOrFatal(nil)
	defer release()
	store, hasEmb, err := buildMemStore()
	if err != nil {
		log.Fatalf("%v", err)
	}
	adapter := newMemoryStoreAdapter(store, hasEmb)
	plugin.Serve(map[string]goplugin.Plugin{"memory": &plugin.MemoryPlugin{Impl: adapter}})
}
