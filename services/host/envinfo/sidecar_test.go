package envinfo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const validSidecar = `schema = 1

[models]
main = "zai/glm-5"
exclusive = false

[agents]
architect = "deepseek/deepseek-r1"
engineer = "zai/glm-5"
review = "google/gemini-3.1-pro-preview"

[memory]
scope = "personal"

[pi]
skills = ["./skills"]

[host.mcp.warehouse]
env_keys = ["WAREHOUSE_TOKEN"]
probe_args = ["probe"]

[[host.services]]
name = "warehouse-proxy"
command = "warehouse-proxy"
args = ["serve"]
port = 19443
probe = "http://127.0.0.1:19443/health"

[inference.backends.zai]
driver = "openai-compatible"
protocol = "openai-completions"
base_url = "https://api.z.ai/api/paas/v4"
auth = "1password"
key_env = "ZAI_API_KEY"

[[inference.models]]
id = "zai/glm-5"
backend = "zai"
upstream_id = "glm-5"
context_window = 200000
max_output_tokens = 32000
reasoning = true
`

func writeSidecar(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "pix.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseValidSidecarProducesTypedFacts(t *testing.T) {
	path := writeSidecar(t, t.TempDir(), validSidecar)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if s.Schema != 1 {
		t.Errorf("Schema = %d, want 1", s.Schema)
	}
	if s.Models.Main != "zai/glm-5" {
		t.Errorf("Models.Main = %q", s.Models.Main)
	}
	if s.Models.Exclusive != false {
		t.Errorf("Models.Exclusive = %v, want false", s.Models.Exclusive)
	}
	if got, want := s.Agents["engineer"], "zai/glm-5"; got != want {
		t.Errorf("Agents[engineer] = %q, want %q", got, want)
	}
	if len(s.Agents) != 3 {
		t.Errorf("len(Agents) = %d, want 3", len(s.Agents))
	}
	if s.Memory.Scope != "personal" {
		t.Errorf("Memory.Scope = %q", s.Memory.Scope)
	}
	if got, want := s.Pi.Skills, []string{"./skills"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Pi.Skills = %v, want %v", got, want)
	}

	wh, ok := s.Host.MCP["warehouse"]
	if !ok {
		t.Fatal("Host.MCP[warehouse] missing")
	}
	if wh.Name != "warehouse" {
		t.Errorf("Host.MCP[warehouse].Name = %q, want warehouse (populated from table key)", wh.Name)
	}
	if len(wh.EnvKeys) != 1 || wh.EnvKeys[0] != "WAREHOUSE_TOKEN" {
		t.Errorf("Host.MCP[warehouse].EnvKeys = %v", wh.EnvKeys)
	}
	if len(wh.ProbeArgs) != 1 || wh.ProbeArgs[0] != "probe" {
		t.Errorf("Host.MCP[warehouse].ProbeArgs = %v", wh.ProbeArgs)
	}

	if len(s.Host.Services) != 1 {
		t.Fatalf("Host.Services = %v, want 1 entry", s.Host.Services)
	}
	svc := s.Host.Services[0]
	if svc.Name != "warehouse-proxy" || svc.Command != "warehouse-proxy" || svc.Port != 19443 {
		t.Errorf("Host.Services[0] = %+v", svc)
	}
	if len(svc.Args) != 1 || svc.Args[0] != "serve" {
		t.Errorf("Host.Services[0].Args = %v", svc.Args)
	}
	if svc.Probe != "http://127.0.0.1:19443/health" {
		t.Errorf("Host.Services[0].Probe = %q", svc.Probe)
	}

	be, ok := s.Inference.Backends["zai"]
	if !ok {
		t.Fatal("Inference.Backends[zai] missing")
	}
	if be.Driver != "openai-compatible" || be.Auth != "1password" || be.KeyEnv != "ZAI_API_KEY" {
		t.Errorf("Inference.Backends[zai] = %+v", be)
	}

	if len(s.Inference.Models) != 1 {
		t.Fatalf("Inference.Models = %v, want 1 entry", s.Inference.Models)
	}
	im := s.Inference.Models[0]
	if im.ID != "zai/glm-5" || im.Backend != "zai" || im.ContextWindow != 200000 || !im.Reasoning {
		t.Errorf("Inference.Models[0] = %+v", im)
	}
}

func TestModelReferencesCollectsMainAndAgentsSorted(t *testing.T) {
	path := writeSidecar(t, t.TempDir(), validSidecar)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	refs := s.ModelReferences()
	if len(refs) != 4 { // main + 3 agents
		t.Fatalf("ModelReferences() = %v, want 4 entries", refs)
	}
	if refs[0].Field != "models.main" || refs[0].Model != "zai/glm-5" {
		t.Errorf("refs[0] = %+v, want models.main -> zai/glm-5", refs[0])
	}
	wantAgentFields := []string{"agents.architect", "agents.engineer", "agents.review"}
	for i, want := range wantAgentFields {
		if refs[i+1].Field != want {
			t.Errorf("refs[%d].Field = %q, want %q (agent order must be stable/sorted)", i+1, refs[i+1].Field, want)
		}
	}
}

func TestModelsExclusiveFlagRoundTrips(t *testing.T) {
	content := strings.Replace(validSidecar, "exclusive = false", "exclusive = true", 1)
	path := writeSidecar(t, t.TempDir(), content)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !s.Models.Exclusive {
		t.Error("Models.Exclusive = false, want true")
	}
}

// AC-18 error shape: pix.toml:<line> plus the exact offending key.
var errorShapeRE = regexp.MustCompile(`^pix\.toml:\d+: .+: ".+"$`)

func TestParseUnknownKeyFailsWithFileLineAndExactKey(t *testing.T) {
	content := "schema = 1\n\n[models]\nmain = \"zai/glm-5\"\ntypo_field = \"oops\"\n"
	path := writeSidecar(t, t.TempDir(), content)

	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("Parse: expected error for unknown key, got nil")
	}
	serr, ok := err.(*Error)
	if !ok {
		t.Fatalf("Parse error type = %T, want *envinfo.Error", err)
	}
	if serr.Line != 5 {
		t.Errorf("Line = %d, want 5", serr.Line)
	}
	if serr.Key != "models.typo_field" {
		t.Errorf("Key = %q, want %q", serr.Key, "models.typo_field")
	}
	if serr.File != "pix.toml" {
		t.Errorf("File = %q, want pix.toml", serr.File)
	}
	if !errorShapeRE.MatchString(serr.Error()) {
		t.Errorf("Error() = %q, does not match AC-18 shape %q", serr.Error(), errorShapeRE.String())
	}
	if !strings.HasPrefix(serr.Error(), "pix.toml:5:") {
		t.Errorf("Error() = %q, want prefix %q", serr.Error(), "pix.toml:5:")
	}
}

func TestParseRejectsUnknownTopLevelSection(t *testing.T) {
	content := "[bogus]\nfield = 1\n"
	path := writeSidecar(t, t.TempDir(), content)

	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected error")
	}
	serr := err.(*Error)
	if serr.Key != "bogus" {
		t.Errorf("Key = %q, want %q", serr.Key, "bogus")
	}
	if serr.Line != 1 {
		t.Errorf("Line = %d, want 1", serr.Line)
	}
	if serr.Reason != "unknown key" {
		t.Errorf("Reason = %q, want %q", serr.Reason, "unknown key")
	}
}

func TestParseRejectsNativeOwnedRootFields(t *testing.T) {
	cases := []struct {
		key  string
		toml string
	}{
		{"kits", "kits = [\"./kit\"]\n"},
		{"workspaces", "workspaces = [\"./\"]\n"},
		{"env", "[env]\nFOO = \"bar\"\n"},
		{"secrets", "[secrets]\nanthropic = { ref = \"op://x\" }\n"},
		{"bindings", "[bindings]\nanthropic = { apiKey = {} }\n"},
		{"mcp", "[mcp]\nservers = []\n"},
		{"resources", "[resources]\nmemory = \"16g\"\n"},
		{"sandboxOptions", "[sandboxOptions]\nmemory = \"16g\"\n"},
		{"ports", "ports = [3000]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			path := writeSidecar(t, t.TempDir(), tc.toml)
			_, err := ParseSidecar(path)
			if err == nil {
				t.Fatalf("expected rejection for native-owned field %q", tc.key)
			}
			serr, ok := err.(*Error)
			if !ok {
				t.Fatalf("error type = %T, want *envinfo.Error", err)
			}
			if serr.Key != tc.key {
				t.Errorf("Key = %q, want %q", serr.Key, tc.key)
			}
			if !strings.Contains(serr.Reason, ".sbxenv.yaml") {
				t.Errorf("Reason = %q, want it to name .sbxenv.yaml as the owner", serr.Reason)
			}
			if !strings.HasPrefix(serr.Error(), "pix.toml:") {
				t.Errorf("Error() = %q, want pix.toml:<line> prefix", serr.Error())
			}
		})
	}
}

func TestParseRejectsHostMCPURLAndCommandAsNativeOwned(t *testing.T) {
	for _, key := range []string{"url", "command"} {
		t.Run(key, func(t *testing.T) {
			content := "[host.mcp.warehouse]\n" + key + " = \"https://example.com\"\n"
			path := writeSidecar(t, t.TempDir(), content)
			_, err := ParseSidecar(path)
			if err == nil {
				t.Fatalf("expected rejection for host.mcp.warehouse.%s", key)
			}
			serr := err.(*Error)
			wantKey := "host.mcp.warehouse." + key
			if serr.Key != wantKey {
				t.Errorf("Key = %q, want %q", serr.Key, wantKey)
			}
			if !strings.Contains(serr.Reason, ".sbxenv.yaml") {
				t.Errorf("Reason = %q, want it to name .sbxenv.yaml as the owner", serr.Reason)
			}
		})
	}
}

func TestParseRejectsUnknownKeyInsideHostServicesEntry(t *testing.T) {
	content := "[[host.services]]\nname = \"x\"\nbogus = 1\n"
	path := writeSidecar(t, t.TempDir(), content)
	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected error")
	}
	serr := err.(*Error)
	if serr.Key != "host.services.bogus" {
		t.Errorf("Key = %q, want %q", serr.Key, "host.services.bogus")
	}
	if serr.Line != 3 {
		t.Errorf("Line = %d, want 3", serr.Line)
	}
}

func TestParseRejectsUnknownKeyInsideInferenceBackend(t *testing.T) {
	content := "[inference.backends.zai]\ndriver = \"openai-compatible\"\nauth = \"1password\"\nbogus = \"x\"\n"
	path := writeSidecar(t, t.TempDir(), content)
	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected error")
	}
	serr := err.(*Error)
	if serr.Key != "inference.backends.zai.bogus" {
		t.Errorf("Key = %q, want %q", serr.Key, "inference.backends.zai.bogus")
	}
}

func TestParseSurfacesRealTOMLSyntaxErrorsWithLine(t *testing.T) {
	content := "schema = 1\n\n[models\nmain = \"x\"\n"
	path := writeSidecar(t, t.TempDir(), content)
	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected syntax error")
	}
	// Whatever shape the underlying parser reports, it must not silently
	// succeed, and our own wrapping (when it is a *Error) still names a file.
	if serr, ok := err.(*Error); ok {
		if serr.File != "pix.toml" {
			t.Errorf("File = %q, want pix.toml", serr.File)
		}
		if serr.Line == 0 {
			t.Error("Line = 0, want a real line number from the syntax error")
		}
	}
}

func TestValidateSkillWorkspacesAcceptsPathInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeSidecar(t, dir, validSidecar)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := ValidateSkillWorkspaces(path, s, []string{dir}); err != nil {
		t.Errorf("ValidateSkillWorkspaces: unexpected error: %v", err)
	}
}

func TestValidateSkillWorkspacesRejectsPathOutsideEveryWorkspace(t *testing.T) {
	dir := t.TempDir()
	otherWorkspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeSidecar(t, dir, validSidecar)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	err = ValidateSkillWorkspaces(path, s, []string{otherWorkspace})
	if err == nil {
		t.Fatal("expected error: skills path is outside the only declared workspace")
	}
	serr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *envinfo.Error", err)
	}
	if serr.Key != "pi.skills" {
		t.Errorf("Key = %q, want %q", serr.Key, "pi.skills")
	}
	if !strings.HasPrefix(serr.Error(), "pix.toml:") {
		t.Errorf("Error() = %q, want pix.toml:<line> prefix", serr.Error())
	}
}

func TestValidateSkillWorkspacesFailsClosedWithNoWorkspaces(t *testing.T) {
	dir := t.TempDir()
	path := writeSidecar(t, dir, validSidecar)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := ValidateSkillWorkspaces(path, s, nil); err == nil {
		t.Fatal("expected fail-closed error with no declared workspaces")
	}
}

// --- Pre-merge findings: quoted-key strictness must come from the real
// decoder's metadata, never a regex line-scanner, and TOML quoting must be
// honored so a quoted dotted segment is one identity, not split. ---

// A quoted unknown key must fail exactly like its bare-key equivalent. The
// old line-scanner's key regex (`^([A-Za-z0-9_-]+...)\s*=`) never matches a
// quoted key, so the line was silently skipped and the key sailed through to
// the final decode, which BurntSushi does not error on for unknown fields.
func TestParseRejectsQuotedUnknownKey(t *testing.T) {
	content := "[models]\nmain = \"zai/glm-5\"\n\"typo_field\" = \"oops\"\n"
	path := writeSidecar(t, t.TempDir(), content)

	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("Parse: expected error for quoted unknown key, got nil (silent bypass)")
	}
	serr, ok := err.(*Error)
	if !ok {
		t.Fatalf("Parse error type = %T, want *envinfo.Error", err)
	}
	if serr.Line != 3 {
		t.Errorf("Line = %d, want 3", serr.Line)
	}
	if serr.Key != "models.typo_field" {
		t.Errorf("Key = %q, want %q", serr.Key, "models.typo_field")
	}
}

// A quoted native-owned key must still be classified as native-owned, not
// merely "unknown key" — the friendlier .sbxenv.yaml redirect must survive
// quoting.
func TestParseRejectsQuotedNativeOwnedRootKey(t *testing.T) {
	content := "\"kits\" = [\"./kit\"]\n"
	path := writeSidecar(t, t.TempDir(), content)

	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected rejection for quoted native-owned key")
	}
	serr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *envinfo.Error", err)
	}
	if serr.Key != "kits" {
		t.Errorf("Key = %q, want %q", serr.Key, "kits")
	}
	if !strings.Contains(serr.Reason, ".sbxenv.yaml") {
		t.Errorf("Reason = %q, want it to name .sbxenv.yaml as the owner", serr.Reason)
	}
}

// A quoted host.mcp.<name>.url/command must still hit the native-owned
// reason even though the field name itself is quoted.
func TestParseRejectsQuotedHostMCPURLAsNativeOwned(t *testing.T) {
	content := "[host.mcp.warehouse]\n\"url\" = \"https://example.com\"\n"
	path := writeSidecar(t, t.TempDir(), content)
	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected rejection for quoted host.mcp.warehouse.url")
	}
	serr := err.(*Error)
	if serr.Key != "host.mcp.warehouse.url" {
		t.Errorf("Key = %q, want %q", serr.Key, "host.mcp.warehouse.url")
	}
	if !strings.Contains(serr.Reason, ".sbxenv.yaml") {
		t.Errorf("Reason = %q, want it to name .sbxenv.yaml as the owner", serr.Reason)
	}
}

// [host.mcp."github.copilot"] is one dynamic identity — the server name
// "github.copilot" — not a split at the embedded dot. Quoting is TOML
// syntax for "this is a single key segment", and the parser must honor it.
func TestParseAcceptsQuotedDottedHostMCPIdentity(t *testing.T) {
	content := "[host.mcp.\"github.copilot\"]\nenv_keys = [\"GH_TOKEN\"]\n"
	path := writeSidecar(t, t.TempDir(), content)

	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: unexpected error for quoted dotted host.mcp identity: %v", err)
	}
	entry, ok := s.Host.MCP["github.copilot"]
	if !ok {
		t.Fatalf("Host.MCP[%q] missing; got keys %v", "github.copilot", s.Host.MCP)
	}
	if entry.Name != "github.copilot" {
		t.Errorf("Host.MCP[%q].Name = %q, want %q", "github.copilot", entry.Name, "github.copilot")
	}
	if len(entry.EnvKeys) != 1 || entry.EnvKeys[0] != "GH_TOKEN" {
		t.Errorf("Host.MCP[%q].EnvKeys = %v", "github.copilot", entry.EnvKeys)
	}
}

// A native-owned field nested inside a quoted dotted identity must still be
// rejected, and the reported key must render the identity quoted as one
// segment (host.mcp."github.copilot".url), never split.
func TestParseRejectsNativeOwnedFieldInsideQuotedDottedIdentity(t *testing.T) {
	content := "[host.mcp.\"github.copilot\"]\nurl = \"https://example.com\"\n"
	path := writeSidecar(t, t.TempDir(), content)

	_, err := ParseSidecar(path)
	if err == nil {
		t.Fatal("expected rejection for native-owned url under a quoted dotted identity")
	}
	serr := err.(*Error)
	wantKey := `host.mcp."github.copilot".url`
	if serr.Key != wantKey {
		t.Errorf("Key = %q, want %q", serr.Key, wantKey)
	}
	if !strings.Contains(serr.Reason, ".sbxenv.yaml") {
		t.Errorf("Reason = %q, want it to name .sbxenv.yaml as the owner", serr.Reason)
	}
}

// Audit: quoted agent, model-backend, and inference-model identities must
// decode as literal freeform names, dots and all — never split, never
// rejected as unknown, since agent/backend names are user-chosen.
func TestParseAcceptsQuotedDottedAgentAndBackendIdentities(t *testing.T) {
	content := `schema = 1

[agents]
"github.copilot" = "zai/glm-5"

[inference.backends."my.custom.backend"]
driver = "openai-compatible"
protocol = "openai-completions"
base_url = "https://example.com"
auth = "1password"
key_env = "X_API_KEY"
`
	path := writeSidecar(t, t.TempDir(), content)
	s, err := ParseSidecar(path)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if got, want := s.Agents["github.copilot"], "zai/glm-5"; got != want {
		t.Errorf("Agents[%q] = %q, want %q", "github.copilot", got, want)
	}
	be, ok := s.Inference.Backends["my.custom.backend"]
	if !ok {
		t.Fatalf("Inference.Backends[%q] missing; got %v", "my.custom.backend", s.Inference.Backends)
	}
	if be.Driver != "openai-compatible" {
		t.Errorf("Inference.Backends[%q].Driver = %q", "my.custom.backend", be.Driver)
	}
}
