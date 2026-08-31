package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"time"

	"pix/host/hosttrust"
	"pix/host/sandbox"
	"pix/host/sys/systest"
)

// stateHome points config.StateDir/DataDir/Path (all via PIX_HOME) at a temp
// dir so a test never touches the real launcher state.
func stateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_HOME", filepath.Join(dir, "pixhome"))
	t.Setenv("HOME", dir)
	return dir
}

func testInput(name string) EffectiveInput {
	return EffectiveInput{
		SandboxName:      name,
		Template:         "docker.io/mcavage/pix:0.0.1",
		PullPolicy:       EffectivePullPolicy,
		PrimaryWorkspace: envinfo.WorkspaceFact{Path: "/repo"},
		PersonalContext:  envinfo.WorkspaceFact{Path: "/ctx"},
	}
}

// The launch composes real RuntimeFacts ONCE and persists the EXACT bytes
// it would hand `sbx env create` — byte-identical to a re-render of the
// same facts, with no second grammar in between.
func TestRenderEffectiveEnvironment_PersistsExactBytes(t *testing.T) {
	stateHome(t)
	in := testInput("pix-repo-abc123")
	eff, err := RenderEffectiveEnvironment(in, nil)
	if err != nil {
		t.Fatalf("RenderEffectiveEnvironment: %v", err)
	}
	onDisk, err := os.ReadFile(eff.Path)
	if err != nil {
		t.Fatalf("read persisted effective file: %v", err)
	}
	if string(onDisk) != string(eff.Bytes) {
		t.Fatalf("persisted bytes differ from rendered bytes:\n on disk: %q\n rendered: %q", onDisk, eff.Bytes)
	}
	again, err := envinfo.RenderEffective(ComposeRuntimeFacts(in))
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if string(again) != string(eff.Bytes) {
		t.Fatal("re-rendering the same facts produced different bytes: the render is not deterministic")
	}
	// It lands in launcher STATE, never config: moving the config dir aside
	// can never orphan a running sandbox from its own removal path.
	state, _ := config.StateDir()
	if !strings.HasPrefix(eff.Path, state) {
		t.Fatalf("effective file %q is not under the state dir %q", eff.Path, state)
	}
	if got := filepath.Base(eff.Path); got != EffectiveEnvFileName {
		t.Fatalf("effective file name = %q, want %q", got, EffectiveEnvFileName)
	}
}

// The effective document names the ACTUAL pix-* sandbox name, attributed
// BEFORE composition: two repositories under ONE environment get two
// distinct names and two distinct files, never a collision.
func TestEffectiveEnvironment_TwoRepositoriesOneEnvironment(t *testing.T) {
	stateHome(t)
	doc := &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1, Name: "authored-name"}
	sel := EnvSelection{Name: "work", Root: "/envs/work", Document: doc, Reviewed: true}

	nameA, nameB := sandbox.Name("/repo/a"), sandbox.Name("/repo/b")
	if nameA == nameB {
		t.Fatal("two repositories must not derive one sandbox name")
	}
	var paths []string
	for _, tc := range []struct{ name, ws string }{{nameA, "/repo/a"}, {nameB, "/repo/b"}} {
		in := testInput(tc.name)
		in.Selection = sel
		in.PrimaryWorkspace = envinfo.WorkspaceFact{Path: tc.ws}
		eff, err := RenderEffectiveEnvironment(in, nil)
		if err != nil {
			t.Fatalf("render %s: %v", tc.name, err)
		}
		if !strings.Contains(string(eff.Bytes), "name: "+tc.name) {
			t.Fatalf("effective document for %s does not name it:\n%s", tc.name, eff.Bytes)
		}
		if strings.Contains(string(eff.Bytes), "authored-name") {
			t.Fatal("composition determined identity: the authored name won over the pre-composition pix-* name")
		}
		paths = append(paths, eff.Path)
	}
	if paths[0] == paths[1] {
		t.Fatalf("both sandboxes share one effective file %q", paths[0])
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("effective file %s missing: %v", p, err)
		}
	}
}

// Create argv is `sbx env create <effective>` and nothing else: no second
// selectable create shape, no `--prune-bindings`, no name-based create.
func TestEnvCreateArgs_IsTheOnlyCreateShape(t *testing.T) {
	got := EnvCreateArgs("/state/environments/pix-x/effective.sbxenv.yaml")
	want := []string{"env", "create", "/state/environments/pix-x/effective.sbxenv.yaml"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("EnvCreateArgs = %v, want %v", got, want)
	}
}

// §6.3 precedence: --model > selected environment [models].main > pi's own
// default (which is expressed as "no model at all", never a fabricated id).
func TestSelectSessionModel_Precedence(t *testing.T) {
	sc := &envinfo.Sidecar{}
	sc.Models.Main = "zai/glm-5"
	for _, tc := range []struct {
		name, explicit string
		sidecar        *envinfo.Sidecar
		want, source   string
	}{
		{"explicit wins", "anthropic/claude-sonnet-5", sc, "anthropic/claude-sonnet-5", "--model"},
		{"environment main", "", sc, "zai/glm-5", "[models].main"},
		{"pi default", "", nil, "", ""},
		{"pi default, sidecar without main", "", &envinfo.Sidecar{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, src := SelectSessionModel(tc.explicit, tc.sidecar)
			if got != tc.want || src != tc.source {
				t.Fatalf("SelectSessionModel = (%q, %q), want (%q, %q)", got, src, tc.want, tc.source)
			}
		})
	}
}

// Drift refuses with canonical attributed key paths and the EXACT recovery
// command — `pix rm NAME && pix run --env ENV`, never a --name.
func TestDecideEnvAttach_DriftRefusalCopy(t *testing.T) {
	id := "inst-1"
	entry := &sandbox.Entry{Name: "pix-repo-abc123", State: sandbox.StateRunning, IdentityVerified: true, InstanceID: &id}
	gate := AttachGate{
		Entry:              entry,
		RecordedInstanceID: id,
		Stored:             sandbox.Fingerprint{"env.FOO": "a", "sandboxOptions.template": "t1"},
		StoredFound:        true,
		Current:            sandbox.Fingerprint{"env.FOO": "b", "sandboxOptions.template": "t1"},
		Reviewed:           true,
	}
	d := DecideEnvAttach(gate, "pix-repo-abc123", "work")
	if d.Attach {
		t.Fatal("a diverged creation fingerprint must refuse, never attach")
	}
	want := `"pix-repo-abc123" no longer matches its recorded creation fingerprint — refusing to attach.
     drifted: env.FOO changed (pix-managed, no pre-composition source)
     recreate it: pix rm pix-repo-abc123 && pix run --env work`
	if d.Refusal != want {
		t.Fatalf("drift refusal copy:\n got: %q\nwant: %q", d.Refusal, want)
	}
	if strings.Contains(d.Refusal, "--name") {
		t.Fatal("the recreate command must never teach --name")
	}
}

// Every other §10.2 condition refuses too, and each names its own reason.
// A STOPPED, schema-verified, identity-matched row is deliberately NOT in
// this refusal table any more (review round 1 blocker #2): it must attach,
// same as a running one — see TestDecideEnvAttach_StoppedRowAttaches.
func TestDecideEnvAttach_EveryConditionRefuses(t *testing.T) {
	id := "inst-1"
	running := func() *sandbox.Entry {
		return &sandbox.Entry{Name: "pix-x", State: sandbox.StateRunning, IdentityVerified: true, InstanceID: &id}
	}
	other := "inst-2"
	unverified := running()
	unverified.IdentityVerified = false
	mismatched := running()
	mismatched.InstanceID = &other

	for _, tc := range []struct {
		name string
		gate AttachGate
		want string
	}{
		{"no row", AttachGate{Reviewed: true}, "is not a schema-verified running or stopped sandbox"},
		{"unverified", AttachGate{Entry: unverified, RecordedInstanceID: id, Reviewed: true}, "is not a schema-verified running or stopped sandbox"},
		{"no record", AttachGate{Entry: running(), Reviewed: true}, "has no recorded creation instance on this host"},
		{"instance mismatch", AttachGate{Entry: mismatched, RecordedInstanceID: id, Reviewed: true}, "is a different instance"},
		{"unreviewed", AttachGate{Entry: running(), RecordedInstanceID: id, Reviewed: false}, "no longer reviewed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideEnvAttach(tc.gate, "pix-x", "work")
			if d.Attach {
				t.Fatal("must refuse")
			}
			if !strings.Contains(d.Refusal, tc.want) {
				t.Fatalf("refusal %q does not name %q", d.Refusal, tc.want)
			}
			if !strings.Contains(d.Refusal, "pix rm pix-x && pix run --env work") {
				t.Fatalf("refusal %q does not carry the exact recreate command", d.Refusal)
			}
		})
	}
	ok := DecideEnvAttach(AttachGate{Entry: running(), RecordedInstanceID: id, Reviewed: true}, "pix-x", "work")
	if !ok.Attach || ok.Refusal != "" {
		t.Fatalf("a fully satisfied gate must attach, got %+v", ok)
	}
}

// TestDecideEnvAttach_StoppedRowAttaches: review round 1 blocker #2. A
// STOPPED sandbox that is otherwise schema-verified, identity-matched, and
// still reviewed must ATTACH — not be refused the way a stale "running
// only" gate used to refuse it. The command layer (run_cmd.go) reads
// entry.State back off this same decision to choose the argv: exec for
// running, the legacy `sbx run --name` reattach (which actually starts a
// stopped sandbox) otherwise.
func TestDecideEnvAttach_StoppedRowAttaches(t *testing.T) {
	id := "inst-1"
	stopped := &sandbox.Entry{Name: "pix-x", State: sandbox.StateStopped, IdentityVerified: true, InstanceID: &id}
	d := DecideEnvAttach(AttachGate{Entry: stopped, RecordedInstanceID: id, Reviewed: true}, "pix-x", "work")
	if !d.Attach || d.Refusal != "" {
		t.Fatalf("a schema-verified stopped, identity-matched, reviewed sandbox must attach, got %+v", d)
	}
}

// A missing launcher HMAC key (what `pix reset` leaves behind) yields ONE
// reset attribution, never N per-interpolation drifts.
func TestResetInvalidated_SingleAttribution(t *testing.T) {
	d := DecideEnvAttach(AttachGate{
		Entry:              &sandbox.Entry{Name: "pix-x", State: sandbox.StateRunning, IdentityVerified: true, InstanceID: strptr("i")},
		RecordedInstanceID: "i",
		StoredFound:        true,
		ResetInvalidated:   true,
		Reviewed:           true,
	}, "pix-x", "work")
	if d.Attach {
		t.Fatal("a reset-invalidated acceptance must refuse")
	}
	if len(d.Drifts) != 1 || d.Drifts[0].ComposedKey != "*" {
		t.Fatalf("want exactly one whole-environment drift, got %+v", d.Drifts)
	}
	if !strings.Contains(d.Refusal, "acceptance invalidated by reset") {
		t.Fatalf("refusal %q does not carry the reset attribution", d.Refusal)
	}
}

func strptr(s string) *string { return &s }

// The creation fingerprint is HMAC-keyed: an authored ${VAR} is never
// fingerprinted as its raw resolved value, and a missing key record is the
// reset-invalidated state rather than an error or an unkeyed digest.
func TestCreationFingerprint_HMACKeyed(t *testing.T) {
	home := stateHome(t)
	cfgDir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1, Env: map[string]string{"TOKEN_REF": "${SECRET_NAME}"}}
	in := testInput("pix-x")
	in.Selection = EnvSelection{Name: "work", Root: "/envs/work", Document: doc}
	facts := ComposeRuntimeFacts(in)

	lookup := func(string) (string, bool) { return "super-secret-value", true }

	// No key record yet: reset-invalidated, no error, no fingerprint.
	fp, reset, err := CreationFingerprint(facts, AttachHMACResolver(cfgDir, lookup))
	if err != nil || !reset || fp != nil {
		t.Fatalf("missing key: got (%v, %v, %v), want (nil, true, nil)", fp, reset, err)
	}

	if _, err := hosttrust.EnsureCreationHMACKey(cfgDir); err != nil {
		t.Fatalf("EnsureCreationHMACKey: %v", err)
	}
	fp, reset, err = CreationFingerprint(facts, AttachHMACResolver(cfgDir, lookup))
	if err != nil || reset {
		t.Fatalf("with key: (%v, %v)", reset, err)
	}
	for k, v := range fp {
		if strings.Contains(v, "super-secret-value") {
			t.Fatalf("fingerprint facet %s leaks the resolved value: %q", k, v)
		}
	}
	if len(fp) == 0 {
		t.Fatal("fingerprint is empty")
	}
}

// The live-holder answer FAILS CLOSED: an sbx state this host cannot read
// is an error, never "nobody is holding it".
func TestEnvironmentHolders_FailsClosedOnUnknown(t *testing.T) {
	stateHome(t)
	key := "pix-held-1"
	if err := RecordSessionEnvironment(key, SessionEnvironment{Name: "work", Root: "/envs/work", SandboxName: key}); err != nil {
		t.Fatalf("RecordSessionEnvironment: %v", err)
	}
	if _, err := EnvironmentHolders(unreadableSbx(), "/envs/work"); err == nil {
		t.Fatal("an unreadable sbx listing must be an error, not an empty holder set")
	}
	held, err := EnvironmentHolders(runningSbx(key), "/envs/work")
	if err != nil {
		t.Fatalf("EnvironmentHolders: %v", err)
	}
	if len(held) != 1 || held[0] != key {
		t.Fatalf("holders = %v, want [%s]", held, key)
	}
	// A DIFFERENT environment root is not held by it.
	held, err = EnvironmentHolders(runningSbx(key), "/envs/home")
	if err != nil || len(held) != 0 {
		t.Fatalf("holders for an unrelated root = (%v, %v), want ([], nil)", held, err)
	}
}

// unreadableSbx makes every `sbx ls` fail: SbxUnknown for every name.
func unreadableSbx() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil },
		RunFn:      func(string, ...string) (string, error) { return "", os.ErrPermission },
		RunTimedFn: func(string, ...string) (string, bool, error) { return "", false, os.ErrPermission },
		RunWithinFn: func(time.Duration, string, ...string) (string, bool, error) {
			return "", false, os.ErrPermission
		},
	}}
}

// runningSbx reports exactly one RUNNING, schema-verified row for name.
func runningSbx(name string) hostenv.Env {
	// A schema-verified `sbx ls --json` listing: the ONE bounded probe
	// every environment-holder answer is read from.
	row := `[{"name":"` + name + `","state":"running","instance_id":"inst-` + name + `"}]`
	return hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil },
		RunFn:      func(string, ...string) (string, error) { return row, nil },
		RunTimedFn: func(string, ...string) (string, bool, error) { return row, false, nil },
		RunWithinFn: func(time.Duration, string, ...string) (string, bool, error) {
			return row, false, nil
		},
	}}
}
