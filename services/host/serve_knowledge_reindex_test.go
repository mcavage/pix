package main

import (
	"reflect"
	"testing"

	"pix/host/config"
	"pix/host/plugin"
)

// recordingKnowledge is a stub plugin.KnowledgeStore that records the bundle
// paths passed to Reindex, so the plugin-path reindex (F2) can be asserted
// without launching a real subprocess.
type recordingKnowledge struct {
	reindexed [][]string
}

func (r *recordingKnowledge) Query(plugin.QueryArgs) (plugin.QueryResult, error) {
	return plugin.QueryResult{}, nil
}
func (r *recordingKnowledge) Reindex(a plugin.ReindexArgs) (plugin.ReindexResult, error) {
	r.reindexed = append(r.reindexed, a.BundlePaths)
	return plugin.ReindexResult{Indexed: len(a.BundlePaths), Bundles: a.BundlePaths}, nil
}
func (r *recordingKnowledge) Health() (plugin.KnowledgeHealth, error) {
	return plugin.KnowledgeHealth{OK: true}, nil
}

// TestReindexKnowledgePlugin_CallsReindexWithBundles is the F2 guard: the plugin
// path must index the configured bundles into the dispensed store, or the
// external plugin serves an empty index.
func TestReindexKnowledgePlugin_CallsReindexWithBundles(t *testing.T) {
	bundles := []string{"/bundles/a", "/bundles/b"}
	rec := &recordingKnowledge{}

	reindexKnowledgePlugin(rec, bundles)

	if len(rec.reindexed) != 1 {
		t.Fatalf("Reindex called %d times, want exactly 1", len(rec.reindexed))
	}
	if !reflect.DeepEqual(rec.reindexed[0], bundles) {
		t.Fatalf("Reindex called with %v, want %v", rec.reindexed[0], bundles)
	}
}

// TestReindexKnowledgePlugin_NoBundlesSkips: no configured bundles means the
// dispensed store is never reindexed (empty index by design, logged).
func TestReindexKnowledgePlugin_NoBundlesSkips(t *testing.T) {
	rec := &recordingKnowledge{}
	reindexKnowledgePlugin(rec, nil)
	if len(rec.reindexed) != 0 {
		t.Fatalf("Reindex called %d times with no bundles, want 0", len(rec.reindexed))
	}
}

// TestReindexKnowledgePlugin_NilStoreSafe: a store that failed to dispense must
// not panic (mirrors knowledgeProxyMux's nil handling).
func TestReindexKnowledgePlugin_NilStoreSafe(t *testing.T) {
	reindexKnowledgePlugin(nil, []string{"/bundles/a"})
}

// TestReindexKnowledgePlugin_UsesConfiguredBundles ties the helper to
// knowledgeBundles(cfg) so the wiring in runServe passes the SAME bundles the
// built-in path indexes.
func TestReindexKnowledgePlugin_UsesConfiguredBundles(t *testing.T) {
	t.Setenv("KNOWLEDGE_BUNDLES", "/x/one,/x/two")
	rec := &recordingKnowledge{}

	reindexKnowledgePlugin(rec, knowledgeBundles(&config.Config{}))

	want := []string{"/x/one", "/x/two"}
	if len(rec.reindexed) != 1 || !reflect.DeepEqual(rec.reindexed[0], want) {
		t.Fatalf("Reindex got %v, want one call with %v", rec.reindexed, want)
	}
}
