// packtrust.go — F5: the Tier-1 trust gate (packs-v2-impl.md F5, packs.md §9).
//
// The trust model splits on whether the pack EXECUTES code on the host:
//
//   - Tier-0: skills / knowledge / config / sandbox-only wrappers. Nothing
//     pack-authored runs on the host → adopt with NO prompt (unchanged from
//     Phase 1).
//   - Tier-1: ANY host-exec facet — an integration.mcp (the gateway spawns its
//     command ON THE HOST), a host=true [[proxy]] wrapper, or a [[bin]]
//     external binary. Adoption halts at the bill-of-materials screen and
//     requires an explicit yes; non-TTY FAILS CLOSED unless --yes.
//
// Acceptance lives in TRUSTED HOST STATE (packtruststore.go — never inside
// the pack payload), keyed by pack identity, over a FINGERPRINT of the entire
// host-exec surface (computeHostExecFingerprint). Switching between accepted
// packs never re-prompts, but ANY change to the surface — a facet added, a
// [[bin]] sha changed, a host proxy script mutated, an MCP argv resolved
// differently (e.g. gog_account) — changes the fingerprint and re-triggers
// the gate. The typed schema is the allowlist: only integration.mcp, host
// [[proxy]], and [[bin]] ever reach an exec path.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hostBoM is the host bill-of-materials: everything a pack would run on (or
// solicit from) THIS machine. Pure data, computed by computeHostBoM, rendered
// by renderHostBoM, gated by packTrustGate, and accepted as a fingerprint in
// the HOST trust store (computeHostExecFingerprint + packtruststore.go).
type hostBoM struct {
	MCP     []hostBoMMCP // MCP servers the gateway will spawn on the host
	Proxies []string     // host=true [[proxy]] wrapper names (bin/<name>)
	Bins    []packBin    // [[bin]] external binaries (path + pinned sha)
	Egress  []string     // union of every facet's declared egress, sorted
	Creds   []string     // credential ENV VAR names solicited (never values)
}

// hostBoMMCP is one host-spawned MCP server: its name plus the exact argv the
// gateway will ultimately run (mcpRegistrar.serverCmd — reused, not re-derived,
// so the screen shows the real shape: gog's hardened flags, or
// `pi-stack-host mcp <name>`).
type hostBoMMCP struct {
	Name string
	Argv []string
}

// tier1 reports whether any host-exec facet is present — the Tier-0/Tier-1
// split of packs.md §9. Egress and creds alone never raise the tier (a
// sandbox wrapper's egress is fenced by the kit allowlist; a credential ref is
// solicited, not executed).
func (b hostBoM) tier1() bool {
	return len(b.MCP) > 0 || len(b.Proxies) > 0 || len(b.Bins) > 0
}

// computeHostBoM enumerates a pack's host bill-of-materials (pure, testable):
// MCP commands (resolved argv via mcp.go's serverCmd), host=true wrappers,
// [[bin]] external binaries, the egress union, and credential VAR names. The
// display registrar uses the bare binary names ("gog", "pi-stack-host") so the
// result is deterministic — the real registration resolves absolute paths, but
// the SHAPE the user reviews is identical.
//
// cfgGogAccount is the RESOLVED fallback account (config gog_account) used
// when the manifest doesn't pin one: the argv the user reviews — and the
// fingerprint the acceptance is recorded over — must be the argv that will
// actually run, so a later gog_account change re-gates (it changes what the
// gateway spawns on the host).
func computeHostBoM(p *packInfo, cfgGogAccount string) hostBoM {
	var b hostBoM
	account := strings.TrimSpace(p.Manifest.GogAccount)
	if account == "" {
		account = strings.TrimSpace(cfgGogAccount)
	}
	if account == "" {
		account = "<gog_account>"
	}
	reg := mcpRegistrar{gog: "gog", account: account, hostBin: "pi-stack-host"}
	for _, name := range packMcpNames(p) {
		b.MCP = append(b.MCP, hostBoMMCP{Name: name, Argv: reg.serverCmd(name)})
	}
	egress := map[string]bool{}
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			b.Proxies = append(b.Proxies, pr.Name)
		}
		for _, e := range pr.Egress {
			if e = strings.TrimSpace(e); e != "" {
				egress[e] = true
			}
		}
	}
	b.Bins = append([]packBin(nil), p.Manifest.Bins...)
	for e := range egress {
		b.Egress = append(b.Egress, e)
	}
	sort.Strings(b.Egress)
	seenCred := map[string]bool{}
	for _, ig := range p.Manifest.Integrations {
		if ig.Env != "" && !seenCred[ig.Env] {
			seenCred[ig.Env] = true
			b.Creds = append(b.Creds, ig.Env)
		}
	}
	return b
}

// computeHostExecFingerprint hashes the ENTIRE host-exec surface of a pack —
// what the Tier-1 acceptance is actually FOR: every MCP's resolved argv,
// every host=true [[proxy]] script's CONTENT (sha256 of the bytes on disk,
// symlink-refused — a mutated script changes the fingerprint), every [[bin]]
// name + pinned sha (the pin itself is verified against the file separately,
// at activation/install/launch), the egress union, and the credential VAR
// names. Entries are sorted into a canonical form so a pure manifest reorder
// never re-gates. Returns the fingerprint plus the per-proxy content hashes
// it was computed over, so the installer can verify the exact bytes it stages
// against what was accepted (no hash-then-install TOCTOU).
//
// An unreadable host proxy script is an ERROR (fail closed): a surface that
// cannot be fingerprinted cannot be accepted or installed.
func computeHostExecFingerprint(root string, b hostBoM) (string, map[string]string, error) {
	proxySHA := map[string]string{}
	var lines []string
	for _, m := range b.MCP {
		lines = append(lines, "mcp\x00"+m.Name+"\x00"+strings.Join(m.Argv, "\x1f"))
	}
	for _, name := range b.Proxies {
		src := filepath.Join(root, "bin", name)
		if isSymlinkPath(src) {
			return "", nil, fmt.Errorf("host wrapper %q is a symlink; refusing to fingerprint it", name)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return "", nil, fmt.Errorf("host wrapper %q: %v (cannot fingerprint the host-exec surface; fail closed)", name, err)
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		proxySHA[name] = sha
		lines = append(lines, "proxy\x00"+name+"\x00"+sha)
	}
	for _, bn := range b.Bins {
		lines = append(lines, "bin\x00"+bn.Name+"\x00"+strings.ToLower(strings.TrimSpace(bn.SHA)))
	}
	for _, e := range b.Egress {
		lines = append(lines, "egress\x00"+e)
	}
	for _, c := range b.Creds {
		lines = append(lines, "cred\x00"+c)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), proxySHA, nil
}

// renderHostBoM prints the review screen: exactly what would run on the host,
// what it reaches, and which credential names are solicited (never values).
func renderHostBoM(out io.Writer, b hostBoM) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "This pack runs code on your host (not just in the sandbox):")
	fmt.Fprintln(out)
	for _, m := range b.MCP {
		fmt.Fprintf(out, "  MCP server (host):   %s  →  op run -- %s\n", m.Name, strings.Join(m.Argv, " "))
	}
	for _, pr := range b.Proxies {
		fmt.Fprintf(out, "  Host wrapper:        %s (bin/%s; on PATH for `pi-stack host` only)\n", pr, pr)
	}
	for _, bn := range b.Bins {
		fmt.Fprintf(out, "  External binary:     %s  sha256:%s  [re-hashed before every launch]\n", bn.Name, strings.ToLower(strings.TrimSpace(bn.SHA)))
	}
	if len(b.Egress) > 0 {
		fmt.Fprintf(out, "  Network egress:      %s\n", strings.Join(b.Egress, ", "))
	}
	if len(b.Creds) > 0 {
		fmt.Fprintf(out, "  Credentials (op://): %s   (you supply your own; never in the pack)\n", strings.Join(b.Creds, ", "))
	}
}

// packTrustGate enforces the Tier-1 adoption gate: render the BoM, then
// require an explicit yes. --yes accepts (the screen still prints, for the
// record). Otherwise: non-TTY FAILS CLOSED (a CI/script adoption must never
// silently enable host code — packs-v2-impl.md F5, non-negotiable), and on a
// TTY the answer defaults to No. A non-nil error means NOT adopted; the caller
// must abort before anything registers, installs, or commits.
func packTrustGate(in io.Reader, out io.Writer, tty, yes bool, packName string, b hostBoM) error {
	renderHostBoM(out, b)
	if yes {
		fmt.Fprintln(out, "\naccepted via --yes")
		return nil
	}
	if !tty || in == nil {
		return fmt.Errorf("pack %q would run the above on your host; refusing to adopt it non-interactively (fail closed) — re-run with --yes to accept", packName)
	}
	fmt.Fprint(out, "\nAdopt this pack and allow the above to run on your machine? [y/N] ")
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return fmt.Errorf("pack %q not adopted (no answer; default is No)", packName)
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("pack %q not adopted (you said no)", packName)
}
