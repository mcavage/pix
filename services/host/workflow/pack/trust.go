// trust.go — the Tier-1 pack trust gate (docs/design/packs.md §9).
//
// The trust model splits on whether the pack EXECUTES code on the host:
//
//   - Tier-0: skills / knowledge / config / sandbox-only wrappers, plus
//     REFERENCE-ONLY integrations (an integration.mcp naming a REMOTE
//     gateway-catalog server or the host-provided gog registration ships no
//     pack-authored executable — the pack contributes only a NAME, and the argv
//     is launcher-built). Nothing pack-authored runs on the host → adopt with
//     NO prompt, non-TTY fine.
//   - Tier-1: ANY host-exec facet — an integration.mcp resolving to a LOCAL
//     stdio host command, a host=true [[proxy]] wrapper, a host=true [[bin]]
//     external binary, or a [[services]] unit. Adoption halts at the
//     bill-of-materials screen and requires an explicit yes; non-TTY FAILS
//     CLOSED unless --yes.
//
// Acceptance lives in TRUSTED HOST STATE (truststore.go — never inside the pack
// payload), keyed by pack identity, over a FINGERPRINT of the entire host-exec
// surface. Switching between accepted packs never re-prompts, but ANY change to
// the surface re-triggers the gate. The typed schema is the allowlist.
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
// solicit from) THIS machine. Pure data — computed by ComputeHostBoM, rendered
// by renderHostBoM, gated by packTrustGate, accepted as a fingerprint.
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
	Services       []packService      // [[services]] long-running units (normalized; U08a, declaration-only)
}

// hostBoMMCP is one host-spawned MCP server: its name plus the exact argv the
// gateway will run (reused from the registrar, not re-derived, so the screen
// shows the real shape).
type hostBoMMCP struct {
	Name string
	Argv []string
}

type hostBoMContainer struct {
	Name      string
	Image     string
	Manifest  string
	EnvKeys   []string
	EnvValues map[string]string
}

type hostBoMRemote struct {
	Name string
	URL  string
}

type hostBoMInference struct {
	Name    string
	URL     string
	Auth    string
	Service string
	Header  string
	Format  string
}

// Tier1 reports whether any host-exec facet is present. Egress and creds alone
// never raise the tier (a sandbox wrapper's egress is fenced by the kit
// allowlist; a credential ref is solicited, not executed). An explicit remote
// MCP endpoint DOES raise it: adopting one sends conversation context to a
// pack-selected third party and may launch OAuth, so it requires consent.
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
	return withPackTrustLock(func() error {
		store, err := loadPackTrustStore()
		if err != nil {
			return fmt.Errorf("pack trust state unreadable: %w", err)
		}
		key := store.TrustKey(p.Root)
		if got, ok := store.acceptedFingerprint(key); !ok || got != fp {
			return fmt.Errorf("pack %s inference credential routing is not accepted (or changed since acceptance) — run `pix pack use %s` to review it", p.Manifest.Name, p.Root)
		}
		return nil
	})
}

// LocalMCPClassifier resolves the registrar's local-vs-gateway partition into a
// predicate: TRUE for a name this host runs as a LOCAL stdio server, i.e.
// attaching it spawns a host command. With the partition ESTABLISHED, a name
// outside the local set (a remote catalog name, or gog) is reference-only
// Tier-0 — nothing pack-authored executes.
//
// UNKNOWN classification FAILS CLOSED: when the local set cannot be established
// (probe error, pix-host unresolved), every non-gog name is treated as
// host-exec so the gate fires. The name still lands in cfg.MCP and is attached
// via --mcp, so one ALREADY registered in the gateway would otherwise run its
// host command with NO gate ever shown. Over-prompting on a transient probe
// failure is acceptable; silently skipping the gate is not.
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
// (RefreshHostPackWrappers). A package var so tests can pin the partition and
// the composition root can supply the real env.
var PackLocalMCP = func() func(string) bool { return func(string) bool { return false } }

// ComputeHostBoM enumerates a pack's host bill-of-materials (pure, testable):
// MCP commands (resolved argv), host=true wrappers, [[bin]] external binaries,
// [[services]], the egress union, and credential VAR names. Display uses bare
// binary names so the result is deterministic; the SHAPE the user reviews is
// identical to what registration resolves.
//
// cfgGogAccount is the RESOLVED fallback account: the argv the user reviews —
// and the fingerprint the acceptance is recorded over — must be the argv that
// will actually run, so a later gog_account change re-gates.
//
// isLocalMCP is the local-vs-gateway partition (LocalMCPClassifier): only an
// integration.mcp resolving to a LOCAL host command enters the BoM. nil means
// "no partition available" and FAILS CLOSED exactly like an unknown probe.
//
// [[bin]] entries enter the BoM ONLY with host=true (mirroring host=true
// proxies), so flipping an inert bin to host=true later is a NEW surface.
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
		// No partition available at all: same fail-closed posture as an
		// unknown probe (round-3 #3) — gate every non-gog name.
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

// ComputeHostExecFingerprint hashes the ENTIRE host-exec surface of a pack —
// what the Tier-1 acceptance is actually FOR: every MCP's resolved argv, every
// host=true [[proxy]] script's CONTENT (sha256 of the bytes on disk,
// symlink-refused), every [[bin]] name + pinned sha, every [[services]] field,
// the egress union, and the credential VAR names. Entries are sorted into a
// canonical form so a pure manifest reorder never re-gates. Returns the
// fingerprint plus the per-proxy content hashes it was computed over, so the
// installer can verify the exact bytes it stages (no hash-then-install TOCTOU).
//
// THE ENCODING IS CANONICAL AND INJECTIVE: the surface is marshaled as a
// structured JSON document (fixed field order, sorted entries, every string
// JSON-escaped) and THAT is hashed. An ad-hoc NUL/newline concatenation is not
// injective for unconstrained strings — a value containing the delimiter bytes
// can encode a DIFFERENT surface with an identical hash.
//
// An unreadable host proxy script is an ERROR (fail closed): a surface that
// cannot be fingerprinted cannot be accepted or installed.
func ComputeHostExecFingerprint(root string, b hostBoM) (string, map[string]string, error) {
	return computeHostExecFingerprintWithSetup(root, b, nil)
}

// computeHostExecFingerprintWithSetup hashes immutable setup-hook snapshots
// when supplied. RunPackSetup executes those same bytes, binding the accepted
// fingerprint to the actual executable instead of re-opening mutable paths
// after the trust decision.
func computeHostExecFingerprintWithSetup(root string, b hostBoM, setupBytes map[string][]byte) (string, map[string]string, error) {
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
		Name      string            `json:"name"`
		Image     string            `json:"image,omitempty"`
		Manifest  string            `json:"manifest,omitempty"`
		EnvKeys   []string          `json:"env_keys,omitempty"`
		EnvValues map[string]string `json:"env_values,omitempty"`
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
	type fpInference struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Auth    string `json:"auth"`
		Service string `json:"service"`
		Header  string `json:"header"`
		Format  string `json:"format"`
	}
	type fpServiceResources struct {
		MemoryMB   int `json:"memory_mb"`
		CPUPercent int `json:"cpu_percent"`
	}
	// fpService pins EVERY [[services]] field (AC-PACK-02): any change to any
	// of them is a different host-exec surface and re-gates.
	type fpService struct {
		Name       string              `json:"name"`
		Runtime    string              `json:"runtime"`
		Activation string              `json:"activation"`
		Path       string              `json:"path,omitempty"`
		SHA        string              `json:"sha,omitempty"`
		Image      string              `json:"image,omitempty"`
		Argv       []string            `json:"argv,omitempty"`
		Env        []string            `json:"env,omitempty"`
		Port       int                 `json:"port,omitempty"`
		Listen     string              `json:"listen,omitempty"`
		Health     string              `json:"health,omitempty"`
		Mounts     []string            `json:"mounts,omitempty"`
		Network    []string            `json:"network,omitempty"`
		Resources  *fpServiceResources `json:"resources,omitempty"`
		License    string              `json:"license"`
		Source     string              `json:"source"`
	}
	type fpDoc struct {
		V             int           `json:"v"`
		MCP           []fpMCP       `json:"mcp"`
		Containers    []fpContainer `json:"container"`
		RemoteMCP     []fpRemote    `json:"remote_mcp"`
		Proxies       []fpProxy     `json:"proxy"`
		Bins          []fpBin       `json:"bin"`
		Egress        []string      `json:"egress"`
		Creds         []string      `json:"cred"`
		Prerequisites []string      `json:"prerequisites"`
		Setup         []fpSetup     `json:"setup"`
		Inference     []fpInference `json:"inference"`
		// Services is ADDITIVE with omitempty on purpose: a pack with no
		// [[services]] keeps its exact prior byte encoding, so every
		// already-accepted fingerprint stays valid. Injectivity holds: the key is
		// present iff a service is declared.
		Services []fpService `json:"services,omitempty"`
	}
	doc := fpDoc{V: 6}
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
		doc.Containers = append(doc.Containers, fpContainer{Name: c.Name, Image: c.Image, Manifest: c.Manifest, EnvKeys: keys, EnvValues: c.EnvValues})
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
	for _, inf := range b.Inference {
		doc.Inference = append(doc.Inference, fpInference{
			Name: inf.Name, URL: inf.URL, Auth: inf.Auth, Service: inf.Service, Header: inf.Header, Format: inf.Format,
		})
	}
	for _, svc := range b.Services {
		fp := fpService{
			Name: svc.Name, Runtime: svc.Runtime, Activation: svc.Activation,
			Path: svc.Path, SHA: strings.ToLower(strings.TrimSpace(svc.SHA)), Image: svc.Image,
			Argv: append([]string(nil), svc.Argv...),
			Env:  append([]string(nil), svc.Env...),
			Port: svc.Port, Listen: svc.Listen, Health: svc.Health,
			Mounts:  append([]string(nil), svc.Mounts...),
			Network: append([]string(nil), svc.Network...),
			License: svc.License, Source: svc.Source,
		}
		// Argv order is semantic (kept); env/mounts/network are sets — sorted
		// so a pure list reorder never re-gates.
		sort.Strings(fp.Env)
		sort.Strings(fp.Mounts)
		sort.Strings(fp.Network)
		if svc.Resources != nil {
			fp.Resources = &fpServiceResources{MemoryMB: svc.Resources.MemoryMB, CPUPercent: svc.Resources.CPUPercent}
		}
		doc.Services = append(doc.Services, fp)
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
// an explicit yes. --yes accepts (the screen still prints, for the record).
// Otherwise a non-TTY FAILS CLOSED — a CI/script adoption must never silently
// enable host code — and on a TTY the answer defaults to No. A non-nil error
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
