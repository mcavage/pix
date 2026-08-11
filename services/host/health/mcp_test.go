package health

import (
	"errors"
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

// TestMCPProbe_UndeclaredIsAGapEvenWhenTheGatewayListsIt is the
// registration-outlived-its-pack case, and the direction of the assertion is
// the whole point: a server the gateway HAS registered but no active pack
// declares is the WORSE state, not the better one — it is a live host command
// nothing can vouch for. Reporting it "registered ✓" is exactly the lie this
// probe exists to stop telling.
func TestMCPProbe_UndeclaredIsAGapEvenWhenTheGatewayListsIt(t *testing.T) {
	// "slack" IS in the mcpls listing, so registration answered YES.
	r := check(t, mcpProbe(t, "mcpls", MCPServer{Name: "slack", Undeclared: true}), mcpBudget)
	if r.Status != StatusAbsent {
		t.Fatalf("status = %s, want absent — a registered-but-undeclared server is a gap (%+v)", r.Status, r)
	}
	if !strings.Contains(r.Evidence, "no active pack declares it") {
		t.Errorf("evidence must say WHY it is a gap, got %q", r.Evidence)
	}
	if !strings.Contains(r.Fix, "sbx mcp rm slack") {
		t.Errorf("fix = %q, want the removal of the orphaned registration", r.Fix)
	}
	// The un-registered half of the same case: still a gap, different repair.
	r2 := check(t, mcpProbe(t, "mcpnone", MCPServer{Name: "slack", Undeclared: true}), mcpBudget)
	if r2.Status != StatusAbsent || !strings.Contains(r2.Fix, "pix config unset mcp slack") {
		t.Fatalf("unregistered undeclared server = %+v, want absent with the config repair", r2)
	}
}

// TestMCPProbe_RegisteredCommandNotOnPATHIsAGap: a gateway lists a
// REGISTRATION, not a working server, so a `command` server whose binary was
// removed lists exactly like a healthy one. The binary is therefore resolved
// before anything else is believed about the server — LookPath is injected so
// the case is pinned without touching the real PATH.
func TestMCPProbe_RegisteredCommandNotOnPATHIsAGap(t *testing.T) {
	p := mcpProbe(t, "mcpls", MCPServer{Name: "slack", Command: "slack-mcp", RegisterFix: "pix mcp add slack"})
	p.LookPath = func(string) (string, error) { return "", errors.New("not found in $PATH") }
	r := check(t, p, mcpBudget)
	if r.Status != StatusAbsent {
		t.Fatalf("status = %s, want absent — a registration naming a missing binary is a verified gap (%+v)", r.Status, r)
	}
	if !strings.Contains(r.Evidence, "not on PATH") || !strings.Contains(r.Evidence, "slack-mcp") {
		t.Errorf("evidence must name the unresolvable binary, got %q", r.Evidence)
	}
	// The same server with a resolvable binary is not a gap.
	p.LookPath = func(name string) (string, error) { return "/usr/local/bin/" + name, nil }
	if ok := check(t, p, mcpBudget); ok.Status != StatusReady {
		t.Fatalf("a resolvable command server = %+v, want ready", ok)
	}
}

// TestMCPProbe_NoDeclaredProbeIsUnverifiedNotHealthy: registration is real
// evidence, it is just not evidence of HEALTH. A pack that declares no probe
// gets "working order is unverified" — never "answering", which would claim a
// check that never ran.
func TestMCPProbe_NoDeclaredProbeIsUnverifiedNotHealthy(t *testing.T) {
	r := check(t, mcpProbe(t, "mcpls", local("slack")), mcpBudget)
	if !strings.Contains(r.Evidence, "no health probe declared, so working order is unverified") {
		t.Errorf("a server with no probe must be reported unverified, got %q", r.Evidence)
	}
	if strings.Contains(r.Evidence, "registered and answering") {
		t.Errorf("silence is not evidence: %q", r.Evidence)
	}
}

// TestMCPProbe_DeclaredProbeDecidesWorkingOrder: the pack-declared probe is the
// only thing that can answer "does this server actually work". A non-zero exit
// is a verified gap; a clean exit upgrades the note to "answering"; a probe that
// could not run at all is unknown, never a gap.
func TestMCPProbe_DeclaredProbeDecidesWorkingOrder(t *testing.T) {
	fixture := buildFixture(t)
	for _, tc := range []struct {
		name     string
		probe    []string
		want     Status
		evidence string
	}{
		{"clean exit", []string{fixture, "healthy"}, StatusReady, "registered and answering"},
		{"non-zero exit", []string{fixture, "broken"}, StatusAbsent, "its own health probe fails"},
		// A probe that could not run at all is UNKNOWN, never a gap: pix learned
		// nothing about the server, and a repair command for an unverified gap is
		// the thing this whole model exists to refuse.
		{"probe not runnable", []string{"/nonexistent/probe-9x7z"}, StatusUnknown, "health probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := local("slack")
			s.Probe = tc.probe
			r := check(t, mcpProbe(t, "mcpls", s), mcpBudget)
			if r.Status != tc.want {
				t.Fatalf("status = %s, want %s (%+v)", r.Status, tc.want, r)
			}
			if !strings.Contains(r.Evidence, tc.evidence) {
				t.Errorf("evidence = %q, want it to contain %q", r.Evidence, tc.evidence)
			}
			if tc.want == StatusUnknown && r.Fix != "" {
				t.Errorf("an unknown must carry no repair command, got %q", r.Fix)
			}
		})
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
