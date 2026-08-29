// hostservices_env_test.go — E2.7's red-first tests (P0-11, AC-40 mechanism
// half, docs/design/environments.md §10.4): the default-UNION-live-holder
// desired set, the synchronous collision refusal, and unknown-fails-closed.
package launch

import (
	"strings"
	"testing"

	"pix/host/envinfo"
)

func svc(name string, port int64) envinfo.HostService {
	return envinfo.HostService{Name: name, Command: name, Port: port}
}

// default-only: no environment is live, only the machine default's
// services are wanted.
func TestDesiredHostServices_DefaultOnly(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "home", Services: []envinfo.HostService{svc("home-proxy", 19001)}},
		{Name: "work", Live: false, Services: []envinfo.HostService{svc("work-proxy", 19002)}},
	}
	got, err := DesiredHostServices("home", envs)
	if err != nil {
		t.Fatalf("DesiredHostServices: %v", err)
	}
	if len(got) != 1 || got[0].Service.Name != "home-proxy" || got[0].Environment != "home" {
		t.Fatalf("got %+v, want only the default's home-proxy", got)
	}
}

// holder-only: no default is selected, but a live holder's services are
// still wanted.
func TestDesiredHostServices_HolderOnly(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "work", Live: true, Services: []envinfo.HostService{svc("work-proxy", 19002)}},
	}
	got, err := DesiredHostServices("", envs)
	if err != nil {
		t.Fatalf("DesiredHostServices: %v", err)
	}
	if len(got) != 1 || got[0].Service.Name != "work-proxy" || got[0].Environment != "work" {
		t.Fatalf("got %+v, want only the live holder's work-proxy", got)
	}
}

// both: default plus a distinct live holder union together.
func TestDesiredHostServices_Both(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "home", Services: []envinfo.HostService{svc("home-proxy", 19001)}},
		{Name: "work", Live: true, Services: []envinfo.HostService{svc("work-proxy", 19002)}},
	}
	got, err := DesiredHostServices("home", envs)
	if err != nil {
		t.Fatalf("DesiredHostServices: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want both home-proxy and work-proxy", got)
	}
	names := map[string]bool{}
	for _, w := range got {
		names[w.Service.Name] = true
	}
	if !names["home-proxy"] || !names["work-proxy"] {
		t.Fatalf("got %+v, missing one of home-proxy/work-proxy", got)
	}
}

// neither: no default selected, and every registered environment is
// proven not live — the desired set is empty.
func TestDesiredHostServices_Neither(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "home", Live: false, Services: []envinfo.HostService{svc("home-proxy", 19001)}},
		{Name: "work", Live: false, Services: []envinfo.HostService{svc("work-proxy", 19002)}},
	}
	got, err := DesiredHostServices("", envs)
	if err != nil {
		t.Fatalf("DesiredHostServices: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want an empty desired set", got)
	}
}

// stale/unknown: a holder state that could not be proven preserves the
// running service, fail closed — never torn down on a guess.
func TestDesiredHostServices_UnknownHolderStatePreservesService(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "work", Live: false, Unknown: true, Services: []envinfo.HostService{svc("work-proxy", 19002)}},
	}
	got, err := DesiredHostServices("", envs)
	if err != nil {
		t.Fatalf("DesiredHostServices: %v", err)
	}
	if len(got) != 1 || got[0].Service.Name != "work-proxy" {
		t.Fatalf("got %+v, want the unknown-state environment's service preserved", got)
	}
}

// A stopped-then-restarted holder's service disappears from the desired
// set the instant liveness is proven false (no Unknown), and reappears
// once it is proven live again — this is a pure recompute, not state kept
// across calls.
func TestDesiredHostServices_RecomputesEveryCall(t *testing.T) {
	stopped := []EnvironmentServiceSnapshot{{Name: "work", Live: false, Services: []envinfo.HostService{svc("work-proxy", 19002)}}}
	if got, err := DesiredHostServices("", stopped); err != nil || len(got) != 0 {
		t.Fatalf("stopped: got %+v, err %v, want empty", got, err)
	}
	running := []EnvironmentServiceSnapshot{{Name: "work", Live: true, Services: []envinfo.HostService{svc("work-proxy", 19002)}}}
	if got, err := DesiredHostServices("", running); err != nil || len(got) != 1 {
		t.Fatalf("running: got %+v, err %v, want one", got, err)
	}
}

// A unit-name collision between two DIFFERENT environments refuses
// synchronously, naming both claimants.
func TestDesiredHostServices_UnitNameCollisionRefusesNamingBothClaimants(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "home", Services: []envinfo.HostService{svc("proxy", 19001)}},
		{Name: "work", Live: true, Services: []envinfo.HostService{svc("proxy", 19002)}},
	}
	_, err := DesiredHostServices("home", envs)
	if err == nil {
		t.Fatal("want a collision refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "home") || !strings.Contains(err.Error(), "work") || !strings.Contains(err.Error(), `"proxy"`) {
		t.Fatalf("refusal %q does not name both claimants and the unit name", err.Error())
	}
}

// A port collision between two DIFFERENT environments refuses
// synchronously, naming both claimants.
func TestDesiredHostServices_PortCollisionRefusesNamingBothClaimants(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "home", Services: []envinfo.HostService{svc("home-proxy", 19001)}},
		{Name: "work", Live: true, Services: []envinfo.HostService{svc("work-proxy", 19001)}},
	}
	_, err := DesiredHostServices("home", envs)
	if err == nil {
		t.Fatal("want a collision refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "home") || !strings.Contains(err.Error(), "work") || !strings.Contains(err.Error(), "19001") {
		t.Fatalf("refusal %q does not name both claimants and the port", err.Error())
	}
}

// The SAME environment repeating its own service name/port across two
// entries is not a cross-environment collision and is not refused here.
func TestDesiredHostServices_SameEnvironmentRepeatIsNotACollision(t *testing.T) {
	envs := []EnvironmentServiceSnapshot{
		{Name: "home", Services: []envinfo.HostService{svc("proxy", 19001), svc("proxy", 19001)}},
	}
	got, err := DesiredHostServices("home", envs)
	if err != nil {
		t.Fatalf("DesiredHostServices: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want the duplicate collapsed to one entry", got)
	}
}
