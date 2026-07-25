// knowledge_plugin.go — adapts the built-in knowledge store (knowledge.go's
// *knowledgeStore) to the go-plugin `plugin.KnowledgeStore` interface, and
// provides a self-exec serve entry point.
//
// Mirrors memory_plugin.go exactly: the store keeps its own methods, and this
// adapter re-expresses the three operations (query, reindex, health) as the
// typed calls the plugin transport speaks. Each method maps 1:1 onto a
// *knowledgeStore method + the plugin structs.
//
// Unlike the memory adapter (which recomputes health() itself), the knowledge
// health is authoritative in the store — health() already reports vector
// availability, the bundle list, and the concept count — so the adapter just
// delegates. buildKnowledgeStore mirrors buildMemStore's (store, hasVector)
// shape; hasVector is redundant with store.embedder != nil and is ignored here.
//
// The later `plugin` subcommand unit calls servePluginKnowledge(); nothing is
// registered in main.go here.

package main

import (
	"log"

	"pix/host/plugin"

	goplugin "github.com/hashicorp/go-plugin"
)

// knowledgeStoreAdapter wraps the real *knowledgeStore and satisfies
// plugin.KnowledgeStore.
type knowledgeStoreAdapter struct {
	store *knowledgeStore
}

// newKnowledgeStoreAdapter builds an adapter around an existing store.
func newKnowledgeStoreAdapter(store *knowledgeStore) *knowledgeStoreAdapter {
	return &knowledgeStoreAdapter{store: store}
}

var _ plugin.KnowledgeStore = (*knowledgeStoreAdapter)(nil)

func (a *knowledgeStoreAdapter) Query(req plugin.QueryArgs) (plugin.QueryResult, error) {
	return plugin.QueryResult{Concepts: a.store.query(req.Query, req.Bundles, req.Limit)}, nil
}

func (a *knowledgeStoreAdapter) Reindex(req plugin.ReindexArgs) (plugin.ReindexResult, error) {
	n, bundles, err := a.store.reindex(req.BundlePaths)
	if err != nil {
		return plugin.ReindexResult{}, err
	}
	return plugin.ReindexResult{Indexed: n, Bundles: bundles}, nil
}

func (a *knowledgeStoreAdapter) Health() (plugin.KnowledgeHealth, error) {
	return a.store.health(), nil
}

// servePluginKnowledge constructs the store the same way memoryMux()/memory
// does (buildKnowledgeStore) and serves it as a go-plugin. Called by the
// `plugin` subcommand (wired in a later unit); intentionally not registered in
// main.go.
func servePluginKnowledge() {
	store, _, err := buildKnowledgeStore()
	if err != nil {
		log.Fatalf("%v", err)
	}
	adapter := newKnowledgeStoreAdapter(store)
	plugin.Serve(map[string]goplugin.Plugin{"knowledge": &plugin.KnowledgePlugin{Impl: adapter}})
}
