package env

// effective_l1_test.go — L1 (security re-review): the pix-memory bearer
// token must never print in `pix env --effective` output. These tests
// drive RenderEffectiveDocument (env_cmd.go's --effective's ONE call
// site) with a REAL, canary-valued token file on disk, then scan the
// captured output for that canary — the same technique a reviewer would
// use against a real leak, rather than asserting on the redaction code's
// own internals only.

import (
	"os"
	"strings"
	"testing"

	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/stack"
)

// canaryToken is a value that could never occur by accident (unlike a
// real 64-hex-digit token, this is instantly recognizable in a failure
// diff or a captured log), planted as PIX_HOME's real, on-disk pix-memory
// bearer token for every test below.
const canaryToken = "CANARY-TOKEN-DO-NOT-LEAK-93f7a2"

// writeCanaryToken plants canaryToken at the exact path
// container.ReadMemoryAuthToken reads, so RenderEffectiveDocument resolves
// a REAL (if fake-valued) token exactly the way a post-`pix setup` host
// would.
func writeCanaryToken(t *testing.T, home pixhome.Paths) {
	t.Helper()
	if err := os.MkdirAll(home.StateMemory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := container.MemoryAuthTokenPath(home)
	if err := os.WriteFile(path, []byte(canaryToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRenderEffectiveDocument_NeverPrintsTheRealMemoryToken is L1's own
// proof for D17 `none` (no environment selected): the built-in pix-memory
// MCP declaration still renders (a real launch would add the exact same
// one), but its token query parameter is redacted.
func TestRenderEffectiveDocument_NeverPrintsTheRealMemoryToken(t *testing.T) {
	home := pixhome.New(t.TempDir())
	writeCanaryToken(t, home)

	doc, err := RenderEffectiveDocument(home, "")
	if err != nil {
		t.Fatalf("RenderEffectiveDocument: %v", err)
	}
	got := string(doc)
	if strings.Contains(got, canaryToken) {
		t.Fatalf("--effective output leaked the real memory token:\n%s", got)
	}
	if !strings.Contains(got, "token="+container.RedactedTokenPlaceholder) {
		t.Fatalf("--effective output did not show the redacted token marker; got:\n%s", got)
	}
	if !strings.Contains(got, "pix-memory") {
		t.Fatalf("--effective output dropped the pix-memory server entirely rather than redacting it; got:\n%s", got)
	}
}

// TestRenderEffectiveDocument_NeverPrintsTheRealMemoryToken_WithSelectedEnvironment
// is the same canary proof against a NAMED, selected environment (the
// other resolution branch ComputeEffective takes), so the redaction is
// proven on both paths through the same function, not just D17 `none`.
func TestRenderEffectiveDocument_NeverPrintsTheRealMemoryToken_WithSelectedEnvironment(t *testing.T) {
	home := pixhome.New(t.TempDir())
	writeCanaryToken(t, home)
	root := home.EnvironmentDir("work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)

	doc, err := RenderEffectiveDocument(home, "work")
	if err != nil {
		t.Fatalf("RenderEffectiveDocument: %v", err)
	}
	got := string(doc)
	if strings.Contains(got, canaryToken) {
		t.Fatalf("--effective output leaked the real memory token for a selected environment:\n%s", got)
	}
	if !strings.Contains(got, "token="+container.RedactedTokenPlaceholder) {
		t.Fatalf("--effective output did not show the redacted token marker; got:\n%s", got)
	}
}

// TestComputeEffective_InternalFactsStillCarryTheRealToken proves the OTHER
// half of the contract: redaction is presentation-only, applied by
// RenderEffectiveDocument alone, never by ComputeEffective itself — a
// caller that needs the real, canonical fact (none exists today besides
// RenderEffectiveDocument, but the split is the point) is not handed an
// already-redacted value it could mistake for the real credential.
func TestComputeEffective_InternalFactsStillCarryTheRealToken(t *testing.T) {
	home := pixhome.New(t.TempDir())
	writeCanaryToken(t, home)

	facts, err := ComputeEffective(home, "")
	if err != nil {
		t.Fatalf("ComputeEffective: %v", err)
	}
	id, err := stack.ID(home.Home)
	if err != nil {
		t.Fatalf("stack.ID: %v", err)
	}
	wantName, err := stack.MCPMemoryName(id)
	if err != nil {
		t.Fatalf("stack.MCPMemoryName: %v", err)
	}
	found := false
	for _, s := range facts.MCPServers {
		if s.Name == wantName {
			found = true
			if !strings.Contains(s.URL, canaryToken) {
				t.Fatalf("ComputeEffective's own facts must carry the REAL token internally; got URL %q", s.URL)
			}
		}
	}
	if !found {
		t.Fatal("ComputeEffective did not compose the pix-memory built-in at all")
	}
}

func TestRedactMemoryURLToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no token param", "http://127.0.0.1:1/mcp", "http://127.0.0.1:1/mcp"},
		{"bare token param", "http://127.0.0.1:1/mcp?token=" + canaryToken, "http://127.0.0.1:1/mcp?token=" + container.RedactedTokenPlaceholder},
		{"token followed by another param", "http://x/mcp?token=" + canaryToken + "&x=1", "http://x/mcp?token=" + container.RedactedTokenPlaceholder + "&x=1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := container.RedactMemoryURLToken(c.in)
			if got != c.want {
				t.Errorf("RedactMemoryURLToken(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.Contains(got, canaryToken) {
				t.Errorf("RedactMemoryURLToken(%q) still contains the canary token: %q", c.in, got)
			}
		})
	}
}
