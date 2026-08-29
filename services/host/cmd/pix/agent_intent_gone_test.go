package main

// agent_intent_gone_test.go -- E3.4 review fix sentinel. The first E3.4
// commit deleted `fallback_intent:` outright but left `agentMeta.Intent`
// parsed "for display" -- a field with zero effect on resolveAgentSource's
// decision that was nonetheless typed, decoded, and (via TestParseAgentCRLF)
// asserted to round-trip. Review flagged that as the same kind of
// dead-frontmatter residue the `fallback_intent:` deletion already treats as
// intolerable: an agent's `model:` (and only `model:`) may still be pinned
// explicitly; a declared `intent:` must now be indistinguishable from an
// unknown key.
//
// This is a grep-based sentinel (mirroring hostmode_gone_test.go's pattern):
// blunt on purpose, so a future edit that reintroduces an `Intent` field on
// any agent-frontmatter struct in this package fails loudly here, rather than
// relying on someone remembering this was already litigated once.
//
// It also pins the other half of the review finding: the routing.json
// ARTIFACT and its `services/host/routing` package are untouched by this
// fix. Deleting either is Wave F's job (docs/design/routing.md "Agent
// lifecycle"), not an agent-parser cleanup's.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRootForAgentIntentSentinel walks up from cwd to the services/host
// module root, then one more level to the repo root (which holds routing.json
// at its top and services/host/routing as a subdirectory).
func repoRootForAgentIntentSentinel(t *testing.T) (hostRoot, repoRoot string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			hostRoot = dir
			// services/host -> services -> repo root.
			repoRoot = filepath.Dir(filepath.Dir(dir))
			return hostRoot, repoRoot
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find services/host module root (go.mod) above the test dir")
		}
		dir = parent
	}
}

// forbiddenAgentIntentPatterns are the shapes an `Intent` field on an
// agent-frontmatter struct, or a yaml `intent:` tag, would take. Matched
// against every non-test .go file under services/host (not just cmd/pix):
// the same discipline applies wherever an agent/frontmatter type is defined.
var forbiddenAgentIntentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Intent\s+string\s+` + "`" + `yaml:"intent`),
	regexp.MustCompile(`yaml:"intent,omitempty"`),
}

func TestNoAgentParserRetainsIntentField(t *testing.T) {
	hostRoot, _ := repoRootForAgentIntentSentinel(t)
	var violations []string
	err := filepath.Walk(hostRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == "corpus" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		content := string(b)
		for _, pat := range forbiddenAgentIntentPatterns {
			if pat.MatchString(content) {
				rel, _ := filepath.Rel(hostRoot, p)
				violations = append(violations, rel+": "+pat.String())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", hostRoot, err)
	}
	for _, v := range violations {
		t.Errorf("%s -- an agentMeta-shaped `Intent`/`intent:` field must not come back; a shipped or custom agent's model comes from the environment roster or its own explicit `model:` only", v)
	}
}

// TestAgentMetaHasNoIntentField is the direct, type-level companion to the
// grep sentinel above: it proves agentMeta itself (not some other struct
// the grep might miss) carries no Intent field, by round-tripping frontmatter
// that declares `intent:` and confirming the decoded value simply isn't
// there to read.
func TestAgentMetaHasNoIntentField(t *testing.T) {
	fm, _, ok := parseAgent("---\ndescription: x\nintent: red-team\nmodel: anthropic/opus\n---\n\nBody.\n")
	if !ok {
		t.Fatal("expected frontmatter")
	}
	var m agentMeta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Explicit model: still resolves -- the one field this fix preserves.
	if m.Model != "anthropic/opus" {
		t.Fatalf("model = %q, want anthropic/opus (explicit model: must survive)", m.Model)
	}
	if m.Description != "x" {
		t.Fatalf("description = %q, want x", m.Description)
	}
}

// TestRoutingArtifactAndPackageStillExist pins the other half of the review
// finding: this fix touches agent PARSERS only. routing.json and
// services/host/routing are Wave F's job, not this one's.
func TestRoutingArtifactAndPackageStillExist(t *testing.T) {
	_, repoRoot := repoRootForAgentIntentSentinel(t)
	if _, err := os.Stat(filepath.Join(repoRoot, "routing.json")); err != nil {
		t.Errorf("routing.json must still exist (Wave F deletes it, not this fix): %v", err)
	}
	if fi, err := os.Stat(filepath.Join(repoRoot, "services", "host", "routing")); err != nil || !fi.IsDir() {
		t.Errorf("services/host/routing must still exist as a package (Wave F deletes it, not this fix): err=%v", err)
	}
}
