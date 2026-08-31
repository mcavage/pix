package envinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup_hooks_test.go pins the STRICT `[[setup]]` grammar — the v2
// replacement for a pack's authored install/auth hook. Everything here is a
// pure parse: no filesystem beyond the pix.toml itself, because the
// executable's own identity is proven later (envsetup.HashSetupExecutable),
// twice.

func parseSidecarText(t *testing.T, body string) (*Sidecar, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pix.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ParseSidecar(path)
}

func TestSetupHooks_MinimalValidEntryParses(t *testing.T) {
	s, err := parseSidecarText(t, `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
required = true
kind = "install"
`)
	if err != nil {
		t.Fatalf("ParseSidecar: %v", err)
	}
	if len(s.Setup) != 1 {
		t.Fatalf("want 1 setup hook, got %d", len(s.Setup))
	}
	h := s.Setup[0]
	if h.ID != "tool" || h.Command != "./setup-tool" || !h.Required {
		t.Fatalf("hook decoded wrong: %+v", h)
	}
	if strings.Join(h.CheckArgs, ",") != "check" || strings.Join(h.ApplyArgs, ",") != "install" {
		t.Fatalf("argv decoded wrong: %+v", h)
	}
	if h.EffectiveKind() != SetupKindInstall {
		t.Fatalf("kind = %q, want install", h.EffectiveKind())
	}
}

func TestSetupHooks_AbsentKindDefaultsToInstallAndAbsentRequiredIsOptional(t *testing.T) {
	s, err := parseSidecarText(t, `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
`)
	if err != nil {
		t.Fatalf("ParseSidecar: %v", err)
	}
	if got := s.Setup[0].EffectiveKind(); got != SetupKindInstall {
		t.Errorf("absent kind = %q, want %q (never a silently-assumed auth)", got, SetupKindInstall)
	}
	if s.Setup[0].Required {
		t.Error("absent required must be false (optional), never a silently-assumed true")
	}
}

// Every refusal below is one strict-parsing rule. A table keeps them
// readable; each case names the exact rule it pins.
func TestSetupHooks_StrictRefusals(t *testing.T) {
	const head = "schema = 1\n\n"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing id", "[[setup]]\ncommand = \"./t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", "id"},
		{"id grammar", "[[setup]]\nid = \"bad id!\"\ncommand = \"./t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", "[A-Za-z0-9._-]+"},
		{"duplicate id", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n\n[[setup]]\nid=\"t\"\ncommand=\"./u\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", "duplicate id"},
		{"missing command", "[[setup]]\nid=\"t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", "command is required"},
		{"bare command name", "[[setup]]\nid=\"t\"\ncommand=\"setup-tool\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", "PATH"},
		{"dotdot command", "[[setup]]\nid=\"t\"\ncommand=\"../evil/tool\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", ".."},
		{"missing check_args", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\napply_args=[\"a\"]\n", "check_args is required"},
		{"empty check_args", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[]\napply_args=[\"a\"]\n", "check_args is required"},
		{"missing apply_args", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[\"c\"]\n", "apply_args is required"},
		{"unknown kind", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\nkind=\"sudo\"\n", "kind"},
		{"unknown field", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\ntimeout=30\n", "unknown key"},
		{"control char in command", "[[setup]]\nid=\"t\"\ncommand=\"./t\\u001b[2J\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n", "control character"},
		{"control char in argv", "[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[\"c\\u0000\"]\napply_args=[\"a\"]\n", "control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSidecarText(t, head+tc.body)
			if err == nil {
				t.Fatalf("want a refusal naming %q, got a clean parse", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "pix.toml:") {
				t.Fatalf("error %q does not name the file and line", err)
			}
		})
	}
}

// inputs is validated the same way command is: relative, clean, no `..`,
// no control characters — plus its own uniqueness rule (a duplicate
// companion path declared twice in one hook).
func TestSetupHooks_InputsStrictRefusals(t *testing.T) {
	const head = "schema = 1\n\n[[setup]]\nid=\"t\"\ncommand=\"./t\"\ncheck_args=[\"c\"]\napply_args=[\"a\"]\n"
	cases := []struct {
		name string
		line string
		want string
	}{
		{"absolute input", `inputs = ["/etc/passwd"]`, "must be relative"},
		{"dotdot input", `inputs = ["../evil/data"]`, ".."},
		{"unclean input (leading ./)", `inputs = ["./data.txt"]`, "clean"},
		{"unclean input (double slash)", `inputs = ["lib//data.txt"]`, "clean"},
		{"unclean input (trailing slash)", `inputs = ["lib/"]`, "clean"},
		{"empty input", `inputs = [""]`, "must not be empty"},
		{"duplicate input", `inputs = ["lib/data.txt", "lib/data.txt"]`, "duplicate"},
		{"control char in input", "inputs = [\"lib/data\u0000.txt\"]", "control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSidecarText(t, head+tc.line+"\n")
			if err == nil {
				t.Fatalf("want a refusal naming %q, got a clean parse", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestSetupHooks_ValidInputsParse(t *testing.T) {
	s, err := parseSidecarText(t, `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
inputs = ["lib/helper.sh", "data/config.json"]
`)
	if err != nil {
		t.Fatalf("ParseSidecar: %v", err)
	}
	if got := s.Setup[0].Inputs; len(got) != 2 || got[0] != "lib/helper.sh" || got[1] != "data/config.json" {
		t.Fatalf("inputs decoded wrong: %+v", got)
	}
}

// A hook command is argv, never a shell fragment: nothing in the parser
// interprets a metacharacter, so `;`/`&&`/`$()` land in the argv verbatim
// where os/exec treats them as literal text. The parse must NOT reject them
// (they are legal filename bytes) and must NOT split on them.
func TestSetupHooks_ShellMetacharactersStayLiteralArgv(t *testing.T) {
	s, err := parseSidecarText(t, `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check; rm -rf /", "$(whoami)", "&& echo hi"]
apply_args = ["install"]
`)
	if err != nil {
		t.Fatalf("ParseSidecar: %v", err)
	}
	if got := s.Setup[0].CheckArgs; len(got) != 3 || got[0] != "check; rm -rf /" {
		t.Fatalf("argv was reinterpreted rather than kept literal: %q", got)
	}
}
