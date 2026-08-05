package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pix/host/health"
)

// configload_test.go pins the review finding this file exists to close:
// `pix status` used to EXIT 1 when the config would not load. Status's whole
// contract is that it always exits 0 — it is the landing screen, it runs in
// prompts and under `set -e`, and a user whose config is broken needs to see
// that rather than have their shell die on it.

func TestRenderStatusConfigError_ExitsZeroAndRendersTheIssue(t *testing.T) {
	var out bytes.Buffer
	code := RenderStatusConfigError(&out, "", errors.New("toml: line 4: expected key"), false)
	if code != StatusExit {
		t.Errorf("exit = %d, want %d — status always exits 0", code, StatusExit)
	}
	got := out.String()
	for _, want := range []string{"config", "1 issue", health.DoctorCommand} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
	// Status names no repair itself; that is doctor's job, and printing one
	// here is how the two surfaces start disagreeing.
	if strings.Contains(got, "pix config path") {
		t.Errorf("status printed a repair command:\n%s", got)
	}
}

// The verdict is not LOST by exiting 0: --json still publishes doctor's exit,
// so a machine reader can tell a broken config from a healthy one.
func TestRenderStatusConfigError_JSONPublishesDoctorsVerdict(t *testing.T) {
	var out bytes.Buffer
	if code := RenderStatusConfigError(&out, "work", errors.New("unreadable"), true); code != StatusExit {
		t.Fatalf("exit = %d, want %d", code, StatusExit)
	}
	var v ReportJSONView
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("status --json did not emit valid JSON: %v\n%s", err, out.String())
	}
	if v.Exit != health.ExitNotReady {
		t.Errorf("json exit = %d, want %d (doctor's verdict)", v.Exit, health.ExitNotReady)
	}
	if v.Ready || v.Verdict != "gaps" {
		t.Errorf("json says ready=%v verdict=%q for a config that would not load", v.Ready, v.Verdict)
	}
	if len(v.Checks) != 1 || v.Checks[0].Name != "config" || v.Checks[0].Fix == "" {
		t.Errorf("json must carry the one config check with its fix, got %+v", v.Checks)
	}
}

// Doctor renders the SAME fact and, unlike status, does fail on it: an
// unreadable config is a verified gap in something required, which is exactly
// what doctor's exit 1 means.
func TestConfigLoadSnapshot_IsABlockingRequiredGap(t *testing.T) {
	s := ConfigLoadSnapshot(errors.New("boom"))
	if s.ExitCode() != health.ExitNotReady {
		t.Errorf("exit = %d, want %d", s.ExitCode(), health.ExitNotReady)
	}
	if len(s.Blocking()) != 1 {
		t.Errorf("want exactly one blocking result, got %v", s.Blocking())
	}
	var out bytes.Buffer
	health.RenderDoctorWith(&out, s, health.DoctorOpts{})
	if !strings.Contains(out.String(), "boom") {
		t.Errorf("doctor must show the load error as evidence:\n%s", out.String())
	}
}
