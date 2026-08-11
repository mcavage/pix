// trust.go — the Tier-1 pack trust gate (docs/design/packs.md §9).
//
// The model splits on whether the pack EXECUTES code on the host. Tier-0 —
// skills / knowledge / config / sandbox-only wrappers — adopts with NO prompt,
// non-TTY fine. Tier-1 is ANY host-exec facet (see hostBoM.Tier1): it
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
	"pix/host/hostenv"
	"pix/host/packinfo"
	"sort"
	"strconv"
	"strings"
)

// hostBoM is the host bill-of-materials: everything a pack would run on (or
// solicit from) THIS machine. Pure data — computed, rendered, gated, hashed.
type hostBoM struct {
	MCP            []hostBoMMCP         // built-in MCP servers the gateway spawns on the host
	Containers     []hostBoMContainer   // OCI MCP servers Docker runs on the host
	RemoteMCP      []hostBoMRemote      // remote MCP endpoints attached by the pack
	Proxies        []string             // host=true [[proxy]] wrapper names (bin/<name>)
	SandboxProxies []packinfo.PackProxy // host=false wrappers: sandbox commands forwarding elsewhere
	Bins           []packinfo.Bin       // [[bin]] external binaries (path + pinned sha)
	Egress         []string             // union of every facet's declared egress, sorted
	Creds          []string             // credential ENV VAR names solicited (never values)
	Prerequisites  []string             // pack-authored external state the user must bring
	Setup          []packinfo.SetupStep // pack setup executables, probes, and apply argv
	Inference      []hostBoMInference   // model endpoints plus credential-routing policy
	Services       []packinfo.Service   // [[services]] long-running units (normalized)
}

// The json tags on these types (and on packinfo.Service) ARE the canonical
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

// VerifyPackLaunchTrust closes the adoption-to-launch gap for EVERY mutable
// Tier-1 surface, not only credential-routing inference: a pack stays mutable
// after `pack use`, so before a launch consumes ANY of its host-exec
// contributions (host MCP argv, containers, remote MCP endpoints, host=true
// [[proxy]] scripts, [[bin]] pins, [[setup]] hooks, [[services]] units, or
// inference endpoint/service/header policy), recompute the COMPLETE host-exec
// fingerprint and require an exact launcher-owned trust-store match. Tier-0
// (skills / knowledge / sandbox-only wrappers) passes with no store lookup,
// exactly as adoption promised. "Reference-only integrations" used to be a
// Tier-0 case and no longer can be: every integration declares a transport, and
// all four are host-exec or third-party egress. Nothing here
// executes pack code: setup hooks and wrapper scripts are only HASHED
// (hashHostExecFile), never run.
func VerifyPackLaunchTrust(p *packinfo.Info, env hostenv.Env) error {
	if p == nil {
		return nil
	}
	bom := ComputeHostBoM(p)
	if !bom.Tier1() {
		return nil // Tier-0: no host-exec surface, nothing to re-verify
	}
	fp, _, err := ComputeHostExecFingerprint(p.Root, bom)
	if err != nil {
		return fmt.Errorf("pack %s host-exec trust surface: %w", p.Manifest.Name, err)
	}
	return requireAcceptedFingerprint(p, fp, "host-exec surfaces")
}

// ComputeHostBoM enumerates a pack's host bill-of-materials: MCP commands
// (declared argv), host=true wrappers and [[bin]]s, [[services]], setup hooks,
// inference gateways, the egress union and credential VAR names.
//
// It is a PURE FUNCTION OF THE MANIFEST — no subprocess, no PATH lookup, no
// ambient host state. That property is load-bearing, not incidental: this
// computes what a user consents to, so the same pack must produce the same
// bill of materials on every machine and at every moment. The previous version
// asked a host binary at runtime which servers were "local", which meant the
// answer could change without the pack changing, and every caller had to carry
// a fail-closed guess for when the probe could not answer at all.
//
// Bare binary names (never PATH-resolved paths) keep the reviewed SHAPE
// identical to what registration resolves. A [[bin]] enters ONLY with
// host=true, so flipping an inert bin is a NEW surface.
func ComputeHostBoM(p *packinfo.Info) hostBoM {
	var b hostBoM
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
		case strings.TrimSpace(ig.Command) != "":
			// The reviewed argv is exactly what the pack declared: the bare
			// command plus its literal args. Registration resolves the command
			// to an absolute path at spawn time, which is a property of THIS
			// machine's PATH and deliberately not part of what you consent to.
			b.MCP = append(b.MCP, hostBoMMCP{
				Name: name,
				Argv: append([]string{strings.TrimSpace(ig.Command)}, ig.Args...),
			})
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
		b.Services = append(b.Services, svc.Normalized())
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
// [[proxy]] script's CONTENT, every [[bin]] pin, every [[services]] field,
// setup hook bytes, inference policy, egress and credential VAR names. Entries
// sort canonically, so a pure manifest reorder never re-gates, and the
// per-proxy content hashes come back so the installer verifies the exact bytes
// it stages (no TOCTOU).
//
// THE ENCODING IS CANONICAL AND INJECTIVE: a structured JSON document (fixed
// field order, sorted entries, every string escaped) is hashed, because an
// ad-hoc NUL/newline concatenation is not injective — a value containing the
// delimiter could encode a DIFFERENT surface with an identical hash. An
// unfingerprintable surface is an ERROR: neither acceptable nor installable.
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
	// Require/Apply are ADDITIVE with omitempty, exactly like Services above: a
	// pack using the executable form encodes byte-identically to before, so
	// every already-accepted fingerprint stays valid. They are fingerprinted at
	// all because they are EXECUTABLE INTENT — a declarative step names binaries
	// and argv that will run on this host, so changing one has to re-gate just
	// as editing a hook script does.
	type fpSetup struct {
		ID          string                  `json:"id"`
		Path        string                  `json:"path"`
		SHA         string                  `json:"sha"`
		CheckArgs   []string                `json:"check_args"`
		ApplyArgs   []string                `json:"apply_args"`
		Required    bool                    `json:"required"`
		Description string                  `json:"description"`
		Require     []packinfo.SetupRequire `json:"require,omitempty"`
		Apply       []packinfo.SetupApply   `json:"apply,omitempty"`
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
		Services []packinfo.Service `json:"services,omitempty"`
	}
	doc := fpDoc{V: 6}
	proxySHA := map[string]string{}
	doc.MCP = sortedByKey(b.MCP, func(m hostBoMMCP) string { return m.Name + "\x00" + strings.Join(m.Argv, "\x00") })
	for _, c := range b.Containers {
		c.EnvKeys = sortedByKey(c.EnvKeys, func(k string) string { return k })
		doc.Containers = append(doc.Containers, c)
	}
	doc.Containers = sortedByKey(doc.Containers, func(c hostBoMContainer) string { return c.Name })
	doc.RemoteMCP = sortedByKey(b.RemoteMCP, func(r hostBoMRemote) string { return r.Name })
	for _, name := range b.Proxies {
		sha, err := hashHostExecFile(filepath.Join(root, "bin", name), "host wrapper "+strconv.Quote(name))
		if err != nil {
			return "", nil, err
		}
		proxySHA[name] = sha
		doc.Proxies = append(doc.Proxies, fpProxy{Name: name, SHA: sha})
	}
	doc.Proxies = sortedByKey(doc.Proxies, func(p fpProxy) string { return p.Name })
	for _, bn := range b.Bins {
		doc.Bins = append(doc.Bins, fpBin{Name: bn.Name, SHA: strings.ToLower(strings.TrimSpace(bn.SHA)), Host: bn.Host})
	}
	doc.Bins = sortedByKey(doc.Bins, func(b fpBin) string { return b.Name + "\x00" + b.SHA })
	doc.Egress = sortedByKey(b.Egress, func(e string) string { return e })
	doc.Creds = sortedByKey(b.Creds, func(c string) string { return c })
	doc.Prerequisites = append([]string(nil), b.Prerequisites...)
	for _, s := range b.Setup {
		sha, ok := "", false
		if s.Declarative() {
			// No file to hash: the step IS the manifest data, which the
			// Require/Apply fields below carry into the fingerprint directly.
			ok = true
		}
		if data, snapshotted := setupBytes[s.ID]; snapshotted {
			sum := sha256.Sum256(data)
			sha, ok = hex.EncodeToString(sum[:]), true
		}
		if !ok {
			var err error
			if sha, err = hashHostExecFile(filepath.Join(root, s.Path), "setup hook "+strconv.Quote(s.ID)); err != nil {
				return "", nil, err
			}
		}
		doc.Setup = append(doc.Setup, fpSetup{
			ID: s.ID, Path: filepath.Clean(s.Path), SHA: sha,
			CheckArgs: append([]string(nil), s.CheckArgs...), ApplyArgs: append([]string(nil), s.ApplyArgs...),
			Required: s.Required, Description: s.Description,
			Require: append([]packinfo.SetupRequire(nil), s.Require...),
			Apply:   append([]packinfo.SetupApply(nil), s.Apply...),
		})
	}
	doc.Setup = sortedByKey(doc.Setup, func(s fpSetup) string { return s.ID })
	doc.Inference = append(doc.Inference, b.Inference...)
	for _, svc := range b.Services {
		// Argv order is semantic (kept); env/mounts/network are sets — sorted so
		// a pure list reorder never re-gates.
		svc.SHA = strings.ToLower(strings.TrimSpace(svc.SHA))
		id := func(s string) string { return s }
		svc.Env, svc.Mounts, svc.Network = sortedByKey(svc.Env, id), sortedByKey(svc.Mounts, id), sortedByKey(svc.Network, id)
		doc.Services = append(doc.Services, svc)
	}
	// Names are unique (validatePackServices), so name order is total.
	doc.Services = sortedByKey(doc.Services, func(s packinfo.Service) string { return s.Name })
	enc, err := json.Marshal(doc)
	if err != nil {
		return "", nil, fmt.Errorf("encoding host-exec surface: %v", err)
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:]), proxySHA, nil
}

// sortedByKey returns a sorted COPY of in — the canonical ordering every
// fingerprint section needs, so a pure manifest reorder never re-gates.
func sortedByKey[T any](in []T, key func(T) string) []T {
	out := append([]T(nil), in...)
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// hashHostExecFile is the fingerprint side of the content pin: hash the bytes
// on disk, symlink-refused. An unhashable surface is an ERROR — it can be
// neither accepted nor installed.
func hashHostExecFile(path, label string) (string, error) {
	if packinfo.IsSymlinkPath(path) {
		return "", fmt.Errorf("%s is a symlink; refusing to fingerprint it", label)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %v (cannot fingerprint the host-exec surface; fail closed)", label, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
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
		case packinfo.ServiceRuntimeContainer:
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
		if !s.Declarative() {
			fmt.Fprintf(out, "  %-20s %s — %s (%s %s)\n", kind, s.ID, label, s.Path, strings.Join(s.ApplyArgs, " "))
			continue
		}
		// A declarative step runs no pack-supplied code, but it DOES name
		// binaries and argv that execute on this Mac. Consent means seeing
		// them, so every condition and every remediation is printed — never
		// summarised as a count.
		fmt.Fprintf(out, "  %-20s %s — %s\n", kind, s.ID, label)
		for _, r := range s.Require {
			switch r.Kind {
			case "bin":
				fmt.Fprintf(out, "                       Needs: %s on PATH (install: %s)\n", r.Name, r.Install)
			case "op-ref":
				fmt.Fprintf(out, "                       Needs: %s as a 1Password reference\n", r.Env)
			case "probe":
				fmt.Fprintf(out, "                       Checks: %s\n", strings.Join(r.Argv, " "))
			}
		}
		for _, a := range s.Apply {
			note := ""
			if a.Kind == "interactive" {
				note = "  [interactive; may open a browser]"
			}
			fmt.Fprintf(out, "                       Runs on this Mac: %s%s\n", strings.Join(a.Argv, " "), note)
		}
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
