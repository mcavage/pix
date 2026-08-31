//go:build unix

// envcutover_test.go — the E2.5 escalation's proofs: the properties the
// cutover claims and that nothing else in this package asserts. Each test
// names the claim it pins, because a cutover that quietly kept a second
// create path, or that fingerprinted a create differently from the attach
// that must match it, passes every OTHER test in this package.
package launch

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/sandbox"
	"pix/host/sys/systest"
)

// ── BLOCK 1: the primary workspace is the RUN's project dir ──────────────

// A launch never picks its project workspace by empty-string accident: the
// ONE producer refuses an empty path outright instead of substituting an
// environment root (which §5.1 restriction 4 requires to live OUTSIDE every
// writable workspace it mounts).
func TestPrimaryWorkspaceFact_RefusesEmptyNeverSubstitutes(t *testing.T) {
	if _, err := PrimaryWorkspaceFact(""); err == nil {
		t.Fatal("an empty workspace must be refused, not silently composed")
	}
	if _, err := PrimaryWorkspaceFact("   "); err == nil {
		t.Fatal("a blank workspace must be refused")
	}
	ws := t.TempDir()
	got, err := PrimaryWorkspaceFact(ws)
	if err != nil || got.Path != ws {
		t.Fatalf("PrimaryWorkspaceFact(%q) = (%+v, %v)", ws, got, err)
	}
	if got.Clone || got.ReadOnly {
		t.Fatalf("the project workspace is a read-write bind mount, got %+v", got)
	}
}

// Rendering refuses outright when no primary workspace was composed: a
// sandbox with no project mount is never what the user asked for.
func TestRenderEffectiveEnvironment_RefusesWithoutAPrimaryWorkspace(t *testing.T) {
	stateHome(t)
	in := testInput("pix-nows")
	in.PrimaryWorkspace = envinfo.WorkspaceFact{}
	if _, err := RenderEffectiveEnvironment(in, nil); err == nil {
		t.Fatal("rendering with no primary workspace must fail closed")
	}
}

// The environment ROOT is not a workspace. An environment whose root is a
// real directory still renders exactly the run's own project workspace, the
// personal context, and the caller's additional mounts — never the root.
func TestEffectiveDocument_EnvironmentRootIsNotAWorkspace(t *testing.T) {
	stateHome(t)
	root := t.TempDir()
	ws := t.TempDir()
	extra := t.TempDir()
	in := testInput("pix-rootsep")
	in.Selection = EnvSelection{Name: "work", Root: root, Document: &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1}}
	in.PrimaryWorkspace = envinfo.WorkspaceFact{Path: ws}
	in.AdditionalWorkspaces = WorkspaceFacts([]string{extra})

	eff, err := RenderEffectiveEnvironment(in, nil)
	if err != nil {
		t.Fatalf("RenderEffectiveEnvironment: %v", err)
	}
	doc := string(eff.Bytes)
	if !strings.Contains(doc, "path: "+ws) {
		t.Errorf("effective document does not mount the run's workspace %q:\n%s", ws, doc)
	}
	if !strings.Contains(doc, "path: "+extra) {
		t.Errorf("effective document dropped the additional workspace %q:\n%s", extra, doc)
	}
	if strings.Contains(doc, "path: "+root) {
		t.Errorf("effective document mounts the ENVIRONMENT ROOT %q as a workspace:\n%s", root, doc)
	}
}

// ── BLOCK 2: the creation HMAC key ───────────────────────────────────────

// A create ESTABLISHES the launcher key exactly once, at 0600, before the
// first fingerprint; an attach LOADS that same key and matches. Only a key
// that is gone AFTERWARDS (the post-`pix reset` state) attributes drift, and
// it attributes exactly ONE record. No raw resolved value is ever in a
// fingerprint facet.
func TestCreationHMACKey_EnsuredOnceOnCreateLoadedOnAttach(t *testing.T) {
	home := stateHome(t)
	cfgDir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1, Env: map[string]string{"TOKEN_REF": "${SECRET_NAME}"}}
	in := testInput("pix-key")
	in.Selection = EnvSelection{Name: "work", Root: "/envs/work", Document: doc}
	lookup := func(string) (string, bool) { return "super-secret-value", true }

	// CREATE: the key does not exist yet and is generated here, once.
	resolver, err := CreateHMACResolver(cfgDir, lookup)
	if err != nil {
		t.Fatalf("CreateHMACResolver: %v", err)
	}
	created, reset, err := CreationFingerprint(CreationFactsFor(in), resolver)
	if err != nil || reset || len(created) == 0 {
		t.Fatalf("first interpolated create = (%v, reset=%v, %v); a fresh host's create is NOT reset-invalidated", created, reset, err)
	}
	keyPath := filepath.Join(cfgDir, "creation-hmac.key")
	fi, serr := os.Stat(keyPath)
	if serr != nil {
		t.Fatalf("the creation key record was not established by the create: %v", serr)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("creation key mode = %v, want 0600 (private)", fi.Mode().Perm())
	}
	keyBytes, rerr := os.ReadFile(keyPath)
	if rerr != nil {
		t.Fatal(rerr)
	}

	// ATTACH: loads the SAME key (never generates a second one) and matches.
	current, reset, err := CreationFingerprint(CreationFactsFor(in), AttachHMACResolver(cfgDir, lookup))
	if err != nil || reset {
		t.Fatalf("attach recompute = (reset=%v, %v), want a clean keyed recompute", reset, err)
	}
	if after, _ := os.ReadFile(keyPath); string(after) != string(keyBytes) {
		t.Fatal("the attach path rotated the creation key; it must only ever load it")
	}
	if drifts := envinfo.Attribute(nil, envinfo.Fingerprint(created), envinfo.Fingerprint(current)); len(drifts) > 0 {
		t.Fatalf("create-then-attach must not drift, got %v", drifts)
	}
	for k, v := range current {
		if strings.Contains(v, "super-secret-value") {
			t.Fatalf("facet %s leaks the resolved value: %q", k, v)
		}
	}

	// RESET: the key is gone for a record written BEFORE it went away. That
	// is exactly ONE drift record, never one per interpolated facet.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	_, reset, err = CreationFingerprint(CreationFactsFor(in), AttachHMACResolver(cfgDir, lookup))
	if err != nil || !reset {
		t.Fatalf("a missing key on ATTACH = (reset=%v, %v), want reset-invalidated", reset, err)
	}
	decision := DecideEnvAttach(AttachGate{
		Entry:              runningEntry("pix-key", "inst-1"),
		RecordedInstanceID: "inst-1",
		Stored:             created,
		StoredFound:        true,
		ResetInvalidated:   true,
		Reviewed:           true,
	}, "pix-key", "work")
	if decision.Attach || len(decision.Drifts) != 1 {
		t.Fatalf("reset attribution = %d drifts (attach=%v), want exactly 1", len(decision.Drifts), decision.Attach)
	}
}

// A create that cannot establish a key FAILS, rather than silently
// fingerprinting an environment it could not key.
func TestCreateHMACResolver_NoConfigDirIsAHardFailure(t *testing.T) {
	if _, err := CreateHMACResolver("", nil); err == nil {
		t.Fatal("a create with no config dir must refuse, never fingerprint unkeyed")
	}
}

// ── BLOCK 3: no legacy `sbx run` create path survives ────────────────────

// The cutover's argv sentinel: a real create spawns `sbx env create` and
// then `sbx exec`, and NOTHING on the wire is an `sbx run`.
func TestRunSession_CreateSpawnsEnvCreateThenExec_NeverSbxRun(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	key := SessionName(t.TempDir())
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(fixture, "release"), nil, 0o600)
	}()
	err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", Creating: true,
		EnvCreateArgs: []string{"env", "create", "/state/environments/pix-demo/effective.sbxenv.yaml"},
		Invocation:    []string{"--model", "m"},
	}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	var sawCreate, sawExec bool
	for _, l := range argvLines(t, fixture) {
		switch {
		case l == "env create /state/environments/pix-demo/effective.sbxenv.yaml":
			sawCreate = true
		case strings.HasPrefix(l, "exec "):
			sawExec = true
		case strings.HasPrefix(l, "run"):
			t.Errorf("the cutover spawned a legacy `sbx run`: %q", l)
		}
	}
	if !sawCreate || !sawExec {
		t.Fatalf("create=%v exec=%v; the create must be `sbx env create` followed by `sbx exec`", sawCreate, sawExec)
	}
}

// A create with no effective environment REFUSES; it can never fall back to
// an `sbx run`-shaped create, because there is no such field to fall back to.
func TestRunSession_CreateWithoutEffectiveEnvironmentRefuses(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, sessionFixture)
	spawned := 0
	err := RunSession(SessionSpec{Key: SessionName(t.TempDir()), Name: "pix-demo", Creating: true},
		SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: func(argv []string) *exec.Cmd {
			spawned++
			return nil
		}})
	if err == nil {
		t.Fatal("a create with no effective environment must refuse")
	}
	if spawned != 0 {
		t.Fatalf("%d children spawned; a refused create spawns nothing", spawned)
	}
}

// A lease/state-dir failure is a HARD refusal on the create path: before the
// cutover it fell back to spawning the old create argv with no lease at all.
func TestRunSession_LeaseDirFailure_RefusesAndSpawnsNothing(t *testing.T) {
	isolateState(t)
	// A regular FILE where the state dir must be: leaseDirFor cannot make
	// its directory tree under it.
	blocker := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_HOME", blocker)
	spawned := 0
	err := RunSession(SessionSpec{
		Key: "pix-demo", Name: "pix-demo", Creating: true,
		EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
	}, SessionDeps{Env: realEnv(), Warn: io.Discard, Spawn: func(argv []string) *exec.Cmd {
		spawned++
		return nil
	}})
	if err == nil {
		t.Fatal("a lease-dir failure must refuse the launch, never fall back to a lease-less create")
	}
	if spawned != 0 {
		t.Fatalf("%d children spawned after a lease failure; want 0", spawned)
	}
}

// ── BLOCK 6: receipts, promotion, retention ──────────────────────────────

// A create promotes its bounded intent into the instance-bound lease record
// (E2.3), naming BOTH the instance id and the sandbox name, and clears the
// intent — the receipt, not the intent, is what later authorizes anything.
func TestRunSession_CreatePromotesIntentToAReceipt(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCreateIntent(dir, CreateIntent{
		EnvironmentRoot: "/envs/work", EnvironmentName: "work", SandboxName: "pix-demo",
		Fingerprint: sandbox.Fingerprint{"env.A": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(fixture, "release"), nil, 0o600)
	}()
	if err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", Creating: true,
		EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
		Invocation:    []string{"--model", "m"},
	}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)}); err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	rec, rerr := lease.ReadRecord(dir)
	if rerr != nil || rec.InstanceID != "inst-1" || rec.Name != "pix-demo" {
		t.Fatalf("receipt = %+v (%v), want instance inst-1 name pix-demo", rec, rerr)
	}
	if _, found, _ := ReadCreateIntent(dir); found {
		t.Error("the create intent must be cleared once a receipt exists")
	}
	if report, residue, _ := DiagnoseResidue(dir); residue {
		t.Errorf("a promoted create is not residue: %q", report)
	}
}

// A create whose sandbox never becomes positively identifiable refuses AND
// removes nothing: no receipt means zero `sbx rm`/`sbx env rm` invocations.
func TestRunSession_NoReceipt_RefusesWithZeroRemovals(t *testing.T) {
	isolateState(t)
	// `env create` succeeds but the sandbox never appears in `ls`.
	fixture := installFakeSbx(t, `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls) if [ "$2" = "--json" ]; then echo '[]'; fi; exit 0 ;;
esac
exit 0
`)
	key := SessionName(t.TempDir())
	err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", Creating: true,
		EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
	}, SessionDeps{Env: realEnv(), Poll: CreatePoll{
		Probe:    func(n string) SbxState { return ProbeTaskSandbox(realEnv(), n) },
		Interval: 2 * time.Millisecond, Timeout: 30 * time.Millisecond,
		// The post-exit settle is the real budget here (`sbx env create`
		// exits immediately), so it is shrunk too: this test asserts the
		// refusal and the zero removals, not the production wait.
		PostExitSettle: 50 * time.Millisecond,
	}, Warn: io.Discard, Spawn: fixtureSpawn(t)})
	if err == nil {
		t.Fatal("a create with no positive receipt must refuse")
	}
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "rm") || strings.HasPrefix(l, "env rm") {
			t.Errorf("a create with no receipt removed something: %q", l)
		}
	}
}

// §10.3: the effective file clears ONLY after a positive absent probe. An
// unreadable listing, or one that still shows the sandbox, RETAINS it.
func TestReleaseEffectiveEnv_RetainsUntilPositiveAbsentProbe(t *testing.T) {
	stateHome(t)
	name := "pix-retain"
	path, err := PersistEffectiveEnv(name, []byte("schemaVersion: \"1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := ReleaseEffectiveEnv(unreadableSbx(), name); rerr == nil {
		t.Error("an unreadable listing must be reported, not treated as absence")
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("the effective file was deleted on an unreadable probe: %v", serr)
	}
	if released, rerr := ReleaseEffectiveEnv(runningSbx(name), name); released || rerr != nil {
		t.Fatalf("a still-present sandbox must retain its effective file (released=%v, %v)", released, rerr)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("the effective file was deleted while the sandbox was still listed: %v", serr)
	}
	released, rerr := ReleaseEffectiveEnv(absentSbx(), name)
	if !released || rerr != nil {
		t.Fatalf("a positive absent probe must release (released=%v, %v)", released, rerr)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("the effective file survived a positive absent probe: %v", serr)
	}
}

// ── BLOCK 7: bounded, schema-shaped holder probing ───────────────────────

// The live-holder answer is read from ONE bounded `sbx ls --json`, never an
// unbounded or raw `sbx ls`, and an unreadable listing fails closed.
func TestEnvironmentHolders_UsesOneBoundedJSONListing(t *testing.T) {
	stateHome(t)
	for _, name := range []string{"pix-h1", "pix-h2"} {
		if err := RecordSessionEnvironment(name, SessionEnvironment{Name: "work", Root: "/envs/work", SandboxName: name}); err != nil {
			t.Fatal(err)
		}
	}
	var calls [][]string
	var bounded int
	env := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil },
		RunFn: func(name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			return "", nil
		},
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			calls = append(calls, append([]string{name}, args...))
			return "", false, nil
		},
		RunWithinFn: func(d time.Duration, name string, args ...string) (string, bool, error) {
			bounded++
			calls = append(calls, append([]string{name}, args...))
			return `[{"name":"pix-h1","state":"running","instance_id":"i1"},{"name":"pix-h2","state":"stopped","instance_id":"i2"}]`, false, nil
		},
	}}
	held, err := EnvironmentHolders(env, "/envs/work")
	if err != nil {
		t.Fatalf("EnvironmentHolders: %v", err)
	}
	if len(held) != 1 || held[0] != "pix-h1" {
		t.Fatalf("holders = %v, want [pix-h1] (a stopped sandbox holds nothing)", held)
	}
	if bounded != 1 || len(calls) != 1 {
		t.Fatalf("sbx calls = %v (bounded=%d); want exactly one bounded listing", calls, bounded)
	}
	if strings.Join(calls[0], " ") != "sbx ls --json" {
		t.Fatalf("holder probe ran %q; want the schema-shaped `sbx ls --json`", strings.Join(calls[0], " "))
	}
}

// ── shared fixtures ──────────────────────────────────────────────────────

func runningEntry(name, instance string) *sandbox.Entry {
	id := instance
	return &sandbox.Entry{Name: name, State: sandbox.StateRunning, IdentityVerified: true, InstanceID: &id}
}

func absentSbx() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil },
		RunFn:      func(string, ...string) (string, error) { return "[]", nil },
		RunTimedFn: func(string, ...string) (string, bool, error) { return "[]", false, nil },
		RunWithinFn: func(time.Duration, string, ...string) (string, bool, error) {
			return "[]", false, nil
		},
	}}
}
