// Package negative is a planted fixture: none of the calls here are
// process-boundary bypasses, even though several deliberately LOOK like the
// bypasses the positive fixture proves are caught. None of them may show up
// in processBoundaryHits. See TestProcessBoundaryHits_NegativeFixtures.
package negative

import (
	"fmt"
	"io"
	"log"
	stdos "os"

	notlog "pix/host/testdata/processboundary/negative/notlog"
)

// fakeLogger is an UNRELATED type that happens to define methods named
// Fatal and Exit — the exact method names this guard bans on the real log
// and os packages. Calling them must never be confused with the standard
// library: the selector's X below is a local variable/param, never an
// identifier this file's import block bound to "log" or "os".
type fakeLogger struct{}

func (fakeLogger) Fatal(...interface{}) {}
func (fakeLogger) Exit(int)             {}

// CallsUnrelatedFatalMethod calls .Fatal on a type that is not the log
// package.
func CallsUnrelatedFatalMethod() {
	logger := fakeLogger{}
	logger.Fatal("not the real thing")
}

// ShadowedOsExit takes a PARAMETER named "os" of an unrelated type with its
// own Exit method — proving the ident.Obj != nil shadow check still works
// once resolution goes through a per-file alias table instead of a literal
// "os" string compare. Obj resolves non-nil here because "os" is declared in
// this function's own scope, not the package import.
func ShadowedOsExit(os fakeLogger) {
	os.Exit(1)
}

// AliasCollidesWithLogButIsNot aliases an UNRELATED package (defined in the
// sibling notlog fixture) to the name "log" and calls its Fatal function.
// This is the package-level version of the same proof: the guard matches an
// import by its real PATH ("log", the stdlib package), never by the local
// alias text, so a package that merely calls ITSELF "log" at the import site
// is never confused with the standard logger.
func AliasCollidesWithLogButIsNot() {
	notlog.Fatal("this never touches os.Exit")
}

// print and println are LOCAL functions of the same name as the two
// predeclared builtins this guard bans — proving the builtin check's
// ident.Obj != nil guard (a local func decl resolves to a non-nil Object)
// keeps a package that legitimately names a helper "print" from tripping the
// guard. LocalPrintFunc/LocalPrintlnFunc call them, not the builtins.
func print() {}

func println() {}

func LocalPrintFunc() {
	print()
}

func LocalPrintlnFunc() {
	println()
}

// UnrelatedBareFatal and UnrelatedBareExit are local, UNQUALIFIED functions
// literally named Fatal/Exit — "an unrelated local func" the task names
// directly. A bare call to either is a *ast.CallExpr whose Fun is a plain
// *ast.Ident, never a *ast.SelectorExpr, so it can never be mistaken for
// os.Exit or log.Fatal regardless of import aliasing.
func UnrelatedBareFatal(err error) {}

func UnrelatedBareExit(code int) {}

func CallsUnrelatedBareFuncs() {
	UnrelatedBareFatal(nil)
	UnrelatedBareExit(0)
}

// AllowedLogAndFmtCalls are exactly the log/fmt calls this guard leaves
// alone: log.Println/Printf never end the process (see this file's own "not
// in scope: log.Printf" carve-out), and fmt.Fprintln/Sprintf never touch
// os.Stdout because they take/return a writer or string instead of assuming
// one.
func AllowedLogAndFmtCalls(w io.Writer) {
	log.Println("just a log line")
	log.Printf("also fine: %d", 1)
	fmt.Fprintln(w, "explicit writer, not os.Stdout")
	_ = fmt.Sprintf("%d", 1)
}

// NamesOsStdin references os.Stdin — the one os identifier this guard
// deliberately never bans (see processExits' own comment) — through the
// aliased import "stdos", proving aliased-import support didn't accidentally
// widen processExits itself to cover Stdin too.
func NamesOsStdin() {
	_ = stdos.Stdin
}
