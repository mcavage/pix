package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/uat"
)

func TestEnsureUatWorkerOrFail_AdoptsLiveWorkerWithoutResolvingHostBinary(t *testing.T) {
	// A short root, not t.TempDir(): the descriptive test name embedded in
	// t.TempDir()'s path pushes the unix socket path past the kernel's
	// ~104-108 byte sun_path limit.
	root, err := os.MkdirTemp("", "uatw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	uatState := filepath.Join(root, "uat")
	rec := &uat.Registration{SessionID: "abcd1234", MCPName: "pix-uat-abcd1234"}
	runnerState := filepath.Join(uatState, "sessions", rec.SessionID)
	if err := os.MkdirAll(runnerState, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", uat.SessionSocketPath(runnerState))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	hostBinaryCalled := false
	env := hostenv.Env{HostBinary: func() (string, error) {
		hostBinaryCalled = true
		return "", errors.New("must not be called when a live worker is adopted")
	}}
	if err := ensureUatWorkerOrFail(env, "/repo", uatState, rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostBinaryCalled {
		t.Fatal("adopting a live worker must never resolve pix-host at all")
	}
}

func TestEnsureUatWorkerOrFail_FailsClosedWhenHostBinaryUnresolved(t *testing.T) {
	uatState := filepath.Join(t.TempDir(), "uat")
	rec := &uat.Registration{SessionID: "abcd1234", MCPName: "pix-uat-abcd1234"}
	env := hostenv.Env{HostBinary: func() (string, error) { return "", errors.New("no pix-host") }}
	err := ensureUatWorkerOrFail(env, "/repo", uatState, rec)
	if err == nil {
		t.Fatal("an unresolvable pix-host binary must fail the worker start closed")
	}
}

func TestEnsureUatWorkerOrFail_FailsClosedWhenTheWorkerCannotStart(t *testing.T) {
	uatState := filepath.Join(t.TempDir(), "uat")
	rec := &uat.Registration{SessionID: "abcd1234", MCPName: "pix-uat-abcd1234"}
	env := hostenv.Env{HostBinary: func() (string, error) { return "/nonexistent/pix-host-binary-xyz", nil }}
	err := ensureUatWorkerOrFail(env, "/repo", uatState, rec)
	if err == nil {
		t.Fatal("a pix-host binary that cannot even exec must fail the launch closed, never claim dev UAT")
	}
	if !strings.Contains(err.Error(), "uat-worker") {
		t.Fatalf("error %v should name uat-worker", err)
	}
}

// The rest of this file pins run_cmd.go's CALL ORDER at the source level, the
// same technique TestUATLifecycle_SuccessHandsRegistrationToSandboxTeardown
// (uat_lifecycle_test.go) already uses: defaultShellEnv() is hard-wired to the
// real OS (env.go), so runLaunch cannot be driven end-to-end with a fake sbx
// without reaching outside the process, and a source-order assertion is the
// existing, established way this package proves an ordering invariant that a
// unit test on the extracted pieces (above, and uat/worker_test.go) cannot.
func TestUatWorkerLifecycle_CreateStartsAfterRegisterAndBeforeRunSession(t *testing.T) {
	source, err := os.ReadFile("run_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(source)
	registerMCP := strings.Index(src, "if err := uat.RegisterMCP(defaultShellEnv(), uatRec, o.DevRoot,")
	ensureWorker := strings.Index(src, "if werr := ensureUatWorkerOrFail(defaultShellEnv(), o.DevRoot,")
	runSession := strings.Index(src, "if xerr := launch.RunSession(spec, deps); xerr != nil")
	if registerMCP < 0 || ensureWorker < 0 || runSession < 0 {
		t.Fatal("expected to find RegisterMCP, ensureUatWorkerOrFail (create path) and RunSession in run_cmd.go")
	}
	if !(registerMCP < ensureWorker && ensureWorker < runSession) {
		t.Fatalf("create-path uat-worker must start after the secure session dir exists (RegisterMCP, %d) and before RunSession (%d) can need it; got ensureUatWorkerOrFail at %d", registerMCP, runSession, ensureWorker)
	}
}

func TestUatWorkerLifecycle_CreateFailureRollsBackRegistrationAndMCP(t *testing.T) {
	source, err := os.ReadFile("run_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(source)
	i := strings.Index(src, "if werr := ensureUatWorkerOrFail(defaultShellEnv(), o.DevRoot,")
	if i < 0 {
		t.Fatal("expected create-path ensureUatWorkerOrFail call in run_cmd.go")
	}
	end := i + 400
	if end > len(src) {
		end = len(src)
	}
	block := src[i:end]
	for _, want := range []string{"uat.UnregisterMCP(defaultShellEnv(), uatRec.MCPName)", "uat.DeleteRegistration(defaultShellEnv(), o.Name)", "runFail(d, 1,"} {
		if !strings.Contains(block, want) {
			t.Fatalf("create-path worker-start failure must roll back via %q; block:\n%s", want, block)
		}
	}
}

func TestUatWorkerLifecycle_AttachEnsuresWorkerBeforeRunSession(t *testing.T) {
	source, err := os.ReadFile("run_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(source)
	resolveAttach := strings.Index(src, "uat.ResolveAttachRegistration(defaultShellEnv(), o.Name, o.Dev)")
	ensureWorker := strings.Index(src, "if werr := ensureUatWorkerOrFail(defaultShellEnv(), repoRoot,")
	runSession := strings.Index(src, "if xerr := launch.RunSession(spec, deps); xerr != nil")
	if resolveAttach < 0 || ensureWorker < 0 || runSession < 0 {
		t.Fatal("expected ResolveAttachRegistration, the attach-path ensureUatWorkerOrFail call, and RunSession in run_cmd.go")
	}
	if !(resolveAttach < ensureWorker && ensureWorker < runSession) {
		t.Fatalf("a --dev attach must ensure the worker after reading the registration (%d) and before session attach/RunSession (%d); got %d", resolveAttach, runSession, ensureWorker)
	}
}
