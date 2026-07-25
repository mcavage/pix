// setup_models.go — S08: `pi-stack setup`'s local-model readiness step + the
// completion summary, built on the SHARED ModelReadiness seam
// (modelreadiness.go) so setup and doctor can never disagree about what is
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
// probe CONFIRMED missing (verdictTodo — `ollama list` ran cleanly and lacks
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
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pi-stack/host/config"
)

// setupModelsOutcome is what the local-models step actually did and proved:
// disjoint tag sets on the same readiness axes doctor uses. It feeds both the
// completion summary and the state receipt, so the two can never disagree.
type setupModelsOutcome struct {
	installed          bool           // ollama on PATH (false => optional, nothing claimed)
	configured         []string       // distinct configured tags, sorted
	ready              []missingModel // confirmed pulled BEFORE this run
	missing            []missingModel // confirmed missing by the initial probe
	unverifiable       []missingModel // could not be checked — never pulled
	unverifiableReason string
	consent            string   // "none" | "--pull-models" | "prompt-yes" | "prompt-no"
	pulled             []string // pulled AND verified by the post-pull probe
	pulledUnverified   []string // pulled, but the post-pull probe itself failed
	failed             []string // pull failed, or claimed success the probe contradicts
}

// modelTags flattens a missingModel set to its tags.
func modelTags(ms []missingModel) []string {
	tags := make([]string, 0, len(ms))
	for _, m := range ms {
		tags = append(tags, m.tag)
	}
	return tags
}

// runOllamaPull execs `ollama pull <tag>` with the user's terminal inherited
// (real progress output) when runInteractive is wired; tests fall back to the
// recorded env.run seam.
func runOllamaPull(env shellEnv, tag string) error {
	if env.runInteractive != nil {
		return env.runInteractive("ollama", "pull", tag)
	}
	if env.run != nil {
		_, err := env.run("ollama", "pull", tag)
		return err
	}
	return fmt.Errorf("internal: no runner wired")
}

// setupLocalModels is the whole step: probe once, classify on the shared
// ModelReadiness axes, gather consent, pull confirmed-missing tags (deduped by
// computeMissingModels), verify once, and return the truthful outcome. It
// never installs Ollama and never pulls without consent.
func setupLocalModels(cfg *config.Config, env shellEnv, in io.Reader, out io.Writer, interactive, pullFlag bool) setupModelsOutcome {
	p := probeOllamaAt(env, effectiveOllamaEndpoint(cfg, env))
	rs := []ModelReadiness{
		modelReadiness("watcher", cfg.MemoryWatcherModel, "fact capture", p, requirementOptional),
		modelReadiness("embed", cfg.MemoryEmbedModel, "semantic recall", p, requirementOptional),
		modelReadiness("bridge", cfg.OllamaBridgeModel, "sandbox ollama bridge", p, requirementOptional),
	}
	o := setupModelsOutcome{installed: p.installed, consent: "none"}
	seen := map[string]bool{}
	for _, m := range rs {
		if m.Model != "" && !seen[m.Model] {
			seen[m.Model] = true
			o.configured = append(o.configured, m.Model)
		}
	}
	sort.Strings(o.configured)
	if !p.installed {
		// Not configured: optional, nothing claimed, nothing installed by us.
		return o
	}
	o.ready = filterModelsByVerdict(rs, func(v verdict) bool { return v == verdictReady })
	o.missing = computeMissingModels(rs)
	o.unverifiable = computeUnverifiableModels(rs)
	if len(o.unverifiable) > 0 {
		// Differentiate between invalid tags and probe failure
		var invalidTags []string
		for _, m := range o.unverifiable {
			if !isValidOllamaTag(m.tag) {
				invalidTags = append(invalidTags, m.tag)
			}
		}
		if len(invalidTags) == len(o.unverifiable) {
			o.unverifiableReason = "invalid tag format (config TODO)"
		} else {
			o.unverifiableReason = ollamaVerifyFailureReason(p)
		}
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "local models: could not verify %s (%s) — nothing will be pulled; an unverified tag is never treated as absent.\n",
			strings.Join(modelTags(o.unverifiable), ", "), o.unverifiableReason)
		return o
	}
	if len(o.missing) == 0 {
		return o
	}

	switch {
	case pullFlag:
		o.consent = "--pull-models"
	case interactive:
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Missing local Ollama models (optional — fact capture, semantic recall, the sandbox bridge):")
		for _, m := range o.missing {
			fmt.Fprintf(out, "  %s  (%s)\n", m.tag, strings.Join(m.roles, ", "))
		}
		fmt.Fprintf(out, "Pull %s now? Each download can be several GB of network and disk. [y/N] ", plural(len(o.missing), "model"))
		line, ok := scanYN(bufio.NewScanner(in))
		if !ok || (line != "y" && line != "yes") {
			o.consent = "prompt-no"
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "not pulling (default No) — later: pi-stack setup --pull-models   (or by hand: ollama pull <tag>)")
			return o
		}
		o.consent = "prompt-yes"
	default:
		// Non-interactive without the flag: NEVER download. --yes lands here on
		// purpose — suppressing prompts is not consent to gigabytes.
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "local models not pulled (%s) — a non-interactive setup never downloads without explicit consent.\n",
			strings.Join(modelTags(o.missing), ", "))
		fmt.Fprintln(out, "  pull them: pi-stack setup --pull-models   (or by hand: ollama pull <tag>)")
		return o
	}

	var attempted []string
	for _, m := range o.missing {
		fmt.Fprintf(out, "  pulling %s (%s) ...\n", m.tag, strings.Join(m.roles, ", "))
		if err := runOllamaPull(env, m.tag); err != nil {
			o.failed = append(o.failed, m.tag)
			fmt.Fprintf(out, "  ✗ ollama pull %s failed: %v\n", m.tag, err)
			continue
		}
		attempted = append(attempted, m.tag)
	}
	if len(attempted) > 0 {
		// Verify ONCE after all pulls, never per tag.
		p2 := probeOllamaAt(env, effectiveOllamaEndpoint(cfg, env))
		for _, tag := range attempted {
			switch {
			case p2.listOK && modelPulled(p2.listOut, tag):
				o.pulled = append(o.pulled, tag)
				fmt.Fprintf(out, "  ✓ %s pulled and verified\n", tag)
			case p2.listOK:
				o.failed = append(o.failed, tag)
				fmt.Fprintf(out, "  ✗ %s: pull reported success but `ollama list` does not show it\n", tag)
			default:
				o.pulledUnverified = append(o.pulledUnverified, tag)
				fmt.Fprintf(out, "  ⚠ %s pulled, but could not re-verify (%s)\n", tag, ollamaVerifyFailureReason(p2))
			}
		}
	}
	return o
}

// summaryLine renders the outcome as ONE readiness-axis line for the setup
// summary: glyph + detail, same vocabulary as doctor's render.
func (o setupModelsOutcome) summaryLine() (glyph, detail string) {
	joined := func(tags []string) string {
		s := append([]string(nil), tags...)
		sort.Strings(s)
		return strings.Join(s, ", ")
	}
	switch {
	case !o.installed:
		return "·", "optional — ollama not installed (" + joined(o.configured) + "); install it yourself: https://ollama.com"
	case len(o.unverifiable) > 0:
		return "⚠", "could not verify (" + o.unverifiableReason + ") — nothing pulled"
	case len(o.failed) > 0:
		line := "pull failed: " + joined(o.failed) + " — retry: ollama pull " + strings.Join(o.failed, "; ollama pull ")
		if have := append(append([]string(nil), modelTags(o.ready)...), o.pulled...); len(have) > 0 {
			line += " (pulled: " + joined(have) + ")"
		}
		return "✗", line
	case len(o.pulledUnverified) > 0:
		return "⚠", "pulled " + joined(o.pulledUnverified) + " but could not re-verify (`ollama list` failed after the pulls)"
	case len(o.missing) > len(o.pulled):
		var remaining []string
		for _, m := range o.missing {
			if !containsStr(o.pulled, m.tag) {
				remaining = append(remaining, m.tag)
			}
		}
		return "✗", "not pulled: " + joined(remaining) + " — pull them: pi-stack setup --pull-models (or: ollama pull <tag>)"
	default:
		return "✓", "pulled: " + joined(append(modelTags(o.ready), o.pulled...))
	}
}

// setupModelsReceipt is the launcher-state record of what this setup run
// decided and did about local models. Evidence, not config: it lives in the
// STATE dir (ephemeral runtime state, same home as the serve pidfile and the
// sandbox MCP receipts), never in config.toml — so no default ever petrifies.
type setupModelsReceipt struct {
	Schema  int                      `json:"schema"`
	At      string                   `json:"at"`
	Consent string                   `json:"consent"`
	Models  []setupModelReceiptEntry `json:"models"`
}

type setupModelReceiptEntry struct {
	Tag    string   `json:"tag"`
	Roles  []string `json:"roles,omitempty"`
	Status string   `json:"status"` // ready | pulled | pull-failed | pulled-unverified | missing | unverifiable
}

// buildSetupModelsReceipt reduces an outcome to its receipt.
func buildSetupModelsReceipt(o setupModelsOutcome, now time.Time) setupModelsReceipt {
	rec := setupModelsReceipt{Schema: 1, At: now.UTC().Format(time.RFC3339), Consent: o.consent}
	add := func(ms []missingModel, status string) {
		for _, m := range ms {
			rec.Models = append(rec.Models, setupModelReceiptEntry{Tag: m.tag, Roles: m.roles, Status: status})
		}
	}
	add(o.ready, "ready")
	add(o.unverifiable, "unverifiable")
	for _, m := range o.missing {
		status := "missing"
		switch {
		case containsStr(o.pulled, m.tag):
			status = "pulled"
		case containsStr(o.failed, m.tag):
			status = "pull-failed"
		case containsStr(o.pulledUnverified, m.tag):
			status = "pulled-unverified"
		}
		rec.Models = append(rec.Models, setupModelReceiptEntry{Tag: m.tag, Roles: m.roles, Status: status})
	}
	return rec
}

// writeSetupModelsReceipt writes <stateDir>/setup/models.json with the same
// hardening class as sandboxmcpstate.go: the leaf dir is Lstat-refused if it
// is a symlink (MkdirAll would happily treat a symlink-to-dir as "already a
// directory"), and the file itself goes through atomicWriteInDir (same-dir
// temp + rename never follows a symlinked destination). Dir 0700, file 0600.
func writeSetupModelsReceipt(stateDir string, rec setupModelsReceipt) error {
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
	return atomicWriteInDir(dir, "models.json", append(b, '\n'), 0o600)
}

// receiptSetupModels persists the outcome, best-effort but never silent: a
// receipt is evidence, so a failed write is reported (not fatal — the run
// itself already happened and the summary above is the human record). Skipped
// entirely when ollama isn't installed: there was no decision to receipt.
func receiptSetupModels(env shellEnv, out io.Writer, o setupModelsOutcome) {
	if !o.installed {
		return
	}
	dir, err := setupReceiptStateDir(env)
	if err == nil {
		err = writeSetupModelsReceipt(dir, buildSetupModelsReceipt(o, time.Now()))
	}
	if err != nil {
		fmt.Fprintf(out, "  note: could not write the setup models receipt: %v\n", err)
	}
}

// setupReceiptStateDir resolves the launcher state dir through the env seam
// (tests), falling back to the real config.StateDir.
func setupReceiptStateDir(env shellEnv) (string, error) {
	if env.stateDir != nil {
		return env.stateDir()
	}
	return config.StateDir()
}

// printSetupSummary renders the completion summary: keys, knowledge, pack,
// local models, and gog as SEPARATE readiness axes (doctor's glyph
// vocabulary), then the core-provisioned line. Core stays keys+knowledge+pack
// — local models and gog are optional integrations unless configured, and
// never gate provisioning. One provider key makes keys ready (any one model
// key launches a sandbox); an ACTIVE but EMPTY pack is a TODO, never green.
//
// gog is a LOCAL stdio MCP server: its Google grant runs through the
// installed gog CLI via `pi-stack gog setup` — the ONLY guidance ever printed
// here. Native `sbx mcp auth` is for remote catalog servers and raw `gog auth
// ...` commands are the guided command's internals; neither belongs in this
// summary.
func printSetupSummary(cfg *config.Config, env shellEnv, out io.Writer, models setupModelsOutcome) {
	// The pack/knowledge state may have been persisted by helpers that load
	// their own config (activateDefaultPack, setupKnowledge); reload so the
	// summary reports the SAVED truth, not a stale in-memory copy.
	if fresh, err := config.Load(); err == nil {
		cfg = fresh
	}
	line := func(glyph, label, detail string) {
		fmt.Fprintf(out, "  %s %-12s %s\n", glyph, label, detail)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Setup summary:")

	// keys: ready when at least ONE model-provider ref is configured (any one
	// key launches). Setup's own strict flow requires all three, so after a
	// successful run this reads all of them — but the axis rule stays one-of.
	names, kerr := hostModeProviderKeys(env)
	keysReady := false
	switch {
	case kerr != nil:
		line("⚠", "keys", "could not read hostmode.env ("+kerr.Error()+")")
	case len(names) == 0:
		line("✗", "keys", "no provider key configured — pi-stack secret set ANTHROPIC_API_KEY op://Vault/Item/field")
	default:
		keysReady = true
		line("✓", "keys", strings.Join(names, ", ")+" — one provider key is enough to launch")
	}

	knowledgeReady := len(cfg.KnowledgeBundles) > 0
	if knowledgeReady {
		line("✓", "knowledge", strings.Join(cfg.KnowledgeBundles, ", "))
	} else {
		line("✗", "knowledge", "no bundle configured — add one: pi-stack knowledge init")
	}

	pack := resolveHostStatePack(cfg, "")
	packReady := pack.Active && pack.Exists && (pack.Skills || pack.Knowledge)
	switch {
	case packReady:
		line("✓", "pack", pack.Path+" (active)")
	case pack.Active && pack.Exists:
		line("✗", "pack", "active but empty ("+pack.Path+") — add a skill: pi-stack pack add skill <name>")
	default:
		line("✗", "pack", "no active pack — create one: pi-stack pack new")
	}

	mg, md := models.summaryLine()
	line(mg, "local models", md)

	acct := strings.TrimSpace(cfg.GogAccount)
	switch {
	case acct == "" && !containsStr(cfg.MCP, "gog"):
		line("·", "gog", "optional — not configured; wire it later: pi-stack gog setup")
	case acct == "":
		line("✗", "gog", "enabled but no account authorized — run: pi-stack gog setup")
	case gogSetupAccountHealthy(env, acct):
		line("✓", "gog", acct+" authorized (read-only)")
	default:
		line("✗", "gog", acct+" not verified — run: pi-stack gog setup")
	}

	if keysReady && knowledgeReady && packReady {
		fmt.Fprintln(out, "Core provisioned (keys + knowledge + pack): yes")
	} else {
		fmt.Fprintln(out, "Core provisioned (keys + knowledge + pack): not yet — finish the ✗ items above.")
	}

	// Same two-fact disclosure doctor's footer prints, gated the same way (only
	// when at least one MCP server is configured) so a bare setup stays
	// notice-free. Kept as ONE shared constant (mcpHostTrustNotice,
	// doctor_render.go) so the two surfaces can never say different things.
	if len(cfg.MCP) > 0 {
		fmt.Fprintln(out, mcpHostTrustNotice)
	}
}

// isValidOllamaTag rejects malformed, empty, leading-dash, path-separator,
// or shell-metacharacter model tags. Since exec.Command already prevents shell
// injection, this guards against argv option confusion (`-f`, `--help`) and
// nonsensical values that could confuse parsers or probes. Valid:
// `namespace/model:tag`, letters, numbers, dots, dashes (internal), underscores.
func isValidOllamaTag(tag string) bool {
	if tag == "" || tag[0] == '-' {
		return false
	}
	for _, r := range tag {
		if r <= 32 || r >= 127 {
			return false // whitespace and control chars
		}
		if r == '/' || r == ':' || r == '.' || r == '-' || r == '_' {
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
