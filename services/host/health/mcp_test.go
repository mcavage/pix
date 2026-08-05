package health

import (
	"testing"
	"time"
)

// mcp_test.go pins the property the MCP diagnosis exists for: each of the
// three truths is answered from its own source, and a source that did not
// ANSWER produces unknown, never a verdict.
//
// Every case here execs the REAL fixture executable through the real
// runBounded path (testdata/fixture, built by buildFixture). Nothing stubs
// the probe: the "sbx fell over" and "sbx is not installed" cases are
// produced by a process that actually falls over or a path that actually
// does not exist, so the classification under test is production's.

// mcpProbe builds a probe pointed at the fixture, in the listing mode given.
func mcpProbe(t *testing.T, listMode string, servers ...MCPServer) MCPProbe {
	t.Helper()
	return MCPProbe{Bin: buildFixture(t), ListArgs: []string{listMode}, Servers: servers}
}

func local(name string) MCPServer {
	return MCPServer{Name: name, RegisterFix: "pix mcp register " + name}
}

func remote(name string) MCPServer {
	return MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp bundle"}
}

// TestMCPProbe_NothingConfiguredIsReady: MCP is opt-in. A host that never
// wanted it is healthy, and it still gets a LINE (the report never drops a
// capability just because it is unused).
func TestMCPProbe_NothingConfiguredIsReady(t *testing.T) {
	r := check(t, MCPProbe{}, time.Second)
	if r.Status != StatusReady || r.Detail != "none configured" {
		t.Fatalf("got %+v, want ready/none configured", r)
	}
	if r.Fix != "" {
		t.Errorf("a host with no MCP configured must not be handed a repair: %q", r.Fix)
	}
}

// TestMCPProbe_MissingSbxIsUnknownNotUnregistered is the headline honesty
// property: without sbx there is no registration truth to be had, and
// reporting "not registered" would send a user to run a register command that
// cannot possibly work.
func TestMCPProbe_MissingSbxIsUnknownNotUnregistered(t *testing.T) {
	p := MCPProbe{Bin: "/nonexistent/sbx-9x7z", Servers: []MCPServer{local("slack")}}
	r := check(t, p, time.Second)
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown (got %+v)", r.Status, r)
	}
	if r.Fix != "" {
		t.Errorf("an unknown must never carry a repair command, got %q", r.Fix)
	}
	if !containsAny(r.Evidence, []string{"not on PATH"}) {
		t.Errorf("evidence must say WHY it is unknown, got %q", r.Evidence)
	}
}

// TestMCPProbe_BrokenListingIsUnknown: sbx is installed and angry. "sbx is
// angry" is not "your servers are unregistered" — the readiness version of
// this check rendered every listing failure as an outstanding TODO.
func TestMCPProbe_BrokenListingIsUnknown(t *testing.T) {
	r := check(t, mcpProbe(t, "broken", local("slack")), 2*time.Second)
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown (got %+v)", r.Status, r)
	}
	if r.Fix != "" {
		t.Errorf("unknown must carry no repair, got %q", r.Fix)
	}
}

// TestMCPProbe_HungListingIsUnknown: the deadline is honoured and the answer
// is unknown, not a guess.
func TestMCPProbe_HungListingIsUnknown(t *testing.T) {
	start := time.Now()
	r := check(t, mcpProbe(t, "hang", local("slack")), 300*time.Millisecond)
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown", r.Status)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the probe did not honour its deadline (%s)", time.Since(start))
	}
}

// TestMCPProbe_RefusedListingIsDeniedWithAGatewayFix: a positive refusal is a
// verified condition, and its repair is about the GATEWAY, not any one server
// (pointing a user at `pix mcp register` here would fix nothing).
func TestMCPProbe_RefusedListingIsDenied(t *testing.T) {
	r := check(t, mcpProbe(t, "denied", local("slack")), 2*time.Second)
	if r.Status != StatusDenied {
		t.Fatalf("status = %s, want denied (got %+v)", r.Status, r)
	}
	if r.Fix != MCPGatewayFix {
		t.Errorf("fix = %q, want the gateway fix %q", r.Fix, MCPGatewayFix)
	}
}

// TestMCPProbe_UnregisteredIsAVerifiedGap: a listing that ANSWERED and does
// not hold the name is the one thing that may report absent, with the exact
// register command for that server's kind.
func TestMCPProbe_UnregisteredIsAVerifiedGap(t *testing.T) {
	p := mcpProbe(t, "mcpnone", local("slack"))
	p.AttachmentKnown = true
	r := check(t, p, 2*time.Second)
	if r.Status != StatusAbsent {
		t.Fatalf("status = %s, want absent (got %+v)", r.Status, r)
	}
	if r.Fix != "pix mcp register slack" {
		t.Errorf("fix = %q, want the server's own register command", r.Fix)
	}
}

// TestMCPProbe_UnclassifiedServerFailsClosed: we know it is not registered,
// and we do NOT know what kind of server it is, so there is no repair that is
// safe to recommend. That is unknown, not a gap with a guessed command.
func TestMCPProbe_UnclassifiedServerFailsClosed(t *testing.T) {
	p := mcpProbe(t, "mcpnone", MCPServer{Name: "mystery"})
	p.AttachmentKnown = true
	r := check(t, p, 2*time.Second)
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown (got %+v)", r.Status, r)
	}
	if r.Fix != "" {
		t.Errorf("an unclassified server must get no repair command, got %q", r.Fix)
	}
}

// TestMCPProbe_AttachmentIsNeverInferredFromConfig: registered is not
// attached. With no trustworthy receipt the answer is unknown and names the
// sandbox it could not read for.
func TestMCPProbe_AttachmentUnknownWithoutAReceipt(t *testing.T) {
	p := mcpProbe(t, "mcpls", local("slack"))
	p.Sandbox = "pix-demo"
	r := check(t, p, 2*time.Second)
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown (got %+v)", r.Status, r)
	}
	if !containsAny(r.Evidence, []string{"pix-demo"}) {
		t.Errorf("evidence must name the sandbox it could not read for, got %q", r.Evidence)
	}
}

// TestMCPProbe_RegisteredButNotAttachedIsAGap: with a trustworthy receipt,
// "registered and not in it" IS a verified gap, and the repair is the exact
// workspace-qualified load command.
func TestMCPProbe_RegisteredButNotAttachedIsAGap(t *testing.T) {
	p := mcpProbe(t, "mcpls", local("slack"))
	p.AttachmentKnown, p.Sandbox, p.Workspace = true, "pix-demo", "/w/demo"
	r := check(t, p, 2*time.Second)
	if r.Status != StatusAbsent {
		t.Fatalf("status = %s, want absent (got %+v)", r.Status, r)
	}
	if r.Fix != "pix mcp load slack /w/demo" {
		t.Errorf("fix = %q, want the workspace-qualified load command", r.Fix)
	}
}

// TestMCPProbe_RegisteredAndAttachedIsReady closes the happy path.
func TestMCPProbe_RegisteredAndAttachedIsReady(t *testing.T) {
	p := mcpProbe(t, "mcpls", local("slack"))
	p.AttachmentKnown, p.Attached, p.Sandbox = true, []string{"slack"}, "pix-demo"
	r := check(t, p, 2*time.Second)
	if r.Status != StatusReady {
		t.Fatalf("status = %s, want ready (got %+v)", r.Status, r)
	}
	if r.Fix != "" {
		t.Errorf("a ready result must carry no repair, got %q", r.Fix)
	}
}

// TestMCPProbe_RemoteAuth walks the auth axis on a REGISTERED remote server:
// a positive "not authenticated" is a gap with the auth command, an
// unparseable failure is unknown, and a clean answer is ready.
func TestMCPProbe_RemoteAuth(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		want    Status
		wantFix string
	}{
		{"authok", StatusReady, ""},
		{"authno", StatusAbsent, "pix mcp auth notion"},
		{"authnoise", StatusUnknown, ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p := mcpProbe(t, "mcpls", remote("notion"))
			p.AuthArgs = []string{tc.mode}
			p.AttachmentKnown, p.Attached = true, []string{"notion"}
			r := check(t, p, 2*time.Second)
			if r.Status != tc.want {
				t.Fatalf("status = %s, want %s (got %+v)", r.Status, tc.want, r)
			}
			if r.Fix != tc.wantFix {
				t.Errorf("fix = %q, want %q", r.Fix, tc.wantFix)
			}
		})
	}
}

// TestMCPProbe_LocalServerIsNeverAuthProbed: there is no hosted control-plane
// login for a local stdio server, so asking about one and rendering the
// answer would invent a gap out of an irrelevant question.
func TestMCPProbe_LocalServerIsNeverAuthProbed(t *testing.T) {
	p := mcpProbe(t, "mcpls", local("slack"))
	// If the local path DID auth-probe, this argv would answer "not
	// authenticated" and turn a healthy server into a gap.
	p.AuthArgs = []string{"authno"}
	p.AttachmentKnown, p.Attached = true, []string{"slack"}
	r := check(t, p, 2*time.Second)
	if r.Status != StatusReady {
		t.Fatalf("status = %s, want ready — a local stdio server has no OAuth to fail (%+v)", r.Status, r)
	}
}

// TestMCPProbe_AGapDominatesAnUnknown: with one server verifiably broken and
// another merely unproven, the report leads with the thing it can actually
// fix, and the evidence still carries both.
func TestMCPProbe_AGapDominatesAnUnknown(t *testing.T) {
	p := mcpProbe(t, "mcpnone", local("slack"), MCPServer{Name: "mystery"})
	p.AttachmentKnown = true
	r := check(t, p, 2*time.Second)
	if r.Status != StatusAbsent || r.Fix != "pix mcp register slack" {
		t.Fatalf("got %+v, want absent with slack's register command", r)
	}
	for _, want := range []string{"slack", "mystery"} {
		if !containsAny(r.Evidence, []string{want}) {
			t.Errorf("evidence dropped %q: %q", want, r.Evidence)
		}
	}
}

// TestMCPProbe_RunStripsAFixFromAnUnknown proves the central guarantee still
// holds for this probe when it goes through Run (the only place a Result is
// normalized), not just when Check is called directly.
func TestMCPProbe_RunStripsAFixFromAnUnknown(t *testing.T) {
	s := Run(t.Context(), 2*time.Second, mcpProbe(t, "broken", local("slack")))
	r, ok := s.Find("mcp")
	if !ok {
		t.Fatal("the mcp probe produced no result")
	}
	if r.Fix != "" || len(s.Fixes()) != 0 {
		t.Errorf("an unknown leaked a repair command: %+v / %v", r, s.Fixes())
	}
	if s.ExitCode() != ExitOK {
		t.Errorf("an unknown optional probe must not fail the process, exit = %d", s.ExitCode())
	}
}
