// Package positive is a planted fixture: every reference in this package is
// exactly one of the process-boundary bypasses this guard closes, and every
// one of them must show up in processBoundaryHits. See
// TestProcessBoundaryHits_PositiveFixtures in processboundary_fixtures_test.go
// for the proof.
//
// Everything here is reached through an import ALIAS on purpose — the exact
// hole the original guard had, since it matched only the literal identifiers
// "os" and "fmt" and never looked at log or the two builtins at all.
package positive

import (
	l "log"
	o "os"
)

// AliasedOsExit calls os.Exit through an import alias.
func AliasedOsExit() {
	o.Exit(1)
}

// AliasedOsStdout and AliasedOsStderr name the two stream identifiers through
// the same alias.
func AliasedOsStdout() {
	_ = o.Stdout
}

func AliasedOsStderr() {
	_ = o.Stderr
}

// AliasedLogFatal, AliasedLogFatalf and AliasedLogFatalln call the three
// log.Fatal* funcs through an import alias. Each one ends the process (log
// calls os.Exit(1) internally after printing) exactly like AliasedOsExit
// above — just spelled through the standard logger, and aliased on top.
func AliasedLogFatal(err error) {
	l.Fatal(err)
}

func AliasedLogFatalf(err error) {
	l.Fatalf("%v", err)
}

func AliasedLogFatalln(err error) {
	l.Fatalln(err)
}

// BuiltinPrint and BuiltinPrintln call the two predeclared functions that
// write straight to os.Stderr with NO import at all — the shape the original
// guard's "does this file import os or fmt" gate never even considered.
func BuiltinPrint() {
	print("boom")
}

func BuiltinPrintln() {
	println("boom")
}
