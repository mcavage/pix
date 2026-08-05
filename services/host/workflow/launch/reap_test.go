//go:build unix

package launch

// U04d teardown tests. Every one of them runs a REAL `sbx` fixture script
// (installFakeSbx: an executable on PATH, exec'd as a subprocess) and REAL
// flock state on a REAL filesystem — no System mock, no injected prober — for
// the same reason the U04c2 process tests do: the properties under test are
// "what argv actually reached sbx", "what the kernel says about a lock right
// now", and "what is left on disk afterwards". A mock can only restate the
// implementation's own beliefs about those.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// fastTeardown is the production teardown shape on a test budget: the same
// steps and the same clamping, with bounds small enough that a hung fixture
// fails the test instead of sleeping through 20 s of budget.
func fastTeardown(t *testing.T) TeardownOptions {
	t.Helper()
	return TeardownOptions{
		RmTimeout:    2 * time.Second,
		ProbeTimeout: 2 * time.Second,
		Budget:       8 * time.Second,
		ProbeDelay:   time.Millisecond,
		JournalPath:  filepath.Join(t.TempDir(), "teardown.jsonl"),
	}
}

// removableFixture is an sbx that lists one running, schema-verified sandbox
// until a plain (NON-FORCE) `rm` removes it. A forced rm is a deliberate
// FAILURE here: if any teardown path ever passes -f, these tests break rather
// than silently accepting it.
const removableFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	[ -f "$d/removed" ] && exit 0
	if [ "$2" = "--json" ]; then
		echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
	else
		echo "pix-demo  img  running"
	fi
	exit 0
	;;
rm)
	if [ "$2" = "-f" ]; then
		echo "fixture: refusing a forced removal" >&2
		exit 3
	fi
	touch "$d/removed"
	exit 0
	;;
esac
exit 0
`

// seedRecordedSession writes exactly what a successful create records: the
// immutable instance id, the fingerprint and the pi invocation.
func seedRecordedSession(t *testing.T, key, instanceID string) string {
	t.Helper()
	dir, err := LeaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.CreateRecord(dir, instanceID); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionFingerprint(key, sandbox.Fingerprint{"static_mcp": "slack"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionInvocation(key, []string{"--model", "m"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sbxArgv is argvLines that tolerates a fixture never being called at all —
// which is itself the assertion in every "keeps" case: no probe, no rm, no
// argv.log.
func sbxArgv(t *testing.T, dir string) []string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "argv.log")); os.IsNotExist(err) {
		return nil
	}
	return argvLines(t, dir)
}

// assertLeaseStateCleared checks the FILES a teardown must clear, not the
// directory: the per-sandbox MCP receipt (mcp.json) lives in this SAME
// directory (workspace.MCPStateRoot is also <state>/sandboxes), and U04d
// deliberately does not delete receipts — so a real session's lease dir
// correctly SURVIVES, holding only the receipt, while every identity/ownership
// file inside it is gone.
// assertLeaseStateCleared checks that a PROVEN teardown left nothing behind:
// every file this domain writes, and — since U04e — the lease DIRECTORY itself.
//
// The directory assertion is the load-bearing half, and it is new. lease's
// ClearState removes only the files it owns and then removes the dir "unless
// something this package does not own is still in it"; the launcher's
// per-sandbox MCP receipt (mcp.json + mcp.json.lock) lived in the very same
// <state>/sandboxes/<name> directory, so ENOTEMPTY was the NORMAL outcome and
// every reaped session leaked its directory forever. Deleting the receipt store
// is what makes the sweep complete, so this is the test that would catch a
// second store being planted there again.
func assertLeaseStateCleared(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"record.json", "keep.json", "fingerprint.json", "invocation.json", "refs.lock", "lifecycle.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the teardown (err %v)", name, err)
		}
	}
	if entries, err := os.ReadDir(dir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the lease dir %s survived the teardown holding %v; a proven teardown must clear it completely", dir, names)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat lease dir %s: %v", dir, err)
	}
}

func journalOf(t *testing.T, opts TeardownOptions) []TeardownJournalEntry {
	t.Helper()
	entries, err := ReadTeardownJournal(opts.JournalPath)
	if err != nil {
		t.Fatalf("ReadTeardownJournal: %v", err)
	}
	return entries
}

func lastVerdict(t *testing.T, opts TeardownOptions) TeardownJournalEntry {
	t.Helper()
	entries := journalOf(t, opts)
	if len(entries) == 0 {
		t.Fatal("the journal is empty: every teardown attempt must be journalled")
	}
	return entries[len(entries)-1]
}

// TestTeardown_RemovesWithoutForceAndClearsState is the happy path end to end:
// a fully recorded session whose instance still matches is removed with a
// PLAIN `sbx rm`, the absence is confirmed, and every piece of state a future
// create/attach/reaper would read is gone.
func TestTeardown_RemovesWithoutForceAndClearsState(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	opts := fastTeardown(t)

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownRemoved {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownRemoved)
	}
	argv := argvLines(t, fixture)
	var sawRm bool
	for _, l := range argv {
		if strings.HasPrefix(l, "rm") {
			sawRm = true
			if l != "rm pix-demo" {
				t.Errorf("removal argv = %q, want exactly %q (never -f)", l, "rm pix-demo")
			}
		}
	}
	if !sawRm {
		t.Fatalf("no rm reached sbx; argv was %v", argv)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		left, _ := os.ReadDir(dir)
		t.Errorf("lease state survived a confirmed removal (%v): %v", err, left)
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownRemoved || entry.Sandbox != "pix-demo" {
		t.Errorf("journal entry = %+v, want a removed verdict for pix-demo", entry)
	}
}

// TestTeardown_KeepIsHeld: an identity-bound keep blocks the AUTOMATIC reaper
// outright — nothing is removed, nothing is cleared, and the refusal is
// journalled with the identity that holds it.
func TestTeardown_KeepIsHeld(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	if err := lease.SetKeep(dir, "someone@else"); err != nil {
		t.Fatal(err)
	}
	opts := fastTeardown(t)

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownKeptKeep {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptKeep)
	}
	if !strings.Contains(res.Detail, "refusing to reap: keep is held") {
		t.Errorf("detail = %q, want it to name the keep refusal", res.Detail)
	}
	for _, l := range sbxArgv(t, fixture) {
		if strings.HasPrefix(l, "rm") {
			t.Fatalf("a held keep still reached `sbx rm`: %q", l)
		}
	}
	if _, err := lease.ReadRecord(dir); err != nil {
		t.Errorf("state must survive a keep refusal: %v", err)
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownKeptKeep {
		t.Errorf("journal verdict = %q, want %q", entry.Verdict, TeardownKeptKeep)
	}
}

// TestTeardown_ExplicitRmIgnoresAKeptZeroLeaseBox: a keep stops the automatic
// reaper, not the operator. An explicitly named `pix rm` on a KEPT box with
// zero references removes it — without force.
func TestTeardown_ExplicitRmIgnoresAKeptZeroLeaseBox(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	if err := lease.SetKeep(dir, "me@host"); err != nil {
		t.Fatal(err)
	}
	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerExplicit, fastTeardown(t))
	if res.Verdict != TeardownRemoved {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownRemoved)
	}
	for _, l := range argvLines(t, fixture) {
		if strings.Contains(l, "-f") {
			t.Fatalf("an explicit non-force rm used force: %q", l)
		}
	}
}

// TestTeardown_InstanceMismatchKeeps: the name was reused for a DIFFERENT
// instance. Removing it would destroy somebody else's sandbox.
func TestTeardown_InstanceMismatchKeeps(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-OLD")
	opts := fastTeardown(t)

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownKeptMismatch {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptMismatch)
	}
	for _, l := range sbxArgv(t, fixture) {
		if strings.HasPrefix(l, "rm") {
			t.Fatalf("a mismatched instance still reached `sbx rm`: %q", l)
		}
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownKeptMismatch {
		t.Errorf("journal verdict = %q, want %q", entry.Verdict, TeardownKeptMismatch)
	}
}

// TestTeardown_UntrustedProbeKeeps: an sbx that cannot answer is UNKNOWN, never
// absent, and unknown keeps. Two shapes, one posture: a failing sbx, and a row
// whose schema this parser could not vouch for.
func TestTeardown_UntrustedProbeKeeps(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"sbx-fails", "echo \"$@\" >> \"$(dirname \"$0\")/argv.log\"\nexit 7\n"},
		{"unverified-row", "d=\"$(dirname \"$0\")\"\necho \"$@\" >> \"$d/argv.log\"\n[ \"$1\" = \"ls\" ] && { echo '{\"sandboxes\":[{\"Sandbox\":\"pix-demo\",\"Status\":\"running\",\"ID\":\"inst-1\"}]}'; exit 0; }\nexit 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			fixture := installFakeSbx(t, tc.script)
			key := "pix-demo"
			dir := seedRecordedSession(t, key, "inst-1")
			res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, fastTeardown(t))
			if res.Verdict != TeardownKeptUnknown {
				t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnknown)
			}
			for _, l := range sbxArgv(t, fixture) {
				if strings.HasPrefix(l, "rm") {
					t.Fatalf("an untrusted probe still reached `sbx rm`: %q", l)
				}
			}
			if _, err := lease.ReadRecord(dir); err != nil {
				t.Errorf("state must survive an unknown verdict: %v", err)
			}
		})
	}
}

// TestTeardown_UnownedKeeps: no creation record at all, or a record with no
// fingerprint/invocation beside it, is not ownership — the automatic reaper
// leaves it alone and never runs a removal.
func TestTeardown_UnownedKeeps(t *testing.T) {
	t.Run("no-record", func(t *testing.T) {
		isolateState(t)
		fixture := installFakeSbx(t, removableFixture)
		if _, err := LeaseDirFor("pix-demo"); err != nil {
			t.Fatal(err)
		}
		res := TeardownSandbox(realEnv(), "pix-demo", "pix-demo", TriggerSession, fastTeardown(t))
		if res.Verdict != TeardownKeptUnowned {
			t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnowned)
		}
		for _, l := range sbxArgv(t, fixture) {
			if strings.HasPrefix(l, "rm") {
				t.Fatalf("an unowned sandbox still reached `sbx rm`: %q", l)
			}
		}
	})
	t.Run("record-without-fingerprint", func(t *testing.T) {
		isolateState(t)
		installFakeSbx(t, removableFixture)
		dir, err := LeaseDirFor("pix-demo")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lease.CreateRecord(dir, "inst-1"); err != nil {
			t.Fatal(err)
		}
		res := TeardownSandbox(realEnv(), "pix-demo", "pix-demo", TriggerSession, fastTeardown(t))
		if res.Verdict != TeardownKeptUnowned {
			t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnowned)
		}
	})
}

// TestTeardown_NonPixNameRefused: the pix-* scope is enforced by the PLANNER
// (sandbox.PlanRemove), before any probe or lock, so a name this tool does not
// own can never reach a removal.
func TestTeardown_NonPixNameRefused(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	res := TeardownSandbox(realEnv(), "pix-demo", "someones-prod-box", TriggerExplicit, fastTeardown(t))
	if res.Verdict != TeardownKeptUnowned {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnowned)
	}
	if _, err := os.Stat(filepath.Join(fixture, "argv.log")); err == nil {
		t.Fatal("an out-of-scope name reached sbx at all")
	}
}

// TestTeardown_AlreadyAbsentClearsStaleState: the recorded sandbox is
// positively gone. Nothing is removed, but the stale record MUST go — a
// surviving record is what makes the next create for this key fail as a
// relabel.
func TestTeardown_AlreadyAbsentClearsStaleState(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, "d=\"$(dirname \"$0\")\"\necho \"$@\" >> \"$d/argv.log\"\n[ \"$1\" = \"ls\" ] && { [ \"$2\" = \"--json\" ] && echo '[]'; exit 0; }\nexit 0\n")
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	opts := fastTeardown(t)

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownAlreadyAbsent {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownAlreadyAbsent)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stale lease state survived: %v", err)
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownAlreadyAbsent {
		t.Errorf("journal verdict = %q, want %q", entry.Verdict, TeardownAlreadyAbsent)
	}
}

// TestTeardown_UnconfirmedRemovalRetainsState: `sbx rm` claimed success but the
// box is still listed. The outcome is UNKNOWN in the only way that matters —
// so the verdict is failed and the state is KEPT as evidence.
func TestTeardown_UnconfirmedRemovalRetainsState(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	if [ "$2" = "--json" ]; then
		echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
	else
		echo "pix-demo  img  running"
	fi
	exit 0
	;;
rm) exit 0 ;;
esac
exit 0
`)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	opts := fastTeardown(t)
	opts.ProbeRetries = 2

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownFailed {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownFailed)
	}
	if !strings.Contains(res.Detail, "3 probes") {
		t.Errorf("detail = %q, want it to report the 1+2 absent probes it actually made", res.Detail)
	}
	if _, err := lease.ReadRecord(dir); err != nil {
		t.Errorf("an unconfirmed removal must RETAIN its evidence: %v", err)
	}
}

// TestTeardownOptions_BoundsAreClampedToTheCeiling: the declared per-step
// bounds sum to more than the total budget, so the composition is what must be
// bounded. A caller cannot widen any of them past the shipped ceiling.
func TestTeardownOptions_BoundsAreClampedToTheCeiling(t *testing.T) {
	o := TeardownOptions{RmTimeout: time.Hour, ProbeTimeout: time.Hour, Budget: time.Hour}.withDefaults()
	if o.RmTimeout != TeardownRmTimeout || o.ProbeTimeout != TeardownProbeTimeout || o.Budget != TeardownBudget {
		t.Fatalf("withDefaults did not clamp: %+v", o)
	}
	if TeardownRmTimeout > 15*time.Second || TeardownProbeTimeout > 3*time.Second || TeardownProbeRetries != 2 || TeardownBudget > 20*time.Second {
		t.Fatalf("the shipped bounds exceed the contract: rm=%s probe=%s retries=%d budget=%s",
			TeardownRmTimeout, TeardownProbeTimeout, TeardownProbeRetries, TeardownBudget)
	}
	// A budget already spent yields NO step time at all — the step is skipped,
	// never run unbounded.
	spent := TeardownOptions{Now: func() time.Time { return time.Unix(1000, 0) }}.withDefaults()
	if got := budgetedTimeout(TeardownRmTimeout, spent, time.Unix(999, 0)); got != 0 {
		t.Fatalf("budgetedTimeout past the deadline = %s, want 0", got)
	}
	if got := budgetedTimeout(TeardownRmTimeout, spent, time.Unix(1001, 0)); got != time.Second {
		t.Fatalf("budgetedTimeout with 1s left = %s, want 1s (the remaining budget, not the step's own bound)", got)
	}
}

// TestTeardown_BudgetExhaustedKeeps: a clock already past the deadline never
// runs the removal — it reports honestly instead.
func TestTeardown_BudgetExhaustedKeeps(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-1")
	opts := fastTeardown(t)
	// A clock that has already jumped past the deadline by the first probe.
	calls := 0
	opts.Now = func() time.Time {
		calls++
		return time.Unix(1000, 0).Add(time.Duration(calls) * time.Hour)
	}
	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownKeptUnknown {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnknown)
	}
	for _, l := range sbxArgv(t, fixture) {
		if strings.HasPrefix(l, "rm") {
			t.Fatalf("an exhausted budget still reached `sbx rm`: %q", l)
		}
	}
}

// TestTeardownJournal_BoundedAnd0600: the journal is capped and never
// group/other readable — it names sandboxes, identities and hosts.
func TestTeardownJournal_BoundedAnd0600(t *testing.T) {
	isolateState(t)
	path := filepath.Join(t.TempDir(), "teardown.jsonl")
	o := TeardownOptions{JournalPath: path}.withDefaults()
	for i := 0; i < TeardownJournalMaxEntries+25; i++ {
		if err := appendTeardownJournal(o, TeardownResult{
			Sandbox: "pix-demo", Key: "pix-demo", Trigger: TriggerSession,
			Verdict: TeardownKeptBusy, Detail: strings.Repeat("x", 40),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	entries, err := ReadTeardownJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != TeardownJournalMaxEntries {
		t.Errorf("journal holds %d entries, want the %d cap", len(entries), TeardownJournalMaxEntries)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi.Size() > teardownJournalMaxBytes {
		t.Errorf("journal is %d bytes, want <= %d", fi.Size(), teardownJournalMaxBytes)
	}
	// A corrupt tail line hides nothing else.
	if err := os.WriteFile(path, []byte("{\"verdict\":\"removed\"}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadTeardownJournal(path); err != nil || len(got) != 1 {
		t.Errorf("ReadTeardownJournal over a corrupt line = (%v, %v), want the 1 good entry", got, err)
	}
}

// TestSweepOrphans_OnlyOwnedZeroLeaseSessions: the sweep's candidate list is
// this host's own lease state, and each candidate still has to pass the full
// proof. An unowned lease dir is reported and left alone.
func TestSweepOrphans_OnlyOwnedZeroLeaseSessions(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	[ -f "$d/removed" ] && exit 0
	if [ "$2" = "--json" ]; then
		echo '[{"name":"pix-owned","state":"running","instance_id":"inst-1"},{"name":"pix-unowned","state":"running","instance_id":"inst-2"}]'
	else
		echo "pix-owned  img  running"
		echo "pix-unowned  img  running"
	fi
	exit 0
	;;
rm) touch "$d/removed"; exit 0 ;;
esac
exit 0
`)
	owned := seedRecordedSession(t, "pix-owned", "inst-1")
	unownedDir, err := LeaseDirFor("pix-unowned")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	results, err := SweepOrphans(realEnv(), &out, fastTeardown(t))
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	byName := map[string]TeardownVerdict{}
	for _, r := range results {
		byName[r.Sandbox] = r.Verdict
	}
	if byName["pix-owned"] != TeardownRemoved {
		t.Errorf("owned zero-lease session = %q, want %q", byName["pix-owned"], TeardownRemoved)
	}
	if byName["pix-unowned"] != TeardownKeptUnowned {
		t.Errorf("unowned session = %q, want %q", byName["pix-unowned"], TeardownKeptUnowned)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Errorf("the swept session's state survived: %v", err)
	}
	if _, err := os.Stat(unownedDir); err != nil {
		t.Errorf("an unowned lease dir must be left alone: %v", err)
	}
	if !strings.Contains(out.String(), "pix-unowned: kept-unowned") {
		t.Errorf("sweep output = %q, want it to report every candidate's verdict", out.String())
	}
}

// TestRm_FlagShapeRefusals: the two refusals that are safety properties, not
// ergonomics — force is never bulk, and a bare rm on a non-TTY refuses.
func TestRm_FlagShapeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts RmOptions
		want string
	}{
		{"force-with-all", RmOptions{All: true, Force: true, Interactive: true}, "--force removes an explicitly named sandbox only"},
		{"force-with-orphans", RmOptions{Orphans: true, Force: true, Interactive: true}, "--force removes an explicitly named sandbox only"},
		{"all-with-orphans", RmOptions{All: true, Orphans: true, Interactive: true}, "pick one"},
		{"bare-non-tty", RmOptions{}, "refusing bare `pix rm` on a non-interactive terminal"},
		{"bare-tty", RmOptions{Interactive: true}, "name a sandbox to remove"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			fixture := installFakeSbx(t, removableFixture)
			var out, errOut strings.Builder
			err := Rm(realEnv(), &out, &errOut, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Rm error = %v, want it to contain %q", err, tc.want)
			}
			if _, serr := os.Stat(filepath.Join(fixture, "argv.log")); serr == nil {
				t.Fatal("a refused shape still reached sbx")
			}
		})
	}
}

// TestRm_NamedNonForceRemovesAndForceIsTheOnlyForcedSeam: the default named
// path never forces; --force is the one seam that does.
func TestRm_NamedNonForceRemovesAndForceIsTheOnlyForcedSeam(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	seedRecordedSession(t, "pix-demo", "inst-1")
	var out, errOut strings.Builder
	if err := Rm(realEnv(), &out, &errOut, RmOptions{Names: []string{"pix-demo"}, Interactive: true, Teardown: fastTeardown(t)}); err != nil {
		t.Fatalf("Rm: %v (stderr %q)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "removed pix-demo") {
		t.Errorf("stdout = %q, want it to report the removal", out.String())
	}
	for _, l := range argvLines(t, fixture) {
		if strings.Contains(l, "-f") {
			t.Fatalf("the default named path forced a removal: %q", l)
		}
	}

	// The forced seam: a fixture that ONLY accepts -f proves --force really is
	// the forced call, not a relabelled non-force one.
	isolateState(t)
	forceOnly := installFakeSbx(t, "d=\"$(dirname \"$0\")\"\necho \"$@\" >> \"$d/argv.log\"\n[ \"$1\" = \"rm\" ] && { [ \"$2\" = \"-f\" ] || exit 9; }\nexit 0\n")
	out.Reset()
	errOut.Reset()
	if err := Rm(realEnv(), &out, &errOut, RmOptions{Names: []string{"pix-demo"}, Force: true, Interactive: true, Teardown: fastTeardown(t)}); err != nil {
		t.Fatalf("Rm --force: %v (stderr %q)", err, errOut.String())
	}
	if got := argvLines(t, forceOnly); len(got) == 0 || got[0] != "rm -f pix-demo" {
		t.Errorf("forced argv = %v, want [rm -f pix-demo]", got)
	}
}
