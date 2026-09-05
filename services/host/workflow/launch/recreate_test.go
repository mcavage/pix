package launch

import (
	"strings"
	"testing"

	"pix/host/envinfo"
	"pix/host/sandbox"
)

// fullProof is the proof set an ordinary, host-mounted, idle Pix sandbox
// presents: fresh listing, positively zero holders, no keep marker, direct
// mount.
func fullProof() RecreateProof {
	return RecreateProof{
		FreshListing: true,
		Holders:      KnownHolders(0),
		Workspace:    WorkspaceDirect,
	}
}

func templateDrift() (stored, current sandbox.Fingerprint) {
	stored = sandbox.Fingerprint{
		"sandboxOptions.template":   "sha-old",
		"sandboxOptions.pullPolicy": "missing",
		"env.PIX_MEMORY_SCOPE":      "work",
	}
	current = sandbox.Fingerprint{
		"sandboxOptions.template":   "sha-new",
		"sandboxOptions.pullPolicy": "missing",
		"env.PIX_MEMORY_SCOPE":      "work",
	}
	return
}

func gateFor(stored, current sandbox.Fingerprint, proof RecreateProof) AttachGate {
	return AttachGate{
		Entry:              runningEntry("pix-proj-abcd1234", "inst-1"),
		RecordedInstanceID: "inst-1",
		Stored:             stored,
		StoredFound:        true,
		Current:            current,
		Reviewed:           true,
		Tree:               &envinfo.Tree{},
		Proof:              proof,
	}
}

// TestDecideEnvAttach_TemplateOnlyDriftRecreatesAutomatically is the
// regression for the defect this change exists to fix: an ordinary Pix
// image upgrade changes the pinned template, and v1 refused every attach
// afterwards, forcing the user through a manual `pix rm && pix run` loop.
func TestDecideEnvAttach_TemplateOnlyDriftRecreatesAutomatically(t *testing.T) {
	stored, current := templateDrift()
	d := DecideEnvAttach(gateFor(stored, current, fullProof()), "pix-proj-abcd1234", "work")

	if d.Attach {
		t.Fatalf("template drift must not attach to the stale sandbox")
	}
	if d.Refusal != "" {
		t.Fatalf("template-only drift must not refuse; got refusal:\n%s", d.Refusal)
	}
	if d.Recreate == nil {
		t.Fatalf("template-only drift with full proof must plan a recreate")
	}
	if d.Recreate.SandboxName != "pix-proj-abcd1234" || d.Recreate.InstanceID != "inst-1" {
		t.Fatalf("recreate plan lost identity: %+v", d.Recreate)
	}
	if err := d.Recreate.Validate(); err != nil {
		t.Fatalf("recreate plan failed validation: %v", err)
	}
	if !strings.Contains(d.Recreate.Reason, "sandboxOptions.template") {
		t.Fatalf("recreate reason must name what drifted, got %q", d.Recreate.Reason)
	}
}

// TestDecideEnvAttach_KitDriftIsRecreationSafe covers the other half of a
// version bump: the pinned kit references move with the template.
func TestDecideEnvAttach_KitDriftIsRecreationSafe(t *testing.T) {
	stored := sandbox.Fingerprint{"kits[0]": "old", "sandboxOptions.template": "sha-old"}
	current := sandbox.Fingerprint{"kits[0]": "new", "sandboxOptions.template": "sha-new"}
	d := DecideEnvAttach(gateFor(stored, current, fullProof()), "pix-proj-abcd1234", "work")
	if d.Recreate == nil {
		t.Fatalf("pinned kit + template drift must be recreation-safe, got refusal:\n%s", d.Refusal)
	}
}

// TestDecideEnvAttach_SubstantiveDriftStillRefuses proves the safety half:
// an authored fact (a mount, a secret, an env var, an MCP server) is never
// recreated automatically, however idle the sandbox is.
func TestDecideEnvAttach_SubstantiveDriftStillRefuses(t *testing.T) {
	cases := map[string]string{
		"env var":              "env.GITHUB_TOKEN",
		"mount":                "additionalWorkspaces[]",
		"mcp server":           "mcp.servers[github].url",
		"secret":               "secrets.api.ref",
		"port":                 "ports[0].host",
		"binding":              "bindings.anthropic.apiKey.domains[api.anthropic.com]",
		"whole-environment":    "*",
		"workspace (singular)": "workspace",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			stored := sandbox.Fingerprint{key: "old", "sandboxOptions.template": "sha-old"}
			current := sandbox.Fingerprint{key: "new", "sandboxOptions.template": "sha-new"}
			d := DecideEnvAttach(gateFor(stored, current, fullProof()), "pix-proj-abcd1234", "work")
			if d.Recreate != nil {
				t.Fatalf("%s drift must never recreate automatically", name)
			}
			if d.Attach || d.Refusal == "" {
				t.Fatalf("%s drift must refuse with guidance", name)
			}
			if !strings.Contains(d.Refusal, "pix rm") {
				t.Fatalf("refusal must print the exact recovery sequence, got:\n%s", d.Refusal)
			}
		})
	}
}

// TestDecideEnvAttach_SafeDriftBlockedByProof proves each individual gate
// still stops an automatic recreate, and that the refusal SAYS which one.
func TestDecideEnvAttach_SafeDriftBlockedByProof(t *testing.T) {
	stored, current := templateDrift()
	cases := []struct {
		name   string
		mutate func(*RecreateProof)
		want   string
	}{
		{"stale listing", func(p *RecreateProof) { p.FreshListing = false }, "not re-read on this launch"},
		{"unknown holders", func(p *RecreateProof) { p.Holders = UnknownHolders() }, "could not be determined"},
		{"live holder", func(p *RecreateProof) { p.Holders = KnownHolders(1) }, "1 live session node"},
		{"keep marker", func(p *RecreateProof) { p.Keep = true }, "keep marker"},
		{"clone workspace", func(p *RecreateProof) { p.Workspace = WorkspaceClone }, "unpushed commits"},
		{"unknown workspace", func(p *RecreateProof) { p.Workspace = WorkspaceUnknown }, "workspace mode could not be determined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proof := fullProof()
			tc.mutate(&proof)
			d := DecideEnvAttach(gateFor(stored, current, proof), "pix-proj-abcd1234", "work")
			if d.Recreate != nil {
				t.Fatalf("%s must block automatic recreation", tc.name)
			}
			if !strings.Contains(d.Refusal, tc.want) {
				t.Fatalf("refusal must explain %q, got:\n%s", tc.want, d.Refusal)
			}
			if !strings.Contains(d.Refusal, "pix rm") {
				t.Fatalf("refusal must still print the manual sequence, got:\n%s", d.Refusal)
			}
		})
	}
}

// TestDecideEnvAttach_UnreviewedEnvironmentNeverRecreates: trust is not
// bypassed by the new path. An environment that is no longer reviewed
// refuses even when the only drift is a pinned template.
func TestDecideEnvAttach_UnreviewedEnvironmentNeverRecreates(t *testing.T) {
	stored, current := templateDrift()
	g := gateFor(stored, current, fullProof())
	g.Reviewed = false
	d := DecideEnvAttach(g, "pix-proj-abcd1234", "work")
	if d.Recreate != nil {
		t.Fatalf("an unreviewed environment must never be recreated automatically")
	}
	if d.Attach {
		t.Fatalf("an unreviewed environment must not attach")
	}
}

// TestDecideEnvAttach_IdentityGatesUnchanged proves the new path did not
// weaken any pre-existing ownership gate: an unverified row, a missing
// record, and an instance mismatch all still refuse before drift is even
// consulted.
func TestDecideEnvAttach_IdentityGatesUnchanged(t *testing.T) {
	stored, current := templateDrift()
	t.Run("no row", func(t *testing.T) {
		g := gateFor(stored, current, fullProof())
		g.Entry = nil
		if d := DecideEnvAttach(g, "pix-proj-abcd1234", "work"); d.Attach || d.Recreate != nil {
			t.Fatalf("an absent row authorizes nothing")
		}
	})
	t.Run("instance mismatch", func(t *testing.T) {
		g := gateFor(stored, current, fullProof())
		g.RecordedInstanceID = "inst-other"
		if d := DecideEnvAttach(g, "pix-proj-abcd1234", "work"); d.Attach || d.Recreate != nil {
			t.Fatalf("a different instance authorizes nothing")
		}
	})
	t.Run("reset invalidated", func(t *testing.T) {
		g := gateFor(stored, current, fullProof())
		g.ResetInvalidated = true
		d := DecideEnvAttach(g, "pix-proj-abcd1234", "work")
		if d.Recreate != nil {
			t.Fatalf("a reset-invalidated fingerprint has unknown scope; it must never auto-recreate")
		}
		if d.Attach {
			t.Fatalf("a reset-invalidated fingerprint must refuse")
		}
	})
}

// TestPlanSafeRecreate_NoDriftPlansNothing: an empty drift set is not
// "safe to recreate", it is "nothing to do".
func TestPlanSafeRecreate_NoDriftPlansNothing(t *testing.T) {
	plan, blockers := PlanSafeRecreate("pix-x-1", "inst", nil, fullProof())
	if plan != nil || blockers != nil {
		t.Fatalf("no drift must plan nothing: plan=%v blockers=%v", plan, blockers)
	}
}

// TestRecreatePlan_ValidateScope: the plan re-asserts the pix-* namespace
// and the recorded instance immediately before it is executed.
func TestRecreatePlan_ValidateScope(t *testing.T) {
	if err := (&RecreatePlan{SandboxName: "not-pix", InstanceID: "i"}).Validate(); err == nil {
		t.Fatalf("a non-pix name must never be recreated")
	}
	if err := (&RecreatePlan{SandboxName: "pix-a-1", InstanceID: " "}).Validate(); err == nil {
		t.Fatalf("an empty instance id must never be recreated")
	}
}
