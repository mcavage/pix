// store_test.go is the focused behavioral coverage for
// remember/recall/forget/stats: exact-hash reaffirm, semantic dedupe,
// profile scoping, the watcher daily budget, and the secret filter's
// capture-time gate. Ported/condensed from services/host's memory_test
// suite (pix-v2 U2): same behaviors, a smaller focused set rather than a
// line-for-line mirror.
package store

import (
	"testing"
)

func openTestStore(t *testing.T, embedder Embedder) *Store {
	t.Helper()
	st, err := Open(t.TempDir()+"/memory.db", embedder)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRememberReaffirmsExactDuplicate(t *testing.T) {
	st := openTestStore(t, nil)
	first, err := st.Remember(RememberInput{Content: "prefers tabs"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Reaffirmed {
		t.Fatal("first remember of new content reported reaffirmed=true")
	}
	second, err := st.Remember(RememberInput{Content: "  prefers tabs  "}) // whitespace-insensitive hash
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reaffirmed || second.ID != first.ID {
		t.Fatalf("exact-duplicate remember: want reaffirm of %s, got %+v", first.ID, second)
	}
	stats := st.Stats("")
	if stats.Active != 1 {
		t.Fatalf("Stats().Active = %d, want 1 (reaffirm must not create a second row)", stats.Active)
	}
}

func TestRememberSemanticDedupe(t *testing.T) {
	// A stub embedder: near-identical vectors for near-identical text.
	embed := func(text string) []float64 {
		if text == "the user likes cats" {
			return []float64{1, 0, 0}
		}
		return []float64{0.99, 0.01, 0}
	}
	st := openTestStore(t, embed)
	first, err := st.Remember(RememberInput{Content: "the user likes cats", Dedupe: 0.9, HasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Remember(RememberInput{Content: "the user really likes cats", Dedupe: 0.9, HasDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reaffirmed || second.ID != first.ID {
		t.Fatalf("near-duplicate remember with dedupe=0.9: want reaffirm of %s, got %+v", first.ID, second)
	}
}

func TestRememberSourceWatcherIsSpoofResistant(t *testing.T) {
	st := openTestStore(t, nil)
	res, err := st.Remember(RememberInput{Content: "spoofed watcher row", Source: "watcher"})
	if err != nil {
		t.Fatal(err)
	}
	var source string
	if err := st.db.QueryRow("SELECT source FROM memories WHERE id = ?", res.ID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "unknown" {
		t.Fatalf("Remember with Source:\"watcher\" stored source=%q, want \"unknown\" (only rememberWatcherCapture may write \"watcher\")", source)
	}
}

func TestRecallStarListsNewestFirstAcrossOllamaDown(t *testing.T) {
	st := openTestStore(t, nil) // nil embedder == "Ollama down": keyword/star only
	var last string
	for _, c := range []string{"first", "second", "third"} {
		res, err := st.Remember(RememberInput{Content: c})
		if err != nil {
			t.Fatal(err)
		}
		last = res.ID
	}
	hits, err := st.Recall("*", 10, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("Recall(\"*\") returned %d hits, want 3", len(hits))
	}
	if hits[0].ID != last {
		t.Fatalf("Recall(\"*\")[0].ID = %s, want the most recently inserted row %s", hits[0].ID, last)
	}
}

func TestRecallKeywordMatchesContent(t *testing.T) {
	st := openTestStore(t, nil)
	res, err := st.Remember(RememberInput{Content: "the deploy pipeline uses argo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Remember(RememberInput{Content: "unrelated fact about lunch"}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.Recall("argo pipeline", 10, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != res.ID {
		t.Fatalf("Recall(\"argo pipeline\") = %+v, want exactly the argo row", hits)
	}
}

func TestForgetByPrefixAndMiss(t *testing.T) {
	st := openTestStore(t, nil)
	res, err := st.Remember(RememberInput{Content: "forget me"})
	if err != nil {
		t.Fatal(err)
	}
	prefix := res.ID[:8]
	if !st.Forget(prefix, "") {
		t.Fatalf("Forget(%q) (unambiguous prefix) returned false", prefix)
	}
	if st.Forget(prefix, "") {
		t.Fatal("Forget of an already-deleted id/prefix returned true, want false (no live match)")
	}
	if st.Forget("no-such-id", "") {
		t.Fatal("Forget of a nonexistent id returned true")
	}
	stats := st.Stats("")
	if stats.Active != 0 || stats.Deleted != 1 {
		t.Fatalf("Stats() = %+v, want 0 active, 1 deleted", stats)
	}
}

func TestProfileScopingWriteAndRead(t *testing.T) {
	st := openTestStore(t, nil)
	if _, err := st.Remember(RememberInput{Content: "shared base fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Remember(RememberInput{Content: "work-only fact", Profile: "work"}); err != nil {
		t.Fatal(err)
	}

	defaultHits, err := st.Recall("*", 10, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultHits) != 1 {
		t.Fatalf("default profile Recall(\"*\") = %d hits, want 1 (must not see \"work\"-only row)", len(defaultHits))
	}

	workHits, err := st.Recall("*", 10, 0, "", "", "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(workHits) != 2 {
		t.Fatalf("\"work\" profile Recall(\"*\") = %d hits, want 2 (own row UNION shared default)", len(workHits))
	}
}

func TestProfileScopingForgetCannotCrossProfiles(t *testing.T) {
	st := openTestStore(t, nil)
	res, err := st.Remember(RememberInput{Content: "personal secret plan", Profile: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Forget(res.ID, "work") {
		t.Fatal("Forget from a different profile deleted another profile's row")
	}
	if !st.Forget(res.ID, "personal") {
		t.Fatal("Forget from the owning profile failed")
	}
}

func TestWatcherDailyBudgetEnforcedAtInsert(t *testing.T) {
	st := openTestStore(t, nil)
	stored := 0
	for i := 0; i < watcherDailyBudget+3; i++ {
		res, err := st.rememberWatcherCapture(RememberInput{Content: uniqueContent(i)})
		if err != nil {
			t.Fatal(err)
		}
		if res.BudgetExceeded {
			continue
		}
		stored++
	}
	if stored != watcherDailyBudget {
		t.Fatalf("watcher captures stored = %d, want exactly the daily budget %d", stored, watcherDailyBudget)
	}
	remaining, err := st.WatcherBudgetRemaining()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("WatcherBudgetRemaining() = %d, want 0 after exhausting the day's budget", remaining)
	}
}

func uniqueContent(i int) string {
	b := make([]byte, 0, 20)
	b = append(b, "budget item "...)
	b = append(b, byte('a'+i%26), byte('0'+i/26))
	return string(b)
}

func TestSecretFilterBlocksCaptureButNotExplicitRemember(t *testing.T) {
	secret := "AKIAABCDEFGHIJKLMNOP" // AWS-access-key-shaped
	if !containsSecretShape(secret) {
		t.Fatalf("containsSecretShape(%q) = false, want true", secret)
	}
	st := openTestStore(t, nil)
	// Explicit remember is NOT filtered: the filter is capture-only.
	res, err := st.Remember(RememberInput{Content: "my key is " + secret})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID == "" {
		t.Fatal("explicit Remember of secret-shaped content was refused; the secret filter must not apply to explicit remember")
	}
}

func TestObserveRefusesInExplicitCaptureMode(t *testing.T) {
	st := openTestStore(t, nil)
	accepted, reason := st.Observe("I always use vim", "", false, "")
	if accepted {
		t.Fatalf("Observe with default (explicit) capture mode accepted, want refused; reason=%q", reason)
	}
	if reason == "" {
		t.Fatal("Observe refusal carries no reason")
	}
}

func TestSnapshotRefusesToOverwriteLiveDB(t *testing.T) {
	st := openTestStore(t, nil)
	if _, err := st.Snapshot(st.Path()); err == nil {
		t.Fatal("Snapshot(live db path) succeeded, want a refusal")
	}
}
