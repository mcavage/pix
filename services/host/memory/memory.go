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

// Forget deletes one fact by id (or id prefix). A miss is reported as a miss,
// never as a deletion.
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
		return cli.WriteJSONOut(m.Out, res)
	}
	if ok {
		fmt.Fprintf(m.Out, "forgot %s\n", id)
	} else {
		fmt.Fprintf(m.Out, "no fact matched %q\n", id)
	}
	return nil
}

// Learnings lists the recurring lessons seen at least min times — the
// promotable set the `promote` skill reads.
func (m CLI) Learnings(min int, asJSON bool) error {
	res, err := m.Client.Call("promotable", map[string]any{"minFrequency": min, "profile": m.Profile})
	if err != nil {
		return err
	}
	cands := rpc.AsList(res["candidates"])
	if asJSON {
		return cli.WriteJSONOut(m.Out, map[string]any{"candidates": cands})
	}
	if len(cands) == 0 {
		fmt.Fprintf(m.Out, "(no learnings seen %d+ times)\n", min)
		return nil
	}
	for _, cn := range cands {
		freq := 0
		if f, ok := cn["frequency"].(float64); ok {
			freq = int(f)
		}
		fmt.Fprintf(m.Out, "%s  %s  (%dx)  %s\n", memoryTimestamp(rpc.Str(cn, "createdAt")), shortID(rpc.Str(cn, "id")), freq, rpc.Str(cn, "content"))
	}
	return nil
}

// Stats prints the daemon's counts by kind and durability.
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
	fmt.Fprintf(m.Out, "active %d  (durable %d, perishable %d)  facts %d  learnings %d  deleted %d\n",
		num("active"), num("durable"), num("perishable"), num("facts"), num("learnings"), num("deleted"))
	return nil
}

// memoryMeta renders the trailing "[kind·durability·project score]" annotation.
func memoryMeta(h map[string]any) string {
	var parts []string
	if k := rpc.Str(h, "kind"); k != "" {
		parts = append(parts, k)
	}
	if d := rpc.Str(h, "durability"); d != "" {
		parts = append(parts, d)
	}
	if p := rpc.Str(h, "project"); p != "" {
		parts = append(parts, p)
	}
	meta := strings.Join(parts, "·")
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
