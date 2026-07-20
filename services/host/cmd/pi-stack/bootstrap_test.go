package main

import (
	"bytes"
	"strings"
	"testing"
)

// bootstrap with a key already present is a no-op that returns true and never
// touches op (no prompt).
func TestBootstrapProviderKeys_PresentNoOp(t *testing.T) {
	var opCalled bool
	env := shellEnv{
		lookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil },
		readFile: func(string) (string, error) { return "", nil }, // no refs
		run: func(name string, args ...string) (string, error) {
			if name == "op" {
				opCalled = true
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
				return "anthropic\ngithub\n", nil
			}
			return "", nil
		},
	}
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
	env := shellEnv{
		lookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil },
		readFile: func(string) (string, error) { return "", nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
				return "github\n", nil // no model key
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	if bootstrapProviderKeys(env, strings.NewReader("y\n"), &out, false) {
		t.Error("no key + no TTY must return false")
	}
}
