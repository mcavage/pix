package main

import (
	"os"
	"testing"
)

// TestMain points this package's config resolution at a throwaway dir before
// any test runs.
//
// This is a blast-radius guard, not a convenience. Plenty of code here reaches
// cfg.Save(), and Save resolves its destination from the ENVIRONMENT
// ($PIX_CONFIG, else $XDG_CONFIG_HOME/pix, else ~/.config/pix) rather than from
// anything the caller passes. So a test that hand-builds a *config.Config and
// calls a function that happens to save it overwrites the DEVELOPER'S OWN
// config.toml — silently, while passing, destroying a real machine's bindings
// and roster.
//
// That is not hypothetical: it happened while writing this package's ollama
// tests, and it was only recoverable because a backup had been taken minutes
// earlier for an unrelated reason. Several existing tests already carry a
// per-test t.Setenv with a comment explaining the hazard, which is the tell that
// the default was wrong: remembering must not be what stands between a test and
// the developer's home directory. (It also explains three doctor tests that
// failed on a clean checkout — they were reading the developer's real
// op-refs.env, which lives beside config.toml.)
//
// Scope is deliberately ONE variable, and deliberately the LOWEST-priority hop
// in the resolver. Setting XDG_CONFIG_HOME moves config.toml and the op-refs.env
// beside it — the two files a test can actually destroy — while leaving the
// higher-priority PIX_CONFIG free, so a test that isolates itself with EITHER
// variable still wins. Grabbing PIX_CONFIG instead was tried and reverted: it
// outranks the XDG_CONFIG_HOME several tests already use, so it quietly funnelled
// them into one shared file where they polluted each other.
//
// An already-set value is left alone, for that reason plus one more: a few tests
// re-exec this binary as a subprocess with a specific config env to drive a
// save-failure path, and the child runs this TestMain too.
//
// HOME, XDG_DATA_HOME and ROUTING_DIR are NOT redirected. Tests inject those
// themselves to build fixtures or to force write failures, and pre-empting them
// turns a targeted safety net into unrelated breakage. A guard that has to be
// fought gets deleted.
func TestMain(m *testing.M) {
	var tmp string
	if os.Getenv("PIX_CONFIG") == "" && os.Getenv("XDG_CONFIG_HOME") == "" {
		dir, err := os.MkdirTemp("", "pix-test-config-")
		if err != nil {
			panic("test sandbox: " + err.Error())
		}
		if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
			panic("test sandbox: " + err.Error())
		}
		tmp = dir
	}
	code := m.Run()
	if tmp != "" {
		_ = os.RemoveAll(tmp) // not a defer: os.Exit does not run them
	}
	os.Exit(code)
}
