// trust.go — the Tier-1 pack trust gate (docs/design/packs.md §9).
//
// The model splits on whether the pack EXECUTES code on the host. Tier-0 —
// skills / knowledge / config / sandbox-only wrappers, plus REFERENCE-ONLY
// integrations where the pack contributes only a NAME and the argv is
// launcher-built — adopts with NO prompt, non-TTY fine. Tier-1 is ANY host-exec
// facet (a local stdio MCP, a container/remote MCP, a host=true [[proxy]] or
// [[bin]], a [[setup]] hook, an inference gateway, a [[services]] unit): it
// halts at the bill-of-materials screen and requires an explicit yes; non-TTY
// FAILS CLOSED unless --yes.
//
// Acceptance lives in TRUSTED HOST STATE (truststore.go — never inside the pack
// payload), keyed by pack identity, over a FINGERPRINT of the whole host-exec
// surface: switching between accepted packs never re-prompts, ANY change to the
// surface re-gates. The typed schema is the allowlist.
package pack

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/mcp"
	"sort"
	"strings"
)

// hostBoM is the host bill-of-materials: everything a pack would run on (or
// solicit from) THIS machine. Pure data — computed, rendered, gated, hashed.
type hostBoM struct {
	MCP            []hostBoMMCP       // built-in MCP servers the gateway spawns on the host
	Containers     []hostBoMContainer // OCI MCP servers Docker runs on the host
	RemoteMCP      []hostBoMRemote    // remote MCP endpoints attached by the pack
	Proxies        []string           // host=true [[proxy]] wrapper names (bin/<name>)
	SandboxProxies []PackProxy        // host=false wrappers: sandbox commands forwarding elsewhere
	Bins           []packBin          // [[bin]] external binaries (path + pinned sha)
	Egress         []string           // union of every facet's declared egress, sorted
	Creds          []string           // credential ENV VAR names solicited (never values)
	Prerequisites  []string           // pack-authored external state the user must bring
	Setup          []packSetupStep    // pack setup executables, probes, and apply argv
	Inference      []hostBoMInference // model endpoints plus credential-routing policy
	Services       []packService      // [[services]] long-running units (normalized)
}

// The json tags on these types (and on packService) ARE the canonical
// fingerprint encoding: computeHostExecFingerprintWithSetup marshals the BoM
// itself instead of copying it into a parallel set of hash-only structs. Field
// names, order and omitempty are therefore load-bearing — changing one re-gates
// every already-accepted pack.

// hostBoMMCP is one host-spawned MCP server: its name plus the exact argv the
// gateway will run, reused from the registrar so the screen shows the real one.
type hostBoMMCP struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

type hostBoMContainer struct {
	Name      string            `json:"name"`
	Image     string            `json:"image,omitempty"`
	Manifest  string            `json:"manifest,omitempty"`
	EnvKeys   []string          `json:"env_keys,omitempty"`
	EnvValues map[string]string `json:"env_values,omitempty"`
}

type hostBoMRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type hostBoMInference struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Auth    string `json:"auth"`
	Service string `json:"service"`
	Header  string `json:"header"`
	Format  string `json:"format"`
}

// Tier1 reports whether any host-exec facet is present. Egress and creds alone
// never raise the tier (a sandbox wrapper's egress is fenced by the kit
// allowlist; a credential ref is solicited, not executed). A remote MCP
// endpoint DOES: it sends conversation context to a pack-selected third party.
func (b hostBoM) Tier1() bool {
	return len(b.MCP) > 0 || len(b.Containers) > 0 || len(b.RemoteMCP) > 0 || len(b.Proxies) > 0 || len(b.Bins) > 0 || len(b.Setup) > 0 || len(b.Inference) > 0 || len(b.Services) > 0
}

// VerifyPackInferenceTrust closes the adoption-to-launch gap for
// credential-routing inference: a pack stays mutable after `pack use`, so
// before a sandbox consumes its endpoint/service/header policy, recompute the
// surface and require an exact launcher-owned trust-store match.
func VerifyPackInferenceTrust(p *Info, cfgGogAccount string, env hostenv.Env) error {
	if p == nil {
		return nil
	}
	bom := ComputeHostBoM(p, cfgGogAccount, LocalMCPClassifier(env, env.HostBinary))
	if len(bom.Inference) == 0 {
		return nil
	}
	fp, _, err := ComputeHostExecFingerprint(p.Root, bom)
	if err != nil {
		return fmt.Errorf("pack %s inference trust surface: %w", p.Manifest.Name, err)
	}
	return requireAcceptedFingerprint(p, fp, "inference credential routing")
}

// LocalMCPClassifier resolves the registrar's local-vs-gateway partition into a
// predicate: TRUE for a name this host runs as a LOCAL stdio server, i.e. one
// whose attach spawns a host command. Anything outside that set is
// reference-only Tier-0. UNKNOWN FAILS CLOSED: when the set cannot be
// established (probe error, pix-host unresolved) every non-gog name is treated
// as host-exec, because a name already registered in the gateway would
// otherwise run with NO gate shown. Over-prompting on a transient probe failure
// is acceptable; skipping the gate is not.
func LocalMCPClassifier(env hostenv.Env, hostResolver func() (string, error)) func(string) bool {
	set, known := mcp.LocalMCPNames(env, hostResolver)
	return func(name string) bool {
		if !known {
			return name != config.GWServerName // fail closed: unknown ⇒ gate (except Google Workspace)
		}
		return set[name]
	}
}

// PackLocalMCP builds the classifier for callers without an injected env
// (refreshHostPackWrappers). A package var so tests can pin the partition and
// the composition root can supply the real env.
var PackLocalMCP = func() func(string) bool { return func(string) bool { return false } }

// ComputeHostBoM enumerates a pack's host bill-of-materials (pure, testable):
// MCP commands (resolved argv), host=true wrappers and [[bin]]s, [[services]],
// setup hooks, inference gateways, the egress union and credential VAR names.
// Bare binary names keep the reviewed SHAPE deterministic and identical to what
// registration resolves. cfgGogAccount is the RESOLVED fallback account, so a
// later gog_account change re-gates. isLocalMCP is the local-vs-gateway
// partition; nil FAILS CLOSED exactly like an unknown probe. [[bin]] entries
// enter ONLY with host=true, so flipping an inert bin later is a NEW surface.
func ComputeHostBoM(p *Info, cfgGogAccount string, isLocalMCP func(string) bool) hostBoM {
	var b hostBoM
	account := strings.TrimSpace(p.Manifest.GogAccount)
	if account == "" {
		account = strings.TrimSpace(cfgGogAccount)
	}
	if account == "" {
		account = "<gog_account>"
	}
	reg := mcp.McpRegistrar{Gog: "gog", Account: account, HostBin: "pix-host"}
	if isLocalMCP == nil {
		// No partition at all: same fail-closed posture as an unknown probe.
		isLocalMCP = func(name string) bool { return name != config.GWServerName }
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
				Name: name, Image: strings.TrimSpace(ig.Image), Manifest: strings.TrimSpace(ig.Manifest), EnvKeys: keys, EnvValues: ig.EnvValues,
			})
		case strings.TrimSpace(ig.URL) != "":
			b.RemoteMCP = append(b.RemoteMCP, hostBoMRemote{Name: name, URL: strings.TrimSpace(ig.URL)})
		case isLocalMCP(name):
			b.MCP = append(b.MCP, hostBoMMCP{Name: name, Argv: reg.ServerCmd(name)})
		}
	}
	egress := map[string]bool{}
	for _, pr := range p.Manifest.Proxies {
		if pr.Host {
			b.Proxies = append(b.Proxies, pr.Name)
		} else {
			b.SandboxProxies = append(b.SandboxProxies, pr)
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
	for _, svc := range p.Manifest.Services {
		// Normalized: the shape the user reviews IS the shape the fingerprint
		// pins and a future supervisor consumes.
		b.Services = append(b.Services, svc.normalized())
	}
	if p.Manifest.Inference != nil {
		for name, backend := range p.Manifest.Inference.Backends {
			b.Inference = append(b.Inference, hostBoMInference{
				Name: name, URL: backend.BaseURL, Auth: backend.Auth,
				Service: backend.CredentialService, Header: backend.CredentialHeader, Format: backend.CredentialFormat,
			})
		}
		sort.Slice(b.Inference, func(i, j int) bool { return b.Inference[i].Name < b.Inference[j].Name })
	}
	b.Prerequisites = append(b.Prerequisites, p.Manifest.Prerequisites...)
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

// ComputeHostExecFingerprint hashes the ENTIRE host-exec surface — what the
// Tier-1 acceptance is FOR: every MCP's resolved argv, every host=true
// [[proxy]] script's CONTENT (sha256 of the bytes on disk, symlink-refused),
// every [[bin]] pin, every [[services]] field, setup hook bytes, inference
// policy, egress and credential VAR names. Entries sort canonically, so a pure
// manifest reorder never re-gates, and the per-proxy content hashes come back
// so the installer verifies the exact bytes it stages (no TOCTOU).
//
// THE ENCODING IS CANONICAL AND INJECTIVE: a structured JSON document (fixed
// field order, sorted entries, every string escaped) is what gets hashed. An
// ad-hoc NUL/newline concatenation is not injective — a value containing the
// delimiter bytes could encode a DIFFERENT surface with an identical hash. An
// unfingerprintable surface is an ERROR: it can be neither accepted nor
// installed.
func ComputeHostExecFingerprint(root string, b hostBoM) (string, map[string]string, error) {
	return computeHostExecFingerprintWithSetup(root, b, nil)
}

// computeHostExecFingerprintWithSetup hashes immutable setup-hook snapshots
// when supplied. RunPackSetup executes those same bytes, binding the accepted
// fingerprint to the actual executable rather than a re-opened mutable path.
func computeHostExecFingerprintWithSetup(root string, b hostBoM, setupBytes map[string][]byte) (string, map[string]string, error) {
	// The BoM types carry the json tags, so the surface is marshaled straight
	// out of the reviewed structs. Only the three facets whose hashed shape is
	// NOT the reviewed shape need a local type: a host proxy hashes its script
	// CONTENT, a [[bin]] hashes its pin without its path, and a setup hook adds
	// its script sha.
	type fpProxy struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
	}
	type fpBin struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
		Host bool   `json:"host"`
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
		V             int                `json:"v"`
		MCP           []hostBoMMCP       `json:"mcp"`
		Containers    []hostBoMContainer `json:"container"`
		RemoteMCP     []hostBoMRemote    `json:"remote_mcp"`
		Proxies       []fpProxy          `json:"proxy"`
		Bins          []fpBin            `json:"bin"`
		Egress        []string           `json:"egress"`
		Creds         []string           `json:"cred"`
		Prerequisites []string           `json:"prerequisites"`
		Setup         []fpSetup          `json:"setup"`
		Inference     []hostBoMInference `json:"inference"`
		// Services is ADDITIVE with omitempty on purpose: a pack with no
		// [[services]] keeps its exact prior encoding, so every already-accepted
		// fingerprint stays valid. The key is present iff a service is declared.
		Services []packService `json:"services,omitempty"`
	}
	doc := fpDoc{V: 6}
	proxySHA := map[string]string{}
	doc.MCP = append(doc.MCP, b.MCP...)
	sort.Slice(doc.MCP, func(i, j int) bool {
		if doc.MCP[i].Name != doc.MCP[j].Name {
			return doc.MCP[i].Name < doc.MCP[j].Name
		}
		return strings.Join(doc.MCP[i].Argv, "\x00") < strings.Join(doc.MCP[j].Argv, "\x00")
	})
	for _, c := range b.Containers {
		c.EnvKeys = append([]string(nil), c.EnvKeys...)
		sort.Strings(c.EnvKeys)
		doc.Containers = append(doc.Containers, c)
	}
	sort.Slice(doc.Containers, func(i, j int) bool { return doc.Containers[i].Name < doc.Containers[j].Name })
	doc.RemoteMCP = append(doc.RemoteMCP, b.RemoteMCP...)
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
	doc.Prerequisites = append([]string(nil), b.Prerequisites...)
	for _, s := range b.Setup {
		data, snapshotted := setupBytes[s.ID]
		if !snapshotted {
			path := filepath.Join(root, s.Path)
			if isSymlinkPath(path) {
				return "", nil, fmt.Errorf("setup hook %q is a symlink; refusing to fingerprint it", s.ID)
			}
			var err error
			data, err = os.ReadFile(path)
			if err != nil {
				return "", nil, fmt.Errorf("setup hook %q: %v (cannot fingerprint the host-exec surface; fail closed)", s.ID, err)
			}
		}
		sum := sha256.Sum256(data)
		doc.Setup = append(doc.Setup, fpSetup{
			ID: s.ID, Path: filepath.Clean(s.Path), SHA: hex.EncodeToString(sum[:]),
			CheckArgs: append([]string(nil), s.CheckArgs...), ApplyArgs: append([]string(nil), s.ApplyArgs...),
			Required: s.Required, Description: s.Description,
		})
	}
	sort.Slice(doc.Setup, func(i, j int) bool { return doc.Setup[i].ID < doc.Setup[j].ID })
	doc.Inference = append(doc.Inference, b.Inference...)
	for _, svc := range b.Services {
		// Argv order is semantic (kept); env/mounts/network are sets \u2014 sorted so
		// a pure list reorder never re-gates.
		svc.SHA = strings.ToLower(strings.TrimSpace(svc.SHA))
		svc.Env = append([]string(nil), svc.Env...)
		svc.Mounts = append([]string(nil), svc.Mounts...)
		svc.Network = append([]string(nil), svc.Network...)
		sort.Strings(svc.Env)
		sort.Strings(svc.Mounts)
		sort.Strings(svc.Network)
		doc.Services = append(doc.Services, svc)
	}
	// Names are unique (validatePackServices), so name order is total.
	sort.Slice(doc.Services, func(i, j int) bool { return doc.Services[i].Name < doc.Services[j].Name })
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
		fmt.Fprintf(out, "  Host MCP:            %s\n", m.Name)
		fmt.Fprintf(out, "                       Runs on this Mac: op run -- %s\n", strings.Join(m.Argv, " "))
	}
	for _, c := range b.Containers {
		source := "manifest " + c.Manifest
		if c.Image != "" {
			source = "image " + c.Image
		}
		fmt.Fprintf(out, "  Host MCP:            %s (%s)\n", c.Name, source)
		if len(c.EnvKeys) > 0 {
			fmt.Fprintf(out, "                       Receives: %s\n", strings.Join(c.EnvKeys, ", "))
		}
		if len(c.EnvValues) > 0 {
			keys := make([]string, 0, len(c.EnvValues))
			for key := range c.EnvValues {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Fprintf(out, "                       Uses: %s=%s\n", key, c.EnvValues[key])
			}
		}
	}
	for _, r := range b.RemoteMCP {
		fmt.Fprintf(out, "  Remote MCP:          %s → %s\n", r.Name, r.URL)
	}
	for _, inf := range b.Inference {
		auth := inf.Auth
		if inf.Service != "" {
			auth += " via " + inf.Service
		}
		fmt.Fprintf(out, "  Model gateway:       %s → %s (%s)\n", inf.Name, inf.URL, auth)
	}
	for _, pr := range b.Proxies {
		fmt.Fprintf(out, "  Host wrapper:        %s (bin/%s; on PATH for `pix host` only)\n", pr, pr)
	}
	for _, pr := range b.SandboxProxies {
		destination := "a declared endpoint"
		if len(pr.Egress) > 0 {
			destination = strings.Join(pr.Egress, ", ")
		}
		fmt.Fprintf(out, "  Sandbox command:     %s (bin/%s → %s)\n", pr.Name, pr.Name, destination)
	}
	for _, bn := range b.Bins {
		fmt.Fprintf(out, "  External binary:     %s  sha256:%s  [re-hashed before every launch]\n", bn.Name, strings.ToLower(strings.TrimSpace(bn.SHA)))
	}
	for _, svc := range b.Services {
		fmt.Fprintf(out, "  Host service:        %s (%s, activation %s)\n", svc.Name, svc.Runtime, svc.Activation)
		switch svc.Runtime {
		case serviceRuntimeContainer:
			fmt.Fprintf(out, "                       Runs a container on this Mac: %s\n", svc.Image)
		default:
			line := svc.Path
			if len(svc.Argv) > 0 {
				line += " " + strings.Join(svc.Argv, " ")
			}
			fmt.Fprintf(out, "                       Runs on this Mac: %s\n", line)
			fmt.Fprintf(out, "                       sha256:%s  [verified before every launch]\n", svc.SHA)
		}
		if svc.Port != 0 {
			listen := svc.Listen
			if listen == "" {
				listen = "127.0.0.1"
			}
			health := svc.Health
			if health == "" {
				health = "none declared"
			}
			fmt.Fprintf(out, "                       Listens: %s:%d (loopback only; health %s)\n", listen, svc.Port, health)
		}
		if len(svc.Env) > 0 {
			fmt.Fprintf(out, "                       Env (names only, values stay in 1Password): %s\n", strings.Join(svc.Env, ", "))
		}
		if len(svc.Mounts) > 0 {
			fmt.Fprintf(out, "                       Mounts (pack-relative): %s\n", strings.Join(svc.Mounts, ", "))
		}
		if len(svc.Network) > 0 {
			fmt.Fprintf(out, "                       Network access: %s\n", strings.Join(svc.Network, ", "))
		}
		if r := svc.Resources; r != nil {
			fmt.Fprintf(out, "                       Resources: %d MB memory, %d%% CPU\n", r.MemoryMB, r.CPUPercent)
		}
		fmt.Fprintf(out, "                       License: %s   Source: %s\n", svc.License, svc.Source)
	}
	for _, s := range b.Setup {
		kind := "Optional:"
		if s.Required {
			kind = "Ensures:"
		}
		label := strings.TrimSpace(s.Description)
		if label == "" {
			label = s.ID
		}
		fmt.Fprintf(out, "  %-20s %s — %s (%s %s)\n", kind, s.ID, label, s.Path, strings.Join(s.ApplyArgs, " "))
	}
	coveredEgress := map[string]bool{}
	for _, proxy := range b.SandboxProxies {
		for _, endpoint := range proxy.Egress {
			coveredEgress[endpoint] = true
		}
	}
	var extraEgress []string
	for _, endpoint := range b.Egress {
		if !coveredEgress[endpoint] {
			extraEgress = append(extraEgress, endpoint)
		}
	}
	if len(extraEgress) > 0 {
		fmt.Fprintf(out, "  Network access:      %s\n", strings.Join(extraEgress, ", "))
	}
	if len(b.Prerequisites) > 0 {
		fmt.Fprintln(out, "\nBefore continuing, make sure:")
		for _, item := range b.Prerequisites {
			fmt.Fprintf(out, "  • %s\n", item)
		}
	} else if len(b.Creds) > 0 {
		fmt.Fprintf(out, "  1Password references needed: %s\n", strings.Join(b.Creds, ", "))
	}
}

// packTrustGate enforces the Tier-1 adoption gate: render the BoM, then require
// an explicit yes. --yes accepts (the screen still prints, for the record); a
// non-TTY FAILS CLOSED, and on a TTY the answer defaults to No. A non-nil error
// means NOT adopted: the caller aborts before anything registers or commits.
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
