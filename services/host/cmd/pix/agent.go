// pix agent — manage subagents as first-class objects: ls / new / edit / rm /
// reassess. You manage AGENTS; the router manages MODELS: an agent stores an
// INTENT, not a pinned model, and `agent ls` makes the derivation legible.
// See docs/design/routing.md.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"pix/host/cli"
	"pix/host/inference"
	"pix/host/routing"
	"pix/host/sys"
	"pix/host/workflow/launch"

	"gopkg.in/yaml.v3"
)

// parseBudget rejects non-positive, NaN and Inf (ParseFloat accepts "NaN"/
// "+Inf", which would poison the frontmatter).
func parseBudget(s string) (float64, error) {
	b, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(b) || math.IsInf(b, 0) || b <= 0 {
		return 0, fmt.Errorf("budget must be a positive finite number (got %q)", s)
	}
	return b, nil
}

// agentNameRe forbids path separators and dots, so a name can never traverse
// out of agentsDir() in new/edit/rm (e.g. `agent rm ../README`).
var agentNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

func mustValidName(name string) error {
	if !agentNameRe.MatchString(name) {
		return fmt.Errorf("agent name %q must match %s (lowercase a-z0-9 and dashes, no slashes or dots)", name, agentNameRe.String())
	}
	return nil
}

// agentsDir resolves the directory holding agent markdown files: $PIX_AGENTS_DIR
// else ./agents (run from the repo root).
func agentsDir() string {
	if d := strings.TrimSpace(os.Getenv("PIX_AGENTS_DIR")); d != "" {
		return d
	}
	return "agents"
}

// agentMeta is the typed view of an agent's frontmatter (edits round-trip
// through a yaml.Node, so unknown fields survive).
type agentMeta struct {
	Description string  `yaml:"description,omitempty"`
	Intent      string  `yaml:"intent,omitempty"`
	Model       string  `yaml:"model,omitempty"`
	Tools       string  `yaml:"tools,omitempty"`
	Thinking    string  `yaml:"thinking,omitempty"`
	BudgetUSD   float64 `yaml:"budget_usd,omitempty"`
}

// parseAgent splits an agent file into (frontmatter text, body). hasFM is false
// when the file has no `---` frontmatter block.
func parseAgent(content string) (fm string, body string, hasFM bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content, false
	}
	rest := content[len("---\n"):]
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
		// An explicit pin bypasses intent routing, so flag one that names no
		// registered model: that is almost always a typo, and it would otherwise
		// fail at spawn instead of here.
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
// numbers: a contest names what it beat and on which axis, a sole survivor
// names the CONSTRAINTS that eliminated the field (what you tune in
// policy.json), a fallback says nothing matched.
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

// intentConstraints renders an intent's hard constraints compactly (the same
// filters Resolve applies), so a WHY can explain what eliminated the field.
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

// shortModel drops the provider prefix for compact display (anthropic/claude-
// sonnet-5 -> claude-sonnet-5).
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
		m, _, _ := loadAgentMeta(filepath.Join(agentsDir(), n+".md"))
		model, why := resolveAgentModel(m, reg, sc, pol)
		rows = append(rows, agentRow{n, model, why, m.Intent, m.Tools, m.BudgetUSD})
	}
	if jsonOut {
		return launch.PrintJSONLauncher(d.Out, rows)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
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
	fmt.Println()
	fmt.Println("WHY explains the pick: what the winner beat, or the constraints that left it the only")
	fmt.Println("fit. The accuracy/cost/latency behind it are hand-maintained in scorecard.json (see")
	fmt.Println("`pix models show`). Tune the tradeoffs in policy.json, then `pix models route`.")
	return nil
}

func agentNew(d *cli.Deps, c *AgentNewCmd) error {
	name := c.Name
	if err := mustValidName(name); err != nil {
		return err
	}
	if c.Interactive {
		return launchInteractiveAuthoring(name)
	}
	path := filepath.Join(agentsDir(), name+".md")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("agent %q already exists at %s", name, path)
	}
	intent := c.Intent
	desc := c.Description
	if desc == "" {
		desc = fmt.Sprintf("%s specialist. Describe what this agent is for.", name)
	}
	tools := c.Tools
	budget := budgetArg(c.Budget)

	// Warn, do not block: the user may add the intent to policy next.
	if pol, err := routing.LoadPolicy(); err == nil {
		if _, ok := pol.Intent(intent); !ok {
			fmt.Fprintf(os.Stderr, "note: intent %q is not in policy yet; the agent will inherit the parent model until you add it (pix models show).\n", intent)
		}
	}

	// yaml.Marshal so a description containing ': ' or '#' cannot corrupt the
	// frontmatter; a struct keeps field order and omits empty optionals.
	var fmStruct struct {
		Description string  `yaml:"description"`
		Intent      string  `yaml:"intent"`
		Tools       string  `yaml:"tools,omitempty"`
		BudgetUSD   float64 `yaml:"budget_usd,omitempty"`
	}
	fmStruct.Description = desc
	fmStruct.Intent = intent
	fmStruct.Tools = tools
	if budget != "" {
		b, err := parseBudget(budget)
		if err != nil {
			return err
		}
		fmStruct.BudgetUSD = b
	}
	fmBytes, err := yaml.Marshal(&fmStruct)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("You are the **%s**. (Write the role brief here: what you do, how you\nwork, what good output looks like.)\n", name)
	content := "---\n" + string(fmBytes) + "---\n\n" + body

	if err := os.MkdirAll(agentsDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}

	fmt.Printf("created agent %q\n  %s\n\nNext:\n  1. Edit the role brief in %s\n  2. If %q needs a new task_type, hand-add its scores to\n     %s\n  3. pix models route                      # route it\nOr run `pix agent new %s --interactive` to author it conversationally.\n",
		name, path, path, intent, routing.ScorecardPath(), name)
	return nil
}

func agentEdit(d *cli.Deps, c *AgentEditCmd) error {
	name := c.Name
	if err := mustValidName(name); err != nil {
		return err
	}
	path := filepath.Join(agentsDir(), name+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("agent %q not found: %w", name, err)
	}
	fmText, body, ok := parseAgent(string(b))
	if !ok {
		return fmt.Errorf("agent %q has no frontmatter to edit", name)
	}
	// Round-trip through an ordered node so unknown fields + order survive.
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fmText), &node); err != nil {
		return fmt.Errorf("bad frontmatter: %w", err)
	}
	// budget_usd is the one non-string field, and the only one re-validated here
	// (kong already rejected an unparseable float; this rejects a non-positive
	// or non-finite one before it reaches the frontmatter).
	budget := budgetArg(c.Budget)
	if budget != "" {
		if _, err := parseBudget(budget); err != nil {
			return err
		}
	}
	changed := false
	for _, f := range []struct{ key, val, tag string }{
		{"intent", c.Intent, "!!str"},
		{"description", c.Description, "!!str"},
		{"tools", c.Tools, "!!str"},
		{"budget_usd", budget, "!!float"},
		{"model", c.Model, "!!str"},
	} {
		if f.val == "" {
			continue
		}
		setMappingValue(&node, f.key, f.val, f.tag)
		changed = true
	}
	if !changed {
		return fmt.Errorf("agent edit: nothing to change (try --intent/--description/--tools/--budget/--model)")
	}
	out, err := yaml.Marshal(node.Content[0])
	if err != nil {
		return err
	}
	content := "---\n" + string(out) + "---\n\n" + strings.TrimLeft(body, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Printf("updated %s\n", path)
	return nil
}

func agentRm(d *cli.Deps, c *AgentRmCmd) error {
	name := c.Name
	if err := mustValidName(name); err != nil {
		return err
	}
	path := filepath.Join(agentsDir(), name+".md")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("agent %q not found", name)
	}
	if !c.Yes {
		return fmt.Errorf("agent rm %q removes %s; pass --yes to confirm", name, path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", path)
	return nil
}

// agentReassess re-resolves the roster under the current policy/scorecard
// (zero spend) and recompiles routing.json, printing the routing diff. Scores
// are hand-maintained, so --model only points at scorecard.json and stops.
func agentReassess(d *cli.Deps, c *AgentReassessCmd) error {
	model := c.Model

	// Baseline = the CURRENTLY COMPILED routing.json (what is live), so the diff
	// also catches a stale routing.json after a policy/budget change. Falls back to
	// a fresh resolve when no compiled file exists yet.
	before := readCompiledRoutes()
	if before == nil {
		var err error
		before, err = resolveRoster()
		if err != nil {
			return err
		}
	}

	if model != "" {
		fmt.Fprintf(d.Err, "note: automated eval measurement was removed. Add/edit %q's scores by\n", model)
		fmt.Fprintf(d.Err, "  hand in %s, then re-run\n", routing.ScorecardPath())
		fmt.Fprintln(d.Err, "  `pix agent reassess` (no --model) to re-resolve + recompile.")
		// Usage exit (2), returned rather than os.Exit'd so the guidance is
		// testable in-process like every other handler in this package.
		return cli.SilentError{Code: 2}
	}

	after, err := resolveRoster()
	if err != nil {
		return err
	}

	fmt.Println("routing changes:")
	changed := 0
	// Union of before + after so a REMOVED intent (compiled but no longer in
	// policy) also shows in the diff.
	nameSet := map[string]bool{}
	for n := range before {
		nameSet[n] = true
	}
	for n := range after {
		nameSet[n] = true
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, a := before[n], after[n]
		if b == a {
			continue
		}
		if b == "" {
			b = "(none)"
		}
		if a == "" {
			a = "(removed)"
		}
		fmt.Printf("  %-14s %s -> %s\n", n, b, a)
		changed++
	}
	if changed == 0 {
		fmt.Println("  (none)")
	}
	// Compile to the RIGHT file: `route compile` defaults to the routing override
	// dir, but the image bakes the repo-root routing.json, so a reassess run from
	// the repo must target that file or it never reaches the image.
	compileArgs := []string{"route", "compile"}
	if repo := repoRoutingTarget(); repo != "" {
		compileArgs = append(compileArgs, "--out", repo)
		fmt.Fprintf(os.Stderr, "\ncompiling routing.json (repo target: %s)...\n", repo)
	} else {
		fmt.Fprintln(os.Stderr, "\ncompiling routing.json...")
	}
	if err := execHostBinary(d, compileArgs); err != nil {
		return fmt.Errorf("route compile: %w", err)
	}
	return nil
}

// repoRoutingTarget returns the repo-root routing.json the image bakes, but
// ONLY when the cwd is unmistakably the pix repo (a routing.json next to
// pi-kit/spec.yaml); "" otherwise, so the caller keeps compile's own default.
func repoRoutingTarget() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	routing := filepath.Join(wd, "routing.json")
	spec := filepath.Join(wd, "pi-kit", "spec.yaml")
	if sys.IsRegularFile(routing) && sys.IsRegularFile(spec) {
		return routing
	}
	return ""
}

// launchInteractiveAuthoring hands off to an interactive `pi` session seeded to
// run the agent-new skill, which drives the whole flow (including `pix agent
// new` for the files). This process's stdio becomes pi's.
func launchInteractiveAuthoring(name string) error {
	pi := "pi"
	if _, err := exec.LookPath(pi); err != nil {
		return fmt.Errorf("pi is not on PATH; cannot launch interactive authoring (scaffold non-interactively with `pix agent new %s`)", name)
	}
	seed := fmt.Sprintf("Use the agent-new skill to author a new subagent named %q, end to end: intake, scaffold, decide its scores in scorecard.json if it needs a new task_type, and set the default.", name)
	fmt.Fprintf(os.Stderr, "launching pi to author %q via the agent-new skill; if it does not auto-start, run: /skill:agent-new\n", name)
	piArgs := []string{seed}
	// Force the authoring intent so this does not run on a weak default model.
	if m, err := inference.ResolveSessionModel("authoring"); err == nil && m != "" {
		piArgs = append([]string{"--model", m}, piArgs...)
	}
	cmd := exec.Command(pi, piArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		return err
	}
	return nil
}

// readCompiledRoutes reads the live compiled routing.json (intent -> model)
// the sandbox reads, or nil.
func readCompiledRoutes() map[string]string {
	b, err := os.ReadFile(routing.CompiledRoutingPath())
	if err != nil {
		return nil
	}
	var cr routing.CompiledRouting
	if json.Unmarshal(b, &cr) != nil {
		return nil
	}
	out := map[string]string{}
	for name, r := range cr.Routes {
		out[name] = r.Model
	}
	return out
}

// resolveRoster returns intent -> resolved model for every policy intent.
func resolveRoster() (map[string]string, error) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		return nil, err
	}
	sc, err := routing.LoadScorecard()
	if err != nil {
		return nil, err
	}
	pol, err := routing.LoadPolicy()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, in := range pol.Intents {
		out[in.Name] = routing.Resolve(reg, sc, pol, in).Model
	}
	return out, nil
}

// setMappingValue sets key=val in a YAML mapping node, in place if present and
// appended otherwise. doc is yaml.Unmarshal's top-level document node.
func setMappingValue(doc *yaml.Node, key, val, tag string) {
	if len(doc.Content) == 0 {
		return
	}
	m := doc.Content[0] // mapping node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Value = val
			m.Content[i+1].Tag = tag
			m.Content[i+1].Style = 0
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: val, Tag: tag},
	)
}

// budgetArg renders a parsed --budget back into the frontmatter writer's string
// form. kong parses it as a float64, so a bad value is already rejected by the
// flag layer, naming the flag.
func budgetArg(v float64) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("%g", v)
}
