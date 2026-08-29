// pix models — the launcher side of the model router. `ls`/`show`/`pick`/
// `route` are thin passthroughs to the unchanged `pix-host route` subcommand
// tree (see execHostRoute); bare `pix models` is this file: a launcher-local,
// read-only, FACTS-ONLY status screen (E3.3) — MODEL/BACKEND/SOURCE, nothing
// scored: no WHY, no price, no wired/unwired/retired taxonomy. See
// docs/design/routing.md, docs/design/models-cli.md, docs/design/
// environments.md §6.3/§6.4 for the environment roster this screen also
// reads.

package main

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"pix/host/config"
	"pix/host/workflow/models"
)

// modelFactRow is one line of the bare status screen's table: a model this
// host either bound itself (machine config) or the selected environment
// declared as its own [[inference.models]] entry. Facts only — no score, no
// price, no verified/wired/unwired/retired classification.
type modelFactRow struct {
	Model, Backend, Source string
}

// machineConfigSource is the SOURCE value for a model cfg.Inference.Models
// itself bound (`pix models add`, `pix setup`) — as distinct from a model the
// selected environment declares under its own [inference.*].
const machineConfigSource = "machine config"

// modelFactRows lists every model declared by machine config, then (when an
// environment is selected) every model that environment's own pix.toml
// [[inference.models]] declares — sorted within each source for a
// deterministic render. Two sources can name the same model id; both rows
// are shown, since "declared by machine config or the selected environment"
// (E3.3's own scope) is a union, not a dedup — a reader deciding whether an
// environment's declaration merely repeats or actually adds to the machine's
// needs to see both.
func modelFactRows(cfg *config.Config, facts models.EnvironmentRosterFacts) []modelFactRow {
	var rows []modelFactRow
	bound := append([]config.InferenceModelBinding(nil), cfg.Inference.Models...)
	sort.Slice(bound, func(i, j int) bool {
		if bound[i].Model != bound[j].Model {
			return bound[i].Model < bound[j].Model
		}
		return bound[i].Backend < bound[j].Backend
	})
	for _, b := range bound {
		rows = append(rows, modelFactRow{Model: b.Model, Backend: b.Backend, Source: machineConfigSource})
	}
	if facts.Name != "" {
		ids := make([]string, 0, len(facts.LocalModels))
		for id := range facts.LocalModels {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		source := "environment: " + facts.Name
		for _, id := range ids {
			rows = append(rows, modelFactRow{Model: id, Backend: facts.LocalModels[id], Source: source})
		}
	}
	return rows
}

// renderModelsStatus writes the bare `pix models` screen to out. Read-only:
// no network probe, no write.
func renderModelsStatus(cfg *config.Config, facts models.EnvironmentRosterFacts, out io.Writer) {
	rows := modelFactRows(cfg, facts)
	if len(rows) == 0 {
		fmt.Fprintln(out, "(no models declared by machine config or the selected environment)")
		fmt.Fprintln(out, "pix models add <provider>   wire one in")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tBACKEND\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Model, r.Backend, r.Source)
	}
	tw.Flush()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "pix models add <provider>   wire one in")
}
