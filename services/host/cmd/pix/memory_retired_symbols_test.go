// memory_retired_symbols_test.go — the memory subsystem's grep-based
// sentinel, the same shape (and the same helper) hostmode_gone_test.go uses
// for `pix host` and the knowledge daemon. Two deletions here were made for
// reasons a future change could plausibly undo by accident, so each gets a
// named symbol that must never reappear in a non-test .go file:
//
//   - memEmbedderAvailable: the boot-time Ollama probe newMemStore used to
//     make before returning. It made store construction (and therefore
//     `serve`'s listener and the watcher) wait on a network round trip, and
//     it froze a BOOT-TIME verdict into a store that then could not notice
//     Ollama recovering. The embedder is attached live and unprobed now
//     (buildMemStore), latching off only on a real failure and re-probing on
//     its own schedule — see memory_live_embed_test.go, which proves the
//     behavior; this proves the symbol did not come back to re-establish it.
//   - promotable / MemoryStore.Promotable: the "learnings worth promoting"
//     RPC behind the deleted `/learnings` command and `pix memory learnings`
//     verb. Deleting the command but leaving the RPC would quietly re-grow
//     the surface it existed to serve.
package main

import "testing"

// forbiddenMemorySymbols are identifiers whose deletion is load-bearing. See
// the file comment for what each one cost when it existed.
var forbiddenMemorySymbols = []string{
	"memEmbedderAvailable",
	"func (s *memStore) promotable(",
	"Promotable(",
	"PromotableReq",
	"PromotableResp",
}

func TestNoRetiredMemorySymbols(t *testing.T) {
	root := hostModeRoot(t)
	violations, err := forbiddenSymbolViolations(root, forbiddenMemorySymbols)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, v := range violations {
		t.Errorf("%s — a retired memory symbol is back: the boot-time embedder probe and the promotable/learnings surface were deleted on purpose", v)
	}
}
