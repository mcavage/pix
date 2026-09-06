package hosttools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pix/host/pixhome"
)

func service(t *testing.T, dev bool) *Service {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Config: Config{Home: pixhome.New(filepath.Join(root, "home")), Workspace: root, Pix: "pix", Dev: dev, Guard: func() error { return nil }, Redact: func(v string) string { return strings.ReplaceAll(v, "secret-value", "[redacted]") }}}
	t.Cleanup(s.Close)
	return s
}
func call(t *testing.T, s *Service, name string, args any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := s.Call(name, raw)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func waitJob(t *testing.T, s *Service, id any) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		result := call(t, s, "pix_job_status", map[string]any{"job_id": id})
		if result["status"] == "finished" {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}
func TestNormalAuthorityAndStrictArguments(t *testing.T) {
	s := service(t, false)
	for _, v := range s.Definitions() {
		if v.(map[string]any)["name"] == "pix_host_exec" {
			t.Fatal("normal session advertises host exec")
		}
	}
	for _, tc := range []struct{ name, raw string }{{"pix_host_exec", `{"argv":["touch","/tmp/unauthorized"]}`}, {"pix_host_info", `{"dev":true}`}, {"pix_env_add", `{"source":".","name":"--help"}`}, {"pix_env_test", `{"name":"x","prompt":"hello","timeout_seconds":601}`}, {"pix_env_list", `null`}} {
		if _, err := s.Call(tc.name, json.RawMessage(tc.raw)); err == nil {
			t.Fatalf("accepted %s %s", tc.name, tc.raw)
		}
	}
	s.Config.Guard = func() error { return errors.New("expired") }
	if _, err := s.Call("pix_host_info", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expired session allowed")
	}
}
func TestSourceContainment(t *testing.T) {
	s := service(t, false)
	inside := filepath.Join(s.Config.Workspace, "candidate")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := containedDirectory(s.Config.Workspace, "candidate"); err != nil || got != inside {
		t.Fatalf("%q %v", got, err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Config.Workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"..", "escape", outside} {
		if _, err := containedDirectory(s.Config.Workspace, path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
func TestJobsOutputExitRedactionAndEnvironment(t *testing.T) {
	s := service(t, true)
	t.Setenv("OPENAI_API_KEY", "do-not-inherit")
	started := call(t, s, "pix_host_exec", map[string]any{"argv": []string{"/bin/sh", "-c", `printf 'secret-value\n'; printf '%s' "${OPENAI_API_KEY-unset}"; exit 7`}})
	result := waitJob(t, s, started["job_id"])
	if result["exit_code"] != float64(7) || result["output"] != "[redacted]\nunset" {
		t.Fatalf("%v", result)
	}
	started = call(t, s, "pix_host_exec", map[string]any{"argv": []string{"/bin/sh", "-c", "yes x | head -c 100000"}})
	result = waitJob(t, s, started["job_id"])
	if result["truncated"] != true || len(result["output"].(string)) != outputLimit {
		t.Fatalf("unbounded output: %v", result["truncated"])
	}
	other := service(t, true)
	raw, _ := json.Marshal(map[string]any{"job_id": started["job_id"]})
	if _, err := other.Call("pix_job_status", raw); err == nil {
		t.Fatal("cross-session job visible")
	}
}
func TestCancelTimeoutAndClose(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "close", "guard"} {
		t.Run(mode, func(t *testing.T) {
			s := service(t, true)
			var live atomic.Bool
			live.Store(true)
			s.Config.Guard = func() error {
				if !live.Load() {
					return errors.New("session ended")
				}
				return nil
			}
			args := map[string]any{"argv": []string{"/bin/sh", "-c", "sleep 30 & wait"}}
			if mode == "timeout" {
				args["timeout_seconds"] = 1
			}
			started := call(t, s, "pix_host_exec", args)
			id := started["job_id"].(string)
			switch mode {
			case "cancel":
				call(t, s, "pix_job_cancel", map[string]any{"job_id": id})
			case "close":
				s.Close()
			case "guard":
				live.Store(false)
			}
			deadline := time.Now().Add(8 * time.Second)
			for {
				out, err := s.status(id, false)
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]any
				_ = json.Unmarshal([]byte(out), &result)
				if result["status"] == "finished" {
					if result["exit_code"] == float64(0) {
						t.Fatal("cancel succeeded")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("cancellation did not reap subprocess")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
