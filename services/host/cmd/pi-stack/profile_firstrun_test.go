package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"pi-stack/host/config"
)

func TestExtractProfileFlag(t *testing.T) {
	cases := []struct {
		argv        []string
		wantProfile string
		wantRest    []string
	}{
		{[]string{"run"}, "", []string{"run"}},
		{[]string{"--profile", "work", "run"}, "work", []string{"run"}},
		{[]string{"run", "--profile", "work"}, "work", []string{"run"}},
		{[]string{"--profile=personal", "status"}, "personal", []string{"status"}},
		{[]string{"run", "--", "--profile", "notme"}, "", []string{"run", "--", "--profile", "notme"}},
	}
	for _, tc := range cases {
		flagProfile = ""
		rest, err := extractProfileFlag(tc.argv)
		if err != nil {
			t.Fatalf("extractProfileFlag(%v): %v", tc.argv, err)
		}
		if flagProfile != tc.wantProfile {
			t.Errorf("extractProfileFlag(%v) profile = %q, want %q", tc.argv, flagProfile, tc.wantProfile)
		}
		if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
			t.Errorf("extractProfileFlag(%v) rest = %v, want %v", tc.argv, rest, tc.wantRest)
		}
	}
	flagProfile = ""
}

func TestExtractProfileFlagMissingValue(t *testing.T) {
	flagProfile = ""
	if _, err := extractProfileFlag([]string{"--profile"}); err == nil {
		t.Error("--profile with no value should error")
	}
	if _, err := extractProfileFlag([]string{"--profile="}); err == nil {
		t.Error("--profile= with empty value should error")
	}
	flagProfile = ""
}

func TestLoadResolvedConfigUnknownProfile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.toml"
	if err := os.WriteFile(cfgPath, []byte("[profiles.work]\ngog_account=\"w@x.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	t.Setenv("PI_STACK_PROFILE", "")
	flagProfile = "wrok" // typo
	defer func() { flagProfile = "" }()
	if _, _, err := loadResolvedConfig(); err == nil {
		t.Error("unknown profile should error, not silently use base config")
	}
	flagProfile = "work" // valid
	if _, name, err := loadResolvedConfig(); err != nil || name != "work" {
		t.Errorf("valid profile: name=%q err=%v", name, err)
	}
}

func TestResolveProfileNamePrecedence(t *testing.T) {
	cfg := &config.Config{ActiveProfile: "cfgprof"}

	flagProfile = ""
	t.Setenv("PI_STACK_PROFILE", "")
	if got := resolveProfileName(cfg); got != "cfgprof" {
		t.Errorf("config active: got %q, want cfgprof", got)
	}

	t.Setenv("PI_STACK_PROFILE", "envprof")
	if got := resolveProfileName(cfg); got != "envprof" {
		t.Errorf("env over config: got %q, want envprof", got)
	}

	flagProfile = "flagprof"
	if got := resolveProfileName(cfg); got != "flagprof" {
		t.Errorf("flag over env: got %q, want flagprof", got)
	}
	flagProfile = ""

	empty := &config.Config{}
	t.Setenv("PI_STACK_PROFILE", "")
	if got := resolveProfileName(empty); got != config.DefaultProfile {
		t.Errorf("default: got %q, want %q", got, config.DefaultProfile)
	}
}

// TestValidateProfileUseArgs is the F4 gate for `profile use`: exactly one
// positional name; a trailing flag typo (`use work --jsom`) or an extra
// positional (`use a b`) is a usage error, so it never silently saves
// active_profile.
func TestValidateProfileUseArgs(t *testing.T) {
	if name, err := validateProfileUseArgs([]string{"work"}); err != nil || name != "work" {
		t.Errorf(`use "work" = (%q,%v), want ("work",nil)`, name, err)
	}
	bad := [][]string{
		nil,                // no name
		{"work", "--jsom"}, // trailing flag typo
		{"--jsom"},         // flag where a name belongs
		{"work", "extra"},  // extra positional
		{"a", "b", "c"},    // several extras
	}
	for _, argv := range bad {
		if _, err := validateProfileUseArgs(argv); err == nil {
			t.Errorf("validateProfileUseArgs(%v) = nil error, want usage error", argv)
		} else if !isUsage(err) {
			t.Errorf("validateProfileUseArgs(%v) err = %v, want usageError", argv, err)
		}
	}
}

// TestParseProfileLsArgs is the F4 gate for `profile ls`: only an optional
// --json; any other token (a --jsom typo, a stray positional) is a usage error
// rather than being silently ignored and run as plain ls.
func TestParseProfileLsArgs(t *testing.T) {
	if j, err := parseProfileLsArgs([]string{"--json"}); err != nil || !j {
		t.Errorf("ls --json = (%v,%v), want (true,nil)", j, err)
	}
	if j, err := parseProfileLsArgs(nil); err != nil || j {
		t.Errorf("ls (no args) = (%v,%v), want (false,nil)", j, err)
	}
	for _, argv := range [][]string{{"--jsom"}, {"work"}, {"--json", "extra"}} {
		if _, err := parseProfileLsArgs(argv); err == nil || !isUsage(err) {
			t.Errorf("parseProfileLsArgs(%v) err = %v, want usageError", argv, err)
		}
	}
}

// firstRunFlow now only PRINTS a nudge (setup is explicit); it never handles the
// invocation and never launches anything, on any TTY state.
func TestFirstRunFlowNudgesNeverHandles(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var out bytes.Buffer
		if handled := firstRunFlow(strings.NewReader(""), &out, tty); handled {
			t.Errorf("tty=%v: first run must never handle the invocation", tty)
		}
		s := out.String()
		if !strings.Contains(s, "pi-stack setup") || !strings.Contains(s, "pi-stack run") {
			t.Errorf("tty=%v: nudge should mention setup + run: %q", tty, s)
		}
	}
}
