package pack

import (
	"bytes"
	"strings"
	"testing"

	"pix/host/packinfo"
)

// trustsummary_test.go — the DEFAULT consent screen.
//
// The screen got shorter, and the risk of shortening a consent screen is that
// something a user is consenting to stops being on it. These tests exist to make
// that a test failure rather than a discovery.
//
// The governing rule: every fact the FINGERPRINT covers must be visible by
// default. A fact that re-gates an already-accepted pack while being invisible on
// the screen the user is re-asked on turns re-consent into a mystery prompt.
// Getting this wrong is how a security screen becomes theatre.

// summaryBoM is a bill of materials exercising every facet at once, so one test
// can assert that nothing silently vanished.
func summaryBoM() hostBoM {
	return hostBoM{
		MCP: []hostBoMMCP{{
			Name:    "google-workspace",
			Argv:    []string{"gog", "--readonly", "mcp"},
			EnvKeys: []string{"GOG_KEYRING_PASSWORD", "GOG_ACCOUNT"},
			Probe:   []string{"gog", "--readonly", "gmail", "labels", "list"},
		}},
		Containers: []hostBoMContainer{{
			Name:      "bamboohr",
			Image:     "bamboohr-mcp:0.0.1",
			EnvKeys:   []string{"BAMBOOHR_API_KEY"},
			EnvValues: map[string]string{"BAMBOOHR_COMPANY_DOMAIN": "docker"},
			Probe:     []string{"docker", "image", "inspect", "bamboohr-mcp:0.0.1"},
		}},
		RemoteMCP: []hostBoMRemote{{Name: "notion", URL: "https://mcp.notion.com/mcp"}},
		Inference: []hostBoMInference{{
			Name: "docker-anthropic", URL: "https://gw.docker.com/inference/anthropic",
			Auth: "sbx-session", Service: "sbx-login",
		}},
		Services: []packinfo.Service{{
			Name: "snow-proxy", Runtime: packinfo.ServiceRuntimeDaemon, Activation: "always",
			Command: "snow-proxy", Argv: []string{"--connection", "pix", "--port", "11442"},
			Port: 11442, Listen: "127.0.0.1", Health: "/health", Env: []string{"PATH"},
			License: "MIT", Source: "https://github.com/docker/pix-integrations",
		}},
		SandboxProxies: []packinfo.PackProxy{{Name: "snow", Egress: []string{"host.docker.internal:11442"}}},
		Creds:          []string{"GOG_KEYRING_PASSWORD", "BAMBOOHR_API_KEY"},
		Prerequisites:  []string{"Tailscale is connected"},
		Setup: []packinfo.SetupStep{{
			ID: "slack", Description: "Slack", Required: true,
			Require: []packinfo.SetupRequire{
				{Kind: "bin", Name: "slack-mcp", Install: "./install.sh",
					Hint: "Built from the pix-integrations repo."},
				{Kind: "probe", Argv: []string{"slack-mcp", "auth", "status"}},
			},
			Apply: []packinfo.SetupApply{{Kind: "interactive", Argv: []string{"slack-mcp", "auth", "login"}}},
		}},
	}
}

func summaryScreen(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	renderHostBoMSummary(&out, summaryBoM())
	return out.String()
}

// TestSummaryShowsEveryFingerprintedFact is the rule, as a test.
func TestSummaryShowsEveryFingerprintedFact(t *testing.T) {
	screen := summaryScreen(t)
	for _, c := range []struct{ want, why string }{
		{"gog --readonly mcp", "the exact argv a host MCP server runs is the core of what is consented to"},
		{"op run --", "a credentialed server runs through the op wrapper; showing the bare command would show a command that never runs"},
		{"bamboohr-mcp:0.0.1", "the container image is fingerprinted, so a swapped tag must be visible"},
		{"https://mcp.notion.com/mcp", "a remote endpoint is where conversation content goes"},
		{"gw.docker.com", "a model gateway is where prompts go"},
		{"snow-proxy --connection pix --port 11442", "a supervised daemon's argv executes on this Mac"},
		{"UNPINNED", "the one facet with a weaker guarantee than its neighbours"},
		{"127.0.0.1:11442", "what the daemon binds"},
		{"GOG_KEYRING_PASSWORD", "a solicited credential name"},
		{"GOG_ACCOUNT", "an env name handed to a host command that is NOT a solicited credential"},
		{"BAMBOOHR_COMPANY_DOMAIN=docker", "a literal env value a pack configures a server with"},
		{"slack-mcp auth login", "a remediation that executes on this Mac"},
		{"opens a browser", "a remediation that takes over the terminal"},
		{"snow", "a sandbox command wrapper"},
		{"Tailscale is connected", "a pack-authored prerequisite"},
	} {
		if !strings.Contains(screen, c.want) {
			t.Errorf("the DEFAULT consent screen omits %q — %s\n%s", c.want, c.why, screen)
		}
	}
}

// TestSummaryDisclosesProbesAsExecutable: a probe runs on the host. It is counted
// rather than listed, which is a deliberate trade — but a count of things that
// execute is not the same as silence, and the count must lead somewhere.
func TestSummaryDisclosesProbesAsExecutable(t *testing.T) {
	screen := summaryScreen(t)
	if !strings.Contains(screen, "Health checks, also run here: 3 commands") {
		t.Errorf("probes must be disclosed as commands that run here, with a count:\n%s", screen)
	}
	if !strings.Contains(screen, "d to list") {
		t.Errorf("a count with no way to expand it is a dead end:\n%s", screen)
	}
	// And they must NOT be inside the "runs on this Mac" list, which describes
	// what the pack launches to do work, not what checks it.
	runs := screen[strings.Index(screen, "Runs on this Mac:"):]
	runs = runs[:strings.Index(runs, "Health checks")]
	if strings.Contains(runs, "gmail labels list") {
		t.Errorf("a health probe is in the run list, which misdescribes when it runs:\n%s", runs)
	}
}

// TestSummaryIsSubstantiallyShorterThanDetails is the point of the change. It is
// a behavioural assertion, not a style one: a seventy-line screen is not read,
// and a consent screen nobody reads is not consent.
func TestSummaryIsSubstantiallyShorterThanDetails(t *testing.T) {
	b := summaryBoM()
	var sum, det bytes.Buffer
	renderHostBoMSummary(&sum, b)
	renderHostBoMDetails(&det, b)
	sumLines := strings.Count(sum.String(), "\n")
	detLines := strings.Count(det.String(), "\n")
	if sumLines >= detLines {
		t.Errorf("the summary (%d lines) must be shorter than the details (%d lines)", sumLines, detLines)
	}
}

// TestSummaryHandlesAPackWithNothing: a skills-only pack must not print a header
// followed by a blank screen, or empty rows for facets it does not have.
func TestSummaryHandlesAPackWithNothing(t *testing.T) {
	var out bytes.Buffer
	renderHostBoMSummary(&out, hostBoM{})
	screen := out.String()
	for _, unwanted := range []string{"Runs on this Mac", "Health checks", "0 MCP", "also handed", "credential"} {
		if strings.Contains(screen, unwanted) {
			t.Errorf("an empty pack's screen contains %q:\n%s", unwanted, screen)
		}
	}
}

// TestTrustGateDetailKeyExpandsAndReAsks: `d` must show the rest and ask AGAIN.
//
// The security property is the second half. A user pressing a key to read more
// has not agreed to anything, and if `d` fell through to acceptance the screen
// would adopt a pack on a keystroke that means "I am not ready to answer".
func TestTrustGateDetailKeyExpandsAndReAsks(t *testing.T) {
	b := summaryBoM()

	// d then n: expanded, then refused.
	var out bytes.Buffer
	err := packTrustGateWith(strings.NewReader("d\nn\n"), &out, true, false, false, "p", b)
	if err == nil {
		t.Fatal("`d` followed by `n` must NOT adopt the pack")
	}
	screen := out.String()
	if !strings.Contains(screen, "This pack adds to Pix:") {
		t.Error("the summary must render first")
	}
	if !strings.Contains(screen, "adds these integrations to Pix") {
		t.Error("`d` must render the detailed screen")
	}
	if n := strings.Count(screen, "Activate this pack"); n != 2 {
		t.Errorf("prompted %d times, want 2 — `d` must ask again rather than decide", n)
	}
	// The second prompt must not offer `d`: it is already showing everything, and
	// a key that does nothing reads as a broken screen.
	if n := strings.Count(screen, "d = every detail"); n != 1 {
		t.Errorf("the detail key was offered %d times, want 1 (not on the already-detailed prompt)", n)
	}

	// d then y: expanded, then accepted. The escape hatch must not cost the user
	// their ability to say yes.
	out.Reset()
	if err := packTrustGateWith(strings.NewReader("d\ny\n"), &out, true, false, false, "p", b); err != nil {
		t.Errorf("`d` then `y` must adopt: %v", err)
	}

	// EOF after d is still No.
	out.Reset()
	if err := packTrustGateWith(strings.NewReader("d\n"), &out, true, false, false, "p", b); err == nil {
		t.Error("EOF after `d` must default to No")
	}
}

// TestTrustGateDetailsFlagSkipsTheSummary: --details is for a non-TTY review,
// where there is no prompt to press a key at.
func TestTrustGateDetailsFlagSkipsTheSummary(t *testing.T) {
	var out bytes.Buffer
	if err := packTrustGateWith(nil, &out, false, true, true, "p", summaryBoM()); err != nil {
		t.Fatalf("--yes --details must accept: %v", err)
	}
	screen := out.String()
	if !strings.Contains(screen, "adds these integrations to Pix") {
		t.Errorf("--details must render the detailed screen:\n%s", screen)
	}
	if strings.Contains(screen, "This pack adds to Pix:") {
		t.Errorf("--details must not ALSO print the summary; the screen would be both walls:\n%s", screen)
	}
}
