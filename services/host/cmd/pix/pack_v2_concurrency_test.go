// pack_v2_concurrency_test.go — the Phase-2 concurrency review (the last
// correctness BLOCK): the host-wrapper lifecycle is ONE cross-process
// transaction under the pack trust flock, held ACROSS the filesystem dir
// swap (refreshHostPackWrappers), and `pack rm` decides what to detach only
// UNDER that same lock. The old shape — record intended attribution (locked),
// swap the dir (UNLOCKED), trim attribution (locked) — let two concurrent
// refreshes interleave into "live dir holds B's wrappers, store attributes
// A", so a later clear removed only A and ORPHANED B's executables on the
// host PATH.
//
// Invariant pinned here: the live hostPackBinDir() contents are ALWAYS a
// subset of what the trust store attributes (no orphan wrapper), even under
// concurrent refresh/refresh and refresh/rm — and at rest the two are EQUAL.
// The goroutines below contend on the REAL flock (distinct fds conflict even
// in-process), so these tests also prove the lifecycle is deadlock-free: a
// nested withPackTrustLock acquisition would hang them forever.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"pix/host/config"
)

// liveHostWrapperNames lists hostPackBinDir() (nil when the dir is absent —
// the fail-safe "no wrappers at all" state).
func liveHostWrapperNames(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(hostPackBinDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// assertLiveSubsetOfAttribution fails if any live wrapper is unattributed —
// the orphan a later clear could never remove.
func assertLiveSubsetOfAttribution(t *testing.T, live []string, store *packTrustStore) {
	t.Helper()
	attributed := map[string]bool{}
	if store.Installed != nil {
		for _, n := range store.Installed.Wrappers {
			attributed[n] = true
		}
	}
	for _, n := range live {
		if !attributed[n] {
			t.Errorf("ORPHAN host wrapper %q: live in %s but attributed to nobody (a later clear would strand it on the host PATH); store=%+v",
				n, hostPackBinDir(), store.Installed)
		}
	}
}

// auditHostWrapperInvariant continuously samples "live ⊆ attributed" while
// holding the trust flock, until stop closes. Because the FIXED lifecycle
// holds that same lock across the whole load→swap→attribute transaction, an
// auditor inside the lock can only ever observe COMMITTED states (where the
// step-1 union guarantees the subset). The pre-fix code swapped the dir
// OUTSIDE the lock, so this auditor observes the torn interleavings (live
// holds B's wrappers, store attributes A) directly. Reports at most one
// violation per name through the returned channel (buffered; never blocks).
func auditHostWrapperInvariant(stop <-chan struct{}) (<-chan string, *sync.WaitGroup) {
	violations := make(chan string, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(violations)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = withPackTrustLock(func() error {
				store, err := loadPackTrustStore()
				if err != nil {
					return nil // unreadable mid-write is not this invariant
				}
				attributed := map[string]bool{}
				if store.Installed != nil {
					for _, n := range store.Installed.Wrappers {
						attributed[n] = true
					}
				}
				ents, rerr := os.ReadDir(hostPackBinDir())
				if rerr != nil {
					return nil // absent dir = no wrappers = trivially a subset
				}
				for _, e := range ents {
					if !attributed[e.Name()] {
						select {
						case violations <- e.Name():
						default:
						}
					}
				}
				return nil
			})
		}
	}()
	return violations, &wg
}

// TestRefreshHostPackWrappers_ConcurrentRefreshesNoOrphan: two accepted packs
// refresh concurrently (two `pix host` launches racing, or `pack use`
// racing a launch). Each iteration swaps the whole bin dir to its own set;
// only the single-flock transaction keeps swap and attribution paired. At
// rest the live dir must EXACTLY match the store's attribution — never one
// pack's wrappers under the other's attribution.
func TestRefreshHostPackWrappers_ConcurrentRefreshesNoOrphan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t)
	rootA := phase2HostPack(t, dir, "a", "a-tool")
	rootB := phase2HostPack(t, dir, "b", "b-tool")

	// Accept BOTH surfaces once (acceptance is per identity and survives
	// switches — see TestPackSwitch_BetweenAcceptedPacksNoReprompt).
	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"}, registerServers)
	runPackUse(fakeGitEnv(nil), &out, []string{rootB, "--yes"}, registerServers)

	stop := make(chan struct{})
	violations, auditWG := auditHostWrapperInvariant(stop)

	const iters = 40
	var wg sync.WaitGroup
	for _, root := range []string{rootA, rootB} {
		wg.Add(1)
		go func(root string) {
			defer wg.Done()
			cfg := &config.Config{Pack: root} // each racer's own (stale-able) config view
			var buf bytes.Buffer
			for i := 0; i < iters; i++ {
				if _, err := refreshHostPackWrappers(&buf, cfg, false); err != nil {
					t.Errorf("concurrent refresh of %s: %v\n%s", root, err, buf.String())
					return
				}
			}
		}(root)
	}
	wg.Wait()
	close(stop)
	auditWG.Wait()
	for n := range violations {
		t.Errorf("mid-flight ORPHAN observed under the trust lock: wrapper %q live but attributed to nobody", n)
	}

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	live := liveHostWrapperNames(t)
	assertLiveSubsetOfAttribution(t, live, store)
	// At rest the transaction leaves live == attributed: whoever committed
	// last owns BOTH the dir and the store record.
	if store.Installed == nil || len(live) != len(store.Installed.Wrappers) {
		t.Errorf("live dir and attribution must match at rest: live=%v store=%+v", live, store.Installed)
	}
	// And the live set is exactly ONE pack's wrapper set, never a mix of both.
	if len(live) != 1 || (live[0] != "a-tool" && live[0] != "b-tool") {
		t.Errorf("live dir must hold exactly the last winner's set, got %v", live)
	}
}

// TestPackRm_RacingRefreshNeverOrphans: `pack rm` races a `pix host`
// wrapper refresh whose config view predates the detach. Exactly two legal
// outcomes — rm wins (nothing installed, nothing attributed) or the refresh
// wins (installed AND attributed) — never installed-but-unattributed, which
// is what the pre-lock-snapshot rm allowed (refresh installs after rm
// reported "detached", store cleared).
func TestPackRm_RacingRefreshNeverOrphans(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t)
	root := phase2HostPack(t, dir, "work", "platformio")

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerServers)
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "platformio")); err != nil {
		t.Fatalf("setup: accepted wrapper not installed: %v\n%s", err, out.String())
	}

	stop := make(chan struct{})
	violations, auditWG := auditHostWrapperInvariant(stop)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		// The racing host launch loaded its config BEFORE rm detached.
		if _, err := refreshHostPackWrappers(&buf, &config.Config{Pack: root}, false); err != nil {
			t.Errorf("racing refresh: %v\n%s", err, buf.String())
		}
	}()
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		runPackRm(&buf, nil) // os.Exit(1) on failure would kill the test binary
	}()
	wg.Wait()
	close(stop)
	auditWG.Wait()
	for n := range violations {
		t.Errorf("mid-flight ORPHAN observed under the trust lock: wrapper %q live but attributed to nobody", n)
	}

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	live := liveHostWrapperNames(t)
	assertLiveSubsetOfAttribution(t, live, store)
	attributed := store.Installed != nil && len(store.Installed.Wrappers) > 0
	installed := len(live) > 0
	if installed != attributed {
		t.Errorf("torn outcome: installed=%v attributed=%v (must be rm-wins or refresh-wins, never half); live=%v store=%+v",
			installed, attributed, live, store.Installed)
	}
}
