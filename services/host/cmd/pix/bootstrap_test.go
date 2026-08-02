package main

import (
	"bytes"
	"fmt"
	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// bootstrap with a key already present is a no-op that returns true and never
// touches op (no prompt).
func TestBootstrapProviderKeys_PresentNoOp(t *testing.T) {
	var opCalled bool
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(n string) (string, error) { return "/usr/bin/" + n, nil }, ReadFileFn: func(string) (string, error) { return "", nil }, RunFn: func(name string, args ...string) (string, error) {
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
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(n string) (string, error) { return "/usr/bin/" + n, nil }, ReadFileFn: func(string) (string, error) { return "", nil }, RunFn: func(name string, args ...string) (string, error) {
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

// sbx not on PATH at all is genuine portability: secret.ProbeSbxSecrets/
// secret.SbxAllModelKeysPresent must classify it secret.SbxSecretsAbsent, distinct from a
// present-but-erroring sbx.
func TestProbeSbxSecrets_Absent(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }}}
	if _, state := secret.ProbeSbxSecrets(env); state != secret.SbxSecretsAbsent {
		t.Errorf("sbx not on PATH must classify secret.SbxSecretsAbsent, got %v", state)
	}
}

// sbx on PATH but `sbx secret ls` itself fails is a REAL, diagnosable problem
// — must classify secret.SbxSecretsError, never conflated with "absent".
func TestProbeSbxSecrets_CommandFails(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("control plane down") }}}
	if _, state := secret.ProbeSbxSecrets(env); state != secret.SbxSecretsError {
		t.Errorf("a failing `sbx secret ls` must classify secret.SbxSecretsError, got %v", state)
	}
}

func TestSbxAllModelKeysPresent_DistinguishesAbsentFromError(t *testing.T) {
	absent := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }}}
	if all, state := secret.SbxAllModelKeysPresent(absent); all || state != secret.SbxSecretsAbsent {
		t.Errorf("absent sbx: got all=%v state=%v, want false,secret.SbxSecretsAbsent", all, state)
	}

	errored := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("boom") }}}
	if all, state := secret.SbxAllModelKeysPresent(errored); all || state != secret.SbxSecretsError {
		t.Errorf("erroring sbx: got all=%v state=%v, want false,secret.SbxSecretsError", all, state)
	}

	complete := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "anthropic\nopenai\ngoogle\n", nil }}}
	if all, state := secret.SbxAllModelKeysPresent(complete); !all || state != secret.SbxSecretsOK {
		t.Errorf("complete: got all=%v state=%v, want true,secret.SbxSecretsOK", all, state)
	}
}
