// pix agent — manage subagents as first-class objects: list them with the
// model each one resolves to (and WHY), scaffold a new one, edit fields without
// hand-touching frontmatter, remove one, and re-level the roster after a new
// model or a budget change.
//
// The point (see docs/design/routing.md): you manage AGENTS; the router manages
// MODELS. An agent stores an INTENT, not a pinned model, so its default model is
// derived by complexity from a hand-maintained scorecard. `agent ls` makes that
// legible.

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

	"pix/host/routing"

	"gopkg.in/yaml.v3"
)

// parseBudget parses a budget string and rejects non-positive, NaN, and Inf
// (ParseFloat accepts "NaN"/"+Inf", which would poison the frontmatter).
func parseBudget(s string) (float64, error) {
	b, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(b) || math.IsInf(b, 0) || b <= 0 {
		return 0, fmt.Errorf("budget must be a positive finite number (got %q)", s)
	}
	return b, nil
}

// agentNameRe is the strict validator for an agent name. It forbids path
// separators and dots, so a name can never traverse out of agentsDir() in
// new/edit/rm (e.g. `agent rm ../README`).
var agentNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

func mustValidName(name string) {
	if !agentNameRe.MatchString(name) {
		fatalLauncher(fmt.Errorf("agent name %q must match %s (lowercase a-z0-9 and dashes, no slashes or dots)", name, agentNameRe.String()))
	}
}

// --- small launcher-local arg helpers (distinct names from any host-package
// helpers; cmd/pix is its own package main) ---

func fatalLauncher(err error) {
	fmt.Fprintf(os.Stderr, "pix agent: %v\n", err)
	os.Exit(1)
}

func hasFlagLauncher(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func flagValueLauncher(args []string, name, def string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return def
}

// firstPositional returns the first non-flag arg, skipping a flag's value.
func firstPositional(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// Skip the value of a known value-taking flag.
			if i+1 < len(args) && valueFlags[a] {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// valueFlags are the agent flags that consume the following token as a value, so
// firstPositional does not mistake that value for the <name> positional.
var valueFlags = map[string]bool{
	"--intent": true, "--description": true, "--tools": true,
	"--budget": true, "--model": true,
}

func printJSONLauncher(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func runAgent(argv []string) {
	if len(argv) == 0 {
		fmt.Print(agentUsage)
		os.Exit(2)
	}
	switch argv[0] {
	case "ls", "list":
		agentLs(argv[1:])
	case "new":
		agentNew(argv[1:])
	case "edit":
		agentEdit(argv[1:])
	case "rm", "remove":
		agentRm(argv[1:])
	case "reassess":
		agentReassess(argv[1:])
	case "-h", "--help", "help":
		fmt.Print(agentUsage)
	default:
		fmt.Fprintf(os.Stderr, "pix agent: unknown subcommand %q\n\n%s", argv[0], agentUsage)
		os.Exit(2)
	}
}

// agentsDir resolves the directory holding agent markdown files: $PIX_AGENTS_DIR
// else ./agents (run from the repo root).
func agentsDir() string {
	if d := strings.TrimSpace(os.Getenv("PIX_AGENTS_DIR")); d != "" {
		return d
	}
	return "agents"
}

// agentMeta is the typed view of an agent's frontmatter (a superset round-trips
// through a yaml.Node so edits never drop unknown fields).
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
		// An explicit pin bypasses intent routing, so it also bypasses the
		// registry check. Flag a pin that resolves to no registered model — that
		// is almost always a typo (e.g. anthropic/sonnet-5 for claude-sonnet-4-6)
		// and will fail at spawn, not here.
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

// explainDecision turns a routing Decision into a WHY that answers the real
// question — why THIS model for this agent — in words, not a wall of unmeasured
// numbers. A contest names what it beat and on which axis; a single survivor
// names the CONSTRAINTS that eliminated everything else (the actionable part you
// tune in policy.json); a fallback says nothing matched. The seed-vs-measured
// caveat lives once in the table footer, not on every row.
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
	// A real contest: >1 model cleared the constraints, so the objective broke the
	// tie. Name the runner-up and the axis.
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
	// Sole survivor: the constraints, not the objective, made the choice. Naming
	// them is the useful part (loosen these in policy.json to get a contest).
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

func agentLs(args []string) {
	names, err := listAgents()
	if err != nil {
		fatalLauncher(err)
	}
	reg, rerr := routing.LoadRegistry()
	sc, serr := routing.LoadScorecard()
	pol, perr := routing.LoadPolicy()
	if rerr != nil || serr != nil || perr != nil {
		fatalLauncher(fmt.Errorf("load routing: %v / %v / %v", rerr, serr, perr))
	}
	if hasFlagLauncher(args, "--json") {
		type row struct {
			Name, Model, Why, Intent, Tools string
			Budget                          float64
		}
		var rows []row
		for _, n := range names {
			m, _, _ := loadAgentMeta(filepath.Join(agentsDir(), n+".md"))
			model, why := resolveAgentModel(m, reg, sc, pol)
			rows = append(rows, row{n, model, why, m.Intent, m.Tools, m.BudgetUSD})
		}
		printJSONLauncher(rows)
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tMODEL\tWHY\tTOOLS\tBUDGET")
	for _, n := range names {
		m, _, _ := loadAgentMeta(filepath.Join(agentsDir(), n+".md"))
		model, why := resolveAgentModel(m, reg, sc, pol)
		tools := "all"
		if strings.TrimSpace(m.Tools) != "" {
			tools = fmt.Sprintf("%d", len(strings.Split(m.Tools, ",")))
		}
		budget := "-"
		if m.BudgetUSD > 0 {
			budget = fmt.Sprintf("$%.2f", m.BudgetUSD)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n, model, why, tools, budget)
	}
	tw.Flush()
	fmt.Println()
	fmt.Println("WHY explains the pick: what the winner beat, or the constraints that left it the only")
	fmt.Println("fit. The accuracy/cost/latency behind it are hand-maintained in scorecard.json (see")
	fmt.Println("`pix models show`). Tune the tradeoffs in policy.json, then `pix models route`.")
}

func agentNew(args []string) {
	name := firstPositional(args)
	if name == "" {
		fatalLauncher(fmt.Errorf("agent new: missing <name>"))
	}
	mustValidName(name)
	if hasFlagLauncher(args, "--interactive") {
		launchInteractiveAuthoring(name)
		return
	}
	path := filepath.Join(agentsDir(), name+".md")
	if _, err := os.Stat(path); err == nil {
		fatalLauncher(fmt.Errorf("agent %q already exists at %s", name, path))
	}
	intent := flagValueLauncher(args, "--intent", "code")
	desc := flagValueLauncher(args, "--description", fmt.Sprintf("%s specialist. Describe what this agent is for.", name))
	tools := flagValueLauncher(args, "--tools", "")
	budget := flagValueLauncher(args, "--budget", "")

	// Warn (do not block) on an unknown intent — the user may add it to policy next.
	if pol, err := routing.LoadPolicy(); err == nil {
		if _, ok := pol.Intent(intent); !ok {
			fmt.Fprintf(os.Stderr, "note: intent %q is not in policy yet; the agent will inherit the parent model until you add it (pix models show).\n", intent)
		}
	}

	// Build frontmatter via yaml.Marshal so values are safely quoted (a
	// description containing ': ' or '#' would otherwise corrupt the frontmatter).
	// A struct keeps field order and omits empty optionals.
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
			fatalLauncher(err)
		}
		fmStruct.BudgetUSD = b
	}
	fmBytes, err := yaml.Marshal(&fmStruct)
	if err != nil {
		fatalLauncher(err)
	}
	body := fmt.Sprintf("You are the **%s**. (Write the role brief here: what you do, how you\nwork, what good output looks like.)\n", name)
	content := "---\n" + string(fmBytes) + "---\n\n" + body

	if err := os.MkdirAll(agentsDir(), 0o755); err != nil {
		fatalLauncher(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatalLauncher(err)
	}

	fmt.Printf("created agent %q\n  %s\n\nNext:\n  1. Edit the role brief in %s\n  2. If %q needs a new task_type, hand-add its scores to\n     %s\n  3. pix models route                      # route it\nOr run `pix agent new %s --interactive` to author it conversationally.\n",
		name, path, path, intent, routing.ScorecardPath(), name)
}

func agentEdit(args []string) {
	name := firstPositional(args)
	if name == "" {
		fatalLauncher(fmt.Errorf("agent edit: missing <name>"))
	}
	mustValidName(name)
	path := filepath.Join(agentsDir(), name+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		fatalLauncher(fmt.Errorf("agent %q not found: %w", name, err))
	}
	fmText, body, ok := parseAgent(string(b))
	if !ok {
		fatalLauncher(fmt.Errorf("agent %q has no frontmatter to edit", name))
	}
	// Round-trip through an ordered node so unknown fields + order survive.
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fmText), &node); err != nil {
		fatalLauncher(fmt.Errorf("bad frontmatter: %w", err))
	}
	set := func(key, val, tag string) { setMappingValue(&node, key, val, tag) }
	changed := false
	if v := flagValueLauncher(args, "--intent", ""); v != "" {
		set("intent", v, "!!str")
		changed = true
	}
	if v := flagValueLauncher(args, "--description", ""); v != "" {
		set("description", v, "!!str")
		changed = true
	}
	if v := flagValueLauncher(args, "--tools", ""); v != "" {
		set("tools", v, "!!str")
		changed = true
	}
	if v := flagValueLauncher(args, "--budget", ""); v != "" {
		if _, err := parseBudget(v); err != nil {
			fatalLauncher(err)
		}
		set("budget_usd", v, "!!float")
		changed = true
	}
	if v := flagValueLauncher(args, "--model", ""); v != "" {
		set("model", v, "!!str")
		changed = true
	}
	if !changed {
		fatalLauncher(fmt.Errorf("agent edit: nothing to change (try --intent/--description/--tools/--budget/--model)"))
	}
	out, err := yaml.Marshal(node.Content[0])
	if err != nil {
		fatalLauncher(err)
	}
	content := "---\n" + string(out) + "---\n\n" + strings.TrimLeft(body, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatalLauncher(err)
	}
	fmt.Printf("updated %s\n", path)
}

func agentRm(args []string) {
	name := firstPositional(args)
	if name == "" {
		fatalLauncher(fmt.Errorf("agent rm: missing <name>"))
	}
	mustValidName(name)
	path := filepath.Join(agentsDir(), name+".md")
	if _, err := os.Stat(path); err != nil {
		fatalLauncher(fmt.Errorf("agent %q not found", name))
	}
	if !hasFlagLauncher(args, "--yes") && !hasFlagLauncher(args, "-y") {
		fatalLauncher(fmt.Errorf("agent rm %q removes %s; pass --yes to confirm", name, path))
	}
	if err := os.Remove(path); err != nil {
		fatalLauncher(err)
	}
	fmt.Printf("removed %s\n", path)
}

// agentReassess re-levels the roster: it re-resolves the roster under the
// current policy/scorecard (zero spend) and recompiles routing.json, printing
// the routing diff. --model is no longer measured automatically — scores are
// hand-maintained in scorecard.json; point the user there and stop.
func agentReassess(args []string) {
	model := flagValueLauncher(args, "--model", "")

	// Baseline = the CURRENTLY COMPILED routing.json (what is live), so the diff
	// also catches a stale routing.json after a policy/budget change. Falls back to
	// a fresh resolve when no compiled file exists yet.
	before := readCompiledRoutes()
	if before == nil {
		var err error
		before, err = resolveRoster()
		if err != nil {
			fatalLauncher(err)
		}
	}

	if model != "" {
		fmt.Fprintf(os.Stderr, "note: automated eval measurement was removed. Add/edit %q's scores by\n", model)
		fmt.Fprintf(os.Stderr, "  hand in %s, then re-run\n", routing.ScorecardPath())
		fmt.Fprintln(os.Stderr, "  `pix agent reassess` (no --model) to re-resolve + recompile.")
		os.Exit(2)
	}

	after, err := resolveRoster()
	if err != nil {
		fatalLauncher(err)
	}

	fmt.Println("routing changes:")
	changed := 0
	// Union of before + after so a REMOVED intent (in the compiled file but no
	// longer in policy) also shows in the diff.
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
	// Compile to the RIGHT file. `route compile` with no --out writes to
	// ~/.local/share/pix/routing/routing.json, but the Docker image bakes the repo-root
	// routing.json (Dockerfile COPY). reassess is maintainer-only (needs the repo
	// checkout), so when we are sitting in the repo — a routing.json next to a
	// pi-kit/spec.yaml — target that file, or the reassessment silently never
	// reaches the image.
	compileArgs := []string{"route", "compile"}
	if repo := repoRoutingTarget(); repo != "" {
		compileArgs = append(compileArgs, "--out", repo)
		fmt.Fprintf(os.Stderr, "\ncompiling routing.json (repo target: %s)...\n", repo)
	} else {
		fmt.Fprintln(os.Stderr, "\ncompiling routing.json...")
	}
	if err := runHostVerb(compileArgs); err != nil {
		fatalLauncher(fmt.Errorf("route compile: %w", err))
	}
}

// repoRoutingTarget returns the path of the repo-root routing.json the Docker
// image bakes, but ONLY when the current directory is unmistakably the pix
// repo (a routing.json sitting next to pi-kit/spec.yaml). Otherwise it returns ""
// and the caller falls back to `route compile`'s default (~/.local/share/pix/routing).
func repoRoutingTarget() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	routing := filepath.Join(wd, "routing.json")
	spec := filepath.Join(wd, "pi-kit", "spec.yaml")
	if fileExists(routing) && fileExists(spec) {
		return routing
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// launchInteractiveAuthoring hands off to an interactive `pi` session seeded to
// run the agent-new skill (powered by the authoring intent -> Opus). It does NOT
// scaffold here; the skill drives the whole flow (including `pix agent new`
// for the files). It replaces this process's stdio with pi's.
func launchInteractiveAuthoring(name string) {
	pi := "pi"
	if _, err := exec.LookPath(pi); err != nil {
		fatalLauncher(fmt.Errorf("pi is not on PATH; cannot launch interactive authoring (scaffold non-interactively with `pix agent new %s`)", name))
	}
	seed := fmt.Sprintf("Use the agent-new skill to author a new subagent named %q, end to end: intake, scaffold, decide its scores in scorecard.json if it needs a new task_type, and set the default.", name)
	fmt.Fprintf(os.Stderr, "launching pi to author %q via the agent-new skill; if it does not auto-start, run: /skill:agent-new\n", name)
	piArgs := []string{seed}
	// Force the authoring model (Opus via the authoring intent) so the flow that
	// authors this agent is not run on a weak default model.
	if m, err := resolveSessionModel("authoring"); err == nil && m != "" {
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
		fatalLauncher(err)
	}
}

// readCompiledRoutes reads the compiled routing.json (intent -> model) if it
// exists, else nil. This is the live routing the sandbox reads.
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

// resolveRoster returns intent -> resolved model for every policy intent, using
// the current registry + scorecard + policy on disk.
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

// runHostVerb execs `pix-host <verb...>` with inherited stdio.
func runHostVerb(argv []string) error {
	bin, err := findHostBinary()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// setMappingValue sets key=val (as a scalar string) in a YAML mapping node,
// updating in place if present, appending otherwise. doc is the top-level
// document node from yaml.Unmarshal.
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

const agentUsage = `usage: pix agent <command>

Manage subagents as first-class objects. An agent stores an INTENT, not a pinned
model; the router derives its default model from a hand-maintained scorecard.

commands:
  ls [--json]                          list the roster with each agent's resolved
                                       model and WHY (intent + whether it fell back)
  new NAME [--intent I] [--description D] [--tools a,b] [--budget USD] [--interactive]
                                       scaffold an agent
  edit NAME [--intent I] [--description D] [--tools a,b] [--budget USD] [--model M]
                                       change fields without hand-editing frontmatter
  rm NAME --yes                        remove an agent
  reassess [--model NEW]
                                       re-resolve the roster under the current
                                       policy/scorecard (zero spend) and recompile.
                                       --model points you at hand-editing
                                       scorecard.json instead of measuring. Prints
                                       the routing diff.

Agents live in ./agents (or $PIX_AGENTS_DIR); run from the repo root.
`
