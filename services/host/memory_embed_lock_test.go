// memory_embed_lock_test.go — SRE-1: the Ollama embed is a NETWORK call, and
// s.mu is the single lock every recall/remember serializes through. Holding
// the lock across the embed made one slow (15s) embed head-of-line block every
// other memory operation on the host.
//
// These tests are deliberately structural, not timing-tuned: every "the embed
// is in flight" moment is a channel rendezvous, and the assertion is that
// another store call COMPLETES while the embed is still blocked. They fail on
// the pre-fix code by deadlocking to their timeout, and they cannot pass by
// luck. The second half pins what the two-phase split must not lose:
// exact-hash reaffirm, semantic dedupe, and budget atomicity all still hold
// when several remembers embed concurrently.
package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// blockingEmbedder returns an embedder that signals each call on `entered`
// and then waits for `release` before returning vec. Both channels are the
// synchronization: no sleeps.
func blockingEmbedder(vec []float64, entered chan<- struct{}, release <-chan struct{}) func(string) []float64 {
	return func(string) []float64 {
		entered <- struct{}{}
		<-release
		return vec
	}
}

// waitFor runs fn in a goroutine and fails the test if it has not returned
// within the budget. The budget is an outer bound on a call that should
// return essentially instantly (it touches an in-memory sqlite db), not a
// tuned threshold: on the pre-fix code the call cannot return at all until
// the embed is released, so any generous value separates the two behaviors.
func waitFor(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not complete while an embed was in flight — the store lock is being held across the embed", what)
	}
}

// TestRememberDoesNotHoldStoreLockAcrossEmbed: a remember parked inside the
// embedder must not stop an unrelated recall (and an unrelated forget, a
// writer) from completing.
func TestRememberDoesNotHoldStoreLockAcrossEmbed(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	st, err := newMemStore(":memory:", blockingEmbedder([]float64{1, 0}, entered, release))
	if err != nil {
		t.Fatal(err)
	}

	rememberDone := make(chan error, 1)
	go func() {
		_, err := st.remember(rememberInput{content: "a fact whose embed is going to hang", kind: "fact"})
		rememberDone <- err
	}()
	<-entered // the embed is now in flight

	// "*" recall consults neither FTS nor the embedder, so it exercises the
	// LOCK and nothing else: if it returns, the lock is free.
	waitFor(t, "recall while a remember was embedding", func() {
		if _, err := st.recall("*", 8, 1200, "", "", ""); err != nil {
			t.Errorf("recall: %v", err)
		}
	})
	waitFor(t, "forget while a remember was embedding", func() { st.forget("no-such-id", "") })

	close(release)
	if err := <-rememberDone; err != nil {
		t.Fatalf("remember: %v", err)
	}
	var n int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("stored %d rows, want 1", n)
	}
}

// TestRecallDoesNotHoldStoreLockAcrossEmbed is the read-side twin: recall
// embeds its QUERY, and that embed must likewise happen before the lock.
func TestRecallDoesNotHoldStoreLockAcrossEmbed(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	st, err := newMemStore(":memory:", blockingEmbedder([]float64{1, 0}, entered, release))
	if err != nil {
		t.Fatal(err)
	}

	recallDone := make(chan error, 1)
	go func() {
		_, err := st.recall("a scored query, so the embedder is consulted", 8, 1200, "", "", "")
		recallDone <- err
	}()
	<-entered

	waitFor(t, "star recall while another recall was embedding", func() {
		if _, err := st.recall("*", 8, 1200, "", "", ""); err != nil {
			t.Errorf("recall: %v", err)
		}
	})

	close(release)
	if err := <-recallDone; err != nil {
		t.Fatalf("recall: %v", err)
	}
}

// TestExactHashReaffirmSkipsTheEmbedEntirely: phase 1 exists so the common
// "same fact again" case never pays for a network round trip. The second
// remember must return reaffirmed WITHOUT the embedder being called again.
func TestExactHashReaffirmSkipsTheEmbedEntirely(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	st, err := newMemStore(":memory:", func(string) []float64 {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return []float64{1, 0}
	})
	if err != nil {
		t.Fatal(err)
	}
	const content = "the same fact, twice"
	if _, err := st.remember(rememberInput{content: content, kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	res, err := st.remember(rememberInput{content: content, kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	if reaff, _ := res["reaffirmed"].(bool); !reaff {
		t.Fatalf("second remember of identical content = %v, want reaffirmed", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("embedder called %d time(s), want exactly 1 (an exact-hash reaffirm must not embed)", calls)
	}
}

// TestConcurrentIdenticalRemembersStoreExactlyOneRow is the revalidation
// regression: every caller passes phase 1 (nothing is stored yet), then they
// all embed concurrently. Only the phase-3 re-check under the lock stops the
// losers from each inserting their own copy.
func TestConcurrentIdenticalRemembersStoreExactlyOneRow(t *testing.T) {
	release := make(chan struct{})
	st, err := newMemStore(":memory:", func(string) []float64 {
		<-release // hold every caller inside the embed until all have entered phase 2
		return []float64{1, 0}
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	const content = "one fact, remembered by everybody at once"
	var wg sync.WaitGroup
	results := make([]jsonObj, callers)
	errs := make([]error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.remember(rememberInput{content: content, kind: "fact"})
		}(i)
	}
	close(release)
	wg.Wait()

	newRows := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if reaff, _ := results[i]["reaffirmed"].(bool); !reaff {
			newRows++
		}
	}
	if newRows != 1 {
		t.Errorf("%d caller(s) reported storing a NEW row, want exactly 1 (the rest must reaffirm)", newRows)
	}
	var stored int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored %d rows for identical content under %d concurrent remembers, want exactly 1", stored, callers)
	}
}

// TestConcurrentSemanticDuplicatesCollapseToOneRow: distinct CONTENT (so the
// hash re-check cannot catch them) with identical vectors and dedupe on.
// findSimilar runs in phase 3 under the lock, so the first insert is visible
// to every later caller and they all collapse into it.
func TestConcurrentSemanticDuplicatesCollapseToOneRow(t *testing.T) {
	release := make(chan struct{})
	st, err := newMemStore(":memory:", func(string) []float64 {
		<-release
		return []float64{1, 0} // every content embeds to the SAME vector
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := st.remember(rememberInput{
				content: fmt.Sprintf("semantically identical phrasing number %d", i),
				kind:    "fact", dedupe: 0.9, hasDedupe: true,
			}); err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
		}(i)
	}
	close(release)
	wg.Wait()

	var stored int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored %d rows under %d concurrent semantic duplicates, want exactly 1", stored, callers)
	}
}

// TestWatcherBudgetExactWithConcurrentEmbeds re-proves SEC-2's exact daily cap
// with the embed now OUTSIDE the lock: every caller embeds concurrently, then
// contends for the phase-3 hold where the COUNT and the INSERT still happen as
// one atomic unit. (The sibling test in memory_capture_mode_test.go covers the
// no-embedder store; this one covers the shape the fix actually changed.)
func TestWatcherBudgetExactWithConcurrentEmbeds(t *testing.T) {
	release := make(chan struct{})
	st, err := newMemStore(":memory:", func(s string) []float64 {
		<-release
		// A distinct vector per content: semantic dedupe must not collapse these
		// rows, or the test would stop measuring the budget.
		return []float64{float64(len(s)), 1}
	})
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 30 // far more than memWatcherDailyBudget, on purpose
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := st.rememberWatcherCapture(rememberInput{
				content: fmt.Sprintf("concurrent watcher fact %d", i), kind: "fact",
			}); err != nil {
				t.Errorf("attempt %d: %v", i, err)
			}
		}(i)
	}
	close(release)
	wg.Wait()

	var stored int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM memories WHERE source = 'watcher'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != memWatcherDailyBudget {
		t.Fatalf("stored %d watcher rows under %d concurrent attempts, want exactly %d", stored, attempts, memWatcherDailyBudget)
	}
}
