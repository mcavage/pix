package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDaemonProbe_AnswersTheQuestionTheSandboxAsks. A pack daemon is how the
// SANDBOX reaches something (snow-proxy is the whole Snowflake path), so the only
// useful question is whether it is serving — not whether a process exists. A
// wedged daemon holds its pid and answers nothing, which is exactly the case a
// LaunchAgent's KeepAlive cannot see and why this row exists at all.
func TestDaemonProbe_AnswersTheQuestionTheSandboxAsks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	t.Run("a serving daemon is ready", func(t *testing.T) {
		r := DaemonProbe{Servers: []DaemonServer{
			{Name: "snow-proxy", Listen: host, Port: port, Health: "/health"},
		}}.Check(context.Background())
		if r.Effective() != StatusReady {
			t.Errorf("a daemon answering its health path must be ready, got %v (%s)", r.Effective(), r.Detail)
		}
	})

	t.Run("a wrong health path is a VERIFIED gap, not an unknown", func(t *testing.T) {
		r := DaemonProbe{Servers: []DaemonServer{
			{Name: "snow-proxy", Listen: host, Port: port, Health: "/nope", Fix: "pix serve stop && pix serve"},
		}}.Check(context.Background())
		if r.Effective() != StatusAbsent {
			t.Errorf("we asked the exact question and got a definite no; want a gap, got %v", r.Effective())
		}
		if !strings.Contains(r.Evidence, "404") {
			t.Errorf("evidence should name what the daemon actually said, got: %s", r.Evidence)
		}
		if r.Fix == "" {
			t.Error("a verified gap with a known repair must carry it")
		}
	})

	t.Run("a dead port is a gap", func(t *testing.T) {
		// Port 1 on loopback: nothing listens, and connect fails fast.
		r := DaemonProbe{Servers: []DaemonServer{
			{Name: "dead", Listen: "127.0.0.1", Port: 1, Health: "tcp"},
		}}.Check(context.Background())
		if r.Effective() != StatusAbsent {
			t.Errorf("a daemon nothing is listening for must be a gap, got %v", r.Effective())
		}
	})
}

// TestDaemonProbe_NoDaemonsIsHealthy: most hosts declare none, and a report
// that nags about an absent optional facet teaches people to stop reading it.
// It reads off, not ready — nothing was actually checked, so claiming
// StatusReady would be a green check over a capability nobody exercised.
func TestDaemonProbe_NoDaemonsIsHealthy(t *testing.T) {
	r := DaemonProbe{}.Check(context.Background())
	if r.Effective() != StatusOff {
		t.Errorf("no declared daemons must read off, got %v", r.Effective())
	}
	if r.OK() {
		t.Error("off must never report OK: nothing was verified working")
	}
	if r.Missing() {
		t.Error("off must never report Missing: there is nothing to repair")
	}
	if r.Fix != "" {
		t.Errorf("an off result must carry no fix, got %q", r.Fix)
	}
}

// TestDaemonProbe_UnpinnedIsEvidenceNotAGap. A daemon built from source on this
// machine has no SHA a shared manifest could carry, so it is identified by a name
// on PATH. That is a weaker guarantee the user already accepted on the adoption
// screen — reporting it as a FAULT would cry wolf on every single run.
func TestDaemonProbe_UnpinnedIsEvidenceNotAGap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	r := DaemonProbe{Servers: []DaemonServer{
		{Name: "snow-proxy", Listen: host, Port: port, Health: "/health", Unpinned: true},
	}}.Check(context.Background())
	if r.Effective() != StatusReady {
		t.Errorf("unpinned but serving is ready, got %v (%s)", r.Effective(), r.Detail)
	}
	if !strings.Contains(r.Evidence, "unpinned") {
		t.Errorf("the weaker identity must still be VISIBLE in the evidence, got: %s", r.Evidence)
	}
}

// TestDaemonProbe_IsBoundedByTheContext: this runs inside `pix doctor`, which
// bounds every probe. A daemon that accepts a connection and then never replies
// must not be able to hang the report.
func TestDaemonProbe_IsBoundedByTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stalled := make(chan struct{})
	defer close(stalled)
	p := DaemonProbe{
		Servers: []DaemonServer{{Name: "stalled", Port: 9, Health: "tcp"}},
		Dial: func(_, _ string, budget time.Duration) error {
			select {
			case <-time.After(budget):
				return errors.New("i/o timeout")
			case <-stalled:
				return nil
			}
		},
	}
	done := make(chan Result, 1)
	go func() { done <- p.Check(ctx) }()
	select {
	case r := <-done:
		if r.Effective() != StatusAbsent {
			t.Errorf("a stalled daemon is a gap, got %v", r.Effective())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe outran its context; `pix doctor` must not be hangable by a wedged daemon")
	}
}
