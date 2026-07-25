package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pi-stack/host/config"
)

// Onboarding is the agentic first-run flow (docs/design/onboarding.md). The
// interactive experience lives INSIDE a pi-stack sandbox: the agent onboards the
// user conversationally, writes identity to the memory service (data plane), and
// proposes host-config changes by writing <workspace>/.pi-stack/onboarding.json
// (control plane). This file owns the HOST half: the declarative schema, its
// allowlist validation, the applier, the reconcile-on-next-run path, and the
// flag-driven non-interactive path kept for CI (the sole survivor of the old
// `pi-stack setup` wizard).
//
// SECURITY MODEL: the sandbox is network-fenced and non-root; it can only
// PROPOSE config within onboardingResult's fixed field set. Because we unmarshal
// into a typed struct, a hostile file CANNOT reach host.enabled, plugins.*,
// kits.stack, or arbitrary services — those fields simply do not exist here.
// mcp names are additionally allowlisted, and the host applies everything under
// a confirm gate. No new network listener is introduced (the memory endpoint
// writes data rows; this writes nothing over the wire).

// onboardingResult is the declarative control-plane proposal the in-sandbox
// onboarding agent writes to <workspace>/.pi-stack/onboarding.json. Identity is
// deliberately ABSENT: it is memory data, written live over the memory RPC, not
// host config.
type onboardingResult struct {
	Version            int               `json:"version"`
	GogAccount         string            `json:"gog_account,omitempty"`
	MCP                []string          `json:"mcp,omitempty"`
	Knowledge          *onboardKnowledge `json:"knowledge,omitempty"`
	OllamaBridgeModel  string            `json:"ollama_bridge_model,omitempty"`
	MemoryWatcherModel string            `json:"memory_watcher_model,omitempty"`
}

type onboardKnowledge struct {
	Action string `json:"action"` // "scaffold" | "use" | "skip"
	Source string `json:"source"` // path (scaffold) or path|git-url (use)
}

// onboardingFileName is the per-workspace control-plane proposal, written by the
// agent and consumed by the host on the next run.
const onboardingFileName = "onboarding.json"

// onboardMCPCatalogAllow is the curated set of remote gateway-catalog MCP names
// the onboarding file may enable in addition to gog and the locally-known
// servers. Kept deliberately small; anything else is configured with
// `pi-stack mcp` directly, not via an untrusted onboarding file.
var onboardMCPCatalogAllow = map[string]bool{
	"notion": true, "atlassian": true, "granola": true, "linear": true,
}

// validateOnboardingResult rejects anything outside the allowlist BEFORE it
// touches config. env/hostResolver resolve the locally-known MCP set; when that
// probe fails we fail CLOSED on any non-gog/non-catalog mcp name rather than
// trust an unknown one.
func validateOnboardingResult(r *onboardingResult, cfg *config.Config, env shellEnv, hostResolver func() (string, error)) error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported onboarding schema version %d (want 1)", r.Version)
	}

	localSet, known := localMCPNames(env, hostResolver)
	for _, m := range r.MCP {
		m = strings.TrimSpace(m)
		if m == "" {
			return fmt.Errorf("empty mcp name")
		}
		if m == "gog" || onboardMCPCatalogAllow[m] {
			continue
		}
		if known && localSet[m] {
			continue
		}
		return fmt.Errorf("mcp %q is not an allowlisted server (gog, a locally-known host server, or a curated catalog name); configure it with `pi-stack mcp` instead", m)
	}

	if r.Knowledge != nil {
		switch r.Knowledge.Action {
		case "scaffold", "use", "skip":
		default:
			return fmt.Errorf("knowledge.action %q invalid (want scaffold|use|skip)", r.Knowledge.Action)
		}
		if r.Knowledge.Action != "skip" && strings.TrimSpace(r.Knowledge.Source) == "" {
			return fmt.Errorf("knowledge.action %q needs a source", r.Knowledge.Action)
		}
	}

	for label, v := range map[string]string{
		"ollama_bridge_model":  r.OllamaBridgeModel,
		"memory_watcher_model": r.MemoryWatcherModel,
	} {
		if v != "" && strings.ContainsAny(v, " \t\n\r") {
			return fmt.Errorf("%s %q must not contain whitespace", label, v)
		}
	}
	return nil
}

// applyOnboardingResult applies a VALIDATED proposal onto cfg via the same
// setters the CLI uses, then Save()s. It is idempotent: re-applying identical
// input yields identical config. It returns the human-readable changes it made.
// Caller validates first.
func applyOnboardingResult(r *onboardingResult, cfg *config.Config, env shellEnv, out io.Writer, save func(*config.Config) error) ([]string, error) {
	var changes []string

	if acct := strings.TrimSpace(r.GogAccount); acct != "" && acct != cfg.GogAccount {
		cfg.SetGogAccount(acct)
		if cfg.AddMCP("gog") {
			changes = append(changes, "enabled gog (mcp)")
		}
		changes = append(changes, "gog_account = "+acct)
	}
	for _, m := range r.MCP {
		if cfg.AddMCP(strings.TrimSpace(m)) {
			changes = append(changes, "enabled "+strings.TrimSpace(m)+" (mcp)")
		}
	}
	if r.Knowledge != nil && r.Knowledge.Action != "skip" {
		if err := setupKnowledge(cfg, r.Knowledge.Source, out); err != nil {
			return changes, fmt.Errorf("knowledge %s %q: %w", r.Knowledge.Action, r.Knowledge.Source, err)
		}
		changes = append(changes, "knowledge -> "+r.Knowledge.Source)
	}
	if v := strings.TrimSpace(r.OllamaBridgeModel); v != "" && v != cfg.OllamaBridgeModel {
		cfg.OllamaBridgeModel = v
		changes = append(changes, "ollama_bridge_model = "+v)
	}
	if v := strings.TrimSpace(r.MemoryWatcherModel); v != "" && v != cfg.MemoryWatcherModel {
		cfg.MemoryWatcherModel = v
		changes = append(changes, "memory_watcher_model = "+v)
	}
	cfg.AddService("memory")

	if err := save(cfg); err != nil {
		return changes, fmt.Errorf("saving config: %w", err)
	}
	sort.Strings(changes)
	return changes, nil
}

// reconcileOnboarding reads <workspace>/.pi-stack/onboarding.json if present,
// validates it, shows the diff, applies under a [Y/n] gate (assumeYes skips the
// prompt for CI), registers any newly-enabled MCP servers, then removes the
// file. Absent file is a clean no-op. A validation failure leaves the file in
// place and warns (so a human can inspect it) but never aborts the caller.
func reconcileOnboarding(workspace string, env shellEnv, in io.Reader, out io.Writer, assumeYes, tty bool) {
	path := filepath.Join(workspace, ".pi-stack", onboardingFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return // absent (or unreadable) => nothing to reconcile
	}
	var r onboardingResult
	if err := json.Unmarshal(data, &r); err != nil {
		fmt.Fprintf(out, "pi-stack: ignoring malformed %s: %v\n", path, err)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pi-stack: could not load config to apply onboarding: %v\n", err)
		return
	}
	if err := validateOnboardingResult(&r, cfg, env, hostBinaryResolver); err != nil {
		fmt.Fprintf(out, "pi-stack: refusing onboarding proposal in %s: %v\n", path, err)
		fmt.Fprintln(out, "  Inspect and remove it by hand if it is not what you intended.")
		return
	}

	// Preview: apply against a COPY to compute the diff without committing.
	preview := *cfg
	changes, err := applyOnboardingResult(&r, &preview, env, io.Discard, func(*config.Config) error { return nil })
	if err != nil {
		fmt.Fprintf(out, "pi-stack: onboarding proposal could not be applied: %v\n", err)
		return
	}
	if len(changes) == 0 {
		_ = removeWorkspaceStateFile(workspace, onboardingFileName) // nothing new; clear the marker
		return
	}

	fmt.Fprintln(out, "Onboarding proposed these host-config changes:")
	for _, c := range changes {
		fmt.Fprintf(out, "  + %s\n", c)
	}
	if !assumeYes {
		if !tty {
			fmt.Fprintf(out, "Not a terminal; leaving %s for review. Apply with: pi-stack onboard --apply --yes\n", path)
			return
		}
		if !confirmYN(in, out, "Apply these changes? [Y/n]: ", true) {
			fmt.Fprintf(out, "Left %s in place; not applied.\n", path)
			return
		}
	}

	applied, err := applyOnboardingResult(&r, cfg, env, out, func(c *config.Config) error { return c.Save() })
	if err != nil {
		fmt.Fprintf(out, "pi-stack: applied partially then failed: %v\n", err)
		return
	}
	if len(cfg.MCP) > 0 {
		if err := registerServers(cfg, env, out, nil, hostBinaryResolver, activeContainerMCP(cfg)); err != nil {
			fmt.Fprintf(out, "  mcp register skipped: %v (finish later: pi-stack mcp register)\n", err)
		}
	}
	_ = removeWorkspaceStateFile(workspace, onboardingFileName)
	fmt.Fprintf(out, "Applied %d onboarding change(s) to %s.\n", len(applied), config.Path())
}

const onboardUsage = `usage: pi-stack onboard [flags]

Host-side onboarding (deterministic HOST config only; NO agent handoff). For the
guided flow that configures the host AND hands off to an agent to finish, use
` + "`pi-stack setup`" + `. This command is the flag-driven path for automation/CI.
You can also say "onboard me" to a running agent at any time.

  --account <email>        set the Google Workspace (gog) account + enable gog
  --knowledge <path|url>   scaffold/point the global knowledge base
  --mcp <name>             enable an MCP server (repeatable; allowlisted)
  --model <ollama-model>   set the ollama-bridge model
  --apply                  apply a pending .pi-stack/onboarding.json in this dir
  --yes | --non-interactive  never prompt (CI); apply what is given
  -h | --help              this help

Provider keys come from 1Password via ` + "`pi-stack setup`" + ` (op is required);
onboard never provisions them. The removed --use-sbx-keys / --use-1password
flags now error.

Always ensures the memory service is enabled. Idempotent; safe to re-run.
Provider keys are sbx secrets (proxy-injected, seeded from 1Password) and are
only reported here, never entered.
`

// onboardOpts is the parsed onboard flag set.
type onboardOpts struct {
	account   string
	knowledge string
	mcp       []string
	model     string
	apply     bool
	assumeYes bool
	// pullModels is `pi-stack setup`'s explicit local-model download consent
	// (S08). Parsed here because setup shares this parser; `pi-stack onboard`
	// itself REJECTS it — onboard is the scripted host-config path and never
	// downloads models.
	pullModels bool
	help       bool
}

func parseOnboardArgs(argv []string) (onboardOpts, error) {
	var o onboardOpts
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next := func() (string, error) {
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return argv[i], nil
		}
		var err error
		switch {
		case a == "-h" || a == "--help":
			o.help = true
			return o, nil
		case a == "--apply":
			o.apply = true
		case a == "--use-sbx-keys":
			return o, fmt.Errorf("--use-sbx-keys has been removed: 1Password (op) is now the only provider-key source; run `pi-stack setup` with op installed + signed in")
		case a == "--use-1password":
			return o, fmt.Errorf("--use-1password has been removed: 1Password is now the only provider-key source, so `pi-stack setup` always uses it")
		case a == "--yes" || a == "-y" || a == "--non-interactive":
			o.assumeYes = true
		case a == "--pull-models":
			o.pullModels = true
		case a == "--account":
			o.account, err = next()
		case strings.HasPrefix(a, "--account="):
			o.account = strings.TrimPrefix(a, "--account=")
		case a == "--knowledge":
			o.knowledge, err = next()
		case strings.HasPrefix(a, "--knowledge="):
			o.knowledge = strings.TrimPrefix(a, "--knowledge=")
		case a == "--mcp":
			var v string
			if v, err = next(); err == nil {
				o.mcp = append(o.mcp, v)
			}
		case strings.HasPrefix(a, "--mcp="):
			o.mcp = append(o.mcp, strings.TrimPrefix(a, "--mcp="))
		case a == "--model":
			o.model, err = next()
		case strings.HasPrefix(a, "--model="):
			o.model = strings.TrimPrefix(a, "--model=")
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

// runOnboardCmd is the `pi-stack onboard` entry (and the `setup` deprecation
// alias). With --apply it reconciles a pending onboarding.json; otherwise it
// applies the flag-driven host config and reports what still needs doing.
func runOnboardCmd(argv []string) {
	opts, err := parseOnboardArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack onboard: %v\n\n%s", err, onboardUsage)
		os.Exit(2)
	}
	if opts.help {
		fmt.Print(onboardUsage)
		return
	}
	if opts.pullModels {
		fmt.Fprintln(os.Stderr, "pi-stack onboard: --pull-models belongs to `pi-stack setup` (onboard never downloads models)")
		os.Exit(2)
	}
	env := defaultShellEnv()

	if opts.apply {
		cwd, _ := os.Getwd()
		reconcileOnboarding(cwd, env, os.Stdin, os.Stdout, opts.assumeYes, isTTY(os.Stdin))
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack onboard: loading config: %v\n", err)
		os.Exit(1)
	}

	r := &onboardingResult{
		Version:           1,
		GogAccount:        strings.TrimSpace(opts.account),
		MCP:               opts.mcp,
		OllamaBridgeModel: strings.TrimSpace(opts.model),
	}
	if k := strings.TrimSpace(opts.knowledge); k != "" {
		r.Knowledge = &onboardKnowledge{Action: "use", Source: k}
	}
	if err := validateOnboardingResult(r, cfg, env, hostBinaryResolver); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack onboard: %v\n", err)
		os.Exit(2)
	}
	changes, err := applyOnboardingResult(r, cfg, env, os.Stdout, func(c *config.Config) error { return c.Save() })
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack onboard: %v\n", err)
		os.Exit(1)
	}
	if len(changes) == 0 {
		fmt.Println("onboard: memory ensured; nothing else to change.")
	} else {
		for _, c := range changes {
			fmt.Printf("  + %s\n", c)
		}
	}
	if len(cfg.MCP) > 0 {
		if err := registerServers(cfg, env, os.Stdout, nil, hostBinaryResolver, activeContainerMCP(cfg)); err != nil {
			fmt.Printf("  mcp register skipped: %v (finish later: pi-stack mcp register)\n", err)
		}
	}
	onboardReportReadiness(env, os.Stdout)
}

// onboardReportReadiness prints the outstanding host prerequisites (missing
// model keys, gog auth) without prompting, then the next step.
func onboardReportReadiness(env shellEnv, out io.Writer) {
	sbxOut, sbxOK := "", false
	if _, err := env.lookPath("sbx"); err == nil {
		if o, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = o, true
		}
	}
	if sbxOK {
		anyKey := false
		for _, key := range []string{"anthropic", "openai", "google"} {
			if secretCheck(key, key, sbxOut, sbxOK).state() == stateOK {
				anyKey = true
			}
		}
		if !anyKey {
			fmt.Fprintln(out, `No model provider key set. Set one:  sbx secret set -g anthropic -t "sk-..."`)
		}
	}
	fmt.Fprintln(out, "Next:  pi-stack run   to start working, or  pi-stack setup  for the guided agent handoff.")
}

// confirmYN reads a [Y/n] answer. def is the answer for a bare Enter.
func confirmYN(in io.Reader, out io.Writer, prompt string, def bool) bool {
	fmt.Fprint(out, prompt)
	var line string
	fmt.Fscanln(in, &line)
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return def
	}
	return ans == "y" || ans == "yes"
}
