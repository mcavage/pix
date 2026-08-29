// pix agent — the subagent roster: `agent ls` prints AGENT/MODEL/SOURCE, facts
// only (E3.3): no WHY, no score. You manage AGENTS (their .md files, found by
// resolveAgentsDir — $PIX_AGENTS_DIR, beside the binary, or a repo checkout
// found by climbing from cwd, never a hard cwd requirement); an agent's MODEL
// comes from its own frontmatter `model:` (explicit, wins outright), else the
// machine's selected environment roster (its pix.toml [agents].<name>, else
// [models].main) — see resolveAgentSource and docs/design/environments.md
// §6.3/§6.4. No environment selected and no explicit model: means "(inherit
// parent)": this command never invokes intent-based routing to fill that gap.
//
// Authoring, editing, and removing an agent are hand-edits now (agents/*.md),
// not CLI mutations — see retired.go for the retired new/edit/rm/reassess
// surfaces and their guidance.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"pix/host/cli"
	"pix/host/workflow/launch"
	"pix/host/workflow/models"

	"gopkg.in/yaml.v3"
)

// isDir reports whether p exists and is a directory.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// resolveAgentsDir finds the roster directory WITHOUT depending on the
// caller's cwd (the old "./agents, run from the repo root" contract broke on
// a packaged install, which has no repo checkout, and even inside a
// checkout broke the moment you cd'd into a subdirectory).
//
// $PIX_AGENTS_DIR wins outright, and must exist — a typo there must fail
// loudly, not silently fall through to a different roster. Otherwise this
// tries beside the running binary (a packaged install's bundled copy, if the
// distribution ships one under a share/pix/agents-shaped path next to it),
// then climbs from cwd looking for a repo checkout's agents/ dir — a dev
// convenience, and now depth-independent: `pix agent ls` works from any
// subdirectory of a checkout, not only its root.
func resolveAgentsDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("PIX_AGENTS_DIR")); d != "" {
		if !isDir(d) {
			return "", fmt.Errorf("$PIX_AGENTS_DIR=%q does not exist", d)
		}
		return d, nil
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		bin := filepath.Dir(exe)
		for _, rel := range []string{"agents", filepath.Join("share", "pix", "agents"), filepath.Join("..", "share", "pix", "agents")} {
			if cand := filepath.Join(bin, rel); isDir(cand) {
				return cand, nil
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; {
			if cand := filepath.Join(dir, "agents"); isDir(cand) {
				return cand, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("no agent roster found: a packaged install (e.g. Homebrew) does not currently bundle one, " +
		"and no agents/ dir was found in this checkout or its parents; set $PIX_AGENTS_DIR, or run from inside a pix repo checkout")
}

// agentMeta is the typed view of an agent's frontmatter. It deliberately has
// no `Intent` field: E3.4's review fix removed the last dead parse of
// `intent:` here (it was never read by resolveAgentSource — nothing in this
// package ever routed by intent). A custom agent's `intent:` frontmatter now
// falls into yaml.Unmarshal's ordinary "key not in the struct" case, exactly
// like any other unrecognized field: parsed nowhere, held nowhere, shown
// nowhere.
type agentMeta struct {
	Description string  `yaml:"description,omitempty"`
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

// listAgents resolves the roster dir (see resolveAgentsDir) and returns the
// agent names found in it, plus the dir itself so a caller can build file
// paths without re-resolving (and re-risking a different answer).
func listAgents() (names []string, dir string, err error) {
	dir, err = resolveAgentsDir()
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, dir, fmt.Errorf("read agents dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	sort.Strings(names)
	return names, dir, nil
}

// SOURCE values agentLs prints — exactly the three the design calls for
// (explicit / roster / main) plus the neutral "no roster in effect" case, and
// nothing scored: no WHY, no beaten-alternative, no objective.
const (
	agentSourceExplicit = "explicit model:"
	agentSourceRoster   = "roster [agents]"
	agentSourceMain     = "roster main"
	agentSourceNone     = "no environment roster"
)

// resolveAgentSource is the one place `pix agent ls` decides an agent's MODEL
// and SOURCE. It never invokes intent-based routing (no scored pick, no
// fallback search): an agent's own frontmatter `model:` wins outright
// (agentSourceExplicit); otherwise the machine's selected environment roster
// answers — an authored `[agents].<name>` entry (agentSourceRoster) if the
// sidecar named this agent specifically, else `[models].main` if a roster is
// in effect at all (agentSourceMain); with no explicit model and no
// environment roster, the honest answer is agentSourceNone, MODEL "(inherit
// parent)" — the SAME phrase a launch-time "nothing to pin, inherit the
// session's model" resolution uses.
func resolveAgentSource(name string, m agentMeta, facts models.EnvironmentRosterFacts) (model, source string) {
	if strings.TrimSpace(m.Model) != "" {
		return m.Model, agentSourceExplicit
	}
	if facts.Name == "" {
		return "(inherit parent)", agentSourceNone
	}
	if v, ok := facts.Roster.Agents[name]; ok && strings.TrimSpace(v) != "" {
		return v, agentSourceRoster
	}
	if strings.TrimSpace(facts.Roster.Main) != "" {
		return facts.Roster.Main, agentSourceMain
	}
	return "(inherit parent)", agentSourceNone
}

// agentRow is one roster line: AGENT, MODEL, SOURCE — facts only.
type agentRow struct {
	Name, Model, Source string
}

func agentLs(d *cli.Deps, jsonOut bool) error {
	names, dir, err := listAgents()
	if err != nil {
		return err
	}
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	facts, err := models.ResolveEnvironmentRoster(cfg, names)
	if err != nil {
		return err
	}
	if err := models.ValidateRoster(cfg, facts); err != nil {
		return cli.UsageError{Err: err}
	}
	rows := make([]agentRow, 0, len(names))
	for _, n := range names {
		m, _, err := loadAgentMeta(filepath.Join(dir, n+".md"))
		if err != nil {
			// A malformed frontmatter block resolves nothing, so surfacing it as
			// plain "(inherit parent)" would hide a broken agents/*.md file behind
			// the same row a well-formed, roster-less agent gets. Name the error
			// instead so it's the first thing a roster read shows.
			rows = append(rows, agentRow{Name: n, Model: "(error)", Source: err.Error()})
			continue
		}
		model, source := resolveAgentSource(n, m, facts)
		rows = append(rows, agentRow{Name: n, Model: model, Source: source})
	}
	if jsonOut {
		return launch.PrintJSONLauncher(d.Out, rows)
	}
	tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tMODEL\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Model, r.Source)
	}
	tw.Flush()
	return nil
}
