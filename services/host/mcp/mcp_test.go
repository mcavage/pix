package mcp

import (
	"bytes"
	"errors"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// --- real fixtures, replacing the retired hostenv/hostenvtest package -------
//
// RegisterServers only ever touches env.LookPath, env.RunTimed, env.
// RunInteractive(Quiet) and env.Quiet (verified against mcp.go directly — it
// never calls Getenv/IsFile/HomeDir/ReadFile), so a fixture here needs only ONE
// primitive: a PATH-isolated bin dir a test can drop a real "sbx", "op" or
// pack-declared server executable into. (The pix-host resolver this fixture
// used to need is gone with the local MCP bridge.) Every probe below execs the
// real thing; nothing is a call-keyed double.

// shQuote single-quotes s for embedding in a POSIX shell script, so neither
// glob metacharacters in a case pattern nor shell syntax in canned output can
// leak out of the literal string a test declared.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestRemoteMCPRegistrationCurrentRejectsEndpointSubstring(t *testing.T) {
	want := "https://expected.example/mcp"
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"url":"https://evil.example/?next=https://expected.example/mcp"}`, false, nil
	}}}
	if remoteMCPRegistrationCurrent(env, "meetings", want) {
		t.Fatal("an endpoint embedded inside another URL must not count as the registered endpoint")
	}
}

func TestRemoteMCPRegistrationCurrentRequiresEndpointField(t *testing.T) {
	want := "https://expected.example/mcp?a=1&b=2"
	for _, payload := range []string{
		`{"url":"https://evil.example/mcp","note":"https://expected.example/mcp?a=1&b=2"}`,
		`{"callback_url":"https://expected.example/mcp?a=1&b=2"}`,
		`{"url":"https://evil.example/mcp","nested":{"endpoint":"https://expected.example/mcp?a=1&b=2"}}`,
		`url: https://evil.example/?next=https://expected.example/mcp?a=1&b=2`,
	} {
		env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) { return payload, false, nil }}}
		if remoteMCPRegistrationCurrent(env, "meetings", want) {
			t.Fatalf("non-endpoint evidence was trusted: %s", payload)
		}
	}
}

func TestRemoteMCPRegistrationCurrentCanonicalExactMatch(t *testing.T) {
	want := "https://expected.example/mcp?b=2&a=1"
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"server":{"remote_url":"HTTPS://EXPECTED.EXAMPLE:443/mcp?a=1&b=2"}}`, false, nil
	}}}
	if !remoteMCPRegistrationCurrent(env, "meetings", want) {
		t.Fatal("canonically identical endpoint field was not recognized")
	}
}

// The gog-only-wraps-for-an-explicit-keyring-ref behavior (BuildGogRegistrar)
// used to back the built-in `pix gworkspace setup` wizard's pre-registration
// snapshot and had its own test here. That wizard is retired, and so is the
// vendor special case: op-run wrapping is now decided solely by whether the
// pack-declared server carries EnvKeys, which
// TestExecArgv_CredentialFreeServerIsNeverWrapped and
// TestRegisterServers_WrappedRegistrationSaysSo cover end to end.

// --- DX finding 6: sbx-absent errors go to stderr, not stdout --------------

// TestRunMcpLsCore_SbxAbsentWritesRecoveryToStderr: `mcp ls` promises a
// listing (exit 3 on ErrSbxUnavailable, see RunMcpLsCore's doc). The
// recovery command it prints is an error report, not a listing row — a
// caller piping stdout (`pix mcp ls | jq`) must see NOTHING there, and the
// exact "would run" command on stderr where a human actually looks for it.
func TestRunMcpLsCore_SbxAbsentWritesRecoveryToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	absent := func(string) (string, error) { return "", errors.New("not found") }
	err := RunMcpLsCore(absent, &out, strings.NewReader(""), &errOut)
	if !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("expected ErrSbxUnavailable, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("mcp ls must write nothing to stdout on sbx-absent, got stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "would run: sbx mcp ls") {
		t.Errorf("mcp ls must print the exact recovery command on stderr, got stderr=%q", errOut.String())
	}
}

// TestMcpLsAttachmentNote_NeverClaimsStatusOrDoctorShowLiveness: `pix status`/
// `pix doctor` cannot see inside a running session either (health/mcp.go's
// own attachmentCaveat says so), so the note must not send a reader there to
// learn "what's live" — that would just relocate the same unanswerable
// question. It must instead name the thing that actually DOES something:
// recreating the sandbox, which preloads everything registered. (It used to
// also name `pix mcp load`, the live-attach verb, which was cut: recreating is
// the answer in a stack whose sandboxes are disposable.)
func TestMcpLsAttachmentNote_NeverClaimsStatusOrDoctorShowLiveness(t *testing.T) {
	for _, bad := range []string{"for what's live", "see `pix status`", "see `pix doctor`"} {
		if strings.Contains(mcpLsAttachmentNote, bad) {
			t.Errorf("mcpLsAttachmentNote contains %q, implying status/doctor are an attachment authority they are not", bad)
		}
	}
	for _, want := range []string{"pix status", "pix doctor", "pix rm <box>", "pix run"} {
		if !strings.Contains(mcpLsAttachmentNote, want) {
			t.Errorf("mcpLsAttachmentNote missing %q:\n%s", want, mcpLsAttachmentNote)
		}
	}
}
