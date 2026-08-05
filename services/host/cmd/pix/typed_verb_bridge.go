package main

// typed_verb_bridge.go is the LAST passthrough seam for mcp, pack and memory,
// and it exists only until root.go adopts their typed trees as fields.
//
// DELETE THIS FILE with the root integration (U11d). In root.go:
//
//	Memory memoryCmd `cmd:"" group:"Data" aliases:"mem" help:"recall | remember | forget | learnings | stats."`
//	Pack   packCmd   `cmd:"" group:"Data" help:"new | add | ls | show | use | rm."`
//	Mcp    mcpCmd    `cmd:"" group:"Integrations & credentials" help:"register | ls | load | auth | bundle."`
//
// ...replacing the three `passthrough:""` legacy fields, and delete
// legacyMcpCmd / legacyPackCmd / legacyMemoryCmd with their Run methods. Then
// this file, whose only reason to exist is that the three verbs are typed
// while the field that reaches them is not.
//
// It is ONE seam rather than three because there is one thing to say: parse
// the verb's argv against its tree, and let the root's own exit mapping
// (cli.ExitCode) decide the code. The root's legacyForward wrapper cannot
// return an exit code, so the process exit happens here for a failure, exactly
// as the three deleted argv switches did.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"pix/host/cli"
)

// The verb's prose comes from its Help() method (as it does for every typed
// root child), so kong is handed NO description here — passing one would print
// the same paragraphs twice.
func runMcpCmd(argv []string)  { runTypedVerb[mcpCmd]("pix mcp", argv) }
func runPackCmd(argv []string) { runTypedVerb[packCmd]("pix pack", argv) }
func runMemory(argv []string)  { runTypedVerb[memoryCmd]("pix memory", argv) }

// runTypedVerb parses argv against one verb's typed tree. Help goes to stdout
// (every typed verb's does), and a failure exits with the code the root would
// have produced for the same error.
func runTypedVerb[T any](name string, argv []string) {
	d := newRootDeps()
	err := cli.RunRoot[T](name, "", "", argv, d)
	if err == nil {
		return
	}
	var silent cli.SilentError
	if !errors.As(err, &silent) {
		fmt.Fprintf(d.Err, "pix: %v\n", err)
	}
	os.Exit(cli.ExitCode(err))
}

// typedVerbUsage renders a typed verb's generated usage into a string, for
// help.go's verbUsage table — which exists to answer `pix help <verb>` for a
// verb the root still reaches through a passthrough field. A typed ROOT CHILD
// needs no entry: runHelp re-enters the root as `<verb> --help`. Delete the
// three entries with this file.
func typedVerbUsage[T any](name string) string {
	var b strings.Builder
	d := &cli.Deps{Out: &b, Err: &b}
	_ = cli.RunRoot[T](name, "", "", []string{"--help"}, d)
	return b.String()
}
