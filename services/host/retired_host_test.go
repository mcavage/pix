// retired_host_test.go — the host binary's half of the retirement contract.
// pix-host is not user-facing, but scripts, launchd plists, and `sbx mcp add`
// registrations DO name its subcommands, so a retired one must answer the same
// way the launcher's does: PIX_RETIRED, exit 2, the exact replacement.
package main

import (
	"strings"
	"testing"
)

func TestRetiredHostSubcommands_MessageContract(t *testing.T) {
	if len(retiredHostSubcommands()) == 0 {
		t.Fatal("retiredHostSubcommands is empty; W1 U01a retires the slack alias")
	}
	for name, replacement := range retiredHostSubcommands() {
		msg := retiredHostMessage(name)
		if !strings.HasPrefix(msg, "PIX_RETIRED") {
			t.Errorf("%q: message does not start with PIX_RETIRED:\n%s", name, msg)
		}
		if !strings.Contains(msg, "pix-host "+name) {
			t.Errorf("%q: message does not name the retired subcommand:\n%s", name, msg)
		}
		if !strings.Contains(msg, replacement) {
			t.Errorf("%q: message does not name the replacement %q:\n%s", name, replacement, msg)
		}
		if !strings.HasSuffix(msg, "\n") {
			t.Errorf("%q: message is not newline-terminated", name)
		}
	}
}

// TestRetiredHostSubcommands_AreGoneFromUsage: the usage text is the discovery
// path for the host binary, so a retired subcommand must not be listed there.
func TestRetiredHostSubcommands_AreGoneFromUsage(t *testing.T) {
	text := usageText()
	for name := range retiredHostSubcommands() {
		// A multi-word retirement (`serve knowledge`) may not be advertised as that
		// phrase; its live parent verb (`serve`) still must be.
		if strings.Contains(text, name) {
			t.Errorf("pix-host usage still advertises retired subcommand %q", name)
		}
		if words := strings.Fields(name); len(words) == 1 {
			for _, line := range strings.Split(text, "\n") {
				if t2 := strings.TrimSpace(line); t2 == words[0] || strings.HasPrefix(t2, words[0]+" ") {
					t.Errorf("pix-host usage still lists retired subcommand %q:\n  %s", name, line)
				}
			}
		}
	}
}

// TestServeDropsRetiredServices: `serve` may no longer compose a retired
// capability slot — the alias table it validates against is the whole surface,
// so a retired service name must not be in it.
func TestServeDropsRetiredServices(t *testing.T) {
	for _, name := range []string{"knowledge"} {
		if serveServiceAliases()[name] != "" {
			t.Errorf("serve still composes retired service %q", name)
		}
	}
	if serveServiceAliases()["memory"] == "" {
		t.Error("serve no longer composes memory; the de-composition took a live service with it")
	}
}
