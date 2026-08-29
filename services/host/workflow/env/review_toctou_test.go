package env

// review_toctou_test.go — Wave C security H1: a PARSE-VALID mutation of the
// host-execution footprint landed while the review prompt was open must
// refuse at commit, exit 2, with the family's three-part copy naming
// exactly one runnable `pix env review NAME`, and must leave the trust
// store BYTE-identical (fresh acceptance is never persisted for a bill
// nobody actually read). review_test.go's
// TestReview_MutationDuringPromptFailsClosedAtCommit already covers the
// parse-INVALID class (a symlink swap the reload itself refuses); this file
// covers the class the reload alone cannot catch: the mutated document
// still loads cleanly, so only comparing the freshly recomputed fingerprint
// against the RENDERED bill's fingerprint can prove the consent still
// applies.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// toctouBaseSbxenv is a parse-valid Tier1 document exercising every
// disk-reachable fingerprint facet a prompt-window mutation can touch: a
// host command (name + argv), a credential ref, a binding domain, and an
// authored `${VAR:-default}` interpolation expression. EffectiveMounts are
// caller-typed (never read from disk), so a "mount change during the
// prompt" cannot originate on disk today; the fingerprint compare still
// covers mounts by construction because the WHOLE BillOfMaterials is
// hashed.
const toctouBaseSbxenv = `schemaVersion: "1"
agent: pix

mcp:
  servers:
    - name: warehouse-mcp
      command: warehouse-mcp-server
      args:
        - --stdio
        - ${WAREHOUSE_REGION:-us}

secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key

bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
`

func TestReview_ParseValidMutationDuringPromptRefusesAndStoreBytesUnchanged(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"host command name/binary swap", "command: warehouse-mcp-server", "command: warehouse-mcp-server-taken"},
		{"host command argv swap", "- --stdio", "- --exfiltrate"},
		{"credential ref swap", "op://Personal/Anthropic/api-key", "op://Personal/Anthropic/admin-token"},
		{"binding domain swap", "api.anthropic.com", "attacker.example.com"},
		{"interpolation expression swap", "${WAREHOUSE_REGION:-us}", "${WAREHOUSE_REGION:-eu}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempConfigAndState(t)
			cfg := loadConfig(t)

			// Seed the trust store with an UNRELATED accepted environment so
			// "store bytes unchanged" is proven against a real, populated
			// document rather than an absent file.
			seedRoot := t.TempDir()
			copyFixture(t, "testdata/hostexec-fixture", seedRoot)
			if _, err := Register(cfg, "seed", seedRoot); err != nil {
				t.Fatal(err)
			}
			if _, err := Review(cfg, "seed", prdMounts(), noBareLookPath, ReviewOptions{Out: &bytes.Buffer{}, Yes: true}); err != nil {
				t.Fatalf("seeding review: %v", err)
			}
			storePath := environmentTrustStorePath()
			before, err := os.ReadFile(storePath)
			if err != nil {
				t.Fatal(err)
			}

			root := t.TempDir()
			writeEnvFile(t, root, ".sbxenv.yaml", toctouBaseSbxenv)
			if _, err := Register(cfg, "work", root); err != nil {
				t.Fatal(err)
			}

			mutated := false
			in := &mutateOnFirstRead{
				mutate: func() {
					mutated = true
					p := filepath.Join(root, ".sbxenv.yaml")
					data, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					rewritten := strings.Replace(string(data), tc.old, tc.new, 1)
					if rewritten == string(data) {
						t.Fatalf("test setup error: %q not found in the base document", tc.old)
					}
					if err := os.WriteFile(p, []byte(rewritten), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				r: strings.NewReader("y\n"),
			}

			var out bytes.Buffer
			res, err := Review(cfg, "work", nil, noBareLookPath, ReviewOptions{Out: &out, TTY: true, In: in})
			if !mutated {
				t.Fatal("test setup error: the mutating reader was never invoked")
			}
			if err == nil {
				t.Fatal("Review must refuse when a parse-valid footprint mutation lands during the prompt")
			}
			if got := cli.ExitCode(err); got != 2 {
				t.Errorf("cli.ExitCode(err) = %d, want 2", got)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil", res)
			}

			// Three-part copy, exactly one runnable `pix env review work`.
			msg := err.Error()
			if got := strings.Count(msg, "pix env review work"); got != 1 {
				t.Errorf("refusal must name `pix env review work` exactly once, got %d in:\n%s", got, msg)
			}
			if got := len(strings.Split(strings.TrimRight(msg, "\n"), "\n")); got != 3 {
				t.Errorf("refusal must be the family's three-part form (3 lines), got %d:\n%s", got, msg)
			}

			// The store is BYTE-identical: neither the stale-rendered nor the
			// fresh fingerprint was persisted for the mutated surface.
			after, err := os.ReadFile(storePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("trust store bytes changed across a refused commit:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			ts, err := loadEnvironmentTrustStore()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := ts.Get(Subject(root)); ok {
				t.Error("a refused commit must record nothing for the mutated root")
			}
		})
	}
}

// TestReview_ParseValidMutationDuringPrompt_AbsentStoreStaysAbsent proves the
// refusal writes nothing even when no trust store file exists at all: the
// fail-closed path must not create the document as a side effect.
func TestReview_ParseValidMutationDuringPrompt_AbsentStoreStaysAbsent(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", toctouBaseSbxenv)
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}

	in := &mutateOnFirstRead{
		mutate: func() {
			p := filepath.Join(root, ".sbxenv.yaml")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(strings.Replace(string(data), "- --stdio", "- --exfiltrate", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		r: strings.NewReader("y\n"),
	}
	_, err := Review(cfg, "work", nil, noBareLookPath, ReviewOptions{Out: &bytes.Buffer{}, TTY: true, In: in})
	if err == nil {
		t.Fatal("Review must refuse the prompt-window mutation")
	}
	if _, statErr := os.Stat(environmentTrustStorePath()); !os.IsNotExist(statErr) {
		t.Errorf("a refused commit must not create the trust store, stat err = %v", statErr)
	}
}
