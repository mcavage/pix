// orphan_revalidate_test.go — the orphan-kill decision path is a hard safety
// boundary (killVerifiedOrphan reaches an OS process by pid), so its guard
// (revalidateOrphan) and the identity source it binds against pid reuse
// (processStartTime) are proven directly, plus the typed outcomes
// killVerifiedOrphan reports so a caller can never read "we tried" as "it's
// dead".
package supervise

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// processStartTime must report a value for our own live process, and that
// value must be stable across repeated reads (it is a fixed kernel fact
// about the process, not something that drifts).
func TestProcessStartTimeStableForLiveProcess(t *testing.T) {
	v1, ok := processStartTime(os.Getpid())
	if !ok {
		t.Skip("no /proc start-time source on this platform")
	}
	v2, ok := processStartTime(os.Getpid())
	if !ok || v2 != v1 {
		t.Errorf("start time for our own live pid was not stable: %d vs %d (ok=%v)", v1, v2, ok)
	}
}

// processStartTime must refuse to vouch for a pid that no longer exists.
func TestProcessStartTimeRefusesDeadPid(t *testing.T) {
	dead := exec.Command("true")
	must(t, dead.Start())
	pid := dead.Process.Pid
	_, _ = dead.Process.Wait()
	if _, ok := processStartTime(pid); ok {
		t.Errorf("start time reported for reaped pid %d", pid)
	}
}

// revalidateOrphan is the ONLY thing standing between a failed reattach
// dispense and killVerifiedOrphan actually signaling an OS process. Every
// way it can go stale must refuse to kill, never proceed on ambiguity.
func TestRevalidateOrphanRefusesOnAnyStalenessOrAmbiguity(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "live.sock")
	l, err := net.Listen("unix", sock)
	must(t, err)
	t.Cleanup(func() { l.Close() })

	pid := os.Getpid()
	startNow, startKnown := processStartTime(pid)
	if !startKnown {
		t.Skip("no /proc start-time source on this platform")
	}

	t.Run("matching start time and a live owned socket: verified", func(t *testing.T) {
		ok, reason := revalidateOrphan(pid, "unix", sock, startNow, true)
		if !ok {
			t.Fatalf("expected verified, got refusal: %s", reason)
		}
	})

	t.Run("start time changed: refuses (pid reuse)", func(t *testing.T) {
		ok, reason := revalidateOrphan(pid, "unix", sock, startNow+1, true)
		if ok {
			t.Fatal("revalidated despite a mismatched start time")
		}
		if reason == "" {
			t.Fatal("no reason given for refusing a mismatched start time")
		}
	})

	t.Run("start time unknown at verification: refuses conservatively", func(t *testing.T) {
		ok, reason := revalidateOrphan(pid, "unix", sock, 0, false)
		if ok {
			t.Fatal("revalidated despite no start-time source ever having been available")
		}
		if reason == "" {
			t.Fatal("no reason given for the conservative refusal")
		}
	})

	t.Run("process gone: refuses", func(t *testing.T) {
		dead := exec.Command("true")
		must(t, dead.Start())
		deadPid := dead.Process.Pid
		_, _ = dead.Process.Wait()
		ok, reason := revalidateOrphan(deadPid, "unix", sock, 0, true)
		if ok {
			t.Fatal("revalidated a dead pid")
		}
		if reason == "" {
			t.Fatal("no reason given for refusing a dead pid")
		}
	})

	t.Run("socket no longer verifies: refuses even with a live matching pid", func(t *testing.T) {
		vanished := filepath.Join(dir, "gone.sock")
		ok, reason := revalidateOrphan(pid, "unix", vanished, startNow, true)
		if ok {
			t.Fatal("revalidated against a socket that no longer exists")
		}
		if reason == "" {
			t.Fatal("no reason given for refusing a vanished socket")
		}
	})
}

// killVerifiedOrphan must distinguish "confirmed dead" from every other
// outcome: EventOrphanKilled is only ever correct for the former.
func TestKillVerifiedOrphanDistinguishesOutcomes(t *testing.T) {
	t.Run("a live process it can signal: confirmed dead", func(t *testing.T) {
		cmd := exec.Command("sleep", "30")
		must(t, cmd.Start())
		pid := cmd.Process.Pid
		reaped := make(chan struct{})
		go func() { _ = cmd.Wait(); close(reaped) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })

		if got := killVerifiedOrphan(pid); got != orphanKillConfirmedDead {
			t.Errorf("got %v, want orphanKillConfirmedDead", got)
		}
		<-reaped
		if alive(pid) {
			t.Error("pid still alive after a confirmed kill")
		}
	})

	t.Run("an already-reaped pid: confirmed dead, not a failure", func(t *testing.T) {
		dead := exec.Command("true")
		must(t, dead.Start())
		pid := dead.Process.Pid
		_, _ = dead.Process.Wait()
		if got := killVerifiedOrphan(pid); got != orphanKillConfirmedDead {
			t.Errorf("got %v, want orphanKillConfirmedDead for an already-gone pid", got)
		}
	})
}
