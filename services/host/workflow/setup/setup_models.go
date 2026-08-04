// setup_models.go — S08: `pix setup`'s local-model readiness step + the
// completion summary, built on the SHARED axis.ModelReadiness seam
// (modelgo) so setup and doctor can never disagree about what is
// pulled.
//
// Consent model (the whole point):
//   - `--pull-models` is EXPLICIT consent in any mode, and the ONLY consent a
//     non-interactive setup honors. A broad `--yes` never approves downloads —
//     it suppresses prompts, so the interactive prompt below is simply never
//     reached and nothing is pulled.
//   - An interactive setup without the flag asks ONE aggregate, default-No
//     question listing every confirmed-missing tag with a disk-size warning.
//     Empty answer and EOF are both No.
//
// Evidence model: Ollama is probed ONCE up front (probeOllama); only tags the
// probe CONFIRMED missing (readiness.VerdictTodo — `ollama list` ran cleanly and lacks
// the tag) are ever offered or pulled. An unverifiable probe (daemon down,
// list failed) is never "missing" and never pulled — even with --pull-models.
// After the pulls, ONE verification probe re-checks each pulled tag; a pull
// that claims success but is not visible in that probe is a FAILURE, not a
// success claim. A partial pull failure fails setup (non-zero) with the exact
// retry commands, and the summary reports it truthfully. Setup never installs
// Ollama itself.
//
// The consent decision + per-tag outcome is receipted into launcher state
// (<state-dir>/setup/models.json, symlink-safe atomic write) as durable
// evidence of what THIS setup run did. Doctor has no receipt-reading seam, so
// nothing else consumes it yet — it is setup's own record, not a config
// default (nothing here petrifies into config.toml).
package setup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
	"slices"
	"sort"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/sys"
)

// SetupModelsOutcome is what the local-models step actually did and proved:
// disjoint tag sets on the same readiness axes doctor uses. It feeds both the
// completion summary and the state receipt, so the two can never disagree.
type SetupModelsOutcome struct {
	Installed          bool                // ollama on PATH (false => optional, nothing claimed)
	Configured         []string            // distinct configured tags, sorted
	Ready              []axis.MissingModel // confirmed pulled BEFORE this run
	Missing            []axis.MissingModel // confirmed missing by the initial probe
	Unverifiable       []axis.MissingModel // could not be checked — never pulled
	UnverifiableReason string
	Consent            string   // "none" | "--pull-models" | "prompt-yes" | "prompt-no"
	Pulled             []string // pulled AND verified by the post-pull probe
	pulledUnverified   []string // pulled, but the post-pull probe itself failed
	failed             []string // pull failed, or claimed success the probe contradicts
}

func setupMemoryModelsReady(cfg *config.Config, o SetupModelsOutcome) bool {
	if cfg == nil || !o.Installed || len(o.failed) > 0 || len(o.Unverifiable) > 0 || len(o.pulledUnverified) > 0 {
		return false
	}
	have := map[string]bool{}
	for _, m := range o.Ready {
		have[m.Tag] = true
	}
	for _, tag := range o.Pulled {
		have[tag] = true
	}
	return have[cfg.MemoryWatcherModel] && have[cfg.MemoryEmbedModel]
}

// modelTags flattens a axis.MissingModel set to its tags.
func modelTags(ms []axis.MissingModel) []string {
	tags := make([]string, 0, len(ms))
	for _, m := range ms {
		tags = append(tags, m.Tag)
	}
	return tags
}

// runOllamaPull execs `ollama pull <tag>` with the user's terminal inherited
// (real progress output) when runInteractive is wired; tests fall back to the
// recorded env.Run seam.
func runOllamaPull(env hostenv.Env, tag string) error {
	return env.RunInteractive("ollama", "pull", tag)
}

// SetupLocalModels is the whole step: probe once, classify on the shared
// axis.ModelReadiness axes, gather consent, pull confirmed-missing tags (deduped by
// axis.ComputeMissingModels), verify once, and return the truthful outcome. It
// never installs Ollama and never pulls without consent.
func SetupLocalModels(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive, pullFlag bool) SetupModelsOutcome {
	p := axis.ProbeOllamaAt(env, axis.EffectiveOllamaEndpoint(cfg, env))
	rs := []axis.ModelReadiness{
		axis.EvalModel("watcher", cfg.MemoryWatcherModel, "fact capture", p, readiness.RequirementOptional),
		axis.EvalModel("embed", cfg.MemoryEmbedModel, "semantic recall", p, readiness.RequirementOptional),
		axis.EvalModel("bridge", cfg.OllamaBridgeModel, "sandbox ollama bridge", p, readiness.RequirementOptional),
	}
	o := SetupModelsOutcome{Installed: p.Installed, Consent: "none"}
	seen := map[string]bool{}
	for _, m := range rs {
		if m.Model != "" && !seen[m.Model] {
			seen[m.Model] = true
			o.Configured = append(o.Configured, m.Model)
		}
	}
	sort.Strings(o.Configured)
	if !p.Installed {
		// Not configured: optional, nothing claimed, nothing installed by us.
		return o
	}
	o.Ready = axis.FilterModelsByVerdict(rs, func(v readiness.Verdict) bool { return v == readiness.VerdictReady })
	o.Missing = axis.ComputeMissingModels(rs)
	o.Unverifiable = axis.ComputeUnverifiableModels(rs)
	if len(o.Unverifiable) > 0 {
		// Differentiate between invalid tags and probe failure
		var invalidTags []string
		for _, m := range o.Unverifiable {
			if !axis.IsValidOllamaTag(m.Tag) {
				invalidTags = append(invalidTags, m.Tag)
			}
		}
		if len(invalidTags) == len(o.Unverifiable) {
			o.UnverifiableReason = "invalid tag format (config TODO)"
		} else {
			o.UnverifiableReason = axis.OllamaVerifyFailureReason(p)
		}
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "local models: could not verify %s (%s) — nothing will be pulled; an unverified tag is never treated as absent.\n",
			strings.Join(modelTags(o.Unverifiable), ", "), o.UnverifiableReason)
		return o
	}
	if len(o.Missing) == 0 {
		return o
	}

	switch {
	case pullFlag:
		o.Consent = "--pull-models"
	case interactive:
		// The header is RENDERED FROM THE ROLES PRESENT, not hard-coded. It used to
		// say "optional — fact capture, semantic recall, the sandbox bridge", which
		// was written when all three roles really were progressive enhancement. On
		// a pure-local box the bridge tag is now the ONLY model Pix can call at
		// all, and calling that optional is exactly the class of untrue statement
		// honest verification exists to delete.
		bridgeRequired := ollamaBridgeIsOnlyInferenceModel(cfg)
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Missing local Ollama models:")
		for _, m := range o.Missing {
			roles := strings.Join(m.Roles, ", ")
			if bridgeRequired && m.Tag == cfg.OllamaBridgeModel {
				fmt.Fprintf(out, "  %-18s (%s, inference)  — REQUIRED: the only model Pix can call on this machine\n", m.Tag, roles)
				continue
			}
			fmt.Fprintf(out, "  %-18s (%s)  — optional\n", m.Tag, roles)
		}
		fmt.Fprintf(out, "Pull %s now? Each download can be several GB of network and disk. [y/N] ", cli.Plural(len(o.Missing), "model"))
		line, ok := secret.ScanYN(bufio.NewScanner(in))
		if !ok || (line != "y" && line != "yes") {
			o.Consent = "prompt-no"
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "not pulling (default No) — later: pix setup --pull-models   (or by hand: ollama pull <tag>)")
			return o
		}
		o.Consent = "prompt-yes"
	default:
		// Non-interactive without the flag: NEVER download. --yes lands here on
		// purpose — suppressing prompts is not consent to gigabytes.
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "local models not pulled (%s) — a non-interactive setup never downloads without explicit consent.\n",
			strings.Join(modelTags(o.Missing), ", "))
		fmt.Fprintln(out, "  pull them: pix setup --pull-models   (or by hand: ollama pull <tag>)")
		return o
	}

	var attempted []string
	for _, m := range o.Missing {
		fmt.Fprintf(out, "  pulling %s (%s) ...\n", m.Tag, strings.Join(m.Roles, ", "))
		if err := runOllamaPull(env, m.Tag); err != nil {
			o.failed = append(o.failed, m.Tag)
			fmt.Fprintf(out, "  ✗ ollama pull %s failed: %v\n", m.Tag, err)
			continue
		}
		attempted = append(attempted, m.Tag)
	}
	if len(attempted) > 0 {
		// Verify ONCE after all pulls, never per tag.
		p2 := axis.ProbeOllamaAt(env, axis.EffectiveOllamaEndpoint(cfg, env))
		for _, tag := range attempted {
			switch {
			case p2.ListOK && axis.ModelPulled(p2.ListOut, tag):
				o.Pulled = append(o.Pulled, tag)
				// Success is rendered only from the post-mutation readiness report.
			case p2.ListOK:
				o.failed = append(o.failed, tag)
				fmt.Fprintf(out, "  ✗ %s: pull reported success but `ollama list` does not show it\n", tag)
			default:
				o.pulledUnverified = append(o.pulledUnverified, tag)
				fmt.Fprintf(out, "  ⚠ %s pulled, but could not re-verify (%s)\n", tag, axis.OllamaVerifyFailureReason(p2))
			}
		}
	}
	return o
}

// ollamaBridgeIsOnlyInferenceModel reports whether the configured bridge tag is
// also the inference binding this host is counting on — i.e. a pure-local box
// with nothing callable yet. That is the case where the pull prompt must say
// REQUIRED instead of optional.
func ollamaBridgeIsOnlyInferenceModel(cfg *config.Config) bool {
	if cfg == nil || strings.TrimSpace(cfg.OllamaBridgeModel) == "" {
		return false
	}
	if callable, _ := axis.ConfiguredInferenceSummary(cfg); callable > 0 {
		return false
	}
	for _, b := range cfg.Inference.Models {
		if b.Upstream == cfg.OllamaBridgeModel && axis.OllamaBindingDriver(cfg, b) && b.Available {
			return true
		}
	}
	return false
}

// summaryLine renders the outcome as ONE readiness-axis line for the setup
// summary: glyph + detail, same vocabulary as doctor's render.
func (o SetupModelsOutcome) summaryLine() (glyph, detail string) {
	joined := func(tags []string) string {
		s := append([]string(nil), tags...)
		sort.Strings(s)
		return strings.Join(s, ", ")
	}
	switch {
	case !o.Installed:
		return "·", "optional — ollama not installed (" + joined(o.Configured) + "); install it yourself: https://ollama.com"
	case len(o.Unverifiable) > 0:
		return "⚠", "could not verify (" + o.UnverifiableReason + ") — nothing pulled"
	case len(o.failed) > 0:
		line := "pull failed: " + joined(o.failed) + " — retry: ollama pull " + strings.Join(o.failed, "; ollama pull ")
		if have := append(append([]string(nil), modelTags(o.Ready)...), o.Pulled...); len(have) > 0 {
			line += " (pulled: " + joined(have) + ")"
		}
		return "✗", line
	case len(o.pulledUnverified) > 0:
		return "⚠", "pulled " + joined(o.pulledUnverified) + " but could not re-verify (`ollama list` failed after the pulls)"
	case len(o.Missing) > len(o.Pulled):
		var remaining []string
		for _, m := range o.Missing {
			if !slices.Contains(o.Pulled, m.Tag) {
				remaining = append(remaining, m.Tag)
			}
		}
		return "✗", "not pulled: " + joined(remaining) + " — pull them: pix setup --pull-models (or: ollama pull <tag>)"
	default:
		return "✓", "pulled: " + joined(append(modelTags(o.Ready), o.Pulled...))
	}
}

// SetupModelsReceipt is the launcher-state record of what this setup run
// decided and did about local models. Evidence, not config: it lives in the
// STATE dir (ephemeral runtime state, same home as the serve pidfile and the
// sandbox MCP receipts), never in config.toml — so no default ever petrifies.
type SetupModelsReceipt struct {
	Schema  int                      `json:"schema"`
	At      string                   `json:"at"`
	Consent string                   `json:"consent"`
	Models  []SetupModelReceiptEntry `json:"models"`
}

type SetupModelReceiptEntry struct {
	Tag    string   `json:"tag"`
	Roles  []string `json:"roles,omitempty"`
	Status string   `json:"status"` // ready | pulled | pull-failed | pulled-unverified | missing | unverifiable
}

// BuildSetupModelsReceipt reduces an outcome to its receipt.
func BuildSetupModelsReceipt(o SetupModelsOutcome, now time.Time) SetupModelsReceipt {
	rec := SetupModelsReceipt{Schema: 1, At: now.UTC().Format(time.RFC3339), Consent: o.Consent}
	add := func(ms []axis.MissingModel, status string) {
		for _, m := range ms {
			rec.Models = append(rec.Models, SetupModelReceiptEntry{Tag: m.Tag, Roles: m.Roles, Status: status})
		}
	}
	add(o.Ready, "ready")
	add(o.Unverifiable, "unverifiable")
	for _, m := range o.Missing {
		status := "missing"
		switch {
		case slices.Contains(o.Pulled, m.Tag):
			status = "pulled"
		case slices.Contains(o.failed, m.Tag):
			status = "pull-failed"
		case slices.Contains(o.pulledUnverified, m.Tag):
			status = "pulled-unverified"
		}
		rec.Models = append(rec.Models, SetupModelReceiptEntry{Tag: m.Tag, Roles: m.Roles, Status: status})
	}
	return rec
}

// WriteSetupModelsReceipt writes <stateDir>/setup/models.json with the same
// hardening class as sandboxmcpstate.go: the leaf dir is Lstat-refused if it
// is a symlink (MkdirAll would happily treat a symlink-to-dir as "already a
// directory"), and the file itself goes through atomicWriteInDir (same-dir
// temp + rename never follows a symlinked destination). Dir 0700, file 0600.
func WriteSetupModelsReceipt(stateDir string, rec SetupModelsReceipt) error {
	dir := filepath.Join(stateDir, "setup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write the setup receipt through it", dir)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return sys.AtomicWriteInDir(dir, "models.json", append(b, '\n'), 0o600)
}

// receiptSetupModels persists the outcome, best-effort but never silent: a
// receipt is evidence, so a failed write is reported (not fatal — the run
// itself already happened and the summary above is the human record). Skipped
// entirely when ollama isn't installed: there was no decision to receipt.
func receiptSetupModels(env hostenv.Env, out io.Writer, o SetupModelsOutcome) {
	if !o.Installed {
		return
	}
	dir, err := SetupReceiptStateDir(env)
	if err == nil {
		err = WriteSetupModelsReceipt(dir, BuildSetupModelsReceipt(o, time.Now()))
	}
	if err != nil {
		fmt.Fprintf(out, "  note: could not write the setup models receipt: %v\n", err)
	}
}

// SetupReceiptStateDir resolves the launcher state dir through the env seam
// (tests), falling back to the real config.StateDir.
func SetupReceiptStateDir(env hostenv.Env) (string, error) {
	return env.StateDir()
}

// PrintSetupSummary renders the completion summary: keys, pack, and local
// models as SEPARATE readiness axes (doctor's glyph vocabulary), then the
// core-provisioned line. Core stays keys+pack — local models are an optional
// integration unless configured, and never gate provisioning. One provider
// key makes keys ready (any one model key launches a sandbox); an ACTIVE but
// EMPTY pack is a TODO, never green.
func PrintSetupSummary(cfg *config.Config, env hostenv.Env, out io.Writer, models SetupModelsOutcome) {
	if env.Quiet {
		return
	}
	// The pack state may have been persisted by a helper that loads its own
	// config (activateDefaultPack); reload so the summary reports the SAVED
	// truth, not a stale in-memory copy.
	if fresh, err := config.Load(); err == nil {
		cfg = fresh
	}
	line := func(glyph, label, detail string) {
		fmt.Fprintf(out, "  %s %-12s %s\n", glyph, label, detail)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Setup summary:")

	callable := 0
	candidates := 0
	for _, b := range cfg.Inference.Models {
		if b.Available && inference.Allowed(cfg, b) {
			candidates++
			if b.Verified {
				callable++
			}
		}
	}
	if callable == 0 && candidates == 0 {
		line("✗", "inference", "no callable model")
	} else if callable == 0 {
		line("⚠", "inference", fmt.Sprintf("%d configured model candidate(s); first sandbox inference is the live probe", candidates))
	} else {
		seenBackends := map[string]bool{}
		var backends []string
		for _, binding := range cfg.Inference.Models {
			if binding.Verified && inference.Allowed(cfg, binding) && !seenBackends[binding.Backend] {
				seenBackends[binding.Backend] = true
				backends = append(backends, binding.Backend)
			}
		}
		sort.Strings(backends)
		detail := fmt.Sprintf("%d callable model(s) via %s", callable, strings.Join(backends, ", "))
		if candidates > callable {
			detail += fmt.Sprintf("; %d candidate(s) failed live verification", candidates-callable)
		}
		line("✓", "inference", detail)
	}

	pack := launch.ResolveHostStatePack(cfg, "")
	packReady := pack.Active && pack.Exists && (pack.Skills || pack.Knowledge)
	switch {
	case packReady:
		line("✓", "pack", pack.Path+" (active)")
	case pack.Active && pack.Exists:
		line("✗", "pack", "active but empty ("+pack.Path+") — add a skill: pix pack add skill <name>")
	default:
		// Packs are explicit and undiscoverable in normal setup.
	}

	if config.ServiceEnabled(cfg, "memory") || cfg.Inference.Backends["ollama"].Driver == "ollama" {
		mg, md := models.summaryLine()
		line(mg, "local models", md)
	}

	if callable > 0 {
		fmt.Fprintln(out, "Core ready: verified inference is configured.")
	} else if candidates > 0 {
		fmt.Fprintln(out, "Core configured: inference verification will occur on the first sandbox request.")
	} else {
		fmt.Fprintln(out, "Core not ready: configure and verify at least one model.")
	}

	// Same two-fact disclosure doctor's footer prints, gated the same way (only
	// when at least one MCP server is configured) so a bare setup stays
	// notice-free. Kept as ONE shared constant (doctor.McpHostTrustNotice,
	// doctor_render.go) so the two surfaces can never say different things.
	if len(cfg.MCP) > 0 {
		fmt.Fprintln(out, doctor.McpHostTrustNotice)
	}
}
