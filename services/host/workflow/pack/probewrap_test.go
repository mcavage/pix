package pack

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/sys/systest"
)

// probewrap_test.go — setup's probe must ask the question the gateway asks.
//
// Mark moved his 1Password items into another vault. The references went stale,
// so the MCP gateway could not start google-workspace at all. But setup printed:
//
//     ✓ Google Workspace ...: ready
//         verified by: gog --readonly gmail labels list
//
// because it ran that argv RAW, in a shell where gog's keyring was already
// unlocked. `pix doctor`, which wraps, called the same integration broken in the
// same minute. mcp.OpRunWrap's comment already stated the rule: a probe that does
// not go through the wrapper proves nothing about the thing it claims to check.

// wrapPack writes a real, ACCEPTED pack whose one declarative step provisions an
// integration that may or may not declare a credential.
func wrapPack(t *testing.T, env string, envKeys []string) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(state, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "state"))
	root := t.TempDir()
	mustWritePack(t, root, packinfo.Manifest{
		Name: "work", Schema: 1,
		Integrations: []packinfo.Integration{{
			Name: "Google Workspace", MCP: "gw", Command: "gog",
			Env: env, EnvKeys: envKeys, Setup: "gw-setup",
		}},
		Setup: []packinfo.SetupStep{{
			ID: "gw-setup", Description: "Workspace", Required: true,
			Require: []packinfo.SetupRequire{{Kind: "probe", Argv: []string{"gog", "labels"}}},
			Apply:   []packinfo.SetupApply{{Kind: "exec", Argv: []string{"gog", "auth"}}},
		}},
	})
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := ComputeHostExecFingerprint(root, ComputeHostBoM(p))
	if err != nil {
		t.Fatal(err)
	}
	store := &PackTrustStore{Version: 1}
	store.RecordAcceptance(store.TrustKey(root), PackTrustRecord{
		Path: packinfo.CanonicalizePackRoot(root), Fingerprint: fp})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	return root
}

// runWithWrap records the argv the probe actually executed.
func runWithWrap(t *testing.T, root string, wrap ProbeWrapFn) []string {
	t.Helper()
	var got []string
	env := hostenv.Env{System: &systest.Fake{
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			got = append([]string{name}, args...)
			return "", false, nil
		},
		LookPathFn:       func(n string) (string, error) { return "/usr/bin/" + n, nil },
		ReadFileFn:       func(string) (string, error) { return "", errors.New("none") },
		GetenvFn:         func(string) string { return "" },
		HomeDirFn:        func() string { return "/tmp/h" },
		RunInteractiveFn: func(string, ...string) error { return nil },
	}}
	if err := RunPackSetup(env, &bytes.Buffer{}, root, nil, false, wrap); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return got
}

// TestSetupProbeRunsThroughTheGatewaysWrapper is the regression.
func TestSetupProbeRunsThroughTheGatewaysWrapper(t *testing.T) {
	wrap := func(argv []string) []string {
		return append([]string{"op", "run", "--no-masking", "--env-file=/refs", "--"}, argv...)
	}
	got := runWithWrap(t, wrapPack(t, "GOG_KEYRING_PASSWORD", nil), wrap)
	if len(got) == 0 || got[0] != "op" {
		t.Fatalf("the probe must run through the wrapper the gateway uses, got: %v", got)
	}
	if !strings.Contains(strings.Join(got, " "), "gog labels") {
		t.Errorf("the wrapped argv must still end in the pack's own probe: %v", got)
	}
}

// TestSetupProbeIsNotWrappedWithoutCredentials is the other half, and it is not
// a nicety: `op run --env-file` resolves EVERY reference in the file, so wrapping
// a credential-free probe makes it share fate with unrelated entries. One stale
// BambooHR reference stopping gog is exactly how this host broke.
func TestSetupProbeIsNotWrappedWithoutCredentials(t *testing.T) {
	wrap := func(argv []string) []string { return append([]string{"op", "run", "--"}, argv...) }
	got := runWithWrap(t, wrapPack(t, "", nil), wrap)
	if len(got) == 0 || got[0] != "gog" {
		t.Errorf("a step whose integration declares no credential must run RAW, got: %v", got)
	}
}

// TestSetupProbeWrapsForEnvKeysToo: EnvKeys alone is still a credential handed to
// a host command, so it must be wrapped like Env.
func TestSetupProbeWrapsForEnvKeysToo(t *testing.T) {
	wrap := func(argv []string) []string { return append([]string{"op", "run", "--"}, argv...) }
	got := runWithWrap(t, wrapPack(t, "", []string{"GOG_ACCOUNT"}), wrap)
	if len(got) == 0 || got[0] != "op" {
		t.Errorf("EnvKeys is a credential surface too, got: %v", got)
	}
}
