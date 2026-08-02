package cli

import "errors"

// ErrHelpRequested is the sentinel a command returns when argv asked for help
// (a leading -h/--help). Callers print the relevant usage to STDOUT and exit 0,
// distinguishing a help REQUEST from a usage ERROR (stderr, exit 2).
var ErrHelpRequested = errors.New("help requested")
