package service

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/rpc"
)

func TestStaleServeVersionRequiresPositivePixIdentity(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	env := fakeEnv{ports: map[int]bool{rpc.MemoryPortDefault: true}}
	if from, stale := staleServeVersion(cfg, env, nil, func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: "0.1.7"}, nil
	}); !stale || from != "0.1.7" {
		t.Fatalf("staleServeVersion = %q, %v", from, stale)
	}
	if _, stale := staleServeVersion(cfg, env, nil, func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: "someone-else", Version: "0.1.7"}, nil
	}); stale {
		t.Fatal("a foreign port holder must never be restarted")
	}
}

// TestStaleServeVersionTreatsEmptyVersionAsStale is the regression for the bug
// this fix closes: a daemon old enough to predate the identity method
// reporting a version answers with our own name and Version == "". That must
// be classified stale/incompatible, never silently accepted as current just
// because there was nothing to string-compare.
func TestStaleServeVersionTreatsEmptyVersionAsStale(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	env := fakeEnv{ports: map[int]bool{rpc.MemoryPortDefault: true}}
	from, stale := staleServeVersion(cfg, env, nil, func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: rpc.MemoryName, Version: ""}, nil
	})
	if !stale || from == "" {
		t.Fatalf("an empty-version daemon must be reported stale with a non-empty label, got %q, %v", from, stale)
	}

	// A foreign process that happens to answer with an empty version must still
	// never be touched: the name check comes first.
	if _, stale := staleServeVersion(cfg, env, nil, func(int) (rpc.ServiceIdentity, error) {
		return rpc.ServiceIdentity{Name: "someone-else", Version: ""}, nil
	}); stale {
		t.Fatal("a foreign port holder must never be restarted, even with an empty version")
	}
}

func TestRestartStaleServeLazyUsesSafeStopThenEnsure(t *testing.T) {
	var calls []string
	rl := serveReloader{
		mode: func() serveMode { return serveLazy },
		Stop: func(io.Writer) (bool, error) {
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
		Stop: func(io.Writer) (bool, error) {
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
