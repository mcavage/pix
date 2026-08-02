package main

import (
	"bytes"
	"fmt"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// bootstrap with a key already present is a no-op that returns true and never
// touches op (no prompt).
func TestBootstrapProviderKeys_PresentNoOp(t *testing.T) {
	var opCalled bool
	env := shellEnv{System: &systest.Fake{LookPathFn: func(n string) (string, error) { return "/usr/bin/" + n, nil }, ReadFileFn: func(string) (string, error) { return "", nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "op" {
			opCalled = true
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
			return "anthropic\ngithub\n", nil
		}
		return "", nil
	}}}
	var out bytes.Buffer
	if !bootstrapProviderKeys(env, strings.NewReader(""), &out, false) {
		t.Fatal("present key should bootstrap true")
	}
	if opCalled {
		t.Error("must not touch op when a key is already present")
	}
}

// bootstrap with no key and no TTY returns false (can't provision unattended).
func TestBootstrapProviderKeys_MissingNoTTY(t *testing.T) {
	env := shellEnv{System: &systest.Fake{LookPathFn: func(n string) (string, error) { return "/usr/bin/" + n, nil }, ReadFileFn: func(string) (string, error) { return "", nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
			return "github\n", nil // no model key
		}
		return "", nil
	}}}
	var out bytes.Buffer
	if bootstrapProviderKeys(env, strings.NewReader("y\n"), &out, false) {
		t.Error("no key + no TTY must return false")
	}
}

// --- item 6: sbx-absent (portability) vs sbx-error (diagnostic) -----------

// sbx not on PATH at all is genuine portability: probeSbxSecrets/
// sbxAllModelKeysPresent must classify it sbxSecretsAbsent, distinct from a
// present-but-erroring sbx.
func TestProbeSbxSecrets_Absent(t *testing.T) {
	env := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }}}
	if _, state := probeSbxSecrets(env); state != sbxSecretsAbsent {
		t.Errorf("sbx not on PATH must classify sbxSecretsAbsent, got %v", state)
	}
}

// sbx on PATH but `sbx secret ls` itself fails is a REAL, diagnosable problem
// — must classify sbxSecretsError, never conflated with "absent".
func TestProbeSbxSecrets_CommandFails(t *testing.T) {
	env := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("control plane down") }}}
	if _, state := probeSbxSecrets(env); state != sbxSecretsError {
		t.Errorf("a failing `sbx secret ls` must classify sbxSecretsError, got %v", state)
	}
}

func TestSbxAllModelKeysPresent_DistinguishesAbsentFromError(t *testing.T) {
	absent := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }}}
	if all, state := sbxAllModelKeysPresent(absent); all || state != sbxSecretsAbsent {
		t.Errorf("absent sbx: got all=%v state=%v, want false,sbxSecretsAbsent", all, state)
	}

	errored := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("boom") }}}
	if all, state := sbxAllModelKeysPresent(errored); all || state != sbxSecretsError {
		t.Errorf("erroring sbx: got all=%v state=%v, want false,sbxSecretsError", all, state)
	}

	complete := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "anthropic\nopenai\ngoogle\n", nil }}}
	if all, state := sbxAllModelKeysPresent(complete); !all || state != sbxSecretsOK {
		t.Errorf("complete: got all=%v state=%v, want true,sbxSecretsOK", all, state)
	}
}
