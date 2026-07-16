package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"pi-stack/host/config"
)

// runMemory is the `memory` verb tree — the host-side CLI over the memory daemon
// (:11435), so you can inspect and repair the agent's recall WITHOUT launching a
// sandbox:
//
//	pi-stack memory recall <query> [--limit N] [--project P] [--json]
//	pi-stack memory remember <text...> [--json]
//	pi-stack memory forget <id>
//	pi-stack memory learnings [--min N] [--json]   (recurring learnings)
//	pi-stack memory stats [--json]
//
// Every verb degrades cleanly when the daemon is down: an actionable message on
// stderr + exit code 3 (exitServiceDown), distinct from usage (2) / generic (1).
func runMemory(argv []string) {
	ctx := "memory"
	if len(argv) > 0 && argv[0] != "-h" && argv[0] != "--help" {
		ctx = "memory " + argv[0]
	}
	if err := runMemoryCore(argv, loadResolvedConfig, memoryClient, os.Stdout); err != nil {
		exitFromErr(ctx, err)
	}
}

// runMemoryCore is the testable core of runMemory. It dispatches the memory
// subcommand, resolving config LAZILY so a -h/--help request prints usage even
// when config is broken or names an unknown profile (help must be
// config-independent). load + client are injected so tests can feed a failing
// loader and prove help still works. It returns an error (nil on success/help)
// instead of calling os.Exit; runMemory classifies the error into an exit code.
func runMemoryCore(argv []string, load func() (*config.Config, string, error), client func() rpcClient, out io.Writer) error {
	if len(argv) == 0 {
		return usageErr(memoryUsage)
	}
	// `memory -h` / `memory --help` (no subcommand): print usage, exit 0.
	if argv[0] == "-h" || argv[0] == "--help" {
		fmt.Fprintln(out, memoryUsage)
		return nil
	}
	sub, rest := argv[0], argv[1:]
	// A `<sub> --help` prints the subcommand usage BEFORE any config load: dispatch
	// with a zero client + empty profile, which dispatchMemory only reaches after
	// printing usage (fs.help short-circuits before any RPC), so neither is used.
	if wantsHelp(rest) {
		return dispatchMemory(sub, rest, rpcClient{}, out, "")
	}
	// Resolve the active profile so host-side memory ops are scoped the same way
	// the in-VM extensions are (recall sees {profile}∪{default}; captures stamp
	// the active profile). FAIL LOUD on a config/profile error: silently falling
	// back to "default" would store a `--profile wrok` fact in the shared default
	// bucket (and recall/forget the wrong bucket). Never RPC with a fallback.
	_, profile, err := load()
	if err != nil {
		return err
	}
	return dispatchMemory(sub, rest, client(), out, profile)
}

const memoryUsage = `usage: pi-stack memory <recall|remember|forget|learnings|stats> [args]

  recall <query> [--limit N] [--project P] [--json]   search stored facts
  remember <text...> [--json]                          store a fact
  forget <id> [--json]                                 delete a fact by id/prefix
  learnings [--min N] [--json]                          recurring learnings (promotable)
  stats [--json]                                        counts by kind/durability

Backup/restore are now TOP-LEVEL verbs (they cover config + op-refs + memory):
  pi-stack backup [--out PATH] [--keep N]
  pi-stack restore <archive> [--force]`

// dispatchMemory is the testable core: it runs one subcommand against an
// injected client + writer and returns an error (instead of exiting).
func dispatchMemory(sub string, argv []string, c rpcClient, out io.Writer, profile string) error {
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
		return usageErr(fmt.Sprintf("memory: unknown subcommand %q\n%s", sub, memoryUsage))
	}
}

func memoryRecall(argv []string, c rpcClient, out io.Writer, profile string) error {
	fs := newFlagSet()
	fs.enableJSON()
	limit := fs.int("limit", 8, "n")
	project := fs.str("project", "", "p")
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	const usage = "usage: pi-stack memory recall <query> [--limit N] [--project P] [--json]"
	if fs.help {
		fmt.Fprintln(out, usage)
		return nil
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		return usageErr(usage)
	}
	res, err := c.Call("recall", map[string]any{"query": query, "limit": *limit, "project": *project, "profile": profile})
	if err != nil {
		return err
	}
	hits := asList(res["hits"])
	if fs.json {
		return writeJSONOut(out, map[string]any{"hits": hits})
	}
	if len(hits) == 0 {
		fmt.Fprintln(out, "(no matches)")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(out, "%s  %s%s\n", shortID(str(h, "id")), str(h, "content"), memoryMeta(h))
	}
	return nil
}

func memoryRemember(argv []string, c rpcClient, out io.Writer, profile string) error {
	fs := newFlagSet()
	fs.enableJSON()
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	if fs.help {
		fmt.Fprintln(out, "usage: pi-stack memory remember <text...> [--json]")
		return nil
	}
	content := strings.TrimSpace(strings.Join(positional, " "))
	if content == "" {
		return usageErr("usage: pi-stack memory remember <text...>")
	}
	res, err := c.Call("remember", map[string]any{"content": content, "source": "cli", "profile": profile})
	if err != nil {
		return err
	}
	if fs.json {
		return writeJSONOut(out, res)
	}
	id := shortID(str(res, "id"))
	if reaff, _ := res["reaffirmed"].(bool); reaff {
		fmt.Fprintf(out, "reaffirmed %s\n", id)
	} else {
		fmt.Fprintf(out, "remembered %s\n", id)
	}
	return nil
}

func memoryForget(argv []string, c rpcClient, out io.Writer, profile string) error {
	fs := newFlagSet()
	fs.enableJSON()
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	if fs.help {
		fmt.Fprintln(out, "usage: pi-stack memory forget <id> [--json]")
		return nil
	}
	if len(positional) != 1 {
		return usageErr("usage: pi-stack memory forget <id> [--json]")
	}
	res, err := c.Call("forget", map[string]any{"id": positional[0], "profile": profile})
	if err != nil {
		return err
	}
	ok, _ := res["ok"].(bool)
	if fs.json {
		return writeJSONOut(out, res)
	}
	if ok {
		fmt.Fprintf(out, "forgot %s\n", positional[0])
	} else {
		fmt.Fprintf(out, "no fact matched %q\n", positional[0])
	}
	return nil
}

func memoryLearnings(argv []string, c rpcClient, out io.Writer, profile string) error {
	fs := newFlagSet()
	fs.enableJSON()
	min := fs.int("min", 3)
	positional, perr := fs.parse(argv)
	if perr != nil {
		return perr
	}
	if fs.help {
		fmt.Fprintln(out, "usage: pi-stack memory learnings [--min N] [--json]")
		return nil
	}
	if len(positional) > 0 {
		return usageErr("usage: pi-stack memory learnings [--min N] [--json]")
	}
	res, err := c.Call("promotable", map[string]any{"minFrequency": *min, "profile": profile})
	if err != nil {
		return err
	}
	cands := asList(res["candidates"])
	if fs.json {
		return writeJSONOut(out, map[string]any{"candidates": cands})
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
		fmt.Fprintf(out, "%s  (%dx)  %s\n", shortID(str(cn, "id")), freq, str(cn, "content"))
	}
	return nil
}

func memoryStats(argv []string, c rpcClient, out io.Writer, profile string) error {
	fs := newFlagSet()
	fs.enableJSON()
	positional, perr := fs.parse(argv)
	if perr != nil {
		return perr
	}
	if fs.help {
		fmt.Fprintln(out, "usage: pi-stack memory stats [--json]")
		return nil
	}
	if len(positional) > 0 {
		return usageErr("usage: pi-stack memory stats [--json]")
	}
	res, err := c.Call("stats", map[string]any{"profile": profile})
	if err != nil {
		return err
	}
	if fs.json {
		return writeJSONOut(out, res)
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
	if k := str(h, "kind"); k != "" {
		parts = append(parts, k)
	}
	if d := str(h, "durability"); d != "" {
		parts = append(parts, d)
	}
	if p := str(h, "project"); p != "" {
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
// forms, a `--` terminator, and returns a usageError on any malformed input
// (unknown flag, missing value, non-integer) instead of silently ignoring it. ---

type flagSet struct {
	json    bool
	jsonOK  bool // --json is a recognized flag only after enableJSON(); otherwise it is an unknown-flag usage error
	help    bool // set when a -h/--help token is seen; caller prints usage + exit 0
	ints    map[string]*int
	strs    map[string]*string
	bools   map[string]*bool
	aliases map[string]string // short/alt name -> canonical name
}

// enableJSON registers the built-in --json flag for THIS command. Only commands
// that actually emit JSON call it; on every other command --json is rejected as
// an unknown flag (a usage error) rather than silently swallowed and ignored.
func (f *flagSet) enableJSON() { f.jsonOK = true }

func newFlagSet() *flagSet {
	return &flagSet{ints: map[string]*int{}, strs: map[string]*string{}, bools: map[string]*bool{}, aliases: map[string]string{}}
}

func (f *flagSet) int(name string, def int, alt ...string) *int {
	p := new(int)
	*p = def
	f.ints[name] = p
	f.addAliases(name, alt)
	return p
}

func (f *flagSet) str(name, def string, alt ...string) *string {
	p := new(string)
	*p = def
	f.strs[name] = p
	f.addAliases(name, alt)
	return p
}

func (f *flagSet) bool(name string, alt ...string) *bool {
	p := new(bool)
	f.bools[name] = p
	f.addAliases(name, alt)
	return p
}

func (f *flagSet) addAliases(name string, alt []string) {
	for _, a := range alt {
		f.aliases[a] = name
	}
}

func (f *flagSet) canon(name string) string {
	if c, ok := f.aliases[name]; ok {
		return c
	}
	return name
}

// parse consumes registered flags (plus the built-in --json bool) and returns
// the remaining positional args in order, or a usageError on malformed input.
func (f *flagSet) parse(argv []string) ([]string, error) {
	// Help wins over everything. Pre-scan for a -h/--help token (before any `--`
	// terminator) and, if present, set help + return immediately WITHOUT consuming
	// any value. Otherwise a value-taking flag would swallow the help token as its
	// value (`sync -m --help` -> -m="--help") and proceed to the side-effecting
	// action instead of printing usage.
	if wantsHelp(argv) {
		f.help = true
		return nil, nil
	}
	var pos []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			pos = append(pos, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		// A flag token: strip one or two leading dashes, split --k=v.
		trimmed := strings.TrimLeft(a, "-")
		name, inlineVal, hasInline := strings.Cut(trimmed, "=")
		name = f.canon(name)

		if name == "json" {
			if !f.jsonOK {
				return nil, usageErr(fmt.Sprintf("unknown flag %q", a))
			}
			f.json = true
			continue
		}
		if name == "help" || name == "h" {
			f.help = true
			continue
		}
		if p, ok := f.bools[name]; ok {
			*p = true
			continue
		}
		takeVal := func() (string, error) {
			if hasInline {
				return inlineVal, nil
			}
			if i+1 < len(argv) {
				i++
				return argv[i], nil
			}
			return "", usageErr(fmt.Sprintf("flag %q needs a value", a))
		}
		if p, ok := f.ints[name]; ok {
			v, err := takeVal()
			if err != nil {
				return nil, err
			}
			n, cerr := strconv.Atoi(v)
			if cerr != nil {
				return nil, usageErr(fmt.Sprintf("flag %q needs an integer, got %q", a, v))
			}
			*p = n
			continue
		}
		if p, ok := f.strs[name]; ok {
			v, err := takeVal()
			if err != nil {
				return nil, err
			}
			*p = v
			continue
		}
		return nil, usageErr(fmt.Sprintf("unknown flag %q", a))
	}
	return pos, nil
}

// writeJSONOut prints v as indented JSON.
func writeJSONOut(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// usageErr / exitFromErr classify errors into exit codes: usage (2), service
// down (3), generic (1).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
func usageErr(msg string) error    { return usageError{msg} }

func exitFromErr(ctx string, err error) {
	var exit *exec.ExitError
	switch {
	case err == errServiceDown:
		fmt.Fprintf(os.Stderr, "pi-stack %s: service unreachable — start it with `pi-stack serve`\n", ctx)
		os.Exit(exitServiceDown)
	case isUsage(err):
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	case errors.As(err, &exit):
		// A backup/restore child (pi-stack-host) already printed its own diagnostic
		// to stderr; propagate its exit code without a duplicate message.
		os.Exit(exit.ExitCode())
	default:
		fmt.Fprintf(os.Stderr, "pi-stack %s: %v\n", ctx, err)
		os.Exit(1)
	}
}

func isUsage(err error) bool {
	_, ok := err.(usageError)
	return ok
}
