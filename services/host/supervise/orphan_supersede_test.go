// orphan_supersede_test.go — the reattach refusals that leave a LIVE child
// behind. Refusing to adopt a surviving process is right; walking away from
// one is not. A refused-but-alive child of ours still holds everything its
// unit owns exclusively (for memory, the store flock), and dropping its
// reattach record makes it unfindable, so every later spawn dies on that lock
// and `serve` never starts again until an operator hunts the pid down by hand.
package supervise

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/plugin"
)

// The upgrade deadlock, end to end: a hard supervisor death leaves a live
// child, THEN pix-host is rebuilt or upgraded (new identity) or its plugin
// protocol is bumped. Either way the next supervisor must refuse to adopt the
// survivor AND reap it, then come up fresh.
func TestReattachReapsTheLiveOrphanItRefusesToAdopt(t *testing.T) {
	if testing.Short() {
		t.Skip("real process spawn + reattach timing; covered by the untimed race/metrics CI jobs")
	}
	if _, ok := processStartTime(os.Getpid()); !ok {
		t.Skip("no process start-time source on this platform: revalidateOrphan refuses to kill, by design")
	}
	bin, sha := buildFixture(t)

	// Both cases run the SAME script and differ only in what the persisted
	// state disagrees with the running supervisor about.
	for _, tc := range []struct {
		name string
		// protocol is the version the persisted state claims; wantTag is the
		// grant the NEW supervisor's spec carries (rotating it is what makes
		// the identity differ, so the protocol case keeps it unchanged).
		protocol int
		wantTag  string
	}{
		{name: "executable identity changed", protocol: plugin.ProtocolVersion, wantTag: "new"},
		{name: "plugin protocol version changed", protocol: plugin.ProtocolVersion + 1, wantTag: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			old := fixtureUnit("upgraded", bin, sha, "FIXTURE_TAG=old")
			orphan := startOrphan(t, bin, old)
			rc := orphan.ReattachConfig()
			must(t, SaveReattach(state, old, &goplugin.ReattachConfig{
				Pid: rc.Pid, Protocol: rc.Protocol, Addr: rc.Addr, ProtocolVersion: tc.protocol,
			}, tc.protocol))

			// An identity change rotates the grant (so the fresh child answers
			// "new"); a protocol change keeps the identical spec, and the whole
			// disagreement is the version on disk.
			next := fixtureUnit("upgraded", bin, sha, "FIXTURE_TAG="+tc.wantTag)
			tr := testTree(t, func(c *Config) { c.StateDir = state })
			h, err := tr.Add(next, fixtureHealth)
			must(t, err)

			// The unit comes up FRESH: a new pid, never the orphan's.
			got := describe(t, h)
			if got.WatcherModel != tc.wantTag || got.CaptureReason == strconv.Itoa(rc.Pid) {
				t.Errorf("adopted the superseded orphan: %+v (orphan pid %d)", got, rc.Pid)
			}
			if st, _ := tr.Unit("upgraded"); st.Reattached {
				t.Error("status claims a reattach the supervisor refused")
			}
			if !sawEvent(tr, "upgraded", EventReattachRejected) {
				t.Errorf("no typed reattach-rejected event: %+v", tr.Events())
			}

			// ...and the refused orphan is DEAD, not left holding the flock.
			waitFor(t, "the superseded orphan to be reaped", 3*time.Second,
				func() bool { return !alive(rc.Pid) })
			if !sawEvent(tr, "upgraded", EventOrphanKilled) {
				t.Errorf("no typed orphan-killed event: %+v", tr.Events())
			}

			// The record no longer names the reaped process: it was dropped and
			// rewritten for the generation that actually came up. (A file is
			// expected here — the fresh child persists its own.)
			raw, err := os.ReadFile(reattachPath(state, "upgraded"))
			must(t, err)
			var saved reattachState
			must(t, json.Unmarshal(raw, &saved))
			if saved.Pid == rc.Pid {
				t.Errorf("reattach state still names the reaped orphan (pid %d)", rc.Pid)
			}
		})
	}
}

// The reap is scoped to OUR OWN unit's earlier generation, and nothing else. A
// state file that names a different unit is corruption, not a record we wrote
// for this unit: the pid in it was never proven to be ours, so it is dropped
// and the process left strictly alone. (killVerifiedOrphan reaches a real OS
// process by pid — the blast radius of getting this wrong is somebody else's
// process, so it is pinned rather than argued about.)
func TestReattachRefusesToReapAPidItCannotClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("real process spawn + reattach timing; covered by the untimed race/metrics CI jobs")
	}
	bin, sha := buildFixture(t)
	state := filepath.Join(t.TempDir(), "state")
	spec := fixtureUnit("mislabelled", bin, sha, "FIXTURE_TAG=fresh")
	orphan := startOrphan(t, bin, spec)
	rc := orphan.ReattachConfig()

	// Written by hand: SaveReattach always agrees with the spec it is given, so
	// the only way to produce a file whose Unit contradicts its own path is to
	// write it directly — which is exactly the corruption this guards against.
	raw, err := json.Marshal(reattachState{
		Unit: "some-other-unit", Kind: spec.Kind, Identity: spec.identity(), Pid: rc.Pid,
		Network: rc.Addr.Network(), Address: rc.Addr.String(),
		Protocol: string(rc.Protocol), ProtocolVersion: plugin.ProtocolVersion,
	})
	must(t, err)
	path := reattachPath(state, "mislabelled")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, raw, 0o600))

	tr := testTree(t, func(c *Config) { c.StateDir = state })
	_, err = tr.Add(spec, fixtureHealth)
	must(t, err)

	if !sawEvent(tr, "mislabelled", EventReattachRejected) {
		t.Errorf("no typed reattach-rejected event: %+v", tr.Events())
	}
	if sawEvent(tr, "mislabelled", EventOrphanKilled) {
		t.Error("killed a pid recorded under another unit's name")
	}
	// Give any (buggy) kill the same window the reaping test allows before
	// concluding the process was left alone.
	time.Sleep(200 * time.Millisecond)
	if !alive(rc.Pid) {
		t.Errorf("pid %d was reaped on a record this unit cannot claim", rc.Pid)
	}
}

// The signal-sending primitives address a PROCESS, never a process group: on
// unix, kill(0, …) hits our whole group and kill(-1, …) every process this uid
// can reach. No caller passes a non-positive pid today; these pin that a future
// one cannot turn "reap one orphan" into "kill the supervisor and its world".
func TestKillPathRefusesNonPositivePids(t *testing.T) {
	for _, pid := range []int{0, -1, -os.Getpid()} {
		if processAlive(pid) {
			t.Errorf("processAlive(%d) vouched for a process-group id", pid)
		}
		if ok, _ := revalidateOrphan(pid, "unix", "/nonexistent.sock", 0, true); ok {
			t.Errorf("revalidateOrphan(%d) verified a process-group id", pid)
		}
		if got := killVerifiedOrphan(pid); got != orphanKillSignalFailed {
			t.Errorf("killVerifiedOrphan(%d) = %v, want a refusal", pid, got)
		}
	}
}
