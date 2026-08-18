package memory

import (
	"fmt"
	"io"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/rpc"
)

// The CLI verbs below are the memory daemon's read/write surface. They take
// what they need as ORDINARY PARAMETERS: `pix memory` is a kong tree in
// cmd/pix, so the flag set, its help and its arity are declared once by the
// struct that parses them — never restated here.
//
// What is NOT delegated upward: a blank query/content/id is still refused
// here, because "match everything" and "store nothing" are the daemon's
// problem, not the parser's.

// CLI is the daemon's command surface: the client, where to render, and the
// scope profile, bound once. It is the package's ONLY export because a kong
// tree in cmd/pix now owns the grammar these methods used to re-parse.
type CLI struct {
	Client  rpc.Client
	Out     io.Writer
	Profile string
}

// Recall searches stored facts, newest-scored first.
func (m CLI) Recall(query string, limit int, project string, asJSON bool) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return cli.UsageErr("usage: pix memory recall <query> [--limit N] [--project P] [--json]")
	}
	res, err := m.Client.Call("recall", map[string]any{"query": query, "limit": limit, "project": project, "profile": m.Profile})
	if err != nil {
		return err
	}
	hits := rpc.AsList(res["hits"])
	if asJSON {
		return cli.WriteJSONOut(m.Out, map[string]any{"hits": hits})
	}
	if len(hits) == 0 {
		fmt.Fprintln(m.Out, "(no matches)")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(m.Out, "%s  %s  %s%s\n", memoryTimestamp(rpc.Str(h, "createdAt")), shortID(rpc.Str(h, "id")), rpc.Str(h, "content"), memoryMeta(h))
	}
	return nil
}

// Remember stores one fact, reporting whether the daemon reaffirmed an
// existing one instead of creating a duplicate.
func (m CLI) Remember(content string, asJSON bool) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return cli.UsageErr("usage: pix memory remember <text...> [--json]")
	}
	res, err := m.Client.Call("remember", map[string]any{"content": content, "source": "cli", "profile": m.Profile})
	if err != nil {
		return err
	}
	if asJSON {
		return cli.WriteJSONOut(m.Out, res)
	}
	id := shortID(rpc.Str(res, "id"))
	if reaff, _ := res["reaffirmed"].(bool); reaff {
		fmt.Fprintf(m.Out, "reaffirmed %s\n", id)
	} else {
		fmt.Fprintf(m.Out, "remembered %s\n", id)
	}
	return nil
}

// Forget deletes one fact by id (or id prefix). A miss is a FAILURE (exit 1,
// diagnostic on stderr), never a silent no-op dressed up as success: a caller
// who asked to delete a specific id and got nothing needs that distinguishable
// from an actual deletion, by both exit code and stream.
//
// --json still prints the {"ok":false} result to Out (stdout stays parseable
// either way — a script piping `pix memory forget --json` never has to
// special-case a miss to get valid JSON), but the command still returns the
// error: dispatch's single exit mapper turns that into the honest exit 1, and
// its "pix: …" line lands on stderr same as the plain-text path.
func (m CLI) Forget(id string, asJSON bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return cli.UsageErr("usage: pix memory forget <id> [--json]")
	}
	res, err := m.Client.Call("forget", map[string]any{"id": id, "profile": m.Profile})
	if err != nil {
		return err
	}
	ok, _ := res["ok"].(bool)
	if asJSON {
		if err := cli.WriteJSONOut(m.Out, res); err != nil {
			return err
		}
	} else if ok {
		fmt.Fprintf(m.Out, "forgot %s\n", id)
	}
	if !ok {
		return fmt.Errorf("no fact matched %q", id)
	}
	return nil
}

// Stats prints the daemon's counts by kind. The durable/perishable split is
// gone end to end (host, plugin, and CLI): every row this binary writes is
// durable, so it was never a meaningful distinction to show.
//
// The kind the watcher writes for a rule the user stated ("stop using em
// dashes") is stored under the JSON key "learnings", which reads to a user
// as "things the agent learned" — vague, and easy to confuse with facts. The
// LABEL is therefore "corrections", which is what those rows actually are.
// The key is deliberately NOT renamed: it is the wire contract shared with
// the plugin, the RPC and --json consumers.
func (m CLI) Stats(asJSON bool) error {
	res, err := m.Client.Call("stats", map[string]any{"profile": m.Profile})
	if err != nil {
		return err
	}
	if asJSON {
		return cli.WriteJSONOut(m.Out, res)
	}
	num := func(k string) int {
		if f, ok := res[k].(float64); ok {
			return int(f)
		}
		return 0
	}
	fmt.Fprintf(m.Out, "active %d  facts %d  corrections %d  deleted %d\n",
		num("active"), num("facts"), num("learnings"), num("deleted"))
	return nil
}

// memoryMeta renders the trailing "[kind/project/auto score]" annotation.
// "auto" only appears for a watcher-sourced (experimental-auto capture) row,
// so an auto row is visibly distinguishable from an explicit one — the
// existing `/forget <id>` is the feedback/undo mechanism, this is only the
// visibility half. The separator is "/", matching the sandbox's own
// `/recall` render (extensions/memory-recall.ts's formatHitLine): the same
// row seen through two surfaces should not wear two different punctuations.
// durability is not rendered: the read side was retired (U9) once every row
// this binary writes became durable; the DB column stays, inert, for on-disk
// compatibility only.
func memoryMeta(h map[string]any) string {
	var parts []string
	if k := rpc.Str(h, "kind"); k != "" {
		parts = append(parts, k)
	}
	if p := rpc.Str(h, "project"); p != "" {
		parts = append(parts, p)
	}
	if rpc.Str(h, "source") == "watcher" {
		parts = append(parts, "auto")
	}
	meta := strings.Join(parts, "/")
	if sc, ok := h["score"].(float64); ok {
		if meta != "" {
			meta += " "
		}
		meta += fmt.Sprintf("%.2f", sc)
	}
	if meta == "" {
		return ""
	}
	return "  [" + meta + "]"
}

// memoryTimestamp parses a hit's stored createdAt (RFC3339 or RFC3339Nano,
// whatever the daemon returned) and renders it in the user's LOCAL time zone,
// ISO8601/RFC3339-formatted, for `recall`'s leading column — e.g.
// "2026-07-22T09:15:03-07:00". A hit with no timestamp (or one that fails to
// parse) gets a blank, column-aligned placeholder instead of crashing.
func memoryTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return strings.Repeat(" ", len(time.RFC3339))
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t, err = time.Parse(time.RFC3339, raw)
	}
	if err != nil {
		return strings.Repeat(" ", len(time.RFC3339))
	}
	return t.Local().Format(time.RFC3339)
}

// shortID trims a uuid to its first segment for readable listings.
func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// --- tiny shared flag parser (stdlib flag can't do "positional after flags"
// mixed with a trailing free-text arg cleanly, and we want a shared --json). It
// supports int/string/bool flags with optional short aliases, --k=v and -k v
// forms, a `--` terminator, and returns a cli.UsageError2 on any malformed input
// (unknown flag, missing value, non-integer) instead of silently ignoring it. ---
