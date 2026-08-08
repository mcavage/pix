package corpus

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
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

// ExtractKnownVerbsFromSource is ExtractKnownVerbs' no-build twin: it reads
// cmd/pix/root.go's `type rootCmd struct` directly via go/ast and returns the
// verb name (kong's lowercase-of-the-field-name default) for every field
// carrying a `cmd:""` tag. root.go's own doc comment states this struct IS
// the single source of truth `help --all`'s generated listing renders from
// ("the tree is the single source of truth for all three"), so reading the
// tags directly reaches the identical answer ExtractKnownVerbs gets by
// building and exec'ing the real binary — without the build. corpus is a
// test-only package (U11k) that deliberately does not, and as `package main`
// cannot, import cmd/pix, so source parsing (not a direct import) is the only
// no-exec route available; TestExtractKnownVerbsFromSource_MatchesTheRealBinary
// keeps the two mechanisms from silently diverging.
func ExtractKnownVerbsFromSource(rootGoPath string) (map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rootGoPath, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("corpus: parse %s: %w", rootGoPath, err)
	}
	var fields *ast.FieldList
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "rootCmd" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		fields = st.Fields
		return false
	})
	if fields == nil {
		return nil, fmt.Errorf("corpus: %s has no `type rootCmd struct` — has the verb table moved?", rootGoPath)
	}
	out := map[string]bool{}
	for _, field := range fields.List {
		if field.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
		if _, ok := tag.Lookup("cmd"); !ok {
			continue
		}
		for _, name := range field.Names {
			out[strings.ToLower(name.Name)] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("corpus: rootCmd in %s has no `cmd:\"\"` fields — has the tag shape changed?", rootGoPath)
	}
	return out, nil
}

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

// TestCoverage_EveryKnownVerbHasShardOrRetirement used to skip under
// testing.Short() because it built and exec'd the real pix binary just to
// read off the verb list — which meant it never ran in the fast gate
// (`go test -short ./...`) at all, only in the untimed race/metrics CI jobs.
// It now reads root.go's rootCmd struct directly (ExtractKnownVerbsFromSource,
// no build, no exec), so the deletion guard runs on every fast-gate pass;
// TestExtractKnownVerbsFromSource_MatchesTheRealBinary is the (still Short-
// skipped, still binary-building) proof that source parsing and the real
// dispatcher agree.
func TestCoverage_EveryKnownVerbHasShardOrRetirement(t *testing.T) {
	verbs, err := ExtractKnownVerbsFromSource(realRootGoPath(t))
	if err != nil {
		t.Fatalf("ExtractKnownVerbsFromSource: %v", err)
	}
	if len(verbs) < 10 {
		t.Fatalf("ExtractKnownVerbsFromSource found %d verbs; rootCmd stopped declaring them", len(verbs))
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
		t.Errorf("verbs dispatched by the kong root but neither corpus-covered nor retired: %v\n"+
			"Add a shard (corpus/shards/<verb>.json) or an approved retirement entry (corpus/retirement.jsonl).", missing)
	}
}

// TestExtractKnownVerbsFromSource_MatchesTheRealBinary is what keeps the fast,
// no-build source parser honest: it builds the real binary (genuinely slow,
// hence Short-skipped, same as the coverage test used to be) exactly once and
// asserts ExtractKnownVerbsFromSource reports the IDENTICAL verb set
// ExtractKnownVerbs gets from `pix help --all`. If root.go's struct tags and
// the generated listing ever disagree, this is what notices — the fast
// deletion guard above is only as trustworthy as this equivalence holding.
func TestExtractKnownVerbsFromSource_MatchesTheRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix binary; covered by the untimed race/metrics CI jobs, same as the coverage test this replaces used to be")
	}
	bin := buildPixBinary(t)
	fromBinary, err := ExtractKnownVerbs(bin)
	if err != nil {
		t.Fatalf("ExtractKnownVerbs: %v", err)
	}
	fromSource, err := ExtractKnownVerbsFromSource(realRootGoPath(t))
	if err != nil {
		t.Fatalf("ExtractKnownVerbsFromSource: %v", err)
	}
	var onlyBinary, onlySource []string
	for v := range fromBinary {
		if !fromSource[v] {
			onlyBinary = append(onlyBinary, v)
		}
	}
	for v := range fromSource {
		if !fromBinary[v] {
			onlySource = append(onlySource, v)
		}
	}
	sort.Strings(onlyBinary)
	sort.Strings(onlySource)
	if len(onlyBinary) > 0 || len(onlySource) > 0 {
		t.Errorf("ExtractKnownVerbsFromSource disagrees with the real `pix help --all` binary: only-in-binary=%v only-in-source=%v", onlyBinary, onlySource)
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
