package corpus

import (
	"path/filepath"
	"testing"
)

// --- schema validation ------------------------------------------------------

func TestLoadShards_ValidatesRealShards(t *testing.T) {
	shards, err := LoadShards(realShardsDir(t))
	if err != nil {
		t.Fatalf("LoadShards(real dir) = %v, want nil", err)
	}
	if len(shards) == 0 {
		t.Fatal("LoadShards(real dir) returned zero shards")
	}
	for _, s := range shards {
		if s.Verb == "" {
			t.Errorf("shard with empty verb (file mismatch?)")
		}
		if len(s.Cases) == 0 {
			t.Errorf("shard %q has zero cases", s.Verb)
		}
	}
}

func TestLoadShards_VerbMatchesFilename(t *testing.T) {
	dir := realShardsDir(t)
	shards, err := LoadShards(dir)
	if err != nil {
		t.Fatalf("LoadShards: %v", err)
	}
	for _, s := range shards {
		want := filepath.Join(dir, s.Verb+".json")
		if _, err := statPath(want); err != nil {
			t.Errorf("shard verb %q does not correspond to a %s file", s.Verb, want)
		}
	}
}

func TestValidateShard_RejectsEmptyVerb(t *testing.T) {
	err := ValidateShard(Shard{Verb: "", Cases: []Case{{Name: "help", Args: []string{"x"}}}})
	if err == nil {
		t.Fatal("ValidateShard(empty verb) = nil, want error")
	}
}

func TestValidateShard_RejectsZeroCases(t *testing.T) {
	err := ValidateShard(Shard{Verb: "config", Cases: nil})
	if err == nil {
		t.Fatal("ValidateShard(zero cases) = nil, want error")
	}
}

func TestValidateShard_RejectsDuplicateCaseNames(t *testing.T) {
	s := Shard{Verb: "config", Cases: []Case{
		{Name: "help", Args: []string{"config", "--help"}},
		{Name: "help", Args: []string{"config", "-h"}},
	}}
	err := ValidateShard(s)
	if err == nil {
		t.Fatal("ValidateShard(duplicate case names) = nil, want error")
	}
}

func TestValidateShard_RejectsEmptyArgs(t *testing.T) {
	s := Shard{Verb: "config", Cases: []Case{{Name: "help"}}}
	if err := ValidateShard(s); err == nil {
		t.Fatal("ValidateShard(case with no args) = nil, want error")
	}
}

func TestValidateShard_RejectsBadJSONKeysWithoutStream(t *testing.T) {
	// A jsonKeys contract with no args is nonsensical to catch at schema time
	// (jsonKeys requires the case to actually produce output); a case is
	// otherwise valid without jsonKeys, so this only guards the field is a list.
	s := Shard{Verb: "config", Cases: []Case{{Name: "x", Args: []string{"config", "show"}, JSONKeys: []string{}}}}
	if err := ValidateShard(s); err != nil {
		t.Fatalf("ValidateShard(empty jsonKeys slice) = %v, want nil (nil and empty both mean 'no contract')", err)
	}
}

// --- deletion guard: every known verb is either covered or retired ---------

func TestCoverage_EveryKnownVerbHasShardOrRetirement(t *testing.T) {
	verbs, err := ExtractKnownVerbs(helpGoPath(t))
	if err != nil {
		t.Fatalf("ExtractKnownVerbs: %v", err)
	}
	if len(verbs) == 0 {
		t.Fatal("ExtractKnownVerbs found zero verbs; did help.go's knownVerbs map move?")
	}

	shards, err := LoadShards(realShardsDir(t))
	if err != nil {
		t.Fatalf("LoadShards: %v", err)
	}
	covered := map[string]bool{}
	for _, s := range shards {
		covered[s.Verb] = true
	}

	entries, err := LoadRetirement(realRetirementPath(t))
	if err != nil {
		t.Fatalf("LoadRetirement: %v", err)
	}
	retired := RetiredVerbs(entries)

	var missing []string
	for v := range verbs {
		if !covered[v] && !retired[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		t.Errorf("verbs present in help.go's knownVerbs but neither corpus-covered nor retired: %v\n"+
			"Add a shard (corpus/shards/<verb>.json) or an approved retirement entry (corpus/retirement.jsonl).", missing)
	}
}

func TestCoverage_RetiredVerbsAreNotAlsoShards(t *testing.T) {
	// A verb that is retired should not simultaneously carry a live corpus
	// shard: that would mean the verb still exists in the CLI (so it isn't
	// really retired) or the retirement entry is stale (so it should be
	// removed) — either way it is a signal worth catching, not silently
	// tolerating two contradictory sources of truth.
	shards, err := LoadShards(realShardsDir(t))
	if err != nil {
		t.Fatalf("LoadShards: %v", err)
	}
	entries, err := LoadRetirement(realRetirementPath(t))
	if err != nil {
		t.Fatalf("LoadRetirement: %v", err)
	}
	retired := RetiredVerbs(entries)
	for _, s := range shards {
		if retired[s.Verb] {
			t.Errorf("verb %q has both a live shard and an approved verb-level retirement entry", s.Verb)
		}
	}
}
