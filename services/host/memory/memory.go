package memory

import (
	"fmt"
	"io"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/rpc"
)

// Run moved to cmd/pix as runMemory. It was this package's composition root:
// it reached for the service capability (to auto-start the daemon) and the
// workspace capability (to resolve config), which is a capability calling two
// siblings -- the exact thing arch_test.go forbids, and it caught it.
//
// RunCore below was ALREADY inverted: it takes its config loader and its client
// as parameters. Only the wrapper that chose them needed to move up, which is
// what "invert it into the workflow" means in practice.

// RunCore is the testable core of Run. It dispatches the memory
// subcommand, resolving config LAZILY so a -h/--help request prints usage even
// when config is broken (help must be config-independent). load + client are
// injected so tests can feed a failing loader and prove help still works. It
// returns an error (nil on success/help) instead of calling os.Exit; Run
// classifies the error into an exit code.
func RunCore(argv []string, load func() (*config.Config, string, error), client func() rpc.Client, out io.Writer) error {
	if len(argv) == 0 {
		return cli.UsageErr(Usage)
	}
	// `memory -h` / `memory --help` (no subcommand): print usage, exit 0.
	if argv[0] == "-h" || argv[0] == "--help" {
		fmt.Fprintln(out, Usage)
		return nil
	}
	sub, rest := argv[0], argv[1:]
	// A `<sub> --help` prints the subcommand usage BEFORE any config load: dispatch
	// with a zero client + empty profile, which Dispatch only reaches after
	// printing usage (fs.Help short-circuits before any RPC), so neither is used.
	if cli.WantsHelp(rest) {
		return Dispatch(sub, rest, rpc.Client{}, out, "")
	}
	// Load config to surface a config error up front rather than proceeding with a
	// fallback (config.Load() can still fail on malformed TOML). The second return
	// is always "" now — profiles were removed; the memory daemon's scope column
	// is retained but dormant — threaded through unchanged so Dispatch's
	// signature stays stable.
	_, profile, err := load()
	if err != nil {
		return err
	}
	return Dispatch(sub, rest, client(), out, profile)
}

const Usage = `usage: pix memory <recall|remember|forget|learnings|stats> [args]

  recall <query> [--limit N] [--project P] [--json]   search stored facts
  remember <text...> [--json]                          store a fact
  forget <id> [--json]                                 delete a fact by id/prefix
  learnings [--min N] [--json]                          recurring learnings (promotable)
  stats [--json]                                        counts by kind/durability

Backup/restore are now TOP-LEVEL verbs (they cover config + op-refs + memory):
  pix backup [--out PATH] [--keep N]
  pix restore <archive> [--force]`

// Dispatch is the testable core: it runs one subcommand against an
// injected client + writer and returns an error (instead of exiting).
func Dispatch(sub string, argv []string, c rpc.Client, out io.Writer, profile string) error {
	switch sub {
	case "recall", "search":
		return memoryRecall(argv, c, out, profile)
	case "remember", "add":
		return memoryRemember(argv, c, out, profile)
	case "forget", "rm":
		return memoryForget(argv, c, out, profile)
	case "learnings", "promotable":
		return memoryLearnings(argv, c, out, profile)
	case "stats", "status":
		return memoryStats(argv, c, out, profile)
	default:
		return cli.UsageErr(fmt.Sprintf("memory: unknown subcommand %q\n%s", sub, Usage))
	}
}

func memoryRecall(argv []string, c rpc.Client, out io.Writer, profile string) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	limit := fs.Int("limit", 8, "n")
	project := fs.Str("project", "", "p")
	positional, err := fs.Parse(argv)
	if err != nil {
		return err
	}
	const usage = "usage: pix memory recall <query> [--limit N] [--project P] [--json]"
	if fs.Help {
		fmt.Fprintln(out, usage)
		return nil
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		return cli.UsageErr(usage)
	}
	res, err := c.Call("recall", map[string]any{"query": query, "limit": *limit, "project": *project, "profile": profile})
	if err != nil {
		return err
	}
	hits := rpc.AsList(res["hits"])
	if fs.Json {
		return cli.WriteJSONOut(out, map[string]any{"hits": hits})
	}
	if len(hits) == 0 {
		fmt.Fprintln(out, "(no matches)")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(out, "%s  %s  %s%s\n", memoryTimestamp(rpc.Str(h, "createdAt")), shortID(rpc.Str(h, "id")), rpc.Str(h, "content"), memoryMeta(h))
	}
	return nil
}

func memoryRemember(argv []string, c rpc.Client, out io.Writer, profile string) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	positional, err := fs.Parse(argv)
	if err != nil {
		return err
	}
	if fs.Help {
		fmt.Fprintln(out, "usage: pix memory remember <text...> [--json]")
		return nil
	}
	content := strings.TrimSpace(strings.Join(positional, " "))
	if content == "" {
		return cli.UsageErr("usage: pix memory remember <text...>")
	}
	res, err := c.Call("remember", map[string]any{"content": content, "source": "cli", "profile": profile})
	if err != nil {
		return err
	}
	if fs.Json {
		return cli.WriteJSONOut(out, res)
	}
	id := shortID(rpc.Str(res, "id"))
	if reaff, _ := res["reaffirmed"].(bool); reaff {
		fmt.Fprintf(out, "reaffirmed %s\n", id)
	} else {
		fmt.Fprintf(out, "remembered %s\n", id)
	}
	return nil
}

func memoryForget(argv []string, c rpc.Client, out io.Writer, profile string) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	positional, err := fs.Parse(argv)
	if err != nil {
		return err
	}
	if fs.Help {
		fmt.Fprintln(out, "usage: pix memory forget <id> [--json]")
		return nil
	}
	if len(positional) != 1 {
		return cli.UsageErr("usage: pix memory forget <id> [--json]")
	}
	res, err := c.Call("forget", map[string]any{"id": positional[0], "profile": profile})
	if err != nil {
		return err
	}
	ok, _ := res["ok"].(bool)
	if fs.Json {
		return cli.WriteJSONOut(out, res)
	}
	if ok {
		fmt.Fprintf(out, "forgot %s\n", positional[0])
	} else {
		fmt.Fprintf(out, "no fact matched %q\n", positional[0])
	}
	return nil
}

func memoryLearnings(argv []string, c rpc.Client, out io.Writer, profile string) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	min := fs.Int("min", 3)
	positional, perr := fs.Parse(argv)
	if perr != nil {
		return perr
	}
	if fs.Help {
		fmt.Fprintln(out, "usage: pix memory learnings [--min N] [--json]")
		return nil
	}
	if len(positional) > 0 {
		return cli.UsageErr("usage: pix memory learnings [--min N] [--json]")
	}
	res, err := c.Call("promotable", map[string]any{"minFrequency": *min, "profile": profile})
	if err != nil {
		return err
	}
	cands := rpc.AsList(res["candidates"])
	if fs.Json {
		return cli.WriteJSONOut(out, map[string]any{"candidates": cands})
	}
	if len(cands) == 0 {
		fmt.Fprintf(out, "(no learnings seen %d+ times)\n", *min)
		return nil
	}
	for _, cn := range cands {
		freq := 0
		if f, ok := cn["frequency"].(float64); ok {
			freq = int(f)
		}
		fmt.Fprintf(out, "%s  %s  (%dx)  %s\n", memoryTimestamp(rpc.Str(cn, "createdAt")), shortID(rpc.Str(cn, "id")), freq, rpc.Str(cn, "content"))
	}
	return nil
}

func memoryStats(argv []string, c rpc.Client, out io.Writer, profile string) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	positional, perr := fs.Parse(argv)
	if perr != nil {
		return perr
	}
	if fs.Help {
		fmt.Fprintln(out, "usage: pix memory stats [--json]")
		return nil
	}
	if len(positional) > 0 {
		return cli.UsageErr("usage: pix memory stats [--json]")
	}
	res, err := c.Call("stats", map[string]any{"profile": profile})
	if err != nil {
		return err
	}
	if fs.Json {
		return cli.WriteJSONOut(out, res)
	}
	num := func(k string) int {
		if f, ok := res[k].(float64); ok {
			return int(f)
		}
		return 0
	}
	fmt.Fprintf(out, "active %d  (durable %d, perishable %d)  facts %d  learnings %d  deleted %d\n",
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
