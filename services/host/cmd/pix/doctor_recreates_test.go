// doctor_recreates_test.go — E2.6: `pix doctor --recreates` wiring, and the
// D20/D24 sentinel that this unit adds a `doctor` FLAG, never an eighth
// `env` verb.
package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"pix/host/cli"
)

// D20/D24: `pix doctor --recreates` is a flag on the existing `doctor`
// verb, not a new `pix env` verb. envCmd's dispatchable field count must
// stay exactly the accepted v2 four-verb surface (docs/design/
// pix-v2-surface.md §3.4): List/Show/Default/Trust. There is no
// add/edit/use/forget and no hidden `Rm` pointer — `pix env rm` is simply
// not a field at all, an unknown subcommand gets the ordinary dispatch
// error.
func TestDoctorRecreatesDoesNotAddAnEnvVerb(t *testing.T) {
	typ := reflect.TypeOf(envCmd{})
	if got, want := typ.NumField(), 4; got != want {
		t.Fatalf("envCmd has %d fields, want %d (List/Show/Default/Trust): %v", got, want, typ)
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Name == "Recreates" {
			t.Fatalf("envCmd gained a Recreates field; --recreates belongs on doctorCmd, not env")
		}
	}
}

// `pix doctor --recreates` at count zero: no probes run, exit 0, the file
// path named, never an error.
func TestDoctorCmd_RecreatesFlagZeroCount(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	c := &doctorCmd{Recreates: true}
	if err := c.Run(d); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "recreate records: 0") {
		t.Errorf("output = %q, want a zero-count recreate report", out.String())
	}
	if !strings.Contains(out.String(), "local only, never uploaded") {
		t.Errorf("output missing the local-only/deletable note: %q", out.String())
	}
}
