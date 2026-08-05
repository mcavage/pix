package cli

// flagset.go is the hand-rolled argument parser three verbs still use
// (memory, knowledge, backup). It lives here because it is a CLI primitive and
// three packages need it, NOT because it is good.
//
// It is TRANSITIONAL and should shrink to nothing: every verb migrated to the
// kong contract (see RunRoot) drops its FlagSet use, and when the last one does,
// delete this file. Do not add features to it -- if a verb needs a flag shape
// this cannot express, that verb is ready to migrate.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"pix/host/rpc"
	"strconv"
	"strings"
)

type FlagSet struct {
	Json    bool
	jsonOK  bool // --json is a recognized flag only after enableJSON(); otherwise it is an unknown-flag usage error
	Help    bool // set when a -h/--help token is seen; caller prints usage + exit 0
	ints    map[string]*int
	strs    map[string]*string
	bools   map[string]*bool
	aliases map[string]string // short/alt name -> canonical name
}

func NewFlagSet() *FlagSet {
	return &FlagSet{ints: map[string]*int{}, strs: map[string]*string{}, bools: map[string]*bool{}, aliases: map[string]string{}}
}

// UsageErr / ExitFromErr classify errors into exit codes: usage (2), service
// down (3), generic (1).
type UsageError2 struct{ msg string }

func (e UsageError2) Error() string { return e.msg }
func UsageErr(msg string) error     { return UsageError2{msg} }

func ExitFromErr(ctx string, err error) {
	var exit *exec.ExitError
	switch {
	case err == rpc.ErrServiceDown:
		fmt.Fprintf(os.Stderr, "pix %s: service unreachable — start it with `pix serve`\n", ctx)
		os.Exit(rpc.ExitServiceDown)
	case IsUsage(err):
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	case errors.As(err, &exit):
		// A backup/restore child (pix-host) already printed its own diagnostic
		// to stderr; propagate its exit code without a duplicate message.
		os.Exit(exit.ExitCode())
	default:
		fmt.Fprintf(os.Stderr, "pix %s: %v\n", ctx, err)
		os.Exit(1)
	}
}
func IsUsage(err error) bool {
	_, ok := err.(UsageError2)
	return ok
}

// enableJSON registers the built-in --json flag for THIS command. Only commands
// that actually emit JSON call it; on every other command --json is rejected as
// an unknown flag (a usage error) rather than silently swallowed and ignored.
func (f *FlagSet) EnableJSON() { f.jsonOK = true }

func (f *FlagSet) Int(name string, def int, alt ...string) *int {
	p := new(int)
	*p = def
	f.ints[name] = p
	f.addAliases(name, alt)
	return p
}
func (f *FlagSet) Str(name, def string, alt ...string) *string {
	p := new(string)
	*p = def
	f.strs[name] = p
	f.addAliases(name, alt)
	return p
}
func (f *FlagSet) Bool(name string, alt ...string) *bool {
	p := new(bool)
	f.bools[name] = p
	f.addAliases(name, alt)
	return p
}
func (f *FlagSet) addAliases(name string, alt []string) {
	for _, a := range alt {
		f.aliases[a] = name
	}
}
func (f *FlagSet) canon(name string) string {
	if c, ok := f.aliases[name]; ok {
		return c
	}
	return name
}

// parse consumes registered flags (plus the built-in --json bool) and returns
// the remaining positional args in order, or a UsageError2 on malformed input.
func (f *FlagSet) Parse(argv []string) ([]string, error) {
	// Help wins over everything. Pre-scan for a -h/--help token (before any `--`
	// terminator) and, if present, set help + return immediately WITHOUT consuming
	// any value. Otherwise a value-taking flag would swallow the help token as its
	// value (`sync -m --help` -> -m="--help") and proceed to the side-effecting
	// action instead of printing usage.
	if WantsHelp(argv) {
		f.Help = true
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
				return nil, UsageErr(fmt.Sprintf("unknown flag %q", a))
			}
			f.Json = true
			continue
		}
		if name == "help" || name == "h" {
			f.Help = true
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
			return "", UsageErr(fmt.Sprintf("flag %q needs a value", a))
		}
		if p, ok := f.ints[name]; ok {
			v, err := takeVal()
			if err != nil {
				return nil, err
			}
			n, cerr := strconv.Atoi(v)
			if cerr != nil {
				return nil, UsageErr(fmt.Sprintf("flag %q needs an integer, got %q", a, v))
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
		return nil, UsageErr(fmt.Sprintf("unknown flag %q", a))
	}
	return pos, nil
}

// WriteJSONOut prints v as indented JSON.
func WriteJSONOut(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
