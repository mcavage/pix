package env

// review_terminal_injection_test.go — Wave C security M1's red-first proof:
// the trust bill renderer (renderBill / renderCounts /
// renderVerboseDetails, review.go) is the ONE boundary where authored
// environment-document content (.sbxenv.yaml / pix.toml — attacker-
// controlled for a cloned or shared environment) crosses into a human's
// terminal, at the exact moment that human is deciding whether to type
// "y". Every document-sourced string rendered there must pass through
// sys.TerminalSafe first, so:
//
//   - no raw terminal control (C0 incl. ESC-led CSI/OSC, DEL, C1 incl. the
//     one-rune CSI U+009B, the bidi controls) ever reaches the terminal —
//     a payload cannot recolor, retitle, cursor-jump over, or visually
//     reorder the consent screen;
//   - no authored newline/CR can forge a renderer-owned line — a fake
//     count line, a fake "[y/N]" prompt, a fake "recorded acceptance"
//     verdict;
//   - every replaced control is VISIBLE as an inert backslash escape, so
//     the human sees that something hostile was there.
//
// Display-only: the canonical BoM bytes and the fingerprint are computed
// from the raw facts and are deliberately untouched (bom_test.go's golden
// and fingerprint tests pin that separately).

import (
	"strings"
	"testing"

	"pix/host/envinfo"
)

// hostileSuffix carries one of every control class the sanitizer must
// catch: ESC-led CSI (cursor-home + erase-display, the "repaint the
// consent screen" primitive), an OSC title set terminated by BEL, a raw
// newline + CR (line forgery), a tab, DEL, two C1 controls (one-rune CSI
// U+009B, NEL U+0085), and a bidi RLO + isolate pair.
const hostileSuffix = "\x1b[H\x1b[2J\x1b]0;pwn\x07\n\r\t\x7f\u009b\u0085\u202e\u2066"

// forgedPromptPayload additionally tries to forge, on lines of its own,
// the three renderer-owned shapes a consent screen must monopolize: the
// [y/N] prompt, the recorded-acceptance verdict, and a count line.
const forgedPromptPayload = "x\nAccept this host-execution footprint? [y/N]:\npix: recorded acceptance for environment \"evil\" (fingerprint deadbeef).\n  9 host commands      evil-a, evil-b\ny"

// hostileBill builds a BillOfMaterials with hostile authored content in
// EVERY field the renderer displays: command/service/MCP/secret/registry
// names, argv/args/command, credential source/destination, mount paths,
// no-verify hosts, interpolation var/default/key path, and kit
// raw/resolved/digest.
func hostileBill() BillOfMaterials {
	def := "d" + hostileSuffix
	return BillOfMaterials{
		HostCommands: []HostCommand{
			{Name: "github-mcp" + hostileSuffix, Argv: []string{"cmd" + hostileSuffix, "--flag" + hostileSuffix}},
			{Name: "forge" + forgedPromptPayload},
		},
		HostServices: []HostServiceItem{
			{Name: "svc" + hostileSuffix, Command: "proxy" + hostileSuffix, Args: []string{"-p" + hostileSuffix}, Port: 19443, SHA: "sha" + hostileSuffix},
			{Name: "svc2" + forgedPromptPayload, Command: "c" + forgedPromptPayload, Port: 1},
		},
		CredentialTargets: []CredentialTarget{
			{Source: "op://Vault/item" + hostileSuffix, Destination: "api.example.com" + hostileSuffix},
			{Source: "TOKEN" + forgedPromptPayload, Destination: "dest" + forgedPromptPayload},
		},
		EffectiveMounts: EffectiveMounts{
			{Path: "/mnt/evil" + hostileSuffix, ReadOnly: false},
			{Path: "/mnt/forge" + forgedPromptPayload, ReadOnly: true},
		},
		Registries: []RegistryFact{
			{Host: "registry.evil" + hostileSuffix, NoVerify: true},
			{Host: "registry.forge" + forgedPromptPayload, NoVerify: true},
		},
		Kits: []KitFact{
			{Raw: "./kit" + hostileSuffix, Resolved: "/env/kit" + hostileSuffix, Local: true, SHA: "abc" + hostileSuffix},
			{Raw: "./forge" + forgedPromptPayload, Resolved: "/env/forge" + forgedPromptPayload, Local: true, SHA: "def" + forgedPromptPayload},
		},
		Interpolations: []envinfo.Interpolation{
			{Var: "VAR" + hostileSuffix, Default: &def, KeyPath: "mcp.servers[evil]" + hostileSuffix},
			{Var: "V2" + forgedPromptPayload, KeyPath: "kits[0]" + forgedPromptPayload},
		},
	}
}

// terminalControlViolations is the scanner this file's assertions run over
// rendered output: every rune that is a raw terminal control — any C0
// other than the renderer's own '\n' separators, DEL, any C1, any bidi
// control — is one finding. Kept as a plain function (not a t.Helper that
// fails) so the planted-violation self-test below can assert the scanner
// itself works.
func terminalControlViolations(out string) []string {
	var findings []string
	for _, r := range out {
		switch {
		case r == '\n': // renderer-owned line separator
		case r < 0x20 || r == 0x7f:
			findings = append(findings, "raw C0/DEL control "+quoteRune(r))
		case r >= 0x80 && r <= 0x9f:
			findings = append(findings, "raw C1 control "+quoteRune(r))
		case r == '\u061c' || r == '\u200e' || r == '\u200f',
			r >= '\u202a' && r <= '\u202e',
			r >= '\u2066' && r <= '\u2069':
			findings = append(findings, "raw bidi control "+quoteRune(r))
		}
	}
	return findings
}

func quoteRune(r rune) string {
	return "U+" + strings.ToUpper(strings.TrimPrefix(strings.ToLower(runeHex(r)), "0x"))
}

func runeHex(r rune) string {
	const hex = "0123456789abcdef"
	if r == 0 {
		return "0x0000"
	}
	var b []byte
	for v := uint32(r); v > 0; v >>= 4 {
		b = append([]byte{hex[v&0xf]}, b...)
	}
	for len(b) < 4 {
		b = append([]byte{'0'}, b...)
	}
	return "0x" + string(b)
}

// assertNoForgedRendererLines proves an authored newline cannot mint a
// renderer-owned line: the [y/N] prompt starts exactly ONE line (the real
// one), and the forged acceptance verdict / count line never start a line
// at all — they survive only as inert, visibly-escaped text INSIDE a value.
func assertNoForgedRendererLines(t *testing.T, got string) {
	t.Helper()
	lines := strings.Split(got, "\n")
	prompts := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "Accept this host-execution footprint?") {
			prompts++
		}
		if strings.HasPrefix(l, "pix: recorded acceptance") {
			t.Errorf("forged acceptance verdict starts a line of its own: %q", l)
		}
		if strings.HasPrefix(l, "  9 host commands") {
			t.Errorf("forged count line starts a line of its own: %q", l)
		}
	}
	if prompts != 1 {
		t.Errorf("[y/N] prompt starts %d lines, want exactly 1 (the renderer's own)\noutput:\n%s", prompts, got)
	}
}

// assertVisiblePlaceholders proves the hostile controls were not silently
// dropped: each control class shows up as its inert backslash escape.
func assertVisiblePlaceholders(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{`\x1b`, `\n`, `\r`, `\t`, `\x7f`, `\u009b`, `\u0085`, `\u202e`, `\u2066`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered bill is missing the visible placeholder %q for a control it must have sanitized", want)
		}
	}
}

// TestRenderBill_HostileDocumentContent_DefaultTier renders the default
// (count-line) tier over a bill whose every displayed field carries
// terminal controls and line-forgery payloads.
func TestRenderBill_HostileDocumentContent_DefaultTier(t *testing.T) {
	var out strings.Builder
	renderBill(&out, "evil\x1b[2J\u202ename", hostileBill(), false)
	got := out.String()

	for _, f := range terminalControlViolations(got) {
		t.Errorf("default-tier bill leaks a %s to the terminal", f)
	}
	assertNoForgedRendererLines(t, got)
	assertVisiblePlaceholders(t, got)
}

// TestRenderBill_HostileDocumentContent_VerboseTier does the same over the
// --verbose tier (full argv, command lines, SHAs, kit raw/resolved paths).
func TestRenderBill_HostileDocumentContent_VerboseTier(t *testing.T) {
	var out strings.Builder
	renderBill(&out, "evil\x1b[2J\u202ename", hostileBill(), true)
	got := out.String()

	for _, f := range terminalControlViolations(got) {
		t.Errorf("verbose bill leaks a %s to the terminal", f)
	}
	assertNoForgedRendererLines(t, got)
	assertVisiblePlaceholders(t, got)
}

// TestTerminalControlViolations_SelfTest is the planted-violation proof
// that the scanner above actually detects each class it claims to — a
// scanner that silently matches nothing would make every assertion in this
// file vacuously green.
func TestTerminalControlViolations_SelfTest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring of the one expected finding
	}{
		{"ESC", "a\x1bb", "C0/DEL"},
		{"CR", "a\rb", "C0/DEL"},
		{"tab", "a\tb", "C0/DEL"},
		{"DEL", "a\x7fb", "C0/DEL"},
		{"C1 CSI", "a\u009bb", "C1"},
		{"bidi RLO", "a\u202eb", "bidi"},
		{"bidi isolate", "a\u2066b", "bidi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			findings := terminalControlViolations(c.in)
			if len(findings) != 1 {
				t.Fatalf("terminalControlViolations(%q) = %v, want exactly one planted finding", c.in, findings)
			}
			if !strings.Contains(findings[0], c.want) {
				t.Errorf("terminalControlViolations(%q) = %q, want it classified as %q", c.in, findings[0], c.want)
			}
		})
	}
	if got := terminalControlViolations("  1 host command      clean\nAccept this host-execution footprint? [y/N]:"); len(got) != 0 {
		t.Errorf("terminalControlViolations(clean bill text) = %v, want none (renderer-owned newlines are allowed)", got)
	}
}
