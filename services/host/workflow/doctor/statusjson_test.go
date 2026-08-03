// statusjson_test.go — moved from cmd/pix: the subject is GatherStatus and the
// status JSON shape, both of which live here now.
package doctor

import (
	"errors"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/readiness"
	"pix/host/sys/systest"
)

// TestStatusJSONCarriesChecksAndExit (AC-P0-222): status --json gains the
// shared readiness rows and an `exit` sibling equal to the process exit code,
// ADDITIVELY — the v1 keys keep their names.
func TestStatusJSONCarriesChecksAndExit(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	st := GatherStatus(cfg, "default", fakeStatusEnv())
	if len(st.Checks) == 0 {
		t.Fatal("status --json must carry the shared readiness checks array")
	}
	for _, c := range st.Checks {
		if c.Axis == "" {
			t.Errorf("check %q has no axis", c.Label)
		}
	}
	if st.Exit != readiness.ExitReady {
		t.Errorf("exit = %d, want %d (a provider key is set and memory identifies itself)", st.Exit, readiness.ExitReady)
	}
	// Suppressed-3: an unverifiable axis (no sbx, i.e. inside the sandbox)
	// must never fail a script that only wanted the JSON.
	inVM := fakeStatusEnv()
	systest.Of(inVM.System).LookPathFn = func(string) (string, error) { return "", errNotFoundFixture }
	if got := GatherStatus(cfg, "default", inVM).Exit; got != readiness.ExitReady {
		t.Errorf("unverifiable axes must not fail status: exit = %d", got)
	}
	// A POSITIVELY verified core failure still exits 1.
	noKeys := fakeStatusEnv()
	systest.Of(noKeys.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "", nil // sbx answered: zero keys set
		}
		return "", nil
	}
	if got := GatherStatus(cfg, "default", noKeys).Exit; got != readiness.ExitNotReady {
		t.Errorf("a verified missing model key must exit %d, got %d", readiness.ExitNotReady, got)
	}
}

// TestStatusProbesSecretsOnce (latency): rendering readiness must not cost a
// second `sbx secret ls`. The snapshot is fed the evidence status already
// paid for, and the whole gather stays well inside a second on a fake host.

// TestStatusProbesSecretsOnce (latency): rendering readiness must not cost a
// second `sbx secret ls`. The snapshot is fed the evidence status already
// paid for, and the whole gather stays well inside a second on a fake host.
func TestStatusProbesSecretsOnce(t *testing.T) {
	env := fakeStatusEnv()
	base := systest.Of(env.System).RunFn
	secretProbes, dials := 0, map[int]int{}
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
			secretProbes++
		}
		return base(name, args...)
	}
	baseDial := systest.Of(env.System).DialLocalFn
	systest.Of(env.System).DialLocalFn = func(port int) bool { dials[port]++; return baseDial(port) }

	start := time.Now()
	GatherStatus(&config.Config{Services: []string{"memory"}}, "default", env)
	if el := time.Since(start); el > time.Second {
		t.Errorf("GatherStatus took %s on a fake host — status must stay fast", el)
	}
	if secretProbes != 1 {
		t.Errorf("`sbx secret ls` ran %d times, want exactly 1 (the snapshot reuses the evidence)", secretProbes)
	}
	for port, n := range dials {
		if n > 1 {
			t.Errorf("port %d was dialed %d times, want at most 1", port, n)
		}
	}
}

// errNotFoundFixture is the "this binary is not installed here" error the
// cold-host fixtures below return from lookPath.
var errNotFoundFixture = errors.New("not found")
