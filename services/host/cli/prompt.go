package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// IO is the three things an interactive command needs: where input comes from,
// where output goes, and whether a human is watching. It lives here because
// every interactive verb needs the same triple, and because a workflow that
// takes an IO instead of reaching for os.Stdin is one a test can drive.
type IO struct {
	In    io.Reader
	Out   io.Writer
	IsTTY bool
}

// PromptLine writes prompt and reads a single trimmed line.
//
// NOTE: it constructs a fresh bufio.Reader per call, so anything the previous
// call buffered past its newline is discarded. That is the behaviour the setup
// flow has always had and the fixtures encode; do not "fix" it to a persistent
// reader without checking what reads a multi-line answer.
func PromptLine(sio IO, prompt string) string {
	fmt.Fprint(sio.Out, prompt)
	line, _ := bufio.NewReader(sio.In).ReadString('\n')
	return strings.TrimSpace(line)
}
