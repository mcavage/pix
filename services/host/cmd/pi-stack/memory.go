package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, memoryUsage)
		os.Exit(2)
	}
	sub, rest := argv[0], argv[1:]
	if err := dispatchMemory(sub, rest, memoryClient(), os.Stdout); err != nil {
		exitFromErr("memory "+sub, err)
	}
}

const memoryUsage = `usage: pi-stack memory <recall|remember|forget|learnings|stats> [args]

  recall <query> [--limit N] [--project P] [--json]   search stored facts
  remember <text...> [--json]                          store a fact
  forget <id>                                          delete a fact by id/prefix
  learnings [--min N] [--json]                          recurring learnings (promotable)
  stats [--json]                                        counts by kind/durability`

// dispatchMemory is the testable core: it runs one subcommand against an
// injected client + writer and returns an error (instead of exiting).
func dispatchMemory(sub string, argv []string, c rpcClient, out io.Writer) error {
	switch sub {
	case "recall", "search":
		return memoryRecall(argv, c, out)
	case "remember", "add":
		return memoryRemember(argv, c, out)
	case "forget", "rm":
		return memoryForget(argv, c, out)
	case "learnings", "promotable":
		return memoryLearnings(argv, c, out)
	case "stats", "status":
		return memoryStats(argv, c, out)
	default:
		return usageErr(fmt.Sprintf("memory: unknown subcommand %q\n%s", sub, memoryUsage))
	}
}

func memoryRecall(argv []string, c rpcClient, out io.Writer) error {
	fs := newFlagSet()
	limit := fs.int("limit", 8, "n")
	project := fs.str("project", "", "p")
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		return usageErr("usage: pi-stack memory recall <query> [--limit N] [--project P] [--json]")
	}
	res, err := c.Call("recall", map[string]any{"query": query, "limit": *limit, "project": *project})
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

func memoryRemember(argv []string, c rpcClient, out io.Writer) error {
	fs := newFlagSet()
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	content := strings.TrimSpace(strings.Join(positional, " "))
	if content == "" {
		return usageErr("usage: pi-stack memory remember <text...>")
	}
	res, err := c.Call("remember", map[string]any{"content": content, "source": "cli"})
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

func memoryForget(argv []string, c rpcClient, out io.Writer) error {
	fs := newFlagSet()
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usageErr("usage: pi-stack memory forget <id>")
	}
	res, err := c.Call("forget", map[string]any{"id": positional[0]})
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

func memoryLearnings(argv []string, c rpcClient, out io.Writer) error {
	fs := newFlagSet()
	min := fs.int("min", 3)
	positional, perr := fs.parse(argv)
	if perr != nil {
		return perr
	}
	if len(positional) > 0 {
		return usageErr("usage: pi-stack memory learnings [--min N] [--json]")
	}
	res, err := c.Call("promotable", map[string]any{"minFrequency": *min})
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

func memoryStats(argv []string, c rpcClient, out io.Writer) error {
	fs := newFlagSet()
	positional, perr := fs.parse(argv)
	if perr != nil {
		return perr
	}
	if len(positional) > 0 {
		return usageErr("usage: pi-stack memory stats [--json]")
	}
	res, err := c.Call("stats", nil)
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
	ints    map[string]*int
	strs    map[string]*string
	bools   map[string]*bool
	aliases map[string]string // short/alt name -> canonical name
}

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
			f.json = true
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
	switch {
	case err == errServiceDown:
		fmt.Fprintf(os.Stderr, "pi-stack %s: service unreachable — start it with `pi-stack serve`\n", ctx)
		os.Exit(exitServiceDown)
	case isUsage(err):
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "pi-stack %s: %v\n", ctx, err)
		os.Exit(1)
	}
}

func isUsage(err error) bool {
	_, ok := err.(usageError)
	return ok
}
