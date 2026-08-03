// sandboxcontext_test.go — moved from cmd/pix: the subject is
// resolveMCPSandboxContext and mcpGroupWith, both doctor internals.
package doctor

import (
	"strings"
	"testing"

	"pix/host/readiness"
	"pix/host/sys/systest"
	"pix/host/workspace"
)

func TestDoctorContextResolvesCustomSandboxName(t *testing.T) {
	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)
	stateDir := t.TempDir()
	f := mcpFake()
	f.output["sbx ls"] = "pix-demo  running  " + canon + "\n"
	env := f.env()
	systest.Of(env.System).GetwdFn = func() (string, error) { return ws, nil }
	systest.Of(env.System).StateDirFn = func() (string, error) { return stateDir, nil }
	mustCreateReceipt(t, stateDir, "pix-demo", canon, []string{"slack"})

	ctx := resolveMCPSandboxContext(env)
	if ctx.mode != mcpAttachReceipt || ctx.sandbox != "pix-demo" {
		t.Fatalf("ctx = %+v, want receipt mode on pix-demo (the mapped custom name)", ctx)
	}
	if ctx.ws != canon {
		t.Fatalf("ctx.ws = %q, want %q", ctx.ws, canon)
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, ctx)
	c := findCheck(t, g, "slack attachment")
	if c.Result() != readiness.VerdictReady || !strings.Contains(c.Detail, "pix-demo") {
		t.Fatalf("attachment = %+v, want ready on the custom-named sandbox", c)
	}
}

func TestDoctorContextAmbiguousMappingIsUnverifiable(t *testing.T) {
	ws := t.TempDir()
	canon := workspace.CanonicalPath(ws)
	stateDir := t.TempDir()
	f := mcpFake()
	f.output["sbx ls"] = "pix-a  running  " + canon + "\n"
	env := f.env()
	systest.Of(env.System).GetwdFn = func() (string, error) { return ws, nil }
	systest.Of(env.System).StateDirFn = func() (string, error) { return stateDir, nil }
	mustCreateReceipt(t, stateDir, "pix-a", canon, nil)
	mustCreateReceipt(t, stateDir, "pix-b", canon, nil)

	ctx := resolveMCPSandboxContext(env)
	if ctx.mode != mcpAttachNone || ctx.note == "" {
		t.Fatalf("ctx = %+v, want no-attachment mode with an explanatory note", ctx)
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, ctx)
	if hasCheck(g, "slack attachment") {
		t.Fatalf("an ambiguous mapping must never emit a per-sandbox attachment check: %+v", g)
	}
	note := findCheck(t, g, "attachment")
	if note.Result() != readiness.VerdictUnverifiable || !strings.Contains(note.Detail, "mapping unresolvable") {
		t.Fatalf("attachment note = %+v, want unverifiable naming the mapping problem", note)
	}
}

// mustCreateReceipt writes a full create receipt for sandbox with the given
// canonical workspace, via the production writer.
func mustCreateReceipt(t *testing.T, stateDir, sandbox, ws string, preloaded []string) {
	t.Helper()
	if err := workspace.WriteCreateReceipt(stateDir, sandbox, ws, preloaded, receiptClock); err != nil {
		t.Fatal(err)
	}
}
