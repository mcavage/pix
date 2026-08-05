// pix-host route — the model router CLI (host side). Resolves a declared
// INTENT to a concrete model from the registry + scorecard (route pick),
// compiles the full intent->model map the agent reads (route compile), and
// prints the current tables (route show / models). See docs/design/routing.md.
//
// Everything here answers about THIS HOST by default: the catalog says what a
// model IS, but only a probed backend binding says what this host can CALL, and a
// route compiled to a provider with no key is a guaranteed call-time failure. See
// package pix/host/inference.
//
// `--catalog` restores the host-independent view on every subcommand. It is for
// ONE job: baking the image's default routing.json in a maintainer checkout,
// where filtering by the maintainer's personal keys would be exactly wrong.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/routing"
)

func runRouteHost(args []string) {
	if len(args) < 1 {
		routeUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "pick":
		routePick(args[1:])
	case "compile":
		routeCompile(args[1:])
	case "show":
		routeShow(args[1:])
	case "models":
		routeModels(args[1:])
	case "-h", "--help", "help":
		routeUsage()
	default:
		fmt.Fprintf(os.Stderr, "route: unknown subcommand %q\n\n", args[0])
		routeUsage()
		os.Exit(2)
	}
}

func routeUsage() {
	fmt.Fprint(os.Stderr, `pix-host route: model router

usage:
  route pick <intent> [--json]   resolve one intent to a model (+ rationale)
  route compile [--out PATH]      write the intent->model map (routing.json)
  route show [--json]             registry + scorecard + resolved table
  route models [--json]           list the model registry

Every subcommand describes THIS HOST: the shipped catalog narrowed to the
models a probed backend binding makes callable here. Add --catalog to see the
shipped catalog itself, host-independent (maintainer: baking the image default).

Truth files (disk override, else embedded default):
  `+routing.ModelsPath()+`
  `+routing.ScorecardPath()+`
  `+routing.PolicyPath()+`
`)
}

// routeView is the resolved answer to "what is this command talking about": the
// three truth files, plus whether the registry was narrowed to this host's
// callable bindings. Every subcommand takes one, so none can silently disagree
// with the others about what "available" means.
type routeView struct {
	reg *routing.Registry
	sc  *routing.Scorecard
	pol *routing.Policy
	cfg *config.Config
	// bound is true when reg has been narrowed to probed bindings. False means the
	// caller is looking at the raw catalog — and MUST say so, because the three ways
	// to get here read very differently: they asked for the catalog, nothing is
	// wired yet, or config.toml would not load.
	bound bool
	// catalogOnly records that --catalog was passed, so an explicit catalog request
	// is not reported as the scary "config could not be read".
	catalogOnly bool
}

// loadView loads the three truth sources and narrows them to this host, or dies
// with a clear message. A config that will not load is NOT fatal — failing the
// whole command would hide the router behind an unrelated problem — so it
// degrades to the catalog and says so through bound=false.
func loadView(args []string) routeView {
	reg, err := routing.LoadRegistry()
	if err != nil {
		fatal(err)
	}
	sc, err := routing.LoadScorecard()
	if err != nil {
		fatal(err)
	}
	pol, err := routing.LoadPolicy()
	if err != nil {
		fatal(err)
	}
	v := routeView{reg: reg, sc: sc, pol: pol}
	if hasFlag(args, "--catalog") {
		v.catalogOnly = true
		return v
	}
	cfg, err := config.Load()
	if err != nil {
		return v
	}
	v.cfg = cfg
	v.reg, v.bound = inference.BoundRegistry(cfg, reg)
	return v
}

// scope is the one-line header every human-facing subcommand prints, so the
// numbers below it are never ambiguous about which world they describe.
func (v routeView) scope() string {
	switch {
	case v.bound:
		return fmt.Sprintf("this host — %d of %d catalog models are wired and callable", v.wiredCount(), len(v.reg.Models))
	case v.catalogOnly:
		return "--catalog: the shipped catalog, host-independent"
	case v.cfg == nil:
		return "the shipped catalog (host view unavailable: config could not be read)"
	default:
		return "the shipped catalog — nothing is wired on this host yet, so no model here is proven callable"
	}
}

func (v routeView) wiredCount() int {
	n := 0
	for _, m := range v.reg.Models {
		if m.Available {
			n++
		}
	}
	return n
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "route: %v\n", err)
	os.Exit(1)
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// flagValue returns the value following --name, or def.
func flagValue(args []string, name, def string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return def
}

func routePick(args []string) {
	var name string
	for _, a := range args {
		if a != "" && a[0] != '-' {
			name = a
			break
		}
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "route pick: missing <intent>")
		os.Exit(2)
	}
	v := loadView(args)
	intent, ok := v.pol.Intent(name)
	if !ok {
		// Allow an ad-hoc intent by task type: `route pick code` still works even
		// if there is no named intent, defaulting to accuracy with no ceilings.
		intent = routing.Intent{Name: name, TaskType: name, Objective: "accuracy"}
	}
	d := routing.Resolve(v.reg, v.sc, v.pol, intent)
	if hasFlag(args, "--json") {
		printJSON(d)
		return
	}
	flag := ""
	if !d.ConstraintsMet {
		flag = "  [constraints not met; fallback]"
	}
	fmt.Printf("%s -> %s%s\n", d.Intent, d.Model, flag)
	fmt.Printf("  %s\n", d.Reason)
	fmt.Printf("  scope: %s\n", v.scope())
	if len(d.Alternatives) > 1 {
		fmt.Println("  alternatives:")
		for _, c := range d.Alternatives {
			fmt.Printf("    %-28s acc %.2f  $%.4f  %.0fms  (%s)\n", c.ID, c.Accuracy, c.CostUSD, c.LatencyMs, c.Source)
		}
	}
}

func routeCompile(args []string) {
	v := loadView(args)
	// Validate the TRUTH FILES, not the narrowed view: a host that wired one
	// provider has an "unavailable" majority by design, which is not a config error.
	// Internal consistency is a property of the catalog alone.
	catalog, err := routing.LoadRegistry()
	if err != nil {
		fatal(err)
	}
	if err := routing.Validate(catalog, v.sc, v.pol); err != nil {
		fatal(fmt.Errorf("config invalid, refusing to compile: %w", err))
	}
	cr := routing.Compile(v.reg, v.sc, v.pol, time.Now())
	if v.bound {
		// Rewrite each route to the id its backend actually answers to, and DROP
		// intents with no callable binding. A dropped intent makes subagents
		// inherit the parent model, which is a working degradation; a retained one
		// pointing at an unwired provider is a guaranteed call-time failure.
		cr = routing.MaterializeBindings(cr, inference.Bindings(v.cfg), "")
	}
	out := flagValue(args, "--out", "")
	if out == "" {
		out = routing.CompiledRoutingPath()
	}
	if err := routing.WriteCompiled(out, cr); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s (%d routes)\n", out, len(cr.Routes))
	fmt.Printf("  scope: %s\n", v.scope())
	for name, r := range cr.Routes {
		flag := ""
		if !r.ConstraintsMet {
			flag = " [fallback]"
		}
		fmt.Printf("  %-14s -> %s%s\n", name, r.Model, flag)
	}
	// Naming the dropped intents is the whole point of dropping them: silence here
	// would read as "every intent routed".
	if dropped := droppedIntents(v.pol, cr); len(dropped) > 0 {
		fmt.Printf("\n%d intent(s) have no callable model on this host and were left out:\n  %v\n",
			len(dropped), dropped)
		fmt.Println("  Agents declaring them inherit the parent model. `pix models add <provider>` to wire one.")
	}
	if !v.bound && !v.catalogOnly {
		fmt.Println("\n!  Nothing is wired on this host, so this map is the catalog's, not yours.")
		fmt.Println("   Run `pix setup` (or `pix models add <provider>`), then compile again.")
	}
}

func droppedIntents(pol *routing.Policy, cr routing.CompiledRouting) []string {
	var out []string
	for _, in := range pol.Intents {
		if _, ok := cr.Routes[in.Name]; !ok {
			out = append(out, in.Name)
		}
	}
	return out
}

// Reference workload for the real-dollar cost estimate. One representative agent
// turn: enough input to carry real context, a modest completion. It is a
// COMPARISON unit across models (same tokens for every row), not a prediction of
// your bill. Priced from each model's actual per-Mtok rates via Model.CostFor.
const (
	refInputTokens  = 30_000
	refOutputTokens = 3_000
)

// estRunCost formats the real-dollar cost of the reference workload for a model.
// Local (Ollama/DMR) models are unmetered, so they read "free".
func estRunCost(m routing.Model) string {
	if m.Local {
		return "free"
	}
	return fmt.Sprintf("$%.4f", m.CostFor(refInputTokens, refOutputTokens))
}

// modelStatus is the honest one-word answer to "can I use this model", and it
// needs BOTH inputs: the CATALOG says whether Pix still routes to a model at all
// (available:false means RETIRED — see the notes in models.json), and the
// BINDINGS say whether this host can call it.
func modelStatus(catalog routing.Model, wired, bound bool) string {
	if !catalog.Available {
		return "retired"
	}
	if !bound {
		return "in catalog"
	}
	if wired {
		return "wired"
	}
	return "unwired"
}

func routeShow(args []string) {
	v := loadView(args)
	if hasFlag(args, "--json") {
		printJSON(map[string]any{"registry": v.reg, "scorecard": v.sc, "policy": v.pol, "host_bound": v.bound})
		return
	}
	fmt.Printf("== models ==  (%s)\n", v.scope())
	printModels(v)
	fmt.Printf("\n== resolved intents ==  (est/run priced at %dk in + %dk out per turn)\n",
		refInputTokens/1000, refOutputTokens/1000)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// COST(score) is the scorecard's abstract cost heuristic used to RANK models;
	// EST/RUN is the real dollar cost of the reference workload from live prices.
	fmt.Fprintln(tw, "INTENT\tOBJECTIVE\tMODEL\tACC\tCOST(score)\tEST/RUN\tLATENCY\tOK")
	var unpreferred []string
	for _, in := range v.pol.Intents {
		d := routing.Resolve(v.reg, v.sc, v.pol, in)
		if !d.PreferenceMet {
			unpreferred = append(unpreferred, fmt.Sprintf("%s (prefers %s)", in.Name, strings.Join(in.PreferProviders, ", ")))
		}
		acc, cost, est, lat := "-", "-", "-", "-"
		if d.Chosen != nil {
			acc = fmt.Sprintf("%.2f", d.Chosen.Accuracy)
			cost = fmt.Sprintf("%.2f", d.Chosen.CostUSD)
			lat = fmt.Sprintf("%.0fms", d.Chosen.LatencyMs)
		}
		if m, ok := v.reg.Get(d.Model); ok {
			est = estRunCost(m)
		}
		ok := "yes"
		if !d.ConstraintsMet {
			ok = "FALLBACK"
		}
		// A route whose model is not callable here is not a route. Compile drops
		// it; show has to agree, or the two commands describe different stacks.
		if m, found := v.reg.Get(d.Model); v.bound && (!found || !m.Available) {
			ok = "UNROUTABLE"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", in.Name, d.Objective, d.Model, acc, cost, est, lat, ok)
	}
	tw.Flush()
	// A missed vendor preference is INFORMATION, not a warning: these routes are
	// fully valid, they just could not reach the vendor the policy would have picked
	// first. Reporting it in the OK column would make a working default install read
	// as broken on its most important route.
	if len(unpreferred) > 0 {
		fmt.Printf("\n%d intent(s) resolved off their preferred vendor — valid routes, no key needed:\n  %s\n",
			len(unpreferred), strings.Join(unpreferred, ", "))
		fmt.Println("  `pix models add <provider>` if you want the preference honored.")
	}
	if !v.bound && !v.catalogOnly {
		fmt.Println("\n!  Nothing is wired on this host yet — the table above is the shipped catalog,")
		fmt.Println("   not a promise that any of it is callable. Start with `pix setup`.")
	}
}

func routeModels(args []string) {
	v := loadView(args)
	if hasFlag(args, "--json") {
		printJSON(v.reg)
		return
	}
	fmt.Printf("(%s)\n", v.scope())
	printModels(v)
}

func printModels(v routeView) {
	// The catalog is reloaded rather than carried alongside: STATUS needs the
	// UNNARROWED availability bit to tell "retired from the catalog" apart from "not
	// wired here", and v.reg has already overwritten it.
	catalog, err := routing.LoadRegistry()
	if err != nil {
		catalog = v.reg
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tPROVIDER\tIN $/Mtok\tOUT $/Mtok\tEST/RUN\tLOCAL\tSTATUS")
	for _, m := range v.reg.Models {
		cm, ok := catalog.Get(m.ID)
		if !ok {
			cm = m
		}
		fmt.Fprintf(tw, "%s\t%s\t%.2f\t%.2f\t%s\t%v\t%s\n",
			m.ID, m.Provider, m.InputPerMTok, m.OutputPerMTok, estRunCost(m), m.Local,
			modelStatus(cm, m.Available, v.bound))
	}
	tw.Flush()
	if v.bound {
		fmt.Println("\nSTATUS  wired = a probed backend binding can call it here · unwired = in the")
		fmt.Println("        catalog, not wired on this host (`pix models add <provider>`) ·")
		fmt.Println("        retired = Pix no longer routes to it")
	}
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
