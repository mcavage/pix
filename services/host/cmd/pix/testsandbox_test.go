package main

import (
	"os"
	"testing"
)

// TestMain points this package's config resolution at a throwaway PIX_HOME
// before any test runs.
//
// This is a blast-radius guard, not a convenience. Plenty of code here reaches
// cfg.Save(), and Save resolves its destination from the ENVIRONMENT
// (PIX_HOME, the only variable config.Path/StateDir/DataDir honor in
// production — QA F5 closed the old PIX_CONFIG/XDG_* fallback chain) rather
// than from anything the caller passes. So a test that hand-builds a
// *config.Config and calls a function that happens to save it overwrites the
// DEVELOPER'S OWN config.toml — silently, while passing, destroying a real
// machine's bindings and roster.
//
// That is not hypothetical: it happened while writing this package's ollama
// tests, and it was only recoverable because a backup had been taken minutes
// earlier for an unrelated reason. Several existing tests already carry a
// per-test t.Setenv with a comment explaining the hazard, which is the tell that
// the default was wrong: remembering must not be what stands between a test and
// the developer's home directory.
//
// An already-set value is left alone: a few tests re-exec this binary as a
// subprocess with a specific PIX_HOME to drive a save-failure path, and the
// child runs this TestMain too.
//
// HOME and ROUTING_DIR are NOT redirected. Tests inject those themselves to
// build fixtures or to force write failures, and pre-empting them turns a
// targeted safety net into unrelated breakage. A guard that has to be fought
// gets deleted.
func TestMain(m *testing.M) {
	var tmp string
	if os.Getenv("PIX_HOME") == "" {
		dir, err := os.MkdirTemp("", "pix-test-home-")
		if err != nil {
			panic("test sandbox: " + err.Error())
		}
		if err := os.Setenv("PIX_HOME", dir); err != nil {
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
