// pix agent — the subagent roster: `agent ls` shows each agent's resolved
// model and WHY. You manage AGENTS (their .md files under ./agents); the
// router manages MODELS: an agent stores an INTENT, not a pinned model, and
// `agent ls` makes the derivation legible. See docs/design/routing.md.
//
// Authoring, editing, removing and re-scoring an agent are hand-edits now
// (agents/*.md + scorecard.json), not CLI mutations — see retired.go for the
// retired new/edit/rm/reassess surfaces and their guidance.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"pix/host/cli"
	"pix/host/routing"
	"pix/host/workflow/launch"

	"gopkg.in/yaml.v3"
)

// agentsDir resolves the directory holding agent markdown files: $PIX_AGENTS_DIR
// else ./agents (run from the repo root). Also used by subagents.ts's own
// (independent) roster read — this is the Go side's copy of that same
// resolution rule.
func agentsDir() string {
	if d := strings.TrimSpace(os.Getenv("PIX_AGENTS_DIR")); d != "" {
		return d
	}
	return "agents"
}

// agentMeta is the typed view of an agent's frontmatter.
type agentMeta struct {
	Description string  `yaml:"description,omitempty"`
	Intent      string  `yaml:"intent,omitempty"`
	Model       string  `yaml:"model,omitempty"`
	Tools       string  `yaml:"tools,omitempty"`
	Thinking    string  `yaml:"thinking,omitempty"`
	BudgetUSD   float64 `yaml:"budget_usd,omitempty"`
}

// parseAgent splits an agent file into (frontmatter text, body). hasFM is false
// when the file has no `---` frontmatter block. Line endings are normalized to
// LF before parsing, so a CRLF (Windows-authored) frontmatter block is
// recognized the same as an LF one — the `---\n` prefix check and `\n---`
// terminator search would otherwise both miss a `\r\n` file entirely.
func parseAgent(content string) (fm string, body string, hasFM bool) {
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return "", content, false
	}
	rest := norm[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", content, false
	}
	fm = rest[:end]
	body = rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	return fm, body, true
}

func loadAgentMeta(path string) (agentMeta, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return agentMeta{}, "", err
	}
	fm, body, ok := parseAgent(string(b))
	if !ok {
		return agentMeta{}, body, nil
	}
	var m agentMeta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return agentMeta{}, body, fmt.Errorf("%s: bad frontmatter: %w", path, err)
	}
	return m, body, nil
}

func listAgents() ([]string, error) {
	dir := agentsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("no agents dir at %s (run from the repo root, or set PIX_AGENTS_DIR): %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	sort.Strings(names)
	return names, nil
}

// resolveAgentModel returns the model an agent resolves to and a one-line WHY.
func resolveAgentModel(m agentMeta, reg *routing.Registry, sc *routing.Scorecard, pol *routing.Policy) (model, why string) {
	if strings.TrimSpace(m.Model) != "" {
		// An explicit pin bypasses intent routing, so flag one that names no registered
		// model: almost always a typo, and it would otherwise fail at spawn.
		if reg != nil {
			if _, ok := reg.Get(m.Model); !ok {
				return m.Model, "pinned (UNKNOWN — not in models.json)"
			}
		}
		return m.Model, "pinned (explicit model:)"
	}
	if strings.TrimSpace(m.Intent) == "" {
		return "(inherit parent)", "no intent declared"
	}
	intent, ok := pol.Intent(m.Intent)
	if !ok {
		return "(inherit parent)", fmt.Sprintf("intent %q not in policy", m.Intent)
	}
	d := routing.Resolve(reg, sc, pol, intent)
	return d.Model, explainDecision(intent, d)
}

// explainDecision turns a routing Decision into a WHY in words, not unmeasured
// numbers: a contest names what it beat and on which axis, a sole survivor names the
// CONSTRAINTS that eliminated the field (what you tune in policy.json).
func explainDecision(in routing.Intent, d routing.Decision) string {
	obj := d.Objective
	if obj == "" {
		obj = "accuracy"
	}
	cons := intentConstraints(in)
	if cons == "" {
		cons = "no constraints"
	}
	if !d.ConstraintsMet {
		return fmt.Sprintf("%s: nothing matched (%s) -> fallback", in.Name, cons)
	}
	// A contest: >1 model cleared the constraints, so the objective broke the tie.
	if len(d.Alternatives) > 1 {
		runner := shortModel(d.Alternatives[1].ID)
		switch obj {
		case "cost":
			return fmt.Sprintf("%s: cheapest clearing %s; beat %s", in.Name, cons, runner)
		case "latency":
			return fmt.Sprintf("%s: fastest clearing %s; beat %s", in.Name, cons, runner)
		case "balanced":
			return fmt.Sprintf("%s: best cost/latency/accuracy blend under %s; beat %s", in.Name, cons, runner)
		default:
			return fmt.Sprintf("%s: best accuracy under %s; beat %s", in.Name, cons, runner)
		}
	}
	// Sole survivor: the constraints, not the objective, made the choice.
	return fmt.Sprintf("%s: only model matching %s", in.Name, cons)
}

// intentConstraints renders an intent's hard constraints compactly (the same filters
// Resolve applies), so a WHY can explain what eliminated the field.
func intentConstraints(in routing.Intent) string {
	var p []string
	if len(in.Providers) > 0 {
		p = append(p, strings.Join(in.Providers, "/"))
	}
	if in.MaxCostUSD > 0 {
		p = append(p, fmt.Sprintf("<=$%.2f", in.MaxCostUSD))
	}
	if in.MinAccuracy > 0 {
		p = append(p, fmt.Sprintf(">=%.2f acc", in.MinAccuracy))
	}
	if in.MaxLatencyMs > 0 {
		p = append(p, fmt.Sprintf("<=%.0fs", in.MaxLatencyMs/1000))
	}
	return strings.Join(p, ", ")
}

// shortModel drops the provider prefix for compact display.
func shortModel(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// agentRow is one roster line, resolved once and rendered either way.
type agentRow struct {
	Name, Model, Why, Intent, Tools string
	Budget                          float64
}

func agentLs(d *cli.Deps, jsonOut bool) error {
	names, err := listAgents()
	if err != nil {
		return err
	}
	reg, rerr := routing.LoadRegistry()
	sc, serr := routing.LoadScorecard()
	pol, perr := routing.LoadPolicy()
	if rerr != nil || serr != nil || perr != nil {
		return fmt.Errorf("load routing: %v / %v / %v", rerr, serr, perr)
	}
	rows := make([]agentRow, 0, len(names))
	for _, n := range names {
		m, _, err := loadAgentMeta(filepath.Join(agentsDir(), n+".md"))
		if err != nil {
			// A malformed frontmatter block resolves no intent, so surfacing it as
			// plain "(inherit parent)" would hide a broken agents/*.md file behind
			// the same row a well-formed, intent-less agent gets. Name the error
			// instead so it's the first thing a roster read shows.
			rows = append(rows, agentRow{Name: n, Model: "(error)", Why: err.Error()})
			continue
		}
		model, why := resolveAgentModel(m, reg, sc, pol)
		rows = append(rows, agentRow{n, model, why, m.Intent, m.Tools, m.BudgetUSD})
	}
	if jsonOut {
		return launch.PrintJSONLauncher(d.Out, rows)
	}
	tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tMODEL\tWHY\tTOOLS\tBUDGET")
	for _, r := range rows {
		tools := "all"
		if strings.TrimSpace(r.Tools) != "" {
			tools = fmt.Sprintf("%d", len(strings.Split(r.Tools, ",")))
		}
		budget := "-"
		if r.Budget > 0 {
			budget = fmt.Sprintf("$%.2f", r.Budget)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Model, r.Why, tools, budget)
	}
	tw.Flush()
	fmt.Fprintln(d.Out)
	fmt.Fprintln(d.Out, "WHY explains the pick: what the winner beat, or the constraints that left it the only")
	fmt.Fprintln(d.Out, "fit. The accuracy/cost/latency behind it are hand-maintained in scorecard.json (see")
	fmt.Fprintln(d.Out, "`pix models show`). Tune the tradeoffs in policy.json, then `pix models route`.")
	return nil
}
