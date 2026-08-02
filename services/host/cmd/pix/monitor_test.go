package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"pix/host/monitor"
	"pix/host/monitor/tui"
)

// TestMonitorHelp is the help gate: `-h`/`--help` prints monitorUsage and
// never constructs a hub or runs the TUI — proven by fakes that panic if
// called, mirroring memory_test.go's TestRunMemoryCore_HelpIgnoresBrokenConfig
// pattern.
func TestMonitorHelp(t *testing.T) {
	panicNewHub := func(monitor.HubConfig) *monitor.Hub {
		panic("runMonitorCore must not construct a hub on --help")
	}
	panicRunTUI := func(tui.TUIConfig) error {
		panic("runMonitorCore must not run the TUI on --help")
	}

	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--help", "mybox"}} {
		var out bytes.Buffer
		if err := runMonitorCore(argv, panicNewHub, panicRunTUI, &out, io.Discard); err != nil {
			t.Fatalf("runMonitorCore(%v): %v", argv, err)
		}
		if !strings.Contains(out.String(), "usage: pix monitor") {
			t.Errorf("runMonitorCore(%v) = %q, want monitor usage", argv, out.String())
		}
	}
}

// TestMonitorFlagParse proves --port and the optional positional name reach
// HubConfig unchanged, and that the same name is forwarded to the TUI's
// Filter. The fake newHub captures the config it was given but hands back a
// REAL Hub bound to an ephemeral port (Port:0) so the test never touches the
// literal port 9999 or a real terminal.
func TestMonitorFlagParse(t *testing.T) {
	var gotCfg monitor.HubConfig
	fakeNewHub := func(cfg monitor.HubConfig) *monitor.Hub {
		gotCfg = cfg
		return monitor.NewHub(monitor.HubConfig{Port: 0})
	}
	var gotTUICfg tui.TUIConfig
	fakeRunTUI := func(cfg tui.TUIConfig) error {
		gotTUICfg = cfg
		return nil
	}

	var out bytes.Buffer
	if err := runMonitorCore([]string{"--port", "9999", "mybox"}, fakeNewHub, fakeRunTUI, &out, io.Discard); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if gotCfg.Port != 9999 {
		t.Errorf("HubConfig.Port = %d, want 9999", gotCfg.Port)
	}
	if gotCfg.Filter != "mybox" {
		t.Errorf("HubConfig.Filter = %q, want %q", gotCfg.Filter, "mybox")
	}
	if gotCfg.BindAddr != monitor.DefaultBindAddr {
		t.Errorf("HubConfig.BindAddr = %q, want default %q", gotCfg.BindAddr, monitor.DefaultBindAddr)
	}
	if gotTUICfg.Filter != "mybox" {
		t.Errorf("tui.TUIConfig.Filter = %q, want %q", gotTUICfg.Filter, "mybox")
	}
	if gotTUICfg.Events == nil {
		t.Error("tui.TUIConfig.Events is nil, want the hub's subscriber channel")
	}
	if gotTUICfg.Blob == nil {
		t.Error("tui.TUIConfig.Blob is nil, want the hub's blob lookup")
	}
	if gotTUICfg.Port != 9999 {
		t.Errorf("tui.TUIConfig.Port = %d, want the resolved --port %d (DX-2b: empty-state hint needs it)", gotTUICfg.Port, 9999)
	}
}

// TestMonitorUsageDocumentsStaleImageAndEnvAndCaseSensitivity (DX-2a, DX-3,
// DX-6): monitorUsage must surface the real limitation (events only flow
// from a monitor-enabled image; make load to rebuild), the env gates the
// in-VM extension reads, and that [name] is a case-sensitive substring
// match.
func TestMonitorUsageDocumentsStaleImageAndEnvAndCaseSensitivity(t *testing.T) {
	for _, want := range []string{
		"make load",
		"PIX_MONITOR=0",
		"PIX_MONITOR_URL",
		"host.docker.internal:11437",
		"CASE-SENSITIVE",
	} {
		if !strings.Contains(monitorUsage, want) {
			t.Errorf("monitorUsage missing %q:\n%s", want, monitorUsage)
		}
	}
}

// TestMonitorNoArgs proves the default port (monitor.DefaultPort) and an
// empty filter ("" = all sandboxes) are used when no flags/positional are
// given.
func TestMonitorNoArgs(t *testing.T) {
	var gotCfg monitor.HubConfig
	fakeNewHub := func(cfg monitor.HubConfig) *monitor.Hub {
		gotCfg = cfg
		return monitor.NewHub(monitor.HubConfig{Port: 0})
	}
	fakeRunTUI := func(tui.TUIConfig) error { return nil }

	var out bytes.Buffer
	if err := runMonitorCore(nil, fakeNewHub, fakeRunTUI, &out, io.Discard); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if gotCfg.Port != monitor.DefaultPort {
		t.Errorf("HubConfig.Port = %d, want default %d", gotCfg.Port, monitor.DefaultPort)
	}
	if gotCfg.Filter != "" {
		t.Errorf("HubConfig.Filter = %q, want empty (all sandboxes)", gotCfg.Filter)
	}
	if gotCfg.BindAddr != monitor.DefaultBindAddr {
		t.Errorf("HubConfig.BindAddr = %q, want default %q (SEC-1: secure-by-default)", gotCfg.BindAddr, monitor.DefaultBindAddr)
	}
}

// TestMonitorBindDefaultIsLoopbackNoWarning proves that with no --bind flag,
// runMonitorCore does not print the non-loopback exposure warning.
func TestMonitorBindDefaultIsLoopbackNoWarning(t *testing.T) {
	fakeNewHub := func(cfg monitor.HubConfig) *monitor.Hub {
		return monitor.NewHub(monitor.HubConfig{Port: 0})
	}
	fakeRunTUI := func(tui.TUIConfig) error { return nil }

	var out, errOut bytes.Buffer
	if err := runMonitorCore(nil, fakeNewHub, fakeRunTUI, &out, &errOut); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want no warning for the default loopback bind", errOut.String())
	}
}

// TestMonitorBindNonLoopbackWarns proves --bind 0.0.0.0 parses through to
// HubConfig.BindAddr AND prints a loud warning to stderr (SEC-1: opt-in to a
// wider bind is allowed, but never silent).
func TestMonitorBindNonLoopbackWarns(t *testing.T) {
	var gotCfg monitor.HubConfig
	fakeNewHub := func(cfg monitor.HubConfig) *monitor.Hub {
		gotCfg = cfg
		return monitor.NewHub(monitor.HubConfig{Port: 0})
	}
	fakeRunTUI := func(tui.TUIConfig) error { return nil }

	var out, errOut bytes.Buffer
	if err := runMonitorCore([]string{"--bind", "0.0.0.0"}, fakeNewHub, fakeRunTUI, &out, &errOut); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if gotCfg.BindAddr != "0.0.0.0" {
		t.Errorf("HubConfig.BindAddr = %q, want %q", gotCfg.BindAddr, "0.0.0.0")
	}
	if !strings.Contains(errOut.String(), "WARNING") {
		t.Errorf("stderr = %q, want a WARNING about the exposed bind", errOut.String())
	}
	if !strings.Contains(errOut.String(), "0.0.0.0") {
		t.Errorf("stderr = %q, want it to name the bind address", errOut.String())
	}
}

// TestMonitorBindLoopbackVariantsNoWarning proves every recognized loopback
// spelling (explicit --bind, not just the default) is silent.
func TestMonitorBindLoopbackVariantsNoWarning(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1", "localhost"} {
		fakeNewHub := func(monitor.HubConfig) *monitor.Hub { return monitor.NewHub(monitor.HubConfig{Port: 0}) }
		fakeRunTUI := func(tui.TUIConfig) error { return nil }
		var out, errOut bytes.Buffer
		if err := runMonitorCore([]string{"--bind", addr}, fakeNewHub, fakeRunTUI, &out, &errOut); err != nil {
			t.Fatalf("runMonitorCore(--bind %s): %v", addr, err)
		}
		if errOut.Len() != 0 {
			t.Errorf("--bind %s: stderr = %q, want no warning", addr, errOut.String())
		}
	}
}

// TestMonitorUnknownFlag proves an unrecognized flag is a usage error and
// that the hub is never constructed (usage errors from flag parsing must
// short-circuit before any wiring).
func TestMonitorUnknownFlag(t *testing.T) {
	panicNewHub := func(monitor.HubConfig) *monitor.Hub {
		panic("runMonitorCore must not construct a hub on a flag-parse error")
	}
	panicRunTUI := func(tui.TUIConfig) error {
		panic("runMonitorCore must not run the TUI on a flag-parse error")
	}
	err := runMonitorCore([]string{"--bogus"}, panicNewHub, panicRunTUI, &bytes.Buffer{}, io.Discard)
	if !isUsage(err) {
		t.Errorf("runMonitorCore(--bogus): err = %v, want usageError", err)
	}
}

// TestMonitorTooManyPositional proves a second positional (only one [name]
// filter is accepted) is a usage error.
func TestMonitorTooManyPositional(t *testing.T) {
	panicNewHub := func(monitor.HubConfig) *monitor.Hub {
		panic("runMonitorCore must not construct a hub on a usage error")
	}
	panicRunTUI := func(tui.TUIConfig) error {
		panic("runMonitorCore must not run the TUI on a usage error")
	}
	err := runMonitorCore([]string{"box1", "box2"}, panicNewHub, panicRunTUI, &bytes.Buffer{}, io.Discard)
	if !isUsage(err) {
		t.Errorf("runMonitorCore(box1 box2): err = %v, want usageError", err)
	}
}

// TestMonitorUsage proves verbUsage("monitor") is wired into help.go and
// matches the frozen monitorUsage contract text.
func TestMonitorUsage(t *testing.T) {
	u, ok := verbUsage("monitor")
	if !ok {
		t.Fatal(`verbUsage("monitor") ok = false, want true`)
	}
	if u == "" {
		t.Error(`verbUsage("monitor") is empty`)
	}
	if !strings.HasPrefix(u, "usage: pix monitor") {
		t.Errorf("verbUsage(monitor) = %q, want prefix %q", u, "usage: pix monitor")
	}
}

// TestMonitorKnownVerb proves "monitor" is registered in knownVerbs (so a
// mistyped verb near it gets a did-you-mean hint, and it's recognized as a
// real command by classifyBareArg).
func TestMonitorKnownVerb(t *testing.T) {
	if !knownVerbs["monitor"] {
		t.Error(`knownVerbs["monitor"] = false, want true`)
	}
}
