package uatmatrix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsolatedEnvRehomesEveryPixRoot(t *testing.T) {
	phaseDir := t.TempDir()
	t.Setenv("HOME", "/normal-home-must-not-leak")
	env, err := isolatedEnv(phaseDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HOME":            filepath.Join(phaseDir, "home"),
		"XDG_CONFIG_HOME": filepath.Join(phaseDir, "config"),
		"XDG_DATA_HOME":   filepath.Join(phaseDir, "data"),
		"XDG_STATE_HOME":  filepath.Join(phaseDir, "state"),
		"XDG_CACHE_HOME":  filepath.Join(phaseDir, "cache"),
		"PIX_CONFIG":      filepath.Join(phaseDir, "config", "config.toml"),
	}
	for key, value := range want {
		entry := key + "=" + value
		found := false
		for _, got := range env {
			if got == entry {
				found = true
			}
			if strings.HasPrefix(got, key+"=") && got != entry {
				t.Errorf("%s escaped phase dir: %q", key, got)
			}
		}
		if !found {
			t.Errorf("missing %q", entry)
		}
	}
}

func TestProcHandleWaitThenStopDoesNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := startProcess(ctx, "/bin/sh", []string{"-c", "exit 7"}, os.Environ(), t.TempDir(), filepath.Join(t.TempDir(), "proc.log"))
	if err != nil {
		t.Fatal(err)
	}
	if _, exited := proc.waitExit(2 * time.Second); !exited {
		t.Fatal("child did not exit")
	}
	done := make(chan struct{})
	go func() {
		proc.stop(time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop blocked after waitExit had already observed completion")
	}
}

func TestFreePortNeverReturnsNormalMemoryPort(t *testing.T) {
	for i := 0; i < 20; i++ {
		port, err := freePort()
		if err != nil {
			t.Fatal(err)
		}
		if port == 11435 {
			t.Fatal("freePort returned the normal memory port 11435")
		}
	}
}
