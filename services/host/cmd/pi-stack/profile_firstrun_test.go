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

func TestFirstRunFlowNonTTY(t *testing.T) {
	var out bytes.Buffer
	called := false
	handled := firstRunFlow(strings.NewReader(""), &out, false, func([]string) { called = true })
	if handled {
		t.Error("non-tty first run should not be handled (should continue)")
	}
	if called {
		t.Error("non-tty first run must not launch setup")
	}
	if !strings.Contains(out.String(), "pi-stack setup") {
		t.Errorf("non-tty first run should hint setup: %q", out.String())
	}
}

func TestFirstRunFlowTTYYes(t *testing.T) {
	var out bytes.Buffer
	called := false
	handled := firstRunFlow(strings.NewReader("\n"), &out, true, func([]string) { called = true })
	if !handled || !called {
		t.Errorf("tty Enter should run setup: handled=%v called=%v", handled, called)
	}
}

func TestFirstRunFlowTTYNo(t *testing.T) {
	var out bytes.Buffer
	called := false
	handled := firstRunFlow(strings.NewReader("n\n"), &out, true, func([]string) { called = true })
	if handled || called {
		t.Errorf("tty 'n' should skip setup: handled=%v called=%v", handled, called)
	}
	if !strings.Contains(out.String(), "Skipped") {
		t.Errorf("declining should say Skipped: %q", out.String())
	}
}
