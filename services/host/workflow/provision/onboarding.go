// onboarding.go — the HOST half of the agentic first-run flow
// (docs/design/onboarding.md), plus `pix setup`'s argv parser. The interactive
// experience lives INSIDE a pix sandbox: the agent writes identity to the memory
// service (data plane) and PROPOSES host config by writing
// <workspace>/.pix/onboarding.json (control plane). Here is the schema, its
// allowlist validation, the applier and the reconcile-on-next-run path. `pix
// setup` is the only caller, and it reuses provision's own composition
// (HostBinary, Register, VerifyCatalogMCPReady).
//
// SECURITY MODEL: the sandbox is network-fenced and non-root and can only PROPOSE
// config within OnboardingResult's fixed field set. Unmarshalling into a typed
// struct means a hostile file CANNOT reach host.enabled, plugins.*, kits.stack or
// arbitrary services — those fields do not exist here. mcp names are additionally
// allowlisted, and the host applies under a confirm gate.
package provision

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/workspace"
)

// OnboardingResult is the declarative proposal the in-sandbox agent writes.
// Identity is deliberately ABSENT: it is memory data, written live over the
// memory RPC, not host config.
type OnboardingResult struct {
	Version            int      `json:"version"`
	MCP                []string `json:"mcp,omitempty"`
	OllamaBridgeModel  string   `json:"ollama_bridge_model,omitempty"`
	MemoryWatcherModel string   `json:"memory_watcher_model,omitempty"`
}

// OnboardingFileName is the per-workspace proposal the agent writes and the host
// consumes on the next run.
const OnboardingFileName = "onboarding.json"

// validateOnboardingShape checks only what is true regardless of which pack is
// active: the schema version and the syntactic form of each name. It exists
// because `pix setup` must validate its own flags BEFORE the first pack is
// adopted — at that moment no pack has declared anything, so rejecting an
// undeclared name would reject exactly the case setup is there to enable.
// Name admissibility is checked later, by validateOnboarding, once a pack is
// present to answer the question.
func validateOnboardingShape(r *OnboardingResult) error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported onboarding schema version %d (want 1)", r.Version)
	}
	for _, m := range r.MCP {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("empty mcp name")
		}
		if strings.ContainsAny(m, " \t\n\r") {
			return fmt.Errorf("mcp %q must not contain whitespace", m)
		}
	}
	return nil
}

// validateOnboarding rejects anything outside the allowlist BEFORE it touches
// config. The allowlist is exactly two things: a name the ACTIVE PACK declares,
// and a curated catalog endpoint pix already knows the URL for. Both are pure
// data, so an in-sandbox agent's proposal is checked without running anything.
func validateOnboarding(r *OnboardingResult, declared map[string]config.MCPServer) error {
	if err := validateOnboardingShape(r); err != nil {
		return err
	}
	for _, m := range r.MCP {
		m = strings.TrimSpace(m)
		// The accepted remotes ARE mcp.McpCatalogNames — the single source of
		// truth for the hosted endpoints pix knows — read directly, never
		// copied, so the list cannot drift into a name pix cannot register.
		if _, ok := declared[m]; ok || mcp.McpCatalogNames[m] {
			continue
		}
		return fmt.Errorf("mcp %q is not declared by the active pack and is not a known catalog server; "+
			"activate the pack that provides it, or configure it explicitly with `pix mcp add %s --url <url>`", m, m)
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

// applyOnboarding applies a VALIDATED proposal onto cfg through the same setters
// the CLI uses and returns the human-readable changes. It does NOT save: the
// caller picks preview (a copy) or commit. Idempotent.
//
// There is deliberately NO per-server credential writer. A config that claims
// an integration works while nothing behind it is authorized is the failure
// mode this whole surface was rebuilt to prevent: declaring a server is not
// the same as it working, and only its own probe can say which.
func applyOnboarding(r *OnboardingResult, cfg *config.Config) []string {
	var changes []string
	for _, m := range r.MCP {
		if m = strings.TrimSpace(m); cfg.AddMCP(m) {
			changes = append(changes, "enabled "+m+" (mcp)")
		}
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
	sort.Strings(changes)
	return changes
}

// ReconcileOnboarding reads <workspace>/.pix/onboarding.json if present,
// validates it, shows the diff, applies under a [Y/n] gate (assumeYes skips the
// prompt for CI), registers any newly-enabled MCP servers, then removes the
// file. Absent file is a clean no-op. A refusal leaves the file in place and
// warns (so a human can inspect it) but never aborts the caller.
func ReconcileOnboarding(ws string, env hostenv.Env, in io.Reader, out io.Writer, assumeYes, tty bool) {
	path := filepath.Join(ws, ".pix", OnboardingFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return // absent (or unreadable) => nothing to reconcile
	}
	var r OnboardingResult
	if err := json.Unmarshal(data, &r); err != nil {
		fmt.Fprintf(out, "pix: ignoring malformed %s: %v\n", path, err)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pix: could not load config to apply onboarding: %v\n", err)
		return
	}
	refuse := func(err error, tail string) {
		fmt.Fprintf(out, "pix: refusing onboarding proposal in %s: %v\n", path, err)
		fmt.Fprintln(out, tail)
	}
	// No pack declares servers in v2 (the pack system was deleted — AC-16); an
	// onboarding proposal's MCP names are validated against the shipped
	// catalog alone.
	if err := validateOnboarding(&r, nil); err != nil {
		refuse(err, "  Inspect and remove it by hand if it is not what you intended.")
		return
	}
	// A shipped-catalog remote must be registered AND auth-ready BEFORE
	// anything is persisted — never applied on the promise that someone will
	// run bundle/auth later.
	if err := VerifyCatalogMCPReady(env, r.MCP); err != nil {
		refuse(err, "  Nothing was applied; the file was left in place.")
		return
	}

	// Preview: apply against a COPY to compute the diff without committing.
	preview := *cfg
	changes := applyOnboarding(&r, &preview)
	if len(changes) == 0 {
		_ = workspace.RemoveStateFile(ws, OnboardingFileName) // nothing new; clear the marker
		return
	}
	fmt.Fprintln(out, "Onboarding proposed these host-config changes:")
	for _, c := range changes {
		fmt.Fprintf(out, "  + %s\n", c)
	}
	if !assumeYes {
		if !tty {
			fmt.Fprintf(out, "Not a terminal; leaving %s for review. Apply with: pix setup --apply --yes\n", path)
			return
		}
		if !cli.ConfirmYN(in, out, "Apply these changes? [Y/n]: ", true) {
			fmt.Fprintf(out, "Left %s in place; not applied.\n", path)
			return
		}
	}

	applied := applyOnboarding(&r, cfg)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "pix: applied partially then failed: saving config: %v\n", err)
		return
	}
	if len(cfg.MCP) > 0 {
		// An unwired registrar is REPORTED, never assumed successful: the point
		// of this line is that pix never claims a registration it did not do.
		regErr := fmt.Errorf("no MCP registrar wired")
		if Injected.Register != nil {
			regErr = Injected.Register(cfg, env, out, nil, nil)
		}
		if regErr != nil {
			fmt.Fprintf(out, "  mcp add skipped: %v (finish later: pix mcp add)\n", regErr)
		}
	}
	_ = workspace.RemoveStateFile(ws, OnboardingFileName)
	fmt.Fprintf(out, "Applied %d onboarding change(s) to %s.\n", len(applied), config.Path())
}

// Opts is `pix setup`'s host-phase flag set. kong owns the user-facing grammar and
// hands the host phase the argv its flags COMPOSE TO, so this parser accepts what
// setup_cmd.go emits and nothing else. Retired spellings are answered by the
// command layer, never a second time here.
type Opts struct {
	Mcp        []string
	Model      string
	Apply      bool
	AssumeYes  bool
	PullModels bool // `pix setup`'s explicit local-model download consent (S08)
	Packs      []string
	WithSetup  []string
}

// ParseSetupArgs parses the host phase's argv. --models is ACCEPTED AND
// DISCARDED: kong still declares it, so refusing it here would turn a no-op
// into "unknown flag", but nothing reads it (`pix models` owns roster
// restriction) — so it gets no field to pretend otherwise.
//
// --google-workspace and --credentials are gone entirely, along with the kong
// flags that fed them. They named a vendor transaction that no longer exists in
// core: Google Workspace is an ordinary pack-declared MCP server now, so a
// hidden flag promising to "route setup through the Google Workspace
// transaction" was an offer pix could not honour.
func ParseSetupArgs(argv []string) (Opts, error) {
	var o Opts
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
		case a == "--apply":
			o.Apply = true
		case a == "--yes" || a == "-y" || a == "--non-interactive":
			o.AssumeYes = true
		case a == "--pull-models":
			o.PullModels = true
		case a == "--models":
			_, err = next()
		case strings.HasPrefix(a, "--models="):
		case a == "--mcp":
			var v string
			if v, err = next(); err == nil {
				o.Mcp = append(o.Mcp, v)
			}
		case strings.HasPrefix(a, "--mcp="):
			o.Mcp = append(o.Mcp, strings.TrimPrefix(a, "--mcp="))
		case a == "--model":
			o.Model, err = next()
		case strings.HasPrefix(a, "--model="):
			o.Model = strings.TrimPrefix(a, "--model=")
		case a == "--pack":
			var v string
			if v, err = next(); err == nil {
				o.Packs = append(o.Packs, v)
			}
		case strings.HasPrefix(a, "--pack="):
			o.Packs = append(o.Packs, strings.TrimPrefix(a, "--pack="))
		case a == "--with":
			var v string
			if v, err = next(); err == nil {
				o.WithSetup = append(o.WithSetup, v)
			}
		case strings.HasPrefix(a, "--with="):
			o.WithSetup = append(o.WithSetup, strings.TrimPrefix(a, "--with="))
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}
