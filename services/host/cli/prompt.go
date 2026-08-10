package cli

import "io"

// IO is the three things an interactive command needs: where input comes from,
// where output goes, and whether a human is watching. It lives here because
// every interactive verb needs the same triple, and because a workflow that
// takes an IO instead of reaching for os.Stdin is one a test can drive.
type IO struct {
	In    io.Reader
	Out   io.Writer
	IsTTY bool
}
