package launch

// envlaunch_workspace_test.go — the launch side of sbx 0.39's workspace
// grammar: the environment's OWN authored `additionalWorkspaces:` must
// reach the effective document exactly once, composed by envinfo from the
// document, while this package composes only the RUNTIME mounts.

import (
	"strings"
	"testing"

	"pix/host/envinfo"
)

func workspaceSelection(t *testing.T, body string) EnvSelection {
	t.Helper()
	doc, err := envinfo.ParseBytes([]byte(body), "/env/.sbxenv.yaml", "/env")
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return EnvSelection{Name: "home", Root: "/env", Document: doc}
}

// TestComposeRuntimeFacts_AuthoredAdditionalWorkspacesRenderOnce proves
// the composition contract: envlaunch never copies the authored entries
// into RuntimeFacts.AdditionalWorkspaces (that would duplicate a mount, or
// push a runtime read-write twin ahead of an authored read-only entry),
// and they still reach the rendered document because envinfo renders them
// straight from Document.
func TestComposeRuntimeFacts_AuthoredAdditionalWorkspacesRenderOnce(t *testing.T) {
	in := EffectiveInput{
		Selection:        workspaceSelection(t, "schemaVersion: \"1\"\nagent: pix\nworkspace: ./declared\nadditionalWorkspaces:\n  - path: ./shared\n    readOnly: true\n"),
		SandboxName:      "pix-project-00000000",
		Template:         "docker.io/mcavage/pix:v0.0.0",
		PrimaryWorkspace: envinfo.WorkspaceFact{Path: "/home/user/project"},
		PersonalContext:  envinfo.WorkspaceFact{Path: "/home/user/context"},
	}
	in.AdditionalWorkspaces = WorkspaceFacts([]string{"/home/user/pack/skills"})

	facts := ComposeRuntimeFacts(in)
	for _, ws := range facts.AdditionalWorkspaces {
		if strings.Contains(ws.Path, "shared") {
			t.Fatalf("launch copied an authored additional workspace into the runtime mount list: %+v", facts.AdditionalWorkspaces)
		}
	}

	data, err := envinfo.RenderEffective(facts)
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	out := string(data)
	if n := strings.Count(out, "path: /env/shared"); n != 1 {
		t.Fatalf("authored additional workspace rendered %d times, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "path: /env/shared\n    readOnly: true") {
		t.Errorf("authored readOnly bit lost on the launch path:\n%s", out)
	}
	// The run's OWN workspace is the primary; the authored `workspace:` is
	// overridden by design and must not reappear as a mount.
	if !strings.Contains(out, "workspace:\n  path: /home/user/project\n") {
		t.Errorf("primary workspace is not the run's project workspace:\n%s", out)
	}
	if strings.Contains(out, "/env/declared") {
		t.Errorf("the superseded authored workspace leaked into the effective document:\n%s", out)
	}
}

// TestCreationFactsFor_KeepsAuthoredWorkspaceMounts proves clearing the
// runtime mount list does not take the environment's own declared mounts
// out of the creation fingerprint: they come from Document, which the
// subset keeps, so an attach recomputes them identically.
func TestCreationFactsFor_KeepsAuthoredWorkspaceMounts(t *testing.T) {
	in := EffectiveInput{
		Selection:        workspaceSelection(t, "schemaVersion: \"1\"\nagent: pix\nadditionalWorkspaces:\n  - path: ./shared\n"),
		SandboxName:      "pix-project-00000000",
		PrimaryWorkspace: envinfo.WorkspaceFact{Path: "/home/user/project"},
		AdditionalWorkspaces: []envinfo.WorkspaceFact{
			{Path: "/tmp/per-run-pack-dir"},
		},
	}
	fp, err := envinfo.ComputeFingerprint(CreationFactsFor(in), nil)
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	if fp["workspace"] != "/home/user/project|clone=false" {
		t.Errorf("workspace facet = %q", fp["workspace"])
	}
	if fp["additionalWorkspaces[0]"] != "/env/shared|readOnly=false" {
		t.Errorf("additionalWorkspaces[0] facet = %q, want the AUTHORED mount to survive the creation subset", fp["additionalWorkspaces[0]"])
	}
	for k, v := range fp {
		if strings.Contains(v, "per-run-pack-dir") {
			t.Errorf("a per-run runtime mount reached the creation fingerprint at %s = %q; every attach would refuse", k, v)
		}
	}
}
