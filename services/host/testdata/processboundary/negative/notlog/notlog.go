// Package notlog is a planted decoy: it defines a Fatal function with the
// same name (and a log.Fatal-shaped signature) as the real log package,
// purely so lookalikes.go can import it under the alias "log" and call
// notlog.Fatal(...) through that alias — proving the guard matches an
// import by its real PATH, not the local alias text, so an unrelated
// package that merely calls itself "log" at the import site is never
// confused with the standard logger.
package notlog

// Fatal does nothing. It exists only to be callable as "log.Fatal" through
// an alias import that does not actually name the log package.
func Fatal(v ...interface{}) {}
