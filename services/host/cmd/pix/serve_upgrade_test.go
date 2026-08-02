package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/sys/systest"
)

func TestStaleServeVersionRequiresPositivePixIdentity(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	env := shellEnv{System: &systest.Fake{DialLocalFn: func(int) bool { return true }, GetenvFn: func(string) string { return "" }}}
	if from, stale := staleServeVersion(cfg, env, nil, func(int) (serviceIdentityResult, error) {
		return serviceIdentityResult{Name: identityMemoryName, Version: "0.1.7"}, nil
	}); !stale || from != "0.1.7" {
		t.Fatalf("staleServeVersion = %q, %v", from, stale)
	}
	if _, stale := staleServeVersion(cfg, env, nil, func(int) (serviceIdentityResult, error) {
		return serviceIdentityResult{Name: "someone-else", Version: "0.1.7"}, nil
	}); stale {
		t.Fatal("a foreign port holder must never be restarted")
	}
}

func TestRestartStaleServeLazyUsesSafeStopThenEnsure(t *testing.T) {
	var calls []string
	rl := serveReloader{
		mode: func() serveMode { return serveLazy },
		stopServe: func(io.Writer) (bool, error) {
			calls = append(calls, "stop")
			return true, nil
		},
		ensure: func() error {
			calls = append(calls, "ensure")
			return nil
		},
	}
	var out bytes.Buffer
	restartStaleServe(rl, "0.1.7", "0.1.14", &out)
	if strings.Join(calls, ",") != "stop,ensure" || !strings.Contains(out.String(), "0.1.7 → 0.1.14") {
		t.Fatalf("calls=%v output=%q", calls, out.String())
	}
}

func TestRestartStaleServeUnrecordedOrphanUsesSafeDiscoveryStop(t *testing.T) {
	var calls []string
	rl := serveReloader{
		mode: func() serveMode { return serveDown },
		stopServe: func(io.Writer) (bool, error) {
			calls = append(calls, "stop")
			return true, nil
		},
		ensure: func() error {
			calls = append(calls, "ensure")
			return nil
		},
	}
	var out bytes.Buffer
	restartStaleServe(rl, "0.1.7", "0.1.14", &out)
	if strings.Join(calls, ",") != "stop,ensure" || !strings.Contains(out.String(), "0.1.7 → 0.1.14") {
		t.Fatalf("calls=%v output=%q", calls, out.String())
	}
}
