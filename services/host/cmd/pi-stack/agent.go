// pi-stack agent — manage subagents as first-class objects: list them with the
// model each one resolves to (and WHY), scaffold a new one with a starter eval
// suite, edit fields without hand-touching frontmatter, remove one, and
// re-level the roster after a new model or a budget change.
//
// The point (see docs/design/routing.md): you manage AGENTS; the router manages
// MODELS. An agent stores an INTENT, not a pinned model, so its default model is
// derived by complexity and provable by evals. `agent ls` makes that legible.

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

	"gopkg.in/yaml.v3"
	"pi-stack/host/routing"
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
// helpers; cmd/pi-stack is its own package main) ---

func fatalLauncher(err error) {
	fmt.Fprintf(os.Stderr, "pi-stack agent: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "pi-stack agent: unknown subcommand %q\n\n%s", argv[0], agentUsage)
		os.Exit(2)
	}
}

// agentsDir resolves the directory holding agent markdown files: $PI_STACK_AGENTS_DIR
// else ./agents (run from the repo root).
func agentsDir() string {
	if d := strings.TrimSpace(os.Getenv("PI_STACK_AGENTS_DIR")); d != "" {
		return d
	}
	return "agents"
}

// suiteFor returns the per-agent eval suite path.
func suiteFor(name string) string {
	return filepath.Join("evals", "suites", "agents", name+".yaml")
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
		return nil, fmt.Errorf("no agents dir at %s (run from the repo root, or set PI_STACK_AGENTS_DIR): %w", dir, err)
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
	why = fmt.Sprintf("intent %s", m.Intent)
	if !d.ConstraintsMet {
		why += " (fallback)"
	}
	return d.Model, why
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
			fmt.Fprintf(os.Stderr, "note: intent %q is not in policy yet; the agent will inherit the parent model until you add it (pi-stack route show).\n", intent)
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

	// Starter eval suite (promptfoo). task_type = the intent's task_type when
	// known, else the agent name (a new specialized task_type).
	taskType := name
	if pol, err := routing.LoadPolicy(); err == nil {
		if it, ok := pol.Intent(intent); ok && it.TaskType != "" {
			taskType = it.TaskType
		}
	}
	suite := suiteFor(name)
	if _, err := os.Stat(suite); err != nil {
		if err := os.MkdirAll(filepath.Dir(suite), 0o755); err != nil {
			fatalLauncher(err)
		}
		starter := fmt.Sprintf(`# Eval suite for the %q agent (task_type: %s).
# Add cases that PROVE this agent is good at its job. Each case: a prompt and an
# assertion promptfoo can score (contains/icontains/regex/llm-rubric, or an
# external javascript grader). See evals/suites/code.yaml for examples.

- description: TODO — replace with a real case
  vars:
    prompt: "Ask the agent to do a representative task."
  assert:
    - type: icontains
      value: "TODO expected substring"
  metadata:
    task_type: %s
`, name, taskType, taskType)
		if err := os.WriteFile(suite, []byte(starter), 0o644); err != nil {
			fatalLauncher(err)
		}
	}

	fmt.Printf("created agent %q\n  %s\n  %s\n\nNext:\n  1. Edit the role brief in %s\n  2. Write real eval cases in %s\n  3. pi-stack evals run --budget 1 --save   # measure it\n  4. pi-stack route compile                 # route it\nOr run `pi-stack agent new %s --interactive` to author it conversationally.\n",
		name, path, suite, path, suite, name)
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
		fatalLauncher(fmt.Errorf("agent rm %q removes %s and its eval suite; pass --yes to confirm", name, path))
	}
	if err := os.Remove(path); err != nil {
		fatalLauncher(err)
	}
	removed := path
	if suite := suiteFor(name); suite != "" {
		if err := os.Remove(suite); err == nil {
			removed += " and " + suite
		}
	}
	fmt.Printf("removed %s\n", removed)
}

// agentReassess re-levels the roster. With --model, it evaluates the new model
// across every suite, folds the scores in, and recompiles routing.json. Without
// --model, it just re-resolves the roster under the current policy/scorecard
// (zero spend) — the new-user-budget flow. Either way it prints the routing diff.
func agentReassess(args []string) {
	model := flagValueLauncher(args, "--model", "")
	budget := flagValueLauncher(args, "--budget", "")

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
		hostArgs := []string{"evals", "run", "--models", model, "--save"}
		if budget != "" {
			hostArgs = append(hostArgs, "--budget", budget)
		}
		fmt.Fprintf(os.Stderr, "evaluating %s across all suites...\n", model)
		if err := runHostVerb(hostArgs); err != nil {
			fatalLauncher(fmt.Errorf("evals run: %w", err))
		}
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
	fmt.Fprintln(os.Stderr, "\ncompiling routing.json...")
	if err := runHostVerb([]string{"route", "compile"}); err != nil {
		fatalLauncher(fmt.Errorf("route compile: %w", err))
	}
}

// launchInteractiveAuthoring hands off to an interactive `pi` session seeded to
// run the agent-new skill (powered by the authoring intent -> Opus). It does NOT
// scaffold here; the skill drives the whole flow (including `pi-stack agent new`
// for the files). It replaces this process's stdio with pi's.
func launchInteractiveAuthoring(name string) {
	pi := "pi"
	if _, err := exec.LookPath(pi); err != nil {
		fatalLauncher(fmt.Errorf("pi is not on PATH; cannot launch interactive authoring (scaffold non-interactively with `pi-stack agent new %s`)", name))
	}
	seed := fmt.Sprintf("Use the agent-new skill to author a new subagent named %q, end to end: intake, scaffold, write real eval cases, run them, show the tradeoff table, and set the default.", name)
	fmt.Fprintf(os.Stderr, "launching pi to author %q via the agent-new skill; if it does not auto-start, run: /skill:agent-new\n", name)
	piArgs := []string{seed}
	// Force the authoring model (Opus via the authoring intent) so the flow that
	// writes evals is not run on a weak default model.
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

// runHostVerb execs `pi-stack-host <verb...>` with inherited stdio.
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

const agentUsage = `usage: pi-stack agent <command>

Manage subagents as first-class objects. An agent stores an INTENT, not a pinned
model; the router derives its default model by complexity and proves it by evals.

commands:
  ls [--json]                          list the roster with each agent's resolved
                                       model and WHY (intent + whether it fell back)
  new NAME [--intent I] [--description D] [--tools a,b] [--budget USD] [--interactive]
                                       scaffold an agent + a starter eval suite
  edit NAME [--intent I] [--description D] [--tools a,b] [--budget USD] [--model M]
                                       change fields without hand-editing frontmatter
  rm NAME --yes                        remove an agent and its eval suite
  reassess [--model NEW] [--budget USD]
                                       re-level the roster: with --model, eval the
                                       new model + recompile; without, re-resolve
                                       under the current policy (zero spend). Prints
                                       the routing diff.

Agents live in ./agents (or $PI_STACK_AGENTS_DIR); run from the repo root.
`
