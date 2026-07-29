// packtrust.go — F5: the Tier-1 trust gate (packs-v2-impl.md F5, packs.md §9).
//
// The trust model splits on whether the pack EXECUTES code on the host:
//
//   - Tier-0: skills / knowledge / config / sandbox-only wrappers. Nothing
//     pack-authored runs on the host → adopt with NO prompt (unchanged from
//     Phase 1).
//   - Tier-0 also includes REFERENCE-ONLY integrations (packs.md §9 /
//     packs-v2-impl.md §6): an integration.mcp naming a REMOTE
//     gateway-catalog server (notion/atlassian/…) or the host-provided gog
//     registration ships NO pack-authored executable — the pack contributes
//     only a NAME, and the argv (if any) is launcher-built. No prompt,
//     non-TTY fine.
//   - Tier-1: ANY host-exec facet — an integration.mcp that resolves to a
//     LOCAL stdio host command (mcpRegistrar's local partition:
//     `pix-host mcp <name>` per `pix-host mcp --list`), a
//     host=true [[proxy]] wrapper, or a host=true [[bin]] external binary.
//     Adoption halts at the bill-of-materials screen and requires an
//     explicit yes; non-TTY FAILS CLOSED unless --yes.
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
	"encoding/json"
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
	MCP        []hostBoMMCP       // built-in MCP servers the gateway spawns on the host
	Containers []hostBoMContainer // OCI MCP servers Docker runs on the host
	RemoteMCP  []hostBoMRemote    // remote MCP endpoints attached by the pack
	Proxies    []string           // host=true [[proxy]] wrapper names (bin/<name>)
	Bins       []packBin          // [[bin]] external binaries (path + pinned sha)
	Egress     []string           // union of every facet's declared egress, sorted
	Creds      []string           // credential ENV VAR names solicited (never values)
	Setup      []packSetupStep    // pack setup executables, probes, and apply argv
}

// hostBoMMCP is one host-spawned MCP server: its name plus the exact argv the
// gateway will ultimately run (mcpRegistrar.serverCmd — reused, not re-derived,
// so the screen shows the real shape: gog's hardened flags, or
// `pix-host mcp <name>`).
type hostBoMMCP struct {
	Name string
	Argv []string
}

type hostBoMContainer struct {
	Name     string
	Image    string
	Manifest string
	EnvKeys  []string
}

type hostBoMRemote struct {
	Name string
	URL  string
}

// tier1 reports whether any host-exec facet is present — the Tier-0/Tier-1
// split of packs.md §9. Egress and creds alone never raise the tier (a
// sandbox wrapper's egress is fenced by the kit allowlist; a credential ref is
// solicited, not executed). b.MCP holds only LOCAL host-spawned servers and
// b.Bins only host=true entries (computeHostBoM filters), so a reference-only
// remote MCP or an inert host=false [[bin]] never raises the tier.
func (b hostBoM) tier1() bool {
	return len(b.MCP) > 0 || len(b.Containers) > 0 || len(b.Proxies) > 0 || len(b.Bins) > 0 || len(b.Setup) > 0
}

// localMCPClassifier resolves mcpRegistrar's local-vs-gateway partition into
// a predicate: TRUE for a name treated as a LOCAL stdio server this host runs
// (`pix-host mcp --list`) — i.e. attaching it spawns a host command.
// With the partition ESTABLISHED, a name not in the local set — a remote
// gateway-catalog name (notion/atlassian) or the host-provided gog
// registration (never listed) — is a reference-only Tier-0 fact: nothing
// pack-authored executes.
//
// UNKNOWN classification FAILS CLOSED (round-3 #3): when the local set cannot
// be established (probe error / pix-host unresolved), every non-gog name
// is treated as HOST-EXEC (Tier-1) so the adoption gate fires. The old
// unknown⇒Tier-0 shortcut leaned on registerServers skipping registration on
// the same condition — but the name still lands in cfg.MCP and is attached
// via --mcp, so one ALREADY-registered in the gateway would run its host
// command with NO gate ever shown. Over-prompting on a transient probe
// failure is acceptable; silently skipping the gate is not. gog stays the
// reference-only special case (its registration is launcher-built, never
// pack-authored).
func localMCPClassifier(env shellEnv, hostResolver func() (string, error)) func(string) bool {
	set, known := localMCPNames(env, hostResolver)
	return func(name string) bool {
		if !known {
			return name != gwServerName // fail closed: unknown ⇒ gate (except Google Workspace)
		}
		return set[name]
	}
}

// packLocalMCP builds the classifier for callers without an injected env
// (refreshHostPackWrappers). A package var so tests can pin the partition.
var packLocalMCP = func() func(string) bool {
	return localMCPClassifier(defaultShellEnv(), hostBinaryResolver)
}

// computeHostBoM enumerates a pack's host bill-of-materials (pure, testable):
// MCP commands (resolved argv via mcp.go's serverCmd), host=true wrappers,
// [[bin]] external binaries, the egress union, and credential VAR names. The
// display registrar uses the bare binary names ("gog", "pix-host") so the
// result is deterministic — the real registration resolves absolute paths, but
// the SHAPE the user reviews is identical.
//
// cfgGogAccount is the RESOLVED fallback account (config gog_account) used
// when the manifest doesn't pin one: the argv the user reviews — and the
// fingerprint the acceptance is recorded over — must be the argv that will
// actually run, so a later gog_account change re-gates (it changes what the
// gateway spawns on the host).
//
// isLocalMCP is the local-vs-gateway partition (localMCPClassifier): only an
// integration.mcp that resolves to a LOCAL host command enters the BoM — a
// remote gateway-catalog reference (notion/atlassian/gog) ships no
// pack-authored executable and stays Tier-0 (packs.md §9). nil means "no
// local partition available" and FAILS CLOSED exactly like an unknown probe
// (round-3 #3): every non-gog name classifies as host-exec, so an
// unclassifiable MCP reference is gated rather than silently Tier-0.
//
// [[bin]] entries enter the BoM ONLY with host=true (mirroring host=true
// proxies): an inert host=false bin never enters the accepted surface, so
// flipping it to host=true later is a NEW surface that re-gates.
func computeHostBoM(p *packInfo, cfgGogAccount string, isLocalMCP func(string) bool) hostBoM {
	var b hostBoM
	account := strings.TrimSpace(p.Manifest.GogAccount)
	if account == "" {
		account = strings.TrimSpace(cfgGogAccount)
	}
	if account == "" {
		account = "<gog_account>"
	}
	reg := mcpRegistrar{gog: "gog", account: account, hostBin: "pix-host"}
	if isLocalMCP == nil {
		// No partition available at all: same fail-closed posture as an
		// unknown probe (round-3 #3) — gate every non-gog name.
		isLocalMCP = func(name string) bool { return name != gwServerName }
	}
	seenMCP := map[string]bool{}
	for _, ig := range p.Manifest.Integrations {
		name := strings.TrimSpace(ig.MCP)
		if name == "" || seenMCP[name] {
			continue
		}
		seenMCP[name] = true
		switch {
		case strings.TrimSpace(ig.Image) != "" || strings.TrimSpace(ig.Manifest) != "":
			keys := append([]string(nil), ig.EnvKeys...)
			if env := strings.TrimSpace(ig.Env); env != "" {
				keys = append([]string{env}, keys...)
			}
			b.Containers = append(b.Containers, hostBoMContainer{
				Name: name, Image: strings.TrimSpace(ig.Image), Manifest: strings.TrimSpace(ig.Manifest), EnvKeys: keys,
			})
		case strings.TrimSpace(ig.URL) != "":
			b.RemoteMCP = append(b.RemoteMCP, hostBoMRemote{Name: name, URL: strings.TrimSpace(ig.URL)})
		case isLocalMCP(name):
			b.MCP = append(b.MCP, hostBoMMCP{Name: name, Argv: reg.serverCmd(name)})
		}
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
	for _, bn := range p.Manifest.Bins {
		if bn.Host {
			b.Bins = append(b.Bins, bn)
		}
	}
	b.Setup = append(b.Setup, p.Manifest.Setup...)
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
// ENCODING IS CANONICAL AND INJECTIVE (round-2 B): the surface is marshaled
// as a structured JSON document (fixed field order, sorted entries, every
// string JSON-escaped) and THAT is hashed. The previous ad-hoc NUL/newline
// concatenation was not injective for unconstrained strings — an egress (or
// cred/argv) value containing the delimiter bytes could encode a DIFFERENT
// surface with an identical hash (the reviewer showed a real collision:
// egress ["a","b"] vs ["a\negress\x00b"]). JSON escaping makes any two
// distinct surfaces serialize to distinct bytes. [[bin]] entries also encode
// their Host flag (belt-and-suspenders on top of computeHostBoM only ever
// admitting host=true bins).
//
// An unreadable host proxy script is an ERROR (fail closed): a surface that
// cannot be fingerprinted cannot be accepted or installed.
func computeHostExecFingerprint(root string, b hostBoM) (string, map[string]string, error) {
	type fpProxy struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
	}
	type fpBin struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
		Host bool   `json:"host"`
	}
	type fpMCP struct {
		Name string   `json:"name"`
		Argv []string `json:"argv"`
	}
	type fpContainer struct {
		Name     string   `json:"name"`
		Image    string   `json:"image,omitempty"`
		Manifest string   `json:"manifest,omitempty"`
		EnvKeys  []string `json:"env_keys,omitempty"`
	}
	type fpRemote struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	type fpSetup struct {
		ID          string   `json:"id"`
		Path        string   `json:"path"`
		SHA         string   `json:"sha"`
		CheckArgs   []string `json:"check_args"`
		ApplyArgs   []string `json:"apply_args"`
		Required    bool     `json:"required"`
		Description string   `json:"description"`
	}
	type fpDoc struct {
		V          int           `json:"v"`
		MCP        []fpMCP       `json:"mcp"`
		Containers []fpContainer `json:"container"`
		RemoteMCP  []fpRemote    `json:"remote_mcp"`
		Proxies    []fpProxy     `json:"proxy"`
		Bins       []fpBin       `json:"bin"`
		Egress     []string      `json:"egress"`
		Creds      []string      `json:"cred"`
		Setup      []fpSetup     `json:"setup"`
	}
	doc := fpDoc{V: 4}
	proxySHA := map[string]string{}
	for _, m := range b.MCP {
		doc.MCP = append(doc.MCP, fpMCP{Name: m.Name, Argv: append([]string(nil), m.Argv...)})
	}
	sort.Slice(doc.MCP, func(i, j int) bool {
		if doc.MCP[i].Name != doc.MCP[j].Name {
			return doc.MCP[i].Name < doc.MCP[j].Name
		}
		return strings.Join(doc.MCP[i].Argv, "\x00") < strings.Join(doc.MCP[j].Argv, "\x00")
	})
	for _, c := range b.Containers {
		keys := append([]string(nil), c.EnvKeys...)
		sort.Strings(keys)
		doc.Containers = append(doc.Containers, fpContainer{Name: c.Name, Image: c.Image, Manifest: c.Manifest, EnvKeys: keys})
	}
	sort.Slice(doc.Containers, func(i, j int) bool { return doc.Containers[i].Name < doc.Containers[j].Name })
	for _, r := range b.RemoteMCP {
		doc.RemoteMCP = append(doc.RemoteMCP, fpRemote{Name: r.Name, URL: r.URL})
	}
	sort.Slice(doc.RemoteMCP, func(i, j int) bool { return doc.RemoteMCP[i].Name < doc.RemoteMCP[j].Name })
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
		doc.Proxies = append(doc.Proxies, fpProxy{Name: name, SHA: sha})
	}
	sort.Slice(doc.Proxies, func(i, j int) bool { return doc.Proxies[i].Name < doc.Proxies[j].Name })
	for _, bn := range b.Bins {
		doc.Bins = append(doc.Bins, fpBin{Name: bn.Name, SHA: strings.ToLower(strings.TrimSpace(bn.SHA)), Host: bn.Host})
	}
	sort.Slice(doc.Bins, func(i, j int) bool {
		if doc.Bins[i].Name != doc.Bins[j].Name {
			return doc.Bins[i].Name < doc.Bins[j].Name
		}
		return doc.Bins[i].SHA < doc.Bins[j].SHA
	})
	doc.Egress = append([]string(nil), b.Egress...)
	sort.Strings(doc.Egress)
	doc.Creds = append([]string(nil), b.Creds...)
	sort.Strings(doc.Creds)
	for _, s := range b.Setup {
		path := filepath.Join(root, s.Path)
		if isSymlinkPath(path) {
			return "", nil, fmt.Errorf("setup hook %q is a symlink; refusing to fingerprint it", s.ID)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("setup hook %q: %v (cannot fingerprint the host-exec surface; fail closed)", s.ID, err)
		}
		sum := sha256.Sum256(data)
		doc.Setup = append(doc.Setup, fpSetup{
			ID: s.ID, Path: filepath.Clean(s.Path), SHA: hex.EncodeToString(sum[:]),
			CheckArgs: append([]string(nil), s.CheckArgs...), ApplyArgs: append([]string(nil), s.ApplyArgs...),
			Required: s.Required, Description: s.Description,
		})
	}
	sort.Slice(doc.Setup, func(i, j int) bool { return doc.Setup[i].ID < doc.Setup[j].ID })
	enc, err := json.Marshal(doc)
	if err != nil {
		return "", nil, fmt.Errorf("encoding host-exec surface: %v", err)
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:]), proxySHA, nil
}

// renderHostBoM prints the review screen: exactly what would run on the host,
// what it reaches, and which credential names are solicited (never values).
func renderHostBoM(out io.Writer, b hostBoM) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "This pack adds these integrations to Pix:")
	fmt.Fprintln(out)
	for _, m := range b.MCP {
		fmt.Fprintf(out, "  Local integration:   %s\n", m.Name)
		fmt.Fprintf(out, "                       Runs on this Mac: op run -- %s\n", strings.Join(m.Argv, " "))
	}
	for _, c := range b.Containers {
		source := "manifest " + c.Manifest
		if c.Image != "" {
			source = "image " + c.Image
		}
		fmt.Fprintf(out, "  Local container:     %s (%s)\n", c.Name, source)
		if len(c.EnvKeys) > 0 {
			fmt.Fprintf(out, "                       Receives: %s\n", strings.Join(c.EnvKeys, ", "))
		}
	}
	for _, r := range b.RemoteMCP {
		fmt.Fprintf(out, "  Remote integration:  %s → %s\n", r.Name, r.URL)
	}
	for _, pr := range b.Proxies {
		fmt.Fprintf(out, "  Host wrapper:        %s (bin/%s; on PATH for `pix host` only)\n", pr, pr)
	}
	for _, bn := range b.Bins {
		fmt.Fprintf(out, "  External binary:     %s  sha256:%s  [re-hashed before every launch]\n", bn.Name, strings.ToLower(strings.TrimSpace(bn.SHA)))
	}
	for _, s := range b.Setup {
		kind := "optional"
		if s.Required {
			kind = "required"
		}
		fmt.Fprintf(out, "  Setup hook:         %s (%s) %s %s\n", s.ID, kind, s.Path, strings.Join(s.ApplyArgs, " "))
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
	fmt.Fprint(out, "\nActivate this pack and allow these integrations? [y/N] ")
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
