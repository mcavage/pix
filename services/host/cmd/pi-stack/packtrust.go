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
// Acceptance is recorded in pack.lock (Accepted*), so switching between
// already-adopted packs never re-prompts — but a facet ADDED (or a [[bin]] sha
// CHANGED) since acceptance is no longer covered and re-triggers the gate.
// clonePack scrubs any remote-authored pack.lock, so acceptance can never be
// smuggled in by the pack itself. The typed schema is the allowlist: only
// integration.mcp, host [[proxy]], and [[bin]] ever reach an exec path.
package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// hostBoM is the host bill-of-materials: everything a pack would run on (or
// solicit from) THIS machine. Pure data, computed by computeHostBoM, rendered
// by renderHostBoM, gated by packTrustGate, recorded by acceptHostBoM.
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
func computeHostBoM(p *packInfo) hostBoM {
	var b hostBoM
	account := strings.TrimSpace(p.Manifest.GogAccount)
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

// lockCoversBoM reports whether a previously-recorded acceptance (pack.lock's
// Accepted* fields) covers EVERY element of the current BoM. Covered → no
// re-prompt (trust was granted at adoption); any new MCP/wrapper/bin-pair/
// egress/cred since acceptance → the gate fires again. A [[bin]] is keyed by
// name=sha (packBinPair), so a changed sha is never covered by an old yes.
func lockCoversBoM(l packLock, b hostBoM) bool {
	subset := func(want, have []string) bool {
		for _, w := range want {
			if !containsStr(have, w) {
				return false
			}
		}
		return true
	}
	var mcpNames, binPairs []string
	for _, m := range b.MCP {
		mcpNames = append(mcpNames, m.Name)
	}
	for _, bn := range b.Bins {
		binPairs = append(binPairs, packBinPair(bn))
	}
	return subset(mcpNames, l.AcceptedMCP) &&
		subset(b.Proxies, l.AcceptedHostProxies) &&
		subset(binPairs, l.AcceptedBins) &&
		subset(b.Egress, l.AcceptedEgress) &&
		subset(b.Creds, l.AcceptedCreds)
}

// acceptHostBoM records b as the accepted BoM on lock (called only after the
// gate passed, or when the prior acceptance already covered b). Acceptance is
// always set to the CURRENT BoM — shrinking is deliberate hygiene: a facet
// dropped and later re-added re-prompts, which errs on the safe side. A Tier-0
// BoM clears every accepted field.
func acceptHostBoM(lock *packLock, b hostBoM) {
	lock.AcceptedMCP, lock.AcceptedHostProxies, lock.AcceptedBins = nil, nil, nil
	lock.AcceptedEgress, lock.AcceptedCreds = nil, nil
	for _, m := range b.MCP {
		lock.AcceptedMCP = append(lock.AcceptedMCP, m.Name)
	}
	lock.AcceptedHostProxies = append(lock.AcceptedHostProxies, b.Proxies...)
	for _, bn := range b.Bins {
		lock.AcceptedBins = append(lock.AcceptedBins, packBinPair(bn))
	}
	lock.AcceptedEgress = append(lock.AcceptedEgress, b.Egress...)
	lock.AcceptedCreds = append(lock.AcceptedCreds, b.Creds...)
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
