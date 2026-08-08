package main

import (
	"errors"
	"pix/host/workflow/launch"
	"reflect"
	"strings"
	"testing"
)

func TestLocalKitArgs(t *testing.T) {
	args := []string{"run", "pix", "--kit", "/tmp/base", "--kit", "git+https://example.test/repo#dir=kit", "--kit", "./mixin", "."}
	want := []string{"/tmp/base", "./mixin"}
	if got := launch.LocalKitArgs(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("launch.LocalKitArgs = %v, want %v", got, want)
	}
}

func TestValidateCreateKitsFailsBeforeLaunchWithUpgrade(t *testing.T) {
	var seen []string
	err := launch.ValidateCreateKits([]string{"run", "pix", "--kit", "/tmp/pix/pi-kit", "."}, func(ref string) (string, error) {
		seen = append(seen, ref)
		return "INVALID: artifact: invalid spec.yaml\nraw decoder noise", errors.New("exit status 1")
	})
	if err == nil {
		t.Fatal("expected incompatible kit to fail")
	}
	if !reflect.DeepEqual(seen, []string{"/tmp/pix/pi-kit"}) {
		t.Fatalf("validated refs = %v", seen)
	}
	msg := err.Error()
	for _, want := range []string{"does not match the installed Docker Sandboxes schema", "INVALID: artifact: invalid spec.yaml", "update Pix and Docker Sandboxes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "raw decoder noise") {
		t.Errorf("error should keep the failure concise: %s", msg)
	}
}

func TestValidateCreateKitsSkipsRemoteRefs(t *testing.T) {
	called := false
	err := launch.ValidateCreateKits([]string{"run", "pix", "--kit", "git+https://example.test/repo#dir=kit", "."}, func(string) (string, error) {
		called = true
		return "", nil
	})
	if err != nil || called {
		t.Fatalf("remote kit should not be eagerly validated: called=%v err=%v", called, err)
	}
}

func TestValidateSetupKitChecksUnreleasedCheckout(t *testing.T) {
	var got string
	err := launch.ValidateSetupKit("0.1.14+local", func() (string, error) { return "/src/pix", nil }, func(ref string) (string, error) {
		got = ref
		return "", nil
	})
	if err != nil || got != "/src/pix/pi-kit" {
		t.Fatalf("launch.ValidateSetupKit = ref %q, err %v", got, err)
	}
}
