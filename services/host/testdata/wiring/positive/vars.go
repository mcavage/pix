package positive

import (
	"fmt"

	"pix/host/cli"
)

// FuncLiteral is the plainest positive case: a function literal assigned
// directly to a package var.
var FuncLiteral = func() {}

// VarBoundToFunc is a bare (uncalled) reference to a function declared
// elsewhere in this same package (helpers.go) — the "identifier bound to a
// func" shape.
var VarBoundToFunc = realFunc

// VarBoundToSelectorFunc is a bare reference to an exported stdlib function
// through a selector — the "selector resolving to a func" shape, resolved by
// parsing the real fmt package under GOROOT, not by guessing from the
// SelectorExpr's shape.
var VarBoundToSelectorFunc = fmt.Sprintln

// CliSelectorFunc references a real top-level func in another package of
// THIS module (pix/host/cli), proving the guard resolves a sibling-package
// selector, not just a stdlib one.
var CliSelectorFunc = cli.Usagef

// ExplicitFuncTypeVar has no initializer at all; its declared TYPE is a
// function type, which is itself the seam shape — a zero-value func var is
// still a substitutable dependency slot.
var ExplicitFuncTypeVar func(int) string

// Handler is a named type whose UNDERLYING type is a function type.
type Handler func(string) error

// NamedFuncTypeVar is declared with a named type ALIASING a function type,
// not literal `func(...)` syntax — proving the guard resolves through a type
// declaration instead of only recognizing FuncType syntax directly.
var NamedFuncTypeVar Handler

// ChainedVarBoundToFunc points at another package var (VarBoundToFunc) that
// is itself function-valued — proving the guard follows a chain of
// identifiers, not just one hop.
var ChainedVarBoundToFunc = VarBoundToFunc
