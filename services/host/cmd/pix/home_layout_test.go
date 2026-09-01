package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/pixhome"
	"pix/host/workflow/launch"
)

func TestTrustDirLivesUnderHiddenState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	want := pixhome.New(home).StateTrust
	if got := trustDirOrEmpty(); got != want {
		t.Fatalf("trustDirOrEmpty = %q, want %q", got, want)
	}
	if filepath.Dir(want) != filepath.Join(home, ".state") {
		t.Fatalf("trust dir = %q, want it under hidden state", want)
	}
	if _, err := launch.CreateHMACResolver(want, nil); err != nil {
		t.Fatalf("CreateHMACResolver: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".state", "trust", "creation-hmac.key")); err != nil {
		t.Fatalf("creation key was not written under hidden state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "creation-hmac.key")); !os.IsNotExist(err) {
		t.Fatalf("creation key leaked into PIX_HOME root: %v", err)
	}
}
