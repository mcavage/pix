package main

// redrive_findings3_test.go — rereview redrive findings 3/4 + DX JSON
// finding 2:
//
//  3: status must read each discovered sandbox's receipt even when the
//     current cfg/pack MCP intent is empty — a receipt-only transient name
//     (a `run --pack` mix-in, or a since-switched pack's historical
//     integration) must still surface as a per-sandbox row (human + --json),
//     while the host-global summary (which only ever reflects current
//     cfg/pack intent) correctly stays empty.
//  4: doctor tracks sbx-on-PATH separately from a successful `sbx secret
//     ls`. When sbx is on PATH but the secret probe fails, `sbx mcp ls` must
//     still be attempted — the MCP/gog groups get the on-path truth (not the
//     secret-probe's success/failure) as their "sbx present" signal, so they
//     can render ready/todo instead of falsely degrading to "sbx
//     unavailable". Providers stay unverifiable (that probe genuinely
//     failed); PATH-absent still reads absent everywhere.
//  DX JSON #2: verdict=ready must mean verified working. A note-only check
//     must carry a TRUTHFUL verdict (ready for a confirmed positive fact,
//     unverifiable for "cannot verify"/"not configured"/anything else) —
//     result() must not blanket-override to ready just because note is set.

import (
	"pix/host/hostenv/hostenvtest"
	"pix/host/readiness"
	"pix/host/workflow/doctor"
	"strings"
	"testing"
)

// --- finding 3: per-sandbox MCP rows with an empty current intent ----------

func TestRunDoctor_SecretFailureMcpSuccess(t *testing.T) {
	const hostBin = "/usr/local/bin/pix-host"
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := hostenvtest.Env{
		Present: map[string]bool{"sbx": true},
		HostBin: hostBin,
		Output: map[string]string{
			// "sbx secret ls" deliberately ABSENT -> probeRun/run errors.
			"sbx mcp ls":                 "notion\n",
			"sbx mcp auth status notion": "notion: authorized\n",
			hostBin + " mcp --list":      "google-workspace\n", // notion is not local
		},
	}
	r := doctor.RunDoctor(cfg, f.Build())

	if r.SbxAbsent {
		t.Fatal("sbx IS on PATH — a failing `sbx secret ls` must not set launch.SbxAbsent")
	}

	var modelKey readiness.Check
	found := false
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if c.Label == "model key" {
				modelKey, found = c, true
			}
		}
	}
	if !found {
		t.Fatal("model key check not found")
	}
	if modelKey.Result() != readiness.VerdictUnverifiable {
		t.Errorf("model key verdict = %q, want unverifiable (secret ls failed)", modelKey.Result())
	}

	var notion readiness.Check
	found = false
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if c.Label == "notion" {
				notion, found = c, true
			}
		}
	}
	if !found {
		t.Fatal("notion check not found")
	}
	if notion.Result() != readiness.VerdictReady {
		t.Errorf("notion verdict = %q, want ready (mcp ls + auth status both succeeded)", notion.Result())
	}
	if strings.Contains(strings.ToLower(notion.Detail), "sbx unavailable") ||
		strings.Contains(strings.ToLower(notion.Detail), "gateway") {
		t.Errorf("notion must not read as sbx-unavailable when only the SECRET probe failed: %+v", notion)
	}
}

// TestRunDoctor_SecretAndPathBothAbsent: converse control — sbx genuinely off
// PATH must still degrade both providers AND mcp/gog to their sbx-absent
// messaging (finding 4 must not weaken the true-absent case).
func TestRunDoctor_SecretAndPathBothAbsent(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := hostenvtest.Env{Present: map[string]bool{}}
	r := doctor.RunDoctor(cfg, f.Build())
	if !r.SbxAbsent {
		t.Fatal("sbx off PATH must set launch.SbxAbsent")
	}
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if c.Label == "notion" {
				if c.Result() != readiness.VerdictUnverifiable {
					t.Errorf("notion with sbx absent = %+v, want unverifiable", c)
				}
				if !strings.Contains(c.Detail, "sbx unavailable") {
					t.Errorf("notion detail should say sbx unavailable, got %q", c.Detail)
				}
			}
		}
	}
}

// --- DX JSON finding 2: verdict=ready must mean verified working -----------

func TestDoctorInvariant_NoReadyEvidenceClaimsUnverified(t *testing.T) {
	banned := []string{"cannot verify", "could not verify", "unavailable", "not configured", "missing", "not installed", "not present", "not found"}
	scan := func(t *testing.T, r *readiness.Report) {
		t.Helper()
		for _, g := range r.Groups {
			for _, c := range g.Checks {
				if c.Result() != readiness.VerdictReady {
					continue
				}
				hay := strings.ToLower(c.EvidenceString() + " " + c.Detail)
				for _, b := range banned {
					if strings.Contains(hay, b) {
						t.Errorf("group %q readiness.Check %q is readiness.Verdict=ready but evidence/detail says %q: detail=%q evidence=%q",
							g.Title, c.Label, b, c.Detail, c.Evidence)
					}
				}
			}
		}
	}

	// Cold: everything absent.
	scan(t, doctor.RunDoctor(defaultCfg(), hostenvtest.Env{Present: map[string]bool{}}.Build()))

	// Warm-ish: sbx present, secrets set, mcp registered, ollama absent.
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	warm := hostenvtest.Env{
		Present: map[string]bool{"sbx": true},
		Output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"sbx mcp ls":    "slack\n",
		},
	}
	scan(t, doctor.RunDoctor(cfg, warm.Build()))

	// Secret probe fails, mcp succeeds (this task's own finding-4 fixture).
	notionCfg := defaultCfg()
	notionCfg.MCP = []string{"notion"}
	scan(t, doctor.RunDoctor(notionCfg, hostenvtest.Env{
		Present: map[string]bool{"sbx": true},
		HostBin: "/usr/local/bin/pix-host",
		Output: map[string]string{
			"sbx mcp ls":                         "notion\n",
			"sbx mcp auth status notion":         "notion: authorized\n",
			"/usr/local/bin/pix-host mcp --list": "google-workspace\n",
		},
	}.Build()))

	// No credentialed host MCP servers -> the 1Password "not needed" note.
	scan(t, doctor.RunDoctor(defaultCfg(), hostenvtest.Env{Present: map[string]bool{}}.Build()))
}
