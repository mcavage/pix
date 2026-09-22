package env

import (
	"strings"
	"testing"

	"pix/host/envinfo"
	"pix/host/pixhome"
)

// TestBuiltinMCPFactsUsesScopedNames proves the ONE built-in producer both
// `pix env --effective` and a real launch call resolves THIS PIX_HOME's own
// scoped names, never a bare legacy fallback, and that two different
// PIX_HOMEs diverge exactly the way their container names do.
func TestBuiltinMCPFactsUsesScopedNames(t *testing.T) {
	homeA := pixhome.New(t.TempDir())
	factsA := BuiltinMCPFacts(homeA, "/workspace", "pix-sandbox", false)
	if factsA.MemoryName == "" || factsA.SessionName == "" {
		t.Fatalf("expected resolved scoped names, got %+v", factsA)
	}
	if factsA.MemoryName == "pix-memory" || factsA.SessionName == "pix-session" {
		t.Fatalf("BuiltinMCPFacts must never fall back to the bare legacy names, got %+v", factsA)
	}

	homeB := pixhome.New(t.TempDir())
	factsB := BuiltinMCPFacts(homeB, "/workspace", "pix-sandbox", false)
	if factsA.MemoryName == factsB.MemoryName || factsA.SessionName == factsB.SessionName {
		t.Fatalf("two different PIX_HOME roots must diverge, both got %+v", factsA)
	}
}

// TestBuiltinMCPFactsSessionArgvIsHostTools pins that the session built-in's
// argv is envinfo.WithHostTools' host-tools context — the one subcommand
// cmd/pix's hidden dispatch actually serves for a Gateway-launched server —
// with dev authority reaching the argv only when the launch asked for it.
func TestBuiltinMCPFactsSessionArgvIsHostTools(t *testing.T) {
	home := pixhome.New(t.TempDir())
	normal := BuiltinMCPFacts(home, "/workspace", "pix-sandbox", false)
	if normal.SessionCommand == "" {
		t.Skip("running executable not resolvable in this environment")
	}
	if len(normal.SessionArgs) == 0 || normal.SessionArgs[0] != envinfo.HostToolsSubcommand {
		t.Fatalf("session argv must start with %q, got %q", envinfo.HostToolsSubcommand, normal.SessionArgs)
	}
	joined := strings.Join(normal.SessionArgs, " ")
	for _, want := range []string{"--home " + home.Home, "--workspace /workspace", "--sandbox pix-sandbox"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("session argv %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--dev") {
		t.Fatalf("a non-dev launch must not grant host command authority: %q", joined)
	}
	dev := BuiltinMCPFacts(home, "/workspace", "pix-sandbox", true)
	if !strings.Contains(strings.Join(dev.SessionArgs, " "), "--dev") {
		t.Fatalf("a --dev launch must carry --dev in the session argv: %q", dev.SessionArgs)
	}

	t.Setenv("PIX_HOST_TOOLS_DISABLED", "1")
	disabled := BuiltinMCPFacts(home, "/workspace", "pix-sandbox", true)
	if got := envinfo.BuiltinMCPServers(disabled); len(got) != 1 || !envinfo.IsMemoryMCPName(got[0].Name) {
		t.Fatalf("PIX_HOST_TOOLS_DISABLED must leave only pix-memory, got %+v", got)
	}
}
