package corpus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRegenTarget_ForbidsWholesale(t *testing.T) {
	forbidden := []string{"", "  ", "all", "ALL", "All", "*", "-"}
	for _, target := range forbidden {
		if err := ValidateRegenTarget(target); err == nil {
			t.Errorf("ValidateRegenTarget(%q) = nil, want error (wholesale regeneration must be forbidden)", target)
		}
	}
	allowed := []string{"config", "agent", "models"}
	for _, target := range allowed {
		if err := ValidateRegenTarget(target); err != nil {
			t.Errorf("ValidateRegenTarget(%q) = %v, want nil", target, err)
		}
	}
}

func TestRegenerateShard_OnlyTouchesTarget(t *testing.T) {
	dir := t.TempDir()
	seed := func(verb string) {
		s := Shard{Verb: verb, Cases: []Case{{Name: "help", Args: []string{verb, "--help"}, ExitCode: 0}}}
		if err := RegenerateShard(dir, s); err != nil {
			t.Fatalf("seed RegenerateShard(%s): %v", verb, err)
		}
	}
	seed("config")
	seed("agent")
	seed("models")

	before := map[string][]byte{}
	for _, v := range []string{"agent", "models"} {
		b, err := os.ReadFile(filepath.Join(dir, v+".json"))
		if err != nil {
			t.Fatal(err)
		}
		before[v] = b
	}

	updated := Shard{Verb: "config", Cases: []Case{
		{Name: "help", Args: []string{"config", "--help"}, ExitCode: 0, Contains: []string{"usage: pix config"}},
	}}
	if err := RegenerateShard(dir, updated); err != nil {
		t.Fatalf("RegenerateShard(config): %v", err)
	}

	for _, v := range []string{"agent", "models"} {
		b, err := os.ReadFile(filepath.Join(dir, v+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != string(before[v]) {
			t.Errorf("RegenerateShard(config) also rewrote %s.json — wholesale regeneration must touch only the target shard", v)
		}
	}

	back, err := LoadShards(dir)
	if err != nil {
		t.Fatalf("LoadShards after regen: %v", err)
	}
	var found bool
	for _, s := range back {
		if s.Verb == "config" {
			found = true
			if len(s.Cases) != 1 || s.Cases[0].Contains[0] != "usage: pix config" {
				t.Errorf("regenerated config shard did not round-trip: %+v", s)
			}
		}
	}
	if !found {
		t.Error("regenerated config shard missing after reload")
	}
}

func TestRegenerateShard_RejectsEmptyCases(t *testing.T) {
	dir := t.TempDir()
	err := RegenerateShard(dir, Shard{Verb: "config", Cases: nil})
	if err == nil {
		t.Error("RegenerateShard(zero cases) = nil, want error — a golden shard must never regenerate empty")
	}
}

func TestRegenerateShard_VerbMustMatchNonEmpty(t *testing.T) {
	dir := t.TempDir()
	err := RegenerateShard(dir, Shard{Verb: "", Cases: []Case{{Name: "x", Args: []string{"x"}}}})
	if err == nil {
		t.Error("RegenerateShard(empty verb) = nil, want error")
	}
}
