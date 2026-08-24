//go:build unix

package uat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// TestDeleteRegistrationDropsAnUnverifiableWorkerRecordWithoutSignalingIt is
// the hard-crash/orphan-reaper shape: DeleteRegistration runs (an orphan
// sweep, or a later teardown) long after whatever launcher started the
// worker is gone, with only the on-disk record left to prove what to stop.
// `ps` will correctly report a plain `sleep` as NOT `pix-host uat-worker`, so
// StopWorker (which DeleteRegistration now calls before removeSessionState)
// must refuse to signal it and only drop the stale record — proving the
// integration wiring never signals an arbitrary pid, and still fully clears
// session state.
func TestDeleteRegistrationDropsAnUnverifiableWorkerRecordWithoutSignalingIt(t *testing.T) {
	state, err := os.MkdirTemp("", "uatreap")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state)
	recordDir := filepath.Join(state, "uat")
	sessionDir := filepath.Join(recordDir, "sessions", "abc")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(recordDir, "pix-project.json")
	if err := os.WriteFile(recordPath, []byte(`{"session_id":"abc","mcp_name":"pix-uat-abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	recData, err := json.Marshal(WorkerRecord{PID: cmd.Process.Pid, SessionID: "abc", Socket: SessionSocketPath(sessionDir)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, WorkerPIDFileName), recData, 0o600); err != nil {
		t.Fatal(err)
	}

	sys := &systest.Fake{
		StateDirFn: func() (string, error) { return state, nil },
		ReadFileFn: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
	}
	if err := DeleteRegistration(hostenv.Env{System: sys}, "pix-project"); err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(cmd.Process.Pid, 0) != nil {
		t.Fatal("a pid ps cannot verify as this session's uat-worker must never be signalled by DeleteRegistration")
	}
	for _, path := range []string{recordPath, sessionDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("UAT teardown left %s behind: %v", path, err)
		}
	}
}
