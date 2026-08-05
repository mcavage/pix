// unitview_test.go — U07d: the pack-side [[services]] → supervisor export.
//
// All tests use REAL temp packs on disk (LoadPack over a written pack.toml),
// the real fingerprint code, and the real launcher-owned trust store under an
// isolated PIX_CONFIG/XDG_STATE_HOME. No mocks: acceptance is a real
// pack-trust.json record; rejection and change-detection are the real
// fingerprint comparison failing.
package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

func viewEnv() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{}}
}

// TestAcceptedServices_RejectedBeforeAcceptance: a freshly loaded pack whose
// surface was NEVER accepted exports nothing — the error names the re-review
// path and no view escapes. Consent strictly precedes export.
func TestAcceptedServices_RejectedBeforeAcceptance(t *testing.T) {
	isolatePackHost(t)
	root := writeServicePack(t, validGoPluginService)
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := AcceptedGoPluginServices(p, "", viewEnv())
	if err == nil {
		t.Fatal("unaccepted pack exported services; want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "not accepted") {
		t.Errorf("error = %v, want it to say the pack is not accepted", err)
	}
	if views != nil {
		t.Errorf("views = %+v, want nil on rejection", views)
	}
}

// TestAcceptedServices_AcceptedExportsMinimalView: after a real acceptance the
// export returns exactly the go-plugin entries (container declarations are
// consented but never exported — no runtime consumes them), normalized, with
// an absolute path under the pack root and env reference NAMES only.
func TestAcceptedServices_AcceptedExportsMinimalView(t *testing.T) {
	isolatePackHost(t)
	root := writeServicePack(t, validGoPluginService+validContainerService)
	acceptPackSurface(t, root, "")
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := AcceptedGoPluginServices(p, "", viewEnv())
	if err != nil {
		t.Fatalf("AcceptedGoPluginServices after acceptance: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %+v, want exactly the one go-plugin service (container skipped)", views)
	}
	v := views[0]
	if v.Name != "telemetry" || v.Activation != "on-demand" {
		t.Errorf("view identity = %q/%q, want telemetry/on-demand", v.Name, v.Activation)
	}
	want := filepath.Join(root, "bin/telemetry")
	if v.Path != want || !filepath.IsAbs(v.Path) {
		t.Errorf("view path = %q, want absolute %q", v.Path, want)
	}
	if v.SHA != "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66" {
		t.Errorf("view sha = %q", v.SHA)
	}
	if len(v.Argv) != 2 || v.Argv[0] != "--dir" {
		t.Errorf("view argv = %v", v.Argv)
	}
	if len(v.Env) != 1 || v.Env[0] != "TELEMETRY_TOKEN" {
		t.Errorf("view env = %v, want reference names only", v.Env)
	}
	if v.Port != 12000 || v.Listen != "127.0.0.1" || v.Health != "/healthz" {
		t.Errorf("view front door = %s:%d health %q", v.Listen, v.Port, v.Health)
	}
}

// TestAcceptedServices_ChangeSinceAcceptanceRegates: accept, then change ONE
// fingerprinted field on disk (argv). The reloaded pack must fail the
// fingerprint match — any change to the accepted surface re-gates before a
// single view (and therefore a single staged byte) exists.
func TestAcceptedServices_ChangeSinceAcceptanceRegates(t *testing.T) {
	isolatePackHost(t)
	root := writeServicePack(t, validGoPluginService)
	acceptPackSurface(t, root, "")

	changed := strings.Replace(validGoPluginService, `argv = ["--dir", "data"]`, `argv = ["--dir", "data", "--exfiltrate"]`, 1)
	if changed == validGoPluginService {
		t.Fatal("test bug: argv replacement did not apply")
	}
	manifest := "name = \"svc-pack\"\nschema = 2\n" + changed
	if err := os.WriteFile(filepath.Join(root, PackManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := AcceptedGoPluginServices(p, "", viewEnv())
	if err == nil || views != nil {
		t.Fatalf("changed surface exported views %+v (err=%v); want fail-closed re-gate", views, err)
	}
	if !strings.Contains(err.Error(), "changed since acceptance") {
		t.Errorf("error = %v, want it to name the change-since-acceptance re-gate", err)
	}
}

// TestAcceptedServices_RevalidatesMutatedInfo: the export re-runs the full
// load-time validation, so an Info mutated in memory into a shape LoadPack
// would refuse (here: squatting the reserved memory port) is rejected BEFORE
// the trust check ever runs — reserved ports/loopback/env-shape rules hold at
// the last pack-side gate too, not only at load.
func TestAcceptedServices_RevalidatesMutatedInfo(t *testing.T) {
	isolatePackHost(t)
	root := writeServicePack(t, validGoPluginService)
	acceptPackSurface(t, root, "")
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	p.Manifest.Services[0].Port = 11435 // pix-host memory's reserved front door
	if _, err := AcceptedGoPluginServices(p, "", viewEnv()); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("mutated Info exported (err=%v); want the reserved-port refusal", err)
	}
}

// TestAcceptedServices_NoServicesIsQuietlyEmpty: a pack with no [[services]]
// answers nil/nil without consulting the trust store — nothing to admit.
func TestAcceptedServices_NoServicesIsQuietlyEmpty(t *testing.T) {
	isolatePackHost(t)
	root := writeServicePack(t, "")
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := AcceptedGoPluginServices(p, "", viewEnv())
	if err != nil || views != nil {
		t.Fatalf("no-services pack = (%+v, %v), want (nil, nil)", views, err)
	}
}
