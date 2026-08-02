package slack

import (
	"io"

	"pix/host/config"
	"pix/host/hostenv"
)

// registerOK is the RegisterFn these tests pass where registration is expected
// to succeed. Slack takes registration as a parameter now, so a test that does
// not care about it says so in one word instead of faking an sbx gateway.
func registerOK(*config.Config, hostenv.Env, io.Writer, []string, func() (string, error)) error {
	return nil
}

// registerRecorder is registerOK that remembers what it was asked to register,
// for the two tests whose subject IS "did setup register?". They used to read
// that off the fake's sbx call list; now that registration is a parameter, the
// parameter is the honest place to observe it.
func registerRecorder(got *[]string) RegisterFn {
	return func(_ *config.Config, _ hostenv.Env, _ io.Writer, names []string, _ func() (string, error)) error {
		*got = append(*got, names...)
		return nil
	}
}
