package doctor

import (
	"bytes"
	"strings"
	"testing"

	"pix/host/health"
)

// mcpdisclosure_test.go keeps the host-trust disclosure ATTACHED TO A SURFACE.
//
// The merge that brought U10b's provision loop together with U10c's doctor
// port deleted both places the notice was printed (setup's completion summary
// and the readiness footer), leaving it asserted only as a constant nobody
// rendered. These tests pin its new home: the doctor report, gated on the MCP
// probe actually finding servers configured.

func renderDoctorFor(t *testing.T, mcp health.Result) string {
	t.Helper()
	var out bytes.Buffer
	health.RenderDoctorWith(&out, health.Snapshot{Results: []health.Result{mcp}}, health.DoctorOpts{})
	return out.String()
}

func TestDoctorRender_DisclosesHostMCPTrust_WhenMCPConfigured(t *testing.T) {
	got := renderDoctorFor(t, health.Result{Name: "mcp", Status: health.StatusReady, Detail: "1 registered and attached"})
	for _, want := range []string{"run on the host", "outside the sandbox", "host-user privileges", "sent to your model provider"} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor render missing disclosure fact %q, got:\n%s", want, got)
		}
	}
}

func TestDoctorRender_NoDisclosure_WhenNoMCPConfigured(t *testing.T) {
	got := renderDoctorFor(t, health.Result{Name: "mcp", Status: health.StatusReady, Detail: health.MCPNoneConfigured})
	if strings.Contains(got, McpHostTrustNotice) {
		t.Errorf("doctor must not disclose a risk the user has not taken:\n%s", got)
	}
}

// One constant, two spellings impossible: the alias the rest of the tree
// imports must BE the string health renders.
func TestMcpHostTrustNotice_IsOneDefinition(t *testing.T) {
	if McpHostTrustNotice != health.MCPHostTrustNotice {
		t.Error("doctor.McpHostTrustNotice drifted from health.MCPHostTrustNotice")
	}
}
