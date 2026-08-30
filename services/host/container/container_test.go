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
