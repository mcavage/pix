package service

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pix/host/config"
)

// TestIsDaemonAffecting pins the affecting-key set: exactly the keys serve
// reads at startup trigger propagation; launcher/gateway keys never do.
func TestIsDaemonAffecting(t *testing.T) {
	affecting := []string{"services", "memory_watcher_model", "memory_embed_model"}
	for _, k := range affecting {
		if !IsDaemonAffecting(k) {
			t.Errorf("IsDaemonAffecting(%q) = false, want true", k)
		}
	}
	notAffecting := []string{"google_workspace_account", "ollama_bridge_model", "mcp", "pack", "host.enabled", "host.autonomy", "host.autoserve", "knowledge_bundles"}
	for _, k := range notAffecting {
		if IsDaemonAffecting(k) {
			t.Errorf("IsDaemonAffecting(%q) = true, want false (must trigger NOTHING)", k)
		}
	}
}

// ctl fakes for detectServeMode.
func liveOursCtl() serveCtl {
	return serveCtl{
		pidPath: func() string { return "/x/serve.pid" },
		readPid: func(string) (string, error) { return "77\n", nil },
		kill:    func(int, syscall.Signal) error { return nil },
		verify:  func(int) (bool, bool) { return true, true },
		sleep:   func(time.Duration) {},
	}
}

func deadCtl() serveCtl {
	c := liveOursCtl()
	c.readPid = func(string) (string, error) { return "", os.ErrNotExist }
	c.kill = func(int, syscall.Signal) error { return syscall.ESRCH }
	return c
}

func TestDetectServeMode(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	// The lazy marker now carries the spawned PID (H4); liveOursCtl's pidfile
	// says 77, so markerSame matches and markerStale does not.
	markerSame := func() (int, bool) { return 77, true }
	markerStale := func() (int, bool) { return 55, true }
	markerNone := func() (int, bool) { return 0, false }

	// Managed is authoritative and checked FIRST (a managed service also writes
	// the pidfile, so pidfile+marker must not shadow it).
	if got := detectServeMode(liveOursCtl(), yes, markerSame); got != serveManaged {
		t.Errorf("managed active = %v, want serveManaged", got)
	}
	// Live + ours + marker naming the SAME pid -> lazy.
	if got := detectServeMode(liveOursCtl(), no, markerSame); got != serveLazy {
		t.Errorf("live+matching-marker = %v, want serveLazy", got)
	}
	// Live + ours, no marker -> foreground.
	if got := detectServeMode(liveOursCtl(), no, markerNone); got != serveForeground {
		t.Errorf("live no-marker = %v, want serveForeground", got)
	}
	// H4 regression: a STALE marker (pid from a lazy spawn that crashed before
	// its pidfile landed) + a LATER foreground daemon must detect FOREGROUND —
	// config propagation must never stop+restart a process the user is watching.
	if got := detectServeMode(liveOursCtl(), no, markerStale); got != serveForeground {
		t.Errorf("live+stale-marker = %v, want serveForeground (stale marker must be ignored)", got)
	}
	// Dead pid + STALE marker -> down (marker only consulted for a live pid).
	if got := detectServeMode(deadCtl(), no, markerSame); got != serveDown {
		t.Errorf("dead+stale-marker = %v, want serveDown", got)
	}
	// Live but NOT ours (hijacked/recycled pid) -> down.
	c := liveOursCtl()
	c.verify = func(int) (bool, bool) { return false, true }
	if got := detectServeMode(c, no, markerSame); got != serveDown {
		t.Errorf("live-not-ours = %v, want serveDown", got)
	}
}

// readServeLazyMarkerPid: pid content parses; legacy "lazy" content and a
// missing file are (0, false) — the conservative "treat as foreground" path.
func TestReadServeLazyMarkerPid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir) // the lazy marker lives in the STATE dir now
	if pid, ok := readServeLazyMarkerPid(); ok {
		t.Errorf("missing marker parsed as pid %d", pid)
	}
	path := config.ServeLazyMarkerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("lazy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := readServeLazyMarkerPid(); ok {
		t.Errorf("legacy marker content parsed as pid %d", pid)
	}
	if err := os.WriteFile(path, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := readServeLazyMarkerPid(); !ok || pid != 4242 {
		t.Errorf("pid marker = (%d, %v), want (4242, true)", pid, ok)
	}
}

// reloadRec records which serveReloader ops PropagateConfig invoked.
type reloadRec struct {
	kicked  bool
	stopped bool
	ensured bool
	order   []string
	stopOK  bool  // what the fake Stop returns as its stopped bool
	stopErr error // what the fake Stop returns as its error
}

func (r *reloadRec) reloader(mode serveMode, kickErr, ensureErr error) serveReloader {
	return serveReloader{
		mode: func() serveMode { return mode },
		kickManaged: func() error {
			r.kicked = true
			r.order = append(r.order, "kick")
			return kickErr
		},
		Stop: func(io.Writer) (bool, error) {
			r.stopped = true
			r.order = append(r.order, "stop")
			return r.stopOK, r.stopErr
		},
		ensure: func() error {
			r.ensured = true
			r.order = append(r.order, "ensure")
			return ensureErr
		},
	}
}

func TestPropagateServeConfig_Managed(t *testing.T) {
	rec := &reloadRec{}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveManaged, nil, nil), &out)
	if !rec.kicked || rec.stopped || rec.ensured {
		t.Errorf("managed: kicked=%v stopped=%v ensured=%v, want kick only", rec.kicked, rec.stopped, rec.ensured)
	}
	if !strings.Contains(out.String(), "restarted managed pix services") {
		t.Errorf("managed message = %q", out.String())
	}
}

func TestPropagateServeConfig_ManagedKickFails(t *testing.T) {
	rec := &reloadRec{}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveManaged, os.ErrPermission, nil), &out)
	if !strings.Contains(out.String(), "warning: could not restart the managed") {
		t.Errorf("managed-failure message = %q", out.String())
	}
}

func TestPropagateServeConfig_LazyStopsThenEnsures(t *testing.T) {
	rec := &reloadRec{stopOK: true}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveLazy, nil, nil), &out)
	if rec.kicked {
		t.Error("lazy must not kickstart (no unit exists)")
	}
	if len(rec.order) != 2 || rec.order[0] != "stop" || rec.order[1] != "ensure" {
		t.Errorf("lazy order = %v, want [stop ensure]", rec.order)
	}
	if !strings.Contains(out.String(), "restarted pix services (background)") {
		t.Errorf("lazy message = %q", out.String())
	}
}

func TestPropagateServeConfig_LazyEnsureFails(t *testing.T) {
	rec := &reloadRec{stopOK: true}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveLazy, nil, os.ErrDeadlineExceeded), &out)
	if !strings.Contains(out.String(), "warning: pix services were stopped but did not restart") {
		t.Errorf("lazy-failure message = %q", out.String())
	}
}

// M4: Stop returning stopped=false (refused: stale/hijacked/unverifiable
// pid, or nothing to stop) must NOT re-spawn — ensure would double-start
// against a possibly-still-live daemon — and must warn.
func TestPropagateServeConfig_LazyStopRefusedDoesNotEnsure(t *testing.T) {
	rec := &reloadRec{stopOK: false}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveLazy, nil, nil), &out)
	if !rec.stopped {
		t.Error("Stop not invoked")
	}
	if rec.ensured {
		t.Error("ensure ran despite Stop reporting stopped=false (double-start risk)")
	}
	if !strings.Contains(out.String(), "were not stopped") {
		t.Errorf("refused-stop warning missing: %q", out.String())
	}
}

// M4: a hard stop ERROR also warns and never ensures.
func TestPropagateServeConfig_LazyStopErrorDoesNotEnsure(t *testing.T) {
	rec := &reloadRec{stopOK: false, stopErr: os.ErrPermission}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveLazy, nil, nil), &out)
	if rec.ensured {
		t.Error("ensure ran despite a stop error")
	}
	if !strings.Contains(out.String(), "warning: could not stop the background pix services") {
		t.Errorf("stop-error warning missing: %q", out.String())
	}
}

func TestPropagateServeConfig_ForegroundTouchesNothing(t *testing.T) {
	rec := &reloadRec{}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveForeground, nil, nil), &out)
	if rec.kicked || rec.stopped || rec.ensured {
		t.Errorf("foreground must touch nothing: %+v", rec)
	}
	if !strings.Contains(out.String(), "foreground `pix serve` is running") {
		t.Errorf("foreground message = %q", out.String())
	}
}

func TestPropagateServeConfig_DownTouchesNothing(t *testing.T) {
	rec := &reloadRec{}
	var out bytes.Buffer
	PropagateConfig(rec.reloader(serveDown, nil, nil), &out)
	if rec.kicked || rec.stopped || rec.ensured {
		t.Errorf("down must touch nothing: %+v", rec)
	}
	if !strings.Contains(out.String(), "applies next time pix services start") {
		t.Errorf("down message = %q", out.String())
	}
}
