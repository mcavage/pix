// pi-stack-host route — the model router CLI (host side). Resolves a declared
// INTENT to a concrete model from the registry + scorecard (route pick),
// compiles the full intent->model map the sandbox reads (route compile), and
// prints the current tables (route show / models). See docs/design/routing.md.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"pi-stack/host/routing"
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
	fmt.Fprint(os.Stderr, `pi-stack-host route — model router

usage:
  route pick <intent> [--json]   resolve one intent to a model (+ rationale)
  route compile [--out PATH]      write the intent->model map (routing.json)
  route show [--json]             registry + scorecard + resolved table
  route models [--json]           list the model registry

Truth files (disk override, else embedded default):
  `+routing.ModelsPath()+`
  `+routing.ScorecardPath()+`
  `+routing.PolicyPath()+`
`)
}

// loadAll loads the three truth sources or dies with a clear message.
func loadAll() (*routing.Registry, *routing.Scorecard, *routing.Policy) {
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
	return reg, sc, pol
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
	reg, sc, pol := loadAll()
	intent, ok := pol.Intent(name)
	if !ok {
		// Allow an ad-hoc intent by task type: `route pick code` still works even
		// if there is no named intent, defaulting to accuracy with no ceilings.
		intent = routing.Intent{Name: name, TaskType: name, Objective: "accuracy"}
	}
	d := routing.Resolve(reg, sc, pol, intent)
	if hasFlag(args, "--json") {
		printJSON(d)
		return
	}
	flag := ""
	if !d.ConstraintsMet {
		flag = "  [constraints not met — fallback]"
	}
	fmt.Printf("%s -> %s%s\n", d.Intent, d.Model, flag)
	fmt.Printf("  %s\n", d.Reason)
	if len(d.Alternatives) > 1 {
		fmt.Println("  alternatives:")
		for _, c := range d.Alternatives {
			fmt.Printf("    %-28s acc %.2f  $%.4f  %.0fms  (%s)\n", c.ID, c.Accuracy, c.CostUSD, c.LatencyMs, c.Source)
		}
	}
}

func routeCompile(args []string) {
	reg, sc, pol := loadAll()
	cr := routing.Compile(reg, sc, pol, time.Now())
	out := flagValue(args, "--out", "")
	if out == "" {
		out = routing.CompiledRoutingPath()
	}
	if err := routing.WriteCompiled(out, cr); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s (%d routes)\n", out, len(cr.Routes))
	for name, r := range cr.Routes {
		flag := ""
		if !r.ConstraintsMet {
			flag = " [fallback]"
		}
		fmt.Printf("  %-14s -> %s%s\n", name, r.Model, flag)
	}
}

func routeShow(args []string) {
	reg, sc, pol := loadAll()
	if hasFlag(args, "--json") {
		printJSON(map[string]any{"registry": reg, "scorecard": sc, "policy": pol})
		return
	}
	fmt.Println("== models ==")
	printModels(reg)
	fmt.Println("\n== resolved intents ==")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INTENT\tOBJECTIVE\tMODEL\tACC\tCOST\tLATENCY\tOK")
	for _, in := range pol.Intents {
		d := routing.Resolve(reg, sc, pol, in)
		acc, cost, lat := "-", "-", "-"
		if d.Chosen != nil {
			acc = fmt.Sprintf("%.2f", d.Chosen.Accuracy)
			cost = fmt.Sprintf("$%.4f", d.Chosen.CostUSD)
			lat = fmt.Sprintf("%.0fms", d.Chosen.LatencyMs)
		}
		ok := "yes"
		if !d.ConstraintsMet {
			ok = "FALLBACK"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", in.Name, d.Objective, d.Model, acc, cost, lat, ok)
	}
	tw.Flush()
}

func routeModels(args []string) {
	reg, _, _ := loadAll()
	if hasFlag(args, "--json") {
		printJSON(reg)
		return
	}
	printModels(reg)
}

func printModels(reg *routing.Registry) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tPROVIDER\tIN $/Mtok\tOUT $/Mtok\tLOCAL\tAVAIL")
	for _, m := range reg.Models {
		fmt.Fprintf(tw, "%s\t%s\t%.2f\t%.2f\t%v\t%v\n", m.ID, m.Provider, m.InputPerMTok, m.OutputPerMTok, m.Local, m.Available)
	}
	tw.Flush()
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
