//go:build unix

package launch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"pix/host/lease"
	"pix/host/sandbox"
)

func sampleIntent() CreateIntent {
	return CreateIntent{
		EnvironmentRoot: "/home/user/work",
		EnvironmentName: "work",
		SandboxName:     "pix-work-abcd1234",
		Fingerprint:     sandbox.Fingerprint{"template": "docker.io/mcavage/pix:1.0.0"},
	}
}

// ── write/read/clear ────────────────────────────────────────────────────────

func TestCreateIntent_WriteReadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	in := sampleIntent()
	if err := WriteCreateIntent(dir, in); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	got, found, err := ReadCreateIntent(dir)
	if err != nil || !found {
		t.Fatalf("ReadCreateIntent: found=%v err=%v", found, err)
	}
	if got.EnvironmentRoot != in.EnvironmentRoot || got.SandboxName != in.SandboxName {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero after write")
	}
	if got.CreatedPID != os.Getpid() {
		t.Errorf("CreatedPID = %d, want %d", got.CreatedPID, os.Getpid())
	}
}

func TestCreateIntent_ReadMissingIsNotFoundNotError(t *testing.T) {
	dir := t.TempDir()
	got, found, err := ReadCreateIntent(dir)
	if err != nil || found || got != nil {
		t.Fatalf("ReadCreateIntent on empty dir = (%v, %v, %v), want (nil, false, nil)", got, found, err)
	}
}

func TestCreateIntent_RejectsMissingEnvironmentRoot(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	in.EnvironmentRoot = ""
	if err := WriteCreateIntent(dir, in); err == nil {
		t.Fatal("WriteCreateIntent with empty environment root = nil error, want refusal")
	}
}

func TestCreateIntent_RejectsRelativeEnvironmentRoot(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	in.EnvironmentRoot = "relative/path"
	if err := WriteCreateIntent(dir, in); err == nil {
		t.Fatal("WriteCreateIntent with relative environment root = nil error, want refusal")
	}
}

func TestCreateIntent_RejectsSandboxNameOutsidePixNamespace(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	in.SandboxName = "not-pix-prefixed"
	if err := WriteCreateIntent(dir, in); err == nil {
		t.Fatal("WriteCreateIntent with a non-pix-* name = nil error, want refusal")
	}
}

func TestCreateIntent_Clear(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCreateIntent(dir, sampleIntent()); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	if err := ClearCreateIntent(dir); err != nil {
		t.Fatalf("ClearCreateIntent: %v", err)
	}
	if _, found, err := ReadCreateIntent(dir); err != nil || found {
		t.Fatalf("ReadCreateIntent after clear: found=%v err=%v", found, err)
	}
	// Clearing an already-cleared (or never-written) intent is not an error.
	if err := ClearCreateIntent(dir); err != nil {
		t.Fatalf("ClearCreateIntent (already absent): %v", err)
	}
}

// ── atomicity / symlink safety / perms / concurrency ────────────────────────

func TestCreateIntent_FilePrivatePerms(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCreateIntent(dir, sampleIntent()); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, CreateIntentFileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("createintent.json perm = %o, want 0600", perm)
	}
	dfi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}
}

func TestCreateIntent_NoTempFileLeftAfterWrite(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCreateIntent(dir, sampleIntent()); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after a successful write: %s", e.Name())
		}
	}
}

func TestCreateIntent_RefusesWritingThroughExistingSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sess")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, CreateIntentFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteCreateIntent(dir, sampleIntent()); err == nil {
		t.Fatal("WriteCreateIntent over an existing symlink = nil error, want refusal")
	}
	// The symlink itself must be untouched, and its target must not have
	// been written through.
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("symlink target was modified: %s", data)
	}
	lfi, err := os.Lstat(link)
	if err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Error("createintent.json symlink was replaced instead of refused")
	}
}

func TestCreateIntent_RefusesReadingThroughSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sess")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "secret.json")
	payload, _ := json.Marshal(sampleIntent())
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, CreateIntentFileName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCreateIntent(dir); err == nil {
		t.Fatal("ReadCreateIntent through a symlink = nil error, want refusal")
	}
}

func TestCreateIntent_RefusesSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteCreateIntent(link, sampleIntent()); err == nil {
		t.Fatal("WriteCreateIntent into a symlinked dir = nil error, want refusal")
	}
}

// TestCreateIntent_ConcurrentWritesLeaveOneCompleteFile races many writers
// against the same directory (an intent is overwrite-safe, unlike lease's
// write-once Record) and proves the file left behind is always exactly ONE
// writer's complete, valid JSON — never a torn interleave of two.
func TestCreateIntent_ConcurrentWritesLeaveOneCompleteFile(t *testing.T) {
	dir := t.TempDir()
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := sampleIntent()
			in.SandboxName = "pix-work-" + strconv.Itoa(i)
			_ = WriteCreateIntent(dir, in)
		}(i)
	}
	wg.Wait()

	got, found, err := ReadCreateIntent(dir)
	if err != nil {
		t.Fatalf("ReadCreateIntent after concurrent writers: %v", err)
	}
	if !found {
		t.Fatal("no intent survived concurrent writers")
	}
	if !strings.HasPrefix(got.SandboxName, "pix-work-") {
		t.Errorf("surviving intent has an unexpected sandbox name: %q", got.SandboxName)
	}
}

// ── no secrets: the field set is fixed and allowlisted ──────────────────────

func TestCreateIntent_JSONKeysAreAllowlisted(t *testing.T) {
	in := sampleIntent()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	allowed := map[string]bool{
		"environment_root": true, "environment_name": true, "sandbox_name": true,
		"fingerprint": true, "created_at": true, "created_pid": true,
	}
	for k := range m {
		if !allowed[k] {
			t.Errorf("create intent JSON has an unexpected key %q — no secret/token field belongs here", k)
		}
		lk := strings.ToLower(k)
		if strings.Contains(lk, "secret") || strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "credential") {
			t.Errorf("create intent JSON key %q looks like it could carry a secret", k)
		}
	}
}

// ── promotion: intent -> instance-bound lease record ────────────────────────

func TestPromoteCreateIntent_BindsRecordAndClearsIntent(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	if err := WriteCreateIntent(dir, in); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	rec, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1", SandboxName: in.SandboxName})
	if err != nil {
		t.Fatalf("PromoteCreateIntent: %v", err)
	}
	if rec.InstanceID != "inst-1" || rec.Name != in.SandboxName {
		t.Errorf("promoted record = %+v", rec)
	}
	if _, found, _ := ReadCreateIntent(dir); found {
		t.Error("intent still on disk after promotion")
	}
	if got := LoadCreateReceipt(dir); got == nil || got.InstanceID != "inst-1" || got.SandboxName != in.SandboxName {
		t.Errorf("LoadCreateReceipt after promotion = %+v", got)
	}
}

func TestPromoteCreateIntent_RequiresInstanceIDAndName(t *testing.T) {
	dir := t.TempDir()
	if _, err := PromoteCreateIntent(dir, CreateReceipt{SandboxName: "pix-x"}); err == nil {
		t.Error("PromoteCreateIntent with no instance id = nil error, want refusal")
	}
	if _, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1"}); err == nil {
		t.Error("PromoteCreateIntent with no sandbox name = nil error, want refusal")
	}
}

// preserve existing holders/keep/unknown-fails-closed: a promoted record is
// still a normal lease.Record for everything the existing lifecycle reads.
func TestPromoteCreateIntent_PreservesKeepAndReferenceMachinery(t *testing.T) {
	dir := t.TempDir()
	if _, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1", SandboxName: "pix-x"}); err != nil {
		t.Fatalf("PromoteCreateIntent: %v", err)
	}
	if err := lease.SetKeep(dir, "user@host"); err != nil {
		t.Fatalf("SetKeep: %v", err)
	}
	state, set, err := lease.ReadKeep(dir)
	if err != nil || !set || state.Identity != "user@host" {
		t.Fatalf("ReadKeep after promotion = (%+v, %v, %v)", state, set, err)
	}
	held, err := lease.ReferencesHeld(dir)
	if err != nil {
		t.Fatalf("ReferencesHeld: %v", err)
	}
	if held {
		t.Error("ReferencesHeld = true with nothing holding a reference")
	}
}

// ── lease.Record extension: additive, compatible, still write-once ─────────

func TestLeaseRecord_CreateRecordForIsCompatibleAndImmutable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sbx-1")
	rec, err := lease.CreateRecordFor(dir, "sbx-1", "pix-x")
	if err != nil {
		t.Fatalf("CreateRecordFor: %v", err)
	}
	if rec.Name != "pix-x" {
		t.Errorf("Name = %q, want pix-x", rec.Name)
	}
	read, err := lease.ReadRecord(dir)
	if err != nil || read.Name != "pix-x" {
		t.Fatalf("ReadRecord after CreateRecordFor = (%+v, %v)", read, err)
	}
	if _, err := lease.CreateRecordFor(dir, "sbx-1", "pix-y"); err == nil {
		t.Error("CreateRecordFor with a different name on the same dir = nil error, want refusal")
	}
	// Idempotent same-name call is a no-op, not an error.
	again, err := lease.CreateRecordFor(dir, "sbx-1", "pix-x")
	if err != nil || again.Name != "pix-x" {
		t.Fatalf("CreateRecordFor idempotent re-call = (%+v, %v)", again, err)
	}
}

func TestLeaseRecord_PlainCreateRecordStillWorksWithNoName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sbx-1")
	rec, err := lease.CreateRecord(dir, "sbx-1")
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if rec.Name != "" {
		t.Errorf("Name = %q, want empty for the plain name-keyed lifecycle", rec.Name)
	}
	read, err := lease.ReadRecord(dir)
	if err != nil || read.Name != "" {
		t.Fatalf("ReadRecord = (%+v, %v)", read, err)
	}
}

func TestLeaseRecord_OldRecordWithoutNameFieldDecodesFine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sbx-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Exactly the JSON shape a pre-E2.3 record.json had: no "name" key at all.
	old := `{"instance_id":"sbx-1","created_at":"2024-01-01T00:00:00Z","created_pid":123}`
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := lease.ReadRecord(dir)
	if err != nil {
		t.Fatalf("ReadRecord on a pre-E2.3 record: %v", err)
	}
	if rec.InstanceID != "sbx-1" || rec.Name != "" {
		t.Errorf("ReadRecord = %+v", rec)
	}
}

// ── recovery state table: DecideEnvRemoval ──────────────────────────────────

func verifiedProbe(name, id string) *sandbox.Entry {
	return &sandbox.Entry{Name: name, State: sandbox.StateRunning, InstanceID: &id, IdentityVerified: true}
}

func TestRecoveryStateTable(t *testing.T) {
	receipt := &CreateReceipt{InstanceID: "inst-1", SandboxName: "pix-work-abcd1234"}
	intent := &CreateIntent{SandboxName: "pix-work-abcd1234", EnvironmentRoot: "/w"}

	cases := []struct {
		name string
		in   EnvRemovalInput
		want EnvRemovalVerdict
	}{
		{
			name: "no receipt at all, no intent either: absence is not authority",
			in:   EnvRemovalInput{},
			want: EnvRemovalNoReceipt,
		},
		{
			name: "pre-create absent probe alone is NOT removal authority",
			in:   EnvRemovalInput{ProbeTrusted: true, Probe: nil},
			want: EnvRemovalNoReceipt,
		},
		{
			name: "intent on disk but never promoted: still no receipt",
			in:   EnvRemovalInput{Intent: intent, ProbeTrusted: true, Probe: nil},
			want: EnvRemovalNoReceipt,
		},
		{
			name: "receipt exists, probe untrusted: fail closed",
			in:   EnvRemovalInput{Receipt: receipt, ProbeTrusted: false},
			want: EnvRemovalUnknownProbe,
		},
		{
			name: "receipt exists, trusted probe found nothing: fail closed",
			in:   EnvRemovalInput{Receipt: receipt, ProbeTrusted: true, Probe: nil},
			want: EnvRemovalUnknownProbe,
		},
		{
			name: "receipt exists, probe row not identity-verified: fail closed",
			in: EnvRemovalInput{Receipt: receipt, ProbeTrusted: true, Probe: &sandbox.Entry{
				Name: "pix-work-abcd1234", State: sandbox.StateRunning, IdentityVerified: false,
			}},
			want: EnvRemovalUnknownProbe,
		},
		{
			name: "receipt + stale instance id => refuse removal",
			in:   EnvRemovalInput{Receipt: receipt, ProbeTrusted: true, Probe: verifiedProbe("pix-work-abcd1234", "inst-2")},
			want: EnvRemovalStale,
		},
		{
			name: "receipt + matching name but reused instance => stale",
			in:   EnvRemovalInput{Receipt: receipt, ProbeTrusted: true, Probe: verifiedProbe("pix-work-abcd1234", "inst-DIFFERENT")},
			want: EnvRemovalStale,
		},
		{
			name: "receipt + probe under a DIFFERENT name => stale, never removal authority for the receipted name",
			in:   EnvRemovalInput{Receipt: receipt, ProbeTrusted: true, Probe: verifiedProbe("pix-other-name", "inst-1")},
			want: EnvRemovalStale,
		},
		{
			name: "receipt + fresh probe agrees on id and name => authorized",
			in:   EnvRemovalInput{Receipt: receipt, ProbeTrusted: true, Probe: verifiedProbe("pix-work-abcd1234", "inst-1")},
			want: EnvRemovalAuthorized,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict, detail := DecideEnvRemoval(c.in)
			if verdict != c.want {
				t.Errorf("DecideEnvRemoval() = %s (%s), want %s", verdict, detail, c.want)
			}
			if detail == "" {
				t.Error("verdict has no detail — a verdict with no reason cannot be trusted")
			}
		})
	}
}

// ── cleanup authority: zero invocations unless authorized, exact argv once ─

func TestRecovery_NoReceiptZeroRmInvocations(t *testing.T) {
	calls := 0
	verdict, _, err := CleanupEnv(EnvRemovalInput{}, func(argv []string) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("CleanupEnv: %v", err)
	}
	if verdict != EnvRemovalNoReceipt {
		t.Errorf("verdict = %s, want %s", verdict, EnvRemovalNoReceipt)
	}
	if calls != 0 {
		t.Errorf("remove was called %d times, want 0", calls)
	}
}

func TestRecovery_StaleAndUnknownAlsoZeroRmInvocations(t *testing.T) {
	receipt := &CreateReceipt{InstanceID: "inst-1", SandboxName: "pix-x"}
	inputs := []EnvRemovalInput{
		{Receipt: receipt, ProbeTrusted: false},
		{Receipt: receipt, ProbeTrusted: true, Probe: verifiedProbe("pix-x", "inst-STALE")},
		{Receipt: receipt, ProbeTrusted: true, Probe: nil},
	}
	for i, in := range inputs {
		calls := 0
		verdict, _, err := CleanupEnv(in, func(argv []string) error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("case %d: CleanupEnv: %v", i, err)
		}
		if verdict == EnvRemovalAuthorized {
			t.Fatalf("case %d: unexpectedly authorized", i)
		}
		if calls != 0 {
			t.Errorf("case %d: remove was called %d times, want 0", i, calls)
		}
	}
}

func TestRecovery_AuthorizedCallsExactArgvExactlyOnce(t *testing.T) {
	receipt := &CreateReceipt{InstanceID: "inst-1", SandboxName: "pix-work-abcd1234"}
	var gotArgv []string
	calls := 0
	verdict, _, err := CleanupEnv(EnvRemovalInput{
		Receipt: receipt, ProbeTrusted: true, Probe: verifiedProbe("pix-work-abcd1234", "inst-1"),
	}, func(argv []string) error {
		calls++
		gotArgv = argv
		return nil
	})
	if err != nil {
		t.Fatalf("CleanupEnv: %v", err)
	}
	if verdict != EnvRemovalAuthorized {
		t.Fatalf("verdict = %s, want authorized", verdict)
	}
	if calls != 1 {
		t.Fatalf("remove was called %d times, want exactly 1", calls)
	}
	want := []string{"env", "rm", "-f", "pix-work-abcd1234"}
	if len(gotArgv) != len(want) {
		t.Fatalf("argv = %v, want %v", gotArgv, want)
	}
	for i := range want {
		if gotArgv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", gotArgv, want)
		}
	}
}

// ── diagnosis: next run / orphan sweep can read intent and diagnose partial state ─

func TestDiagnoseResidue_IntentWithNoReceiptIsResidue(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	if err := WriteCreateIntent(dir, in); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	report, residue, err := DiagnoseResidue(dir)
	if err != nil {
		t.Fatalf("DiagnoseResidue: %v", err)
	}
	if !residue {
		t.Fatal("residue = false, want true for an unconfirmed intent")
	}
	if !strings.Contains(report, in.SandboxName) {
		t.Errorf("report %q does not name the sandbox %q", report, in.SandboxName)
	}
}

func TestDiagnoseResidue_NoIntentIsNotResidue(t *testing.T) {
	dir := t.TempDir()
	_, residue, err := DiagnoseResidue(dir)
	if err != nil || residue {
		t.Fatalf("DiagnoseResidue on an empty dir = (residue=%v, err=%v), want (false, nil)", residue, err)
	}
}

func TestDiagnoseResidue_PromotedIntentIsNotResidue(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	if err := WriteCreateIntent(dir, in); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	if _, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1", SandboxName: in.SandboxName}); err != nil {
		t.Fatalf("PromoteCreateIntent: %v", err)
	}
	_, residue, err := DiagnoseResidue(dir)
	if err != nil || residue {
		t.Fatalf("DiagnoseResidue after promotion = (residue=%v, err=%v), want (false, nil)", residue, err)
	}
}

// TestDiagnoseResidue_SurvivingIntentAfterCrashedPromotionIsNotResidue models
// the crash window INSIDE PromoteCreateIntent, between the record write and
// the intent clear: the record (stronger proof) exists, so this is a
// confirmed create, not residue, even though the intent file is still there.
func TestDiagnoseResidue_SurvivingIntentAfterCrashedPromotionIsNotResidue(t *testing.T) {
	dir := t.TempDir()
	in := sampleIntent()
	if err := WriteCreateIntent(dir, in); err != nil {
		t.Fatalf("WriteCreateIntent: %v", err)
	}
	if _, err := lease.CreateRecordFor(dir, "inst-1", in.SandboxName); err != nil {
		t.Fatalf("CreateRecordFor: %v", err)
	}
	// Simulate the crash: the intent file was never cleared.
	if _, found, _ := ReadCreateIntent(dir); !found {
		t.Fatal("test setup: intent should still be on disk")
	}
	_, residue, err := DiagnoseResidue(dir)
	if err != nil || residue {
		t.Fatalf("DiagnoseResidue with a confirmed record but a surviving intent = (residue=%v, err=%v), want (false, nil)", residue, err)
	}
}

// ── crash windows, end to end through the whole state machine ──────────────

func TestRecovery_CrashWindows(t *testing.T) {
	t.Run("window A: nothing ever written", func(t *testing.T) {
		dir := t.TempDir()
		if _, residue, _ := DiagnoseResidue(dir); residue {
			t.Error("empty dir reported as residue")
		}
		verdict, _ := DecideEnvRemoval(EnvRemovalInput{Receipt: LoadCreateReceipt(dir)})
		if verdict != EnvRemovalNoReceipt {
			t.Errorf("verdict = %s, want no-receipt", verdict)
		}
	})

	t.Run("window B: intent written, process died before any receipt", func(t *testing.T) {
		dir := t.TempDir()
		in := sampleIntent()
		if err := WriteCreateIntent(dir, in); err != nil {
			t.Fatalf("WriteCreateIntent: %v", err)
		}
		_, residue, _ := DiagnoseResidue(dir)
		if !residue {
			t.Error("window B should be diagnosed as possible residue")
		}
		verdict, _ := DecideEnvRemoval(EnvRemovalInput{Receipt: LoadCreateReceipt(dir), Intent: &in, ProbeTrusted: true, Probe: verifiedProbe(in.SandboxName, "inst-1")})
		if verdict != EnvRemovalNoReceipt {
			t.Errorf("window B: verdict = %s, want no-receipt (even with a matching live probe)", verdict)
		}
	})

	t.Run("window C: receipt confirmed, fresh probe still matches", func(t *testing.T) {
		dir := t.TempDir()
		in := sampleIntent()
		if err := WriteCreateIntent(dir, in); err != nil {
			t.Fatalf("WriteCreateIntent: %v", err)
		}
		if _, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1", SandboxName: in.SandboxName}); err != nil {
			t.Fatalf("PromoteCreateIntent: %v", err)
		}
		verdict, _ := DecideEnvRemoval(EnvRemovalInput{
			Receipt: LoadCreateReceipt(dir), ProbeTrusted: true, Probe: verifiedProbe(in.SandboxName, "inst-1"),
		})
		if verdict != EnvRemovalAuthorized {
			t.Errorf("window C: verdict = %s, want authorized", verdict)
		}
	})

	t.Run("window D: receipt confirmed, name later reused by a different instance", func(t *testing.T) {
		dir := t.TempDir()
		in := sampleIntent()
		if _, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1", SandboxName: in.SandboxName}); err != nil {
			t.Fatalf("PromoteCreateIntent: %v", err)
		}
		verdict, _ := DecideEnvRemoval(EnvRemovalInput{
			Receipt: LoadCreateReceipt(dir), ProbeTrusted: true, Probe: verifiedProbe(in.SandboxName, "inst-REUSED"),
		})
		if verdict != EnvRemovalStale {
			t.Errorf("window D: verdict = %s, want stale", verdict)
		}
	})

	t.Run("window E: receipt confirmed, probe cannot be trusted", func(t *testing.T) {
		dir := t.TempDir()
		in := sampleIntent()
		if _, err := PromoteCreateIntent(dir, CreateReceipt{InstanceID: "inst-1", SandboxName: in.SandboxName}); err != nil {
			t.Fatalf("PromoteCreateIntent: %v", err)
		}
		verdict, _ := DecideEnvRemoval(EnvRemovalInput{Receipt: LoadCreateReceipt(dir), ProbeTrusted: false})
		if verdict != EnvRemovalUnknownProbe {
			t.Errorf("window E: verdict = %s, want unknown-probe", verdict)
		}
	})
}

// ensure a symlink refusal error is a real error type, not merely a
// not-exist-shaped one, so callers cannot conflate the two.
func TestCreateIntent_SymlinkRefusalIsNotIsNotExist(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sess")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "nope"), filepath.Join(dir, CreateIntentFileName)); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadCreateIntent(dir)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if os.IsNotExist(err) {
		t.Errorf("symlink refusal must not read as IsNotExist: %v", err)
	}
	var pe *os.PathError
	_ = errors.As(err, &pe) // best-effort; the important assertion is above
}
