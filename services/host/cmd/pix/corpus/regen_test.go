package corpus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ErrWholesaleRegeneration is returned by ValidateRegenTarget for any target
// that would rewrite more than one shard at once.
var ErrWholesaleRegeneration = errors.New("wholesale baseline regeneration is forbidden: pass exactly one verb")

// wholesaleTargets are the spellings a careless "regenerate everything" call
// might use; all of them are refused, not just the empty string.
var wholesaleTargets = map[string]bool{"": true, "all": true, "*": true, "-": true}

// ValidateRegenTarget enforces that a corpus-baseline regeneration names
// exactly one verb. The harness has no "regenerate all shards" mode at all —
// there is no argument that means that — so a baseline update is always a
// small, individually reviewable diff to one file, never a silent mass
// rewrite that could launder a real regression as "the new expected output".
func ValidateRegenTarget(target string) error {
	norm := strings.ToLower(strings.TrimSpace(target))
	if wholesaleTargets[norm] {
		return fmt.Errorf("%w (got %q)", ErrWholesaleRegeneration, target)
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w (empty target)", ErrWholesaleRegeneration)
	}
	return nil
}

// RegenerateShard writes exactly one shard file (dir/<verb>.json), after
// validating it. It never touches any other file in dir, and it refuses to
// write an empty shard — a golden baseline that proves nothing is worse than
// no baseline at all, because it would pass forever.
func RegenerateShard(dir string, s Shard) error {
	if err := ValidateRegenTarget(s.Verb); err != nil {
		return err
	}
	if err := ValidateShard(s); err != nil {
		return fmt.Errorf("corpus: refusing to write an invalid shard: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("corpus: marshal shard %q: %w", s.Verb, err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, s.Verb+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("corpus: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("corpus: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

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
