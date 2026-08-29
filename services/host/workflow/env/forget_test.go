package env

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/cli"
)

// ── happy path: unregisters, touches no source bytes ─────────────────────

func TestForget_UnregistersAndLeavesSourceByteIdentical(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	envDir := t.TempDir()
	sentinel := filepath.Join(envDir, ".sbxenv.yaml")
	content := "schemaVersion: \"1\"\nagent: pix\n"
	if err := writeFile(t, sentinel, content); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}

	root, err := Register(cfg, "home", envDir)
	if err != nil {
		t.Fatal(err)
	}

	gotRoot, err := Forget(cfg, "home", NoLiveHolders)
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if gotRoot != root {
		t.Errorf("Forget returned root %q, want %q", gotRoot, root)
	}
	if _, ok := Root(cfg, "home"); ok {
		t.Error("home must no longer resolve after Forget")
	}

	after, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("source file must survive Forget untouched: %v", err)
	}
	if before.ModTime() != after.ModTime() {
		t.Errorf("source mtime changed: before %v, after %v", before.ModTime(), after.ModTime())
	}
	afterBytes, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(beforeBytes) {
		t.Errorf("source bytes changed: before %q, after %q", beforeBytes, afterBytes)
	}
}

// ── unknown name ──────────────────────────────────────────────────────────

func TestForget_UnknownNameRefuses(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	_, err := Forget(cfg, "hoem", NoLiveHolders)
	if err == nil {
		t.Fatal("Forget of an unregistered name must refuse")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
}

// ── current default refuses, with no override ────────────────────────────

func TestForget_CurrentDefaultRefuses(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "home", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}

	_, err := Forget(cfg, "home", NoLiveHolders)
	if err == nil {
		t.Fatal("Forget of the current default must refuse")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var target *ForgetCurrentDefaultError
	if !errors.As(err, &target) {
		t.Errorf("error = %T (%v), want *ForgetCurrentDefaultError", err, err)
	}
	if _, ok := Root(cfg, "home"); !ok {
		t.Error("a refused Forget must leave the registration in place")
	}
	if cfg.Environment != "home" {
		t.Errorf("cfg.Environment = %q, want unchanged %q", cfg.Environment, "home")
	}
}

// ── live holder refuses ───────────────────────────────────────────────────

func TestForget_LiveHolderRefuses(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "home", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	held := func(name string) (bool, error) { return true, nil }

	_, err := Forget(cfg, "home", held)
	if err == nil {
		t.Fatal("Forget with a positive live-holder probe must refuse")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var target *ForgetLiveHolderError
	if !errors.As(err, &target) || target.Unknown {
		t.Errorf("error = %+v, want a non-Unknown *ForgetLiveHolderError", err)
	}
	if _, ok := Root(cfg, "home"); !ok {
		t.Error("a refused Forget must leave the registration in place")
	}
}

// ── unknown holder-probe outcome fails closed ─────────────────────────────

func TestForget_UnknownHolderProbeFailsClosed(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "home", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	unknown := func(name string) (bool, error) { return false, errors.New("sbx unreachable") }

	_, err := Forget(cfg, "home", unknown)
	if err == nil {
		t.Fatal("Forget with an inconclusive holder probe must fail closed (refuse)")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var target *ForgetLiveHolderError
	if !errors.As(err, &target) || !target.Unknown {
		t.Errorf("error = %+v, want an Unknown *ForgetLiveHolderError", err)
	}
	if _, ok := Root(cfg, "home"); !ok {
		t.Error("a refused Forget must leave the registration in place")
	}
}

// ── nil probe defaults to NoLiveHolders ───────────────────────────────────

func TestForget_NilProbeDefaultsToNoLiveHolders(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "home", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := Forget(cfg, "home", nil); err != nil {
		t.Fatalf("Forget with a nil probe: %v", err)
	}
}
