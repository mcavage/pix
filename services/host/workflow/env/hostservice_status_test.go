package env

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pix/host/health"
)

func svcBoM(probe string) BillOfMaterials {
	return BillOfMaterials{HostServices: []HostServiceItem{{
		Name: "snow-proxy", Command: "docker", Port: 11442, Probe: probe,
	}}}
}

// A declared host service must APPEAR on the status surface. It was omitted
// entirely before, and an omitted row reads as "nothing to report" — which,
// for the one entry carrying an explicit health endpoint, is the most
// misleading answer the surface could give.
func TestHostServiceAppearsOnTheStatusSurface(t *testing.T) {
	got := HostServiceStatuses(svcBoM("http://127.0.0.1:1/health"), func(string) (bool, string) { return true, "200 OK" })
	if len(got) != 1 {
		t.Fatalf("want one service row, got %d", len(got))
	}
	if got[0].Kind != ServiceKind || !got[0].Declared {
		t.Fatalf("service row is not tagged as a declared service: %+v", got[0])
	}
	if got[0].Registered != "" {
		t.Fatalf("a host service must carry no registration state (pix registers nothing for one), got %q", got[0].Registered)
	}
}

// Reachability is the environment's own declared probe, actually fetched.
func TestHostServiceReachabilityIsTheDeclaredProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	up := HostServiceStatuses(svcBoM(srv.URL+"/health"), LoopbackHTTPProbe)
	if up[0].Reachable != health.StatusReady {
		t.Fatalf("an answering probe must be Ready, got %s (%s)", up[0].Reachable, up[0].ReachableDetail)
	}
	down := HostServiceStatuses(svcBoM(srv.URL+"/nope"), LoopbackHTTPProbe)
	if down[0].Reachable != health.StatusAbsent {
		t.Fatalf("a probe that answers non-2xx is a verified negative, got %s", down[0].Reachable)
	}
}

// No probe declared is Unknown, never a guessed Ready: this package has no
// other way to know whether a service works, and a port number is not one.
func TestHostServiceWithNoProbeIsUnknown(t *testing.T) {
	got := HostServiceStatuses(svcBoM(""), LoopbackHTTPProbe)
	if got[0].Reachable != health.StatusUnknown {
		t.Fatalf("no declared probe must be Unknown, got %s", got[0].Reachable)
	}
	if !strings.Contains(got[0].ReachableDetail, "no probe declared") {
		t.Fatalf("the reason must name the missing declaration, got %q", got[0].ReachableDetail)
	}
}

// A [[host.services]] entry is a loopback process by contract. `pix env show`
// must not turn a declared "health endpoint" into an outbound request made on
// the user's behalf from a file whose job is to be read.
func TestNonLoopbackProbeIsRefusedNotFetched(t *testing.T) {
	fetched := false
	probe := func(string) (bool, string) { fetched = true; return true, "" }
	for _, raw := range []string{
		"http://example.com/ping",
		"https://127.0.0.1/health",
		"http://169.254.169.254/latest/meta-data",
		"file:///etc/passwd",
	} {
		got := HostServiceStatuses(svcBoM(raw), probe)
		if got[0].Reachable != health.StatusUnknown {
			t.Fatalf("%s must be Unknown, not %s", raw, got[0].Reachable)
		}
		if !strings.Contains(got[0].ReachableDetail, "not fetched") && !strings.Contains(got[0].ReachableDetail, "not a URL") {
			t.Fatalf("%s: the reason must say it was not fetched, got %q", raw, got[0].ReachableDetail)
		}
	}
	if fetched {
		t.Fatal("a non-loopback probe URL was actually requested")
	}
	// ...and localhost/loopback IS fetched, or the guard would be a mute.
	HostServiceStatuses(svcBoM("http://localhost:9/health"), probe)
	if !fetched {
		t.Fatal("a loopback probe URL was refused; the guard is too tight to be useful")
	}
}

// An environment with no services at all pays nothing and prints nothing.
func TestNoHostServicesIsNoRows(t *testing.T) {
	if got := HostServiceStatuses(BillOfMaterials{}, LoopbackHTTPProbe); got != nil {
		t.Fatalf("want no rows, got %+v", got)
	}
}
