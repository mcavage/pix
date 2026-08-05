package corpus

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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

// ExtractKnownVerbs returns the launcher's live top-level verb set by reading
// the KONG ROOT (root.go's `type rootCmd struct`): every field tagged `cmd:""`
// is a verb. It parses the root because the root is the only dispatcher — a
// list beside it can only be a second, stale answer to "what does pix
// accept?" (this guard used to scan a knownVerbs map the switch could out-run).
// A missing struct is an error, the same "warn, don't silently pass" contract
// the scripts/check-*.sh guards use for a moved anchor.
func ExtractKnownVerbs(rootGoPath string) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), rootGoPath, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("corpus: parse %s: %w", rootGoPath, err)
	}
	fields := rootStructFields(file)
	if fields == nil {
		return nil, fmt.Errorf("corpus: could not find `type rootCmd struct` in %s (did the root move?)", rootGoPath)
	}
	out := map[string]bool{}
	for _, f := range fields {
		if f.Tag == nil || len(f.Names) == 0 {
			continue
		}
		tag, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			continue
		}
		st := reflect.StructTag(tag)
		if _, isCmd := st.Lookup("cmd"); !isCmd {
			continue
		}
		// Canonical names only: an alias (`st`, `mem`) is covered by its verb's
		// shard, and a shard per alias would prove nothing extra.
		out[kongVerbName(f.Names[0].Name)] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("corpus: rootCmd in %s has no `cmd:\"\"` fields", rootGoPath)
	}
	return out, nil
}

// rootStructFields returns the fields of `type rootCmd struct`, or nil.
func rootStructFields(file *ast.File) []*ast.Field {
	var fields []*ast.Field
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name == nil || ts.Name.Name != "rootCmd" {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
			fields = st.Fields.List
		}
		return false
	})
	return fields
}

// kongVerbName mirrors kong's default namer: a field name becomes its
// lower-kebab spelling (Ls -> ls, KitRef -> kit-ref).
func kongVerbName(field string) string {
	var b strings.Builder
	for i, r := range field {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
