package env

import (
	"errors"
	"testing"

	"pix/host/health"
	"pix/host/hostenv"
)

// TestIntegrationStatuses_EmptyDeclaration proves a bill of materials with
// no declared MCP servers reports nothing — never a slice of fabricated
// "off" rows for a shape that was never authored.
func TestIntegrationStatuses_EmptyDeclaration(t *testing.T) {
	got := IntegrationStatuses(BillOfMaterials{}, "", true, nil)
	if got != nil {
		t.Fatalf("got %#v, want nil", got)
	}
}

// TestIntegrationStatuses_RegisteredTriState proves registration reuses
// mcp.McpRegEvidenceFrom's own three answers verbatim, so this surface can
// never disagree with `pix mcp ls` about the same listing.
func TestIntegrationStatuses_RegisteredTriState(t *testing.T) {
	bom := BillOfMaterials{MCPServers: []MCPServerFact{
		{Name: "atlassian", URL: "https://mcp.atlassian.com/v1/mcp"},
		{Name: "notion", URL: "https://mcp.notion.com/mcp"},
	}}

	// A successful listing that only names "notion": IntegrationStatuses
	// preserves b.MCPServers' own order (ComputeBoM's job to sort, not
	// this function's), so the input order above is also the output order.
	got := IntegrationStatuses(bom, "notion remote\n", true, nil)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Name != "atlassian" || got[0].Registered != health.StatusAbsent {
		t.Fatalf("atlassian = %+v, want Absent", got[0])
	}
	if got[1].Name != "notion" || got[1].Registered != health.StatusReady {
		t.Fatalf("notion = %+v, want Ready", got[1])
	}

	// A failed listing must report Unknown, never guess absence.
	unknown := IntegrationStatuses(bom, "", false, nil)
	for _, s := range unknown {
		if s.Registered != health.StatusUnknown {
			t.Errorf("%s Registered = %v, want Unknown on a failed listing", s.Name, s.Registered)
		}
	}
}

// TestIntegrationStatuses_DeclaredIsAlwaysTrue proves every returned entry
// answers "declared" the same way: it is a member of the bill of materials,
// so it was, by construction, declared.
func TestIntegrationStatuses_DeclaredIsAlwaysTrue(t *testing.T) {
	bom := BillOfMaterials{MCPServers: []MCPServerFact{{Name: "gog", Command: "gog"}}}
	got := IntegrationStatuses(bom, "", false, nil)
	if len(got) != 1 || !got[0].Declared {
		t.Fatalf("got %#v, want one Declared=true entry", got)
	}
}

// TestIntegrationStatuses_NoProbeDeclaredIsUnknownNeverReady is the false-
// ready-claim regression this file exists to prevent: a server with no
// pix.toml [host.mcp.<name>].probe_args must never report Reachable=Ready
// just because it is registered.
func TestIntegrationStatuses_NoProbeDeclaredIsUnknownNeverReady(t *testing.T) {
	bom := BillOfMaterials{MCPServers: []MCPServerFact{{Name: "notion", URL: "https://mcp.notion.com/mcp"}}}
	got := IntegrationStatuses(bom, "notion remote\n", true, nil)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d", len(got))
	}
	if got[0].Reachable != health.StatusUnknown {
		t.Fatalf("Reachable = %v, want Unknown (no probe declared)", got[0].Reachable)
	}
}

// TestIntegrationStatuses_DeclaredProbeRunsAndClassifies proves a declared
// pix.toml probe actually executes (this file's whole point — HostMCPFact's
// own doc comment promised "pix doctor runs it") and that its exit code
// drives Reachable: zero -> Ready, non-zero -> Absent (a verified negative,
// not Unknown), and a caller-signaled timeout -> Unknown (never guessed
// either way).
func TestIntegrationStatuses_DeclaredProbeRunsAndClassifies(t *testing.T) {
	bom := BillOfMaterials{
		MCPServers: []MCPServerFact{{Name: "warehouse", Command: "warehouse-mcp"}},
		HostMCP:    []HostMCPFact{{Name: "warehouse", ProbeArgs: []string{"warehouse-mcp", "probe"}}},
	}

	var gotArgv []string
	ok := func(name string, args ...string) (string, bool, error) {
		gotArgv = append([]string{name}, args...)
		return "authenticated\nextra line\n", false, nil
	}
	got := IntegrationStatuses(bom, "", false, ok)
	if len(got) != 1 || got[0].Reachable != health.StatusReady {
		t.Fatalf("got %+v, want Reachable=Ready", got)
	}
	if len(gotArgv) != 2 || gotArgv[0] != "warehouse-mcp" || gotArgv[1] != "probe" {
		t.Fatalf("probe argv = %v, want [warehouse-mcp probe]", gotArgv)
	}
	if got[0].ReachableDetail == "" {
		t.Fatal("ReachableDetail is empty for a successful probe")
	}

	failing := func(name string, args ...string) (string, bool, error) {
		return "not authenticated", false, errors.New("exit status 1")
	}
	got = IntegrationStatuses(bom, "", false, failing)
	if got[0].Reachable != health.StatusAbsent {
		t.Fatalf("Reachable = %v, want Absent for a non-zero probe exit", got[0].Reachable)
	}

	timedOut := func(name string, args ...string) (string, bool, error) {
		return "", true, errors.New("context deadline exceeded")
	}
	got = IntegrationStatuses(bom, "", false, timedOut)
	if got[0].Reachable != health.StatusUnknown {
		t.Fatalf("Reachable = %v, want Unknown for a probe timeout", got[0].Reachable)
	}
}

// TestIntegrationStatuses_NilRunnerIsUnknownNeverSkipped proves a
// probe-bearing server with no execution seam wired still appears, answered
// Unknown — never silently dropped, which would read as "nothing to check"
// instead of "could not check".
func TestIntegrationStatuses_NilRunnerIsUnknownNeverSkipped(t *testing.T) {
	bom := BillOfMaterials{
		MCPServers: []MCPServerFact{{Name: "warehouse", Command: "warehouse-mcp"}},
		HostMCP:    []HostMCPFact{{Name: "warehouse", ProbeArgs: []string{"warehouse-mcp", "probe"}}},
	}
	got := IntegrationStatuses(bom, "", false, nil)
	if len(got) != 1 || got[0].Reachable != health.StatusUnknown {
		t.Fatalf("got %+v, want one Unknown entry", got)
	}
}

// TestRunnerFromEnv_NilSystemIsNilRunner proves the hostenv.Env adapter
// fails open (nil) rather than a runner that would panic on first use, the
// same discipline doctor/probes.go's own providerRefScan documents for an
// unwired Options.
func TestRunnerFromEnv_NilSystemIsNilRunner(t *testing.T) {
	if RunnerFromEnv(hostenv.Env{}) != nil {
		t.Fatal("RunnerFromEnv(zero) should be nil")
	}
}
