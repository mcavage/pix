package launch

// sbx_missing_test.go — DX finding 5: `ls`/`rm` (and, through the shared
// sentinel, `mcp`) must agree on ONE sbx-absent contract: the same detectable
// error (mcp.ErrSbxUnavailable), the same install fix in the message
// (health.SbxInstallFix — the exact command doctor already prints), rather
// than each verb inventing its own vague "install the Docker Sandboxes CLI"
// prose with no exit-code story.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/sys/systest"
)

func absentSbxEnv() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) {
		return "", errors.New("not found")
	}}}
}

func TestLs_SbxAbsent_IsTheSharedSentinelWithTheExactInstallFix(t *testing.T) {
	var out bytes.Buffer
	err := Ls(absentSbxEnv(), &out, false)
	if !errors.Is(err, mcp.ErrSbxUnavailable) {
		t.Fatalf("Ls with sbx absent must be mcp.ErrSbxUnavailable, got: %v", err)
	}
	if !strings.Contains(err.Error(), health.SbxInstallFix) {
		t.Errorf("Ls sbx-absent error must name the exact install fix %q, got: %v", health.SbxInstallFix, err)
	}
}

func TestRm_SbxAbsent_IsTheSharedSentinelWithTheExactInstallFix(t *testing.T) {
	var out, errb bytes.Buffer
	err := Rm(absentSbxEnv(), &out, &errb, RmOptions{Names: []string{"pix-x"}})
	if !errors.Is(err, mcp.ErrSbxUnavailable) {
		t.Fatalf("Rm with sbx absent must be mcp.ErrSbxUnavailable, got: %v", err)
	}
	if !strings.Contains(err.Error(), health.SbxInstallFix) {
		t.Errorf("Rm sbx-absent error must name the exact install fix %q, got: %v", health.SbxInstallFix, err)
	}
}
