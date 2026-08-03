// axisfixture_test.go — copies of readiness/axis test fixtures cmd/pix tests
// still need. Copies, not exports: scaffolding stays out of a public API.
package main

import (
	"fmt"
	"pix/host/hostenv"
	"pix/host/readiness/axis"
	"pix/host/sys/systest"
	"testing"
)

func hwMemEnv(t *testing.T, goos string, totalGB float64) hostenv.Env {
	t.Helper()
	env := hostenv.Env{System: &systest.Fake{}}
	switch goos {
	case "darwin":
		systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
			if name == "sysctl" {
				return fmt.Sprintf("%d\n", int64(totalGB*axis.BytesPerGB)), nil
			}
			return "", fmt.Errorf("unexpected command %s", name)
		}
	case "linux":
		systest.Of(env.System).ReadFileFn = func(path string) (string, error) {
			if path != "/proc/meminfo" {
				return "", fmt.Errorf("unexpected file %s", path)
			}
			return fmt.Sprintf("MemFree:  1024 kB\nMemTotal:       %d kB\nSwapTotal: 0 kB\n", int64(totalGB*axis.BytesPerGB/1024)), nil
		}
	}
	return env
}
