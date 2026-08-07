package health

import (
	"strings"
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

// mcpBudget is the ctx budget every case here hands to check()/Run(), except
// the one test that deliberately exercises deadline ENFORCEMENT
// (TestMCPProbe_HungListingIsUnknown, which passes its own short budget).
// The fixture is a compiled Go binary that returns in milliseconds on the
// happy path, so this budget is never actually waited out here — it only
// guards against a genuine hang — and widening it costs nothing in real test
// time. It is ONE named, tunable constant rather than a `2 * time.Second`
// literal repeated at every call site, so a CI box where subprocess spawn is
// measurably slower than a dev laptop needs one edit, not a search-and-
// replace, and it is kept well clear of health.StatusBudget/DoctorBudget
// (the PRODUCT probe budgets `pix status`/`pix doctor` actually run under):
// this is test slack, not a change to what ships.
const mcpBudget = 10 * time.Second

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
	r := check(t, mcpProbe(t, "broken", local("slack")), mcpBudget)
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
	r := check(t, mcpProbe(t, "denied", local("slack")), mcpBudget)
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
	r := check(t, p, mcpBudget)
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
	r := check(t, p, mcpBudget)
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown (got %+v)", r.Status, r)
	}
	if r.Fix != "" {
		t.Errorf("an unclassified server must get no repair command, got %q", r.Fix)
	}
}

// TestMCPProbe_RegisteredNeverClaimsAttached is U04e's honesty property, and
// it is the reason the attachment axis is gone rather than merely defaulted to
// unknown: a REGISTERED server is reported registered, ready, and with an
// explicit caveat that attachment is not checkable — never "attached", and
// never with a `pix mcp load` repair for a gap nothing here verified.
//
// The deleted version answered from a launcher-written receipt: preloaded-at-
// create or loaded-once, both of which are past pix actions, not the state of
// the live session (which any other shell can have torn down and recreated —
// U04d). "Registered and attached" was therefore a ready verdict earned by
// memory, which AGENTS.md safety invariant #13 forbids.
func TestMCPProbe_RegisteredNeverClaimsAttached(t *testing.T) {
	r := check(t, mcpProbe(t, "mcpls", local("slack")), mcpBudget)
	if r.Status != StatusReady {
		t.Fatalf("status = %s, want ready — registration is a live probe and it answered (%+v)", r.Status, r)
	}
	if r.Fix != "" {
		t.Errorf("a registered server must carry no repair, got %q", r.Fix)
	}
	if strings.Contains(r.Detail, "attached") {
		t.Errorf("detail claims attachment from no probe: %q", r.Detail)
	}
	if !strings.Contains(r.Evidence, "not checkable") {
		t.Errorf("evidence must disclose that attachment is unknowable here, got %q", r.Evidence)
	}
	if strings.Contains(r.Evidence, "mcp load") {
		t.Errorf("evidence offers a load repair for an unverified gap: %q", r.Evidence)
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
			r := check(t, p, mcpBudget)
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
	r := check(t, p, mcpBudget)
	if r.Status != StatusReady {
		t.Fatalf("status = %s, want ready — a local stdio server has no OAuth to fail (%+v)", r.Status, r)
	}
}

// TestMCPProbe_AGapDominatesAnUnknown: with one server verifiably broken and
// another merely unproven, the report leads with the thing it can actually
// fix, and the evidence still carries both.
func TestMCPProbe_AGapDominatesAnUnknown(t *testing.T) {
	p := mcpProbe(t, "mcpnone", local("slack"), MCPServer{Name: "mystery"})
	r := check(t, p, mcpBudget)
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
	s := Run(t.Context(), mcpBudget, mcpProbe(t, "broken", local("slack")))
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
