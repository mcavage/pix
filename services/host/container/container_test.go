package container

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRunner is a scripted Runner: each call consumes the next scripted
// response for its docker subcommand, and records every invocation so a test
// can assert exactly what Reconcile issued.
type fakeRunner struct {
	// responses maps "docker subcommand" (args[0]) to a queue of canned
	// (output, error) pairs consumed in order.
	responses map[string][]fakeResponse
	calls     [][]string
}

type fakeResponse struct {
	out string
	err error
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return "", errors.New("no subcommand")
	}
	q := f.responses[args[0]]
	if len(q) == 0 {
		return "", fmt.Errorf("fakeRunner: no scripted response for %q", args[0])
	}
	f.responses[args[0]] = q[1:]
	return q[0].out, q[0].err
}

func (f *fakeRunner) script(sub string, out string, err error) {
	if f.responses == nil {
		f.responses = map[string][]fakeResponse{}
	}
	f.responses[sub] = append(f.responses[sub], fakeResponse{out: out, err: err})
}

func testSpec() Spec {
	return Spec{
		ContainerName: "test-pix-memory",
		Image:         "example.com/pix-memory@sha256:" + strings.Repeat("a", 64),
		HostPort:      18080,
		DataDir:       "/home/agent/.pix/state/memory",
	}
}

func inspectJSON(id, image string, running bool, labels map[string]string) string {
	labelJSON := "{"
	first := true
	for k, v := range labels {
		if !first {
			labelJSON += ","
		}
		first = false
		labelJSON += fmt.Sprintf("%q:%q", k, v)
	}
	labelJSON += "}"
	return fmt.Sprintf(`[{"Id":%q,"Config":{"Image":%q,"Labels":%s},"State":{"Running":%t}}]`, id, image, labelJSON, running)
}

type stubProber struct {
	err   error
	calls []string
}

func (s *stubProber) Probe(baseURL string) error {
	s.calls = append(s.calls, baseURL)
	return s.err
}

func TestReconcile_CreatesWhenAbsent(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", "Error: No such object: test-pix-memory", errors.New("exit status 1"))
	r.script("create", "newid123", nil)
	r.script("start", "", nil)
	prober := &stubProber{}

	res, err := Reconcile(r, spec, prober, ReconcileOptions{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionCreated {
		t.Fatalf("Action = %s, want created", res.Action)
	}
	if res.ID != "newid123" {
		t.Fatalf("ID = %q, want newid123", res.ID)
	}
	if !res.Ready() {
		t.Fatalf("Ready() = false, want true (ProbeErr=%v)", res.ProbeErr)
	}
	if len(prober.calls) != 1 || prober.calls[0] != "http://127.0.0.1:18080" {
		t.Fatalf("prober calls = %v", prober.calls)
	}
	// Verify the create argv actually carries the fingerprint + restart policy.
	var createArgs []string
	for _, c := range r.calls {
		if c[0] == "create" {
			createArgs = c
		}
	}
	if createArgs == nil {
		t.Fatal("docker create was never invoked")
	}
	joined := strings.Join(createArgs, " ")
	for _, want := range []string{"--restart unless-stopped", "pix.managed=true", "pix.fingerprint=" + spec.Fingerprint(), "127.0.0.1:18080:8080", spec.DataDir + ":/data"} {
		if !strings.Contains(joined, want) {
			t.Errorf("create argv %q missing %q", joined, want)
		}
	}
}

func TestReconcile_AdoptsHealthyMatch(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("existing1", spec.Image, true, map[string]string{
		ManagedLabel: "true", FingerprintLabel: spec.Fingerprint(),
	}), nil)
	prober := &stubProber{}

	res, err := Reconcile(r, spec, prober, ReconcileOptions{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionAdopted {
		t.Fatalf("Action = %s, want adopted", res.Action)
	}
	if res.ID != "existing1" {
		t.Fatalf("ID = %q, want existing1", res.ID)
	}
	if !res.Ready() {
		t.Fatalf("Ready() = false")
	}
	for _, c := range r.calls {
		if c[0] == "create" || c[0] == "rm" || c[0] == "start" {
			t.Errorf("adopting a healthy match must not call docker %s", c[0])
		}
	}
}

func TestReconcile_StartsStoppedMatch(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("stopped1", spec.Image, false, map[string]string{
		ManagedLabel: "true", FingerprintLabel: spec.Fingerprint(),
	}), nil)
	r.script("start", "", nil)
	prober := &stubProber{}

	res, err := Reconcile(r, spec, prober, ReconcileOptions{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionStarted {
		t.Fatalf("Action = %s, want started", res.Action)
	}
	if !res.Ready() {
		t.Fatalf("Ready() = false")
	}
}

func TestReconcile_ReplacesMismatchAfterConfirm(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("old1", "example.com/pix-memory@sha256:"+strings.Repeat("b", 64), true, map[string]string{
		ManagedLabel: "true", FingerprintLabel: "sha256:stale",
	}), nil)
	r.script("stop", "", nil)
	r.script("rm", "", nil)
	r.script("create", "new1", nil)
	r.script("start", "", nil)
	prober := &stubProber{}

	var shown Info
	confirmed := false
	opts := ReconcileOptions{ConfirmReplace: func(current Info, want Spec) bool {
		shown = current
		confirmed = true
		return true
	}}

	res, err := Reconcile(r, spec, prober, opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !confirmed {
		t.Fatal("ConfirmReplace was never called for a mismatched container")
	}
	if shown.ID != "old1" {
		t.Fatalf("ConfirmReplace saw ID %q, want old1", shown.ID)
	}
	if res.Action != ActionReplaced {
		t.Fatalf("Action = %s, want replaced", res.Action)
	}
	if res.PreviousImage != "example.com/pix-memory@sha256:"+strings.Repeat("b", 64) {
		t.Errorf("PreviousImage = %q", res.PreviousImage)
	}
	if !res.Ready() {
		t.Fatalf("Ready() = false")
	}
	// Data dir must never appear in a stop/rm argv.
	for _, c := range r.calls {
		if c[0] == "rm" || c[0] == "stop" {
			for _, a := range c {
				if strings.Contains(a, spec.DataDir) {
					t.Errorf("%v touched the data dir", c)
				}
			}
		}
	}
}

func TestReconcile_RefusesReplaceWithoutConfirm(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("old1", "stale-image", true, map[string]string{
		ManagedLabel: "true", FingerprintLabel: "sha256:stale",
	}), nil)

	res, err := Reconcile(r, spec, nil, ReconcileOptions{ConfirmReplace: func(Info, Spec) bool { return false }})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionRefusedReplace {
		t.Fatalf("Action = %s, want refused-replace", res.Action)
	}
	if res.Ready() {
		t.Fatal("a refused replace must never report Ready")
	}
	for _, c := range r.calls {
		if c[0] == "stop" || c[0] == "rm" || c[0] == "create" {
			t.Errorf("refused replace must not touch docker %s", c[0])
		}
	}
}

func TestReconcile_UnmanagedContainerIsTreatedAsMismatch(t *testing.T) {
	// A container someone else created under the same name has no
	// pix.managed label at all: Reconcile must never adopt it silently.
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("stray1", spec.Image, true, map[string]string{}), nil)

	called := false
	res, err := Reconcile(r, spec, nil, ReconcileOptions{ConfirmReplace: func(Info, Spec) bool {
		called = true
		return false
	}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !called {
		t.Fatal("an unmanaged same-name container must route through ConfirmReplace, never be silently adopted")
	}
	if res.Action != ActionRefusedReplace {
		t.Fatalf("Action = %s", res.Action)
	}
}

// TestReconcile_RefusesForeignStackOwner_EvenWithFingerprintMatch proves the
// ownership check runs even when the container would otherwise be adopted.
func TestReconcile_RefusesForeignStackOwner_EvenWithFingerprintMatch(t *testing.T) {
	spec := testSpec()
	spec.StackID = "aaaaaaaaaaaaaaaa"
	r := &fakeRunner{}
	// A DIFFERENT stack's own healthy, matching, running container: same
	// fingerprint (coincidentally identical image/port/data), but its
	// StackLabel names another stack entirely.
	r.script("inspect", inspectJSON("other1", spec.Image, true, map[string]string{
		ManagedLabel: "true", FingerprintLabel: spec.Fingerprint(), StackLabel: "bbbbbbbbbbbbbbbb",
	}), nil)

	res, err := Reconcile(r, spec, nil, ReconcileOptions{ConfirmReplace: func(Info, Spec) bool {
		t.Fatal("ConfirmReplace must never be consulted for a foreign-stack-owned container")
		return true
	}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionRefusedForeignStack {
		t.Fatalf("Action = %s, want refused-foreign-stack", res.Action)
	}
	if res.ForeignStackID != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("ForeignStackID = %q, want the other stack's id", res.ForeignStackID)
	}
	if res.Ready() {
		t.Fatal("a foreign-stack refusal must never report Ready")
	}
	for _, c := range r.calls {
		if c[0] != "inspect" {
			t.Errorf("zero mutations expected, got docker %s", c[0])
		}
	}
}

// TestReconcile_RefusesMissingStackOwnerLabel_EvenIfConfirmReplaceSaysYes
// closes the exact gap the coexistence design calls out: an unmanaged (or
// pre-scoping) container squatting the scoped name must never be replaced
// just because a confirmation prompt said yes.
func TestReconcile_RefusesMissingStackOwnerLabel_EvenIfConfirmReplaceSaysYes(t *testing.T) {
	spec := testSpec()
	spec.StackID = "aaaaaaaaaaaaaaaa"
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("stray1", spec.Image, true, map[string]string{}), nil)

	res, err := Reconcile(r, spec, nil, ReconcileOptions{ConfirmReplace: func(Info, Spec) bool { return true }})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionRefusedForeignStack {
		t.Fatalf("Action = %s, want refused-foreign-stack", res.Action)
	}
	if res.ForeignStackID != "" {
		t.Fatalf("ForeignStackID = %q, want empty (missing label)", res.ForeignStackID)
	}
	for _, c := range r.calls {
		if c[0] != "inspect" {
			t.Errorf("zero mutations expected, got docker %s", c[0])
		}
	}
}

// TestReconcile_AdoptsOwnStack proves the matching-stack path still adopts
// normally: StackID being SET is not itself a reason to refuse, only a
// MISMATCH is.
func TestReconcile_AdoptsOwnStack(t *testing.T) {
	spec := testSpec()
	spec.StackID = "aaaaaaaaaaaaaaaa"
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("mine1", spec.Image, true, map[string]string{
		ManagedLabel: "true", FingerprintLabel: spec.Fingerprint(), StackLabel: spec.StackID,
	}), nil)
	prober := &stubProber{}

	res, err := Reconcile(r, spec, prober, ReconcileOptions{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionAdopted {
		t.Fatalf("Action = %s, want adopted", res.Action)
	}
	if !res.Ready() {
		t.Fatal("Ready() = false")
	}
}

// TestCreateArgs_StampsStackAndHomeLabels proves the two ownership labels
// actually reach the create argv.
func TestCreateArgs_StampsStackAndHomeLabels(t *testing.T) {
	spec := testSpec()
	spec.StackID = "aaaaaaaaaaaaaaaa"
	spec.Home = "/home/agent/.pix"
	args := spec.CreateArgs()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, StackLabel+"=aaaaaaaaaaaaaaaa") {
		t.Errorf("create argv missing stack label: %q", joined)
	}
	if !strings.Contains(joined, HomeLabel+"=/home/agent/.pix") {
		t.Errorf("create argv missing home label: %q", joined)
	}
}

// TestFingerprint_ChangesWithStackID proves StackID participates in the
// fingerprint: two otherwise-identical specs for two different stacks must
// never fingerprint identically.
func TestFingerprint_ChangesWithStackID(t *testing.T) {
	a := testSpec()
	a.StackID = "aaaaaaaaaaaaaaaa"
	b := testSpec()
	b.StackID = "bbbbbbbbbbbbbbbb"
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("two different stack ids must not fingerprint identically")
	}
}

func TestReconcile_ProbeFailureIsNotReady(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("existing1", spec.Image, true, map[string]string{
		ManagedLabel: "true", FingerprintLabel: spec.Fingerprint(),
	}), nil)
	prober := &stubProber{err: errors.New("healthz: connection refused")}

	res, err := Reconcile(r, spec, prober, ReconcileOptions{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionAdopted {
		t.Fatalf("Action = %s", res.Action)
	}
	if res.Ready() {
		t.Fatal("a failed probe must never report Ready")
	}
	if res.ProbeErr == nil {
		t.Fatal("ProbeErr must be set")
	}
}

func TestAbsent(t *testing.T) {
	r := &fakeRunner{}
	r.script("inspect", "Error: No such object: pix-memory", errors.New("exit status 1"))
	absent, err := Absent(r, "pix-memory")
	if err != nil {
		t.Fatalf("Absent: %v", err)
	}
	if !absent {
		t.Fatal("expected absent=true")
	}
}

func TestAbsent_FalseWhenPresent(t *testing.T) {
	r := &fakeRunner{}
	r.script("inspect", inspectJSON("x", "img", true, nil), nil)
	absent, err := Absent(r, "pix-memory")
	if err != nil {
		t.Fatalf("Absent: %v", err)
	}
	if absent {
		t.Fatal("expected absent=false")
	}
}

func TestStopAndRemove_NeverTouchesDataDir(t *testing.T) {
	r := &fakeRunner{}
	r.script("stop", "", nil)
	r.script("rm", "", nil)
	if err := StopAndRemove(r, "pix-memory"); err != nil {
		t.Fatalf("StopAndRemove: %v", err)
	}
	if len(r.calls) != 2 || r.calls[0][0] != "stop" || r.calls[1][0] != "rm" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestStopAndRemove_MissingIsNoop(t *testing.T) {
	r := &fakeRunner{}
	r.script("stop", "Error: No such container: pix-memory", errors.New("exit 1"))
	r.script("rm", "Error: No such container: pix-memory", errors.New("exit 1"))
	if err := StopAndRemove(r, "pix-memory"); err != nil {
		t.Fatalf("StopAndRemove on an absent container must be a no-op, got %v", err)
	}
}

func TestFingerprint_ChangesWithImageOrPortOrData(t *testing.T) {
	base := testSpec()
	fp := base.Fingerprint()

	other := base
	other.Image = "example.com/pix-memory@sha256:" + strings.Repeat("c", 64)
	if other.Fingerprint() == fp {
		t.Error("changing Image did not change the fingerprint")
	}

	other = base
	other.HostPort = 19090
	if other.Fingerprint() == fp {
		t.Error("changing HostPort did not change the fingerprint")
	}

	other = base
	other.DataDir = "/somewhere/else"
	if other.Fingerprint() == fp {
		t.Error("changing DataDir did not change the fingerprint")
	}

	other = base
	other.ContainerName = "a-completely-different-name"
	if other.Fingerprint() != fp {
		t.Error("ContainerName must not affect the fingerprint")
	}
}

func TestCreateArgs_PublishesLoopbackOnly(t *testing.T) {
	spec := testSpec()
	args := spec.CreateArgs()
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-p 0.0.0.0") || strings.Contains(joined, "-p :") {
		t.Errorf("create argv must publish on 127.0.0.1 only: %q", joined)
	}
	if !strings.Contains(joined, "127.0.0.1:18080:8080") {
		t.Errorf("create argv missing loopback publish: %q", joined)
	}
}

// TestCreateArgs_AuthTokenIsAReadOnlyMountNeverAnEnvFile is the security
// re-review round 1 blocker #1 regression: the pix-memory bearer token must
// enter the container ONLY as a read-only bind mount of the host token
// file, at the exact path pix-memory reads (AuthTokenMountPath) — never via
// `--env-file`/`-e`, both of which would write the token's VALUE into the
// container's own Config.Env (docker inspect exposes Config.Env in full to
// anything on this host with inspect access). DataDir remains the only
// WRITABLE mount.
func TestCreateArgs_AuthTokenIsAReadOnlyMountNeverAnEnvFile(t *testing.T) {
	spec := testSpec()
	spec.AuthTokenFile = "/home/agent/.pix/state/memory/auth.token"
	args := spec.CreateArgs()
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--env-file") {
		t.Errorf("create argv must never use --env-file: %q", joined)
	}
	for _, a := range args {
		if a == "-e" || strings.HasPrefix(a, "MEMORY_AUTH_TOKEN=") {
			t.Errorf("create argv must never pass a literal -e NAME=VALUE: %v", args)
		}
	}
	wantMount := spec.AuthTokenFile + ":" + AuthTokenMountPath + ":ro"
	if !strings.Contains(joined, wantMount) {
		t.Errorf("create argv missing read-only auth token mount %q: %q", wantMount, joined)
	}
	// The data mount must stay the only WRITABLE one — the auth token mount
	// must carry :ro and nothing else must.
	if strings.Contains(joined, spec.DataDir+":/data:ro") {
		t.Error("the data mount must remain writable, not :ro")
	}
}

// TestCreateArgs_NoAuthTokenFileOmitsTheMountEntirely: an empty
// AuthTokenFile (pre-`pix setup`, or a caller that never resolved one) must
// not synthesize a mount to a path that does not exist.
func TestCreateArgs_NoAuthTokenFileOmitsTheMountEntirely(t *testing.T) {
	spec := testSpec()
	args := spec.CreateArgs()
	joined := strings.Join(args, " ")
	if strings.Contains(joined, AuthTokenMountPath) {
		t.Errorf("create argv must omit the auth token mount when AuthTokenFile is empty: %q", joined)
	}
}

// TestFingerprint_ChangesWithAuthTokenFilePathButNeverLeaksItsContent: the
// fingerprint tracks the auth-token-file PATH (a changed path is a changed
// container identity) but is computed over Spec fields only — the token's
// CONTENT never enters CreateArgs, Fingerprint, or any label, so it can
// never be derived from anything docker inspect (or this package) exposes.
func TestFingerprint_ChangesWithAuthTokenFilePathButNeverLeaksItsContent(t *testing.T) {
	base := testSpec()
	fp := base.Fingerprint()

	withToken := base
	withToken.AuthTokenFile = "/home/agent/.pix/state/memory/auth.token"
	if withToken.Fingerprint() == fp {
		t.Error("setting AuthTokenFile did not change the fingerprint")
	}

	// The fingerprint (and everything derived from Spec) must never carry the
	// token VALUE itself — only ever its path. There is no field for a token
	// value on Spec at all, which this assertion pins as a design invariant:
	// a future change that adds one and threads it into Fingerprint/CreateArgs
	// would be exactly the regression this test exists to catch.
	if strings.Contains(withToken.Fingerprint(), "sekrit") {
		t.Fatal("fingerprint must never contain a token value")
	}
}

// TestReconcile_StartPortConflict_ClassifiesAndRemovesFailedCreate proves
// round 5's classification half of QA F4's port retry: when `docker create`
// succeeds but the SUBSEQUENT `docker start` loses the bind race (the
// window PortAvailable's pre-check cannot close — something else grabs the
// port between the probe and the real bind), Reconcile must (1) classify
// the failure as *PortInUseError naming spec.HostPort, never an opaque
// wrapped docker error, and (2) remove the container it just created BY ITS
// OWN ID so a caller's reallocate-and-retry never collides with, or later
// adopts, that orphan.
func TestReconcile_StartPortConflict_ClassifiesAndRemovesFailedCreate(t *testing.T) {
	spec := testSpec()
	r := &fakeRunner{}
	r.script("inspect", "Error: No such object: test-pix-memory", errors.New("exit status 1"))
	r.script("create", "created-container-id", nil)
	r.script("start", "Error response from daemon: driver failed programming external connectivity "+
		"on endpoint test-pix-memory: Bind for 0.0.0.0:18080 failed: port is already allocated",
		errors.New("exit status 1"))
	r.script("rm", "created-container-id", nil)

	_, err := Reconcile(r, spec, nil, ReconcileOptions{})
	portErr, ok := err.(*PortInUseError)
	if !ok {
		t.Fatalf("Reconcile error = %v (%T), want *PortInUseError", err, err)
	}
	if portErr.Port != spec.HostPort {
		t.Errorf("PortInUseError.Port = %d, want %d", portErr.Port, spec.HostPort)
	}

	var removedByID bool
	for _, call := range r.calls {
		if len(call) >= 3 && call[0] == "rm" && call[len(call)-1] == "created-container-id" {
			removedByID = true
		}
		// The cleanup must never remove by the container NAME (which could
		// reach a DIFFERENT, unrelated container under the same name in a
		// race); it must name the exact ID docker create just returned.
		if len(call) >= 1 && call[0] == "rm" {
			for _, a := range call {
				if a == spec.containerName() {
					t.Errorf("cleanup removed by NAME (%v), want by the created ID only", call)
				}
			}
		}
	}
	if !removedByID {
		t.Errorf("expected the failed create to be removed by its own ID, calls: %v", r.calls)
	}
}

// TestIsDockerPortConflict_ScopedToTheExactPort proves the classifier never
// fires on a marker phrase that names a DIFFERENT port or no port at all —
// "do not trust arbitrary stderr" without checking it actually names the
// port THIS operation cared about.
func TestIsDockerPortConflict_ScopedToTheExactPort(t *testing.T) {
	cases := []struct {
		name string
		out  string
		port int
		want bool
	}{
		{"exact port allocated", "Bind for 0.0.0.0:18080 failed: port is already allocated", 18080, true},
		{"exact port address in use", "listen tcp 0.0.0.0:18080: bind: address already in use", 18080, true},
		{"different port same marker", "Bind for 0.0.0.0:9999 failed: port is already allocated", 18080, false},
		{"marker absent", "Error: no such image: pix-memory@sha256:deadbeef", 18080, false},
		{"unrelated permission error mentioning a number", "permission denied opening file 18080.log", 18080, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDockerPortConflict(c.out, c.port); got != c.want {
				t.Errorf("isDockerPortConflict(%q, %d) = %v, want %v", c.out, c.port, got, c.want)
			}
		})
	}
}
