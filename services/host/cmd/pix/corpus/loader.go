package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LoadShards reads every *.json file in dir as a Shard, validating each one.
// It returns shards sorted by verb for deterministic iteration.
func LoadShards(dir string) ([]Shard, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("corpus: read shards dir %s: %w", dir, err)
	}
	var shards []Shard
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("corpus: read %s: %w", path, err)
		}
		var s Shard
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("corpus: parse %s: %w", path, err)
		}
		wantVerb := strings.TrimSuffix(e.Name(), ".json")
		if s.Verb != wantVerb {
			return nil, fmt.Errorf("corpus: %s declares verb %q, want %q (filename must match verb)", path, s.Verb, wantVerb)
		}
		if err := ValidateShard(s); err != nil {
			return nil, fmt.Errorf("corpus: %s: %w", path, err)
		}
		shards = append(shards, s)
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].Verb < shards[j].Verb })
	return shards, nil
}

// ValidateShard checks a Shard's internal schema (independent of any file it
// came from): non-empty verb, at least one case, unique case names, and every
// case declares a non-empty argv.
func ValidateShard(s Shard) error {
	if strings.TrimSpace(s.Verb) == "" {
		return fmt.Errorf("shard has empty verb")
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("shard %q has zero cases (a corpus shard must prove at least one contract)", s.Verb)
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("shard %q has a case with an empty name", s.Verb)
		}
		if seen[c.Name] {
			return fmt.Errorf("shard %q has duplicate case name %q", s.Verb, c.Name)
		}
		seen[c.Name] = true
		if len(c.Args) == 0 {
			return fmt.Errorf("shard %q case %q has no args", s.Verb, c.Name)
		}
		switch c.Stream {
		case "", "stdout", "stderr":
		default:
			return fmt.Errorf("shard %q case %q has unknown stream %q (want stdout or stderr)", s.Verb, c.Name, c.Stream)
		}
	}
	return nil
}

// knownVerbsBlockRe extracts the body of help.go's `var knownVerbs = map[string]bool{ ... }`
// literal, and verbKeyRe pulls each quoted key out of that body. Scanning the
// real source (rather than hand-duplicating the list here) means this guard
// can never silently drift from the CLI's actual dispatch table.
var (
	knownVerbsBlockRe = regexp.MustCompile(`(?s)var knownVerbs = map\[string\]bool\{(.*?)\n\}`)
	verbKeyRe         = regexp.MustCompile(`"([a-zA-Z0-9_-]+)"\s*:\s*true`)
)

// ExtractKnownVerbs scans help.go's source text for the knownVerbs map and
// returns its keys. It errors if the map can't be found at all (the anchor
// moved), which is the same "warn, don't silently pass" contract the other
// scripts/check-*.sh guards use for a moved anchor.
func ExtractKnownVerbs(helpGoPath string) (map[string]bool, error) {
	b, err := os.ReadFile(helpGoPath)
	if err != nil {
		return nil, fmt.Errorf("corpus: read %s: %w", helpGoPath, err)
	}
	block := knownVerbsBlockRe.FindSubmatch(b)
	if block == nil {
		return nil, fmt.Errorf("corpus: could not find `var knownVerbs = map[string]bool{...}` in %s (did it move? update knownVerbsBlockRe)", helpGoPath)
	}
	matches := verbKeyRe.FindAllSubmatch(block[1], -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("corpus: knownVerbs block in %s matched but contained no \"verb\": true entries", helpGoPath)
	}
	out := make(map[string]bool, len(matches))
	for _, m := range matches {
		out[string(m[1])] = true
	}
	return out, nil
}
