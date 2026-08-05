// Package negative is a planted fixture: NONE of its package-level vars are
// function-valued, and none may show up in funcValuedGlobalHits. Every case
// here is a shape the ORIGINAL AST-only guard got wrong or could not check at
// all — see TestFuncValuedGlobalHits_PlantedFixtures in
// wiring_fixtures_test.go, which proves it clean now.
package negative

import (
	"errors"
	"io"
	"regexp"
	"time"

	"pix/host/cli"
)

// TimeConstSelector is time.Second: a stdlib CONST selector of a data type
// (time.Duration). The original guard flagged ANY `pkg.Name` selector whose
// package qualifier was a bare identifier, with no check of what it actually
// named — this is exactly the false positive that produced.
var TimeConstSelector = time.Second

// DiscardVarSelector is io.Discard: a stdlib VAR selector whose value is a
// concrete data type implementing io.Writer, not a function. Also a false
// positive under the original guard.
var DiscardVarSelector = io.Discard

// CliSelectorNonFunc is a real exported var in another package of this
// module (pix/host/cli) whose value is the RESULT of calling errors.New, not
// a function — proving cross-package-in-module resolution correctly says
// "not a func", not just "any cross-package selector is a hit".
var CliSelectorNonFunc = cli.ErrHelpRequested

// PlainInt and StringSlice are ordinary literal data: no selector, no
// identifier chase needed, the baseline "never flagged" case.
var PlainInt = 42
var StringSlice = []string{"a", "b"}

// CompiledPattern and SentinelErr are the RESULT of calling a function
// (regexp.MustCompile, errors.New) — a CallExpr, which this guard has always
// correctly left alone: it is ordinary construction, not a substitutable
// seam.
var CompiledPattern = regexp.MustCompile("^ok$")
var SentinelErr = errors.New("boom")

// dataVar is a plain package-level int; AliasToData is a bare identifier
// bound to it — proving a same-package identifier alias to non-func data
// stays unflagged (the "identifiers bound to funcs" catch must not also
// catch identifiers bound to DATA).
var dataVar = 7
var AliasToData = dataVar
