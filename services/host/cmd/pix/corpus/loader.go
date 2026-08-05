package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

// helpAllVerbRe matches one verb row of the generated `pix help --all`
// listing: every verb sits two spaces under its tier heading, and headings and
// prose sit in column 0.
var helpAllVerbRe = regexp.MustCompile(`(?m)^ {2}([a-z][a-z0-9-]*) `)

// ExtractKnownVerbs returns the launcher's live top-level verb set by ASKING
// THE BINARY. `pix help --all` is generated from the kong root (root.go's
// `type rootCmd struct`), which is the only dispatcher, so its listing is the
// dispatcher's own answer to "what does pix accept?" — not a list beside it,
// and not a source scan a moved anchor can silently defeat. Zero verbs is an
// error, the same "warn, don't silently pass" contract the scripts/check-*.sh
// guards use.
func ExtractKnownVerbs(bin string) (map[string]bool, error) {
	listing, err := exec.Command(bin, "help", "--all").Output()
	if err != nil {
		return nil, fmt.Errorf("corpus: %s help --all: %w", bin, err)
	}
	rows := helpAllVerbRe.FindAllSubmatch(listing, -1)
	if len(rows) == 0 {
		return nil, fmt.Errorf("corpus: `%s help --all` listed no verbs (did the generated listing change shape?)", bin)
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[string(row[1])] = true
	}
	return out, nil
}
