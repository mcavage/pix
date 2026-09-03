// sidecar.go parses the strict pix.toml sidecar: docs/design/environments.md
// §5.2. It parses ONLY the design-of-record sections/fields — [models],
// [agents], [memory], [pi].skills, [host.mcp], [[host.services]], and
// [inference.*] — into typed facts. Every other key fails Parse; there is no
// silent default. Native-owned concepts (kits, workspaces, env, secrets,
// bindings, MCP url/command definitions, resources/sandboxOptions, ports)
// belong to .sbxenv.yaml and are refused here, never merged or overridden.
//
// This package does not parse native sbx YAML; that is a leaf environment
// adapter's job, not this sidecar's.
package envinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Sidecar is the strict, typed parse of one pix.toml file.
type Sidecar struct {
	Schema    int64             `toml:"schema"`
	Models    ModelsSection     `toml:"models"`
	Agents    map[string]string `toml:"agents"`
	Memory    MemorySection     `toml:"memory"`
	Pi        PiSection         `toml:"pi"`
	Host      HostSection       `toml:"host"`
	Inference InferenceSection  `toml:"inference"`
	Setup     []SetupHook       `toml:"setup"`
}

// SetupHook is one `[[setup]]` entry: the environment's own, explicitly
// consented replacement for a v1 pack's authored install/auth hook. It is
// NOT a plugin system and NOT a registry — a hook exists only inside the
// environment directory that declares it, runs only under an explicit `pix
// setup --env NAME` after that environment's trust review accepted it, and
// is never reached by `pix run`, `pix doctor`, or any implicit launch.
//
// The grammar is deliberately narrow, because every field here is host
// execution a human has to be able to read in one consent screen:
//
//	[[setup]]
//	id = "tool"                 # [A-Za-z0-9._-]+, unique in this file
//	command = "./setup-tool"    # relative to the environment root, or absolute
//	check_args = ["check"]      # argv that reports readiness; 0 = ready
//	apply_args = ["install"]    # argv that makes it ready
//	required = true             # absent = false = optional (warn, never fail)
//	kind = "install"            # install | auth; absent = install
//	inputs = ["lib/helper.sh"]  # OPTIONAL companion scripts/data, see below
//
// There is no shell, no interpolation, and no env/value injection: argv is
// executed directly (os/exec), so a `command` is a program, never a script
// fragment. A bare command name is refused outright (PATH is ambiguous and
// unfingerprintable); the executable's own content hash is what the trust
// fingerprint binds to.
//
// `inputs` is how a hook declares every companion script or data file it
// needs besides `command` itself. Each entry must be a RELATIVE path
// (never absolute, never containing a `..` segment), already `filepath.
// Clean`-form (no redundant `.`, `//`, or trailing slash), and unique
// within this one hook's own list. Each is resolved against the
// environment root exactly like a relative `command`, content-hashed the
// same way, and rendered in the trust bill alongside it: an `inputs` entry
// is host execution's companion, reviewed with the same rigor as the
// executable it rides with.
//
// The reason `inputs` exists at all: a hook actually runs from a private,
// per-execution snapshot directory containing ONLY the reviewed executable
// and its declared inputs (envsetup.Run's TOCTOU snapshot), with that
// snapshot's own root as the hook's cwd. A companion file living next to
// `command` in the environment directory that is NOT declared here is
// therefore simply not present when the hook runs — a hook must name every
// sibling script or data file it depends on via `inputs`, using paths
// relative to that snapshot cwd (the same relative form declared here),
// or the dependency is unavailable by default.
type SetupHook struct {
	ID        string   `toml:"id"`
	Command   string   `toml:"command"`
	CheckArgs []string `toml:"check_args"`
	ApplyArgs []string `toml:"apply_args"`
	Required  bool     `toml:"required"`
	Kind      string   `toml:"kind"`
	Inputs    []string `toml:"inputs"`
}

// Setup hook kinds. A hook that needs a human at a terminal (a browser
// login, a device-code prompt) declares kind = "auth" so a non-interactive
// run refuses it by name instead of hanging on a prompt nobody can answer.
const (
	SetupKindInstall = "install"
	SetupKindAuth    = "auth"
)

// EffectiveKind is Kind with the documented default applied: an absent kind
// is "install", never a guessed "auth".
func (h SetupHook) EffectiveKind() string {
	if h.Kind == "" {
		return SetupKindInstall
	}
	return h.Kind
}

// setupIDRE is the whole id grammar: no spaces, no path separators, no
// shell metacharacters, no empty string.
var setupIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ModelsSection is the literal model roster main entry, §6.3.
type ModelsSection struct {
	Main string `toml:"main"`
	// Exclusive narrows model resolution to this environment's own
	// [inference.*] definitions only (§6.3): the private-gateway compliance
	// boundary. Absent means false, never a silently-assumed true.
	Exclusive bool `toml:"exclusive"`
}

// MemorySection is the environment's memory scope, §5.2.
type MemorySection struct {
	Scope string `toml:"scope"`
}

// PiSection holds Pi-only load paths. Skills is authored relative to the
// pix.toml directory; ValidateSkillWorkspaces confirms each canonical path
// resolves inside a workspace the effective environment actually mounts.
// Mounting a directory does not by itself tell Pi to load it.
type PiSection struct {
	Skills []string `toml:"skills"`
}

// HostSection covers the two host-only concepts pix.toml may annotate: MCP
// server metadata sbx cannot express, and non-MCP host services.
type HostSection struct {
	// MCP annotates a local-command MCP server declared in .sbxenv.yaml. It
	// may never carry that server's command definition (url/command are
	// native-owned and rejected).
	MCP      map[string]HostMCPEntry `toml:"mcp"`
	Services []HostService           `toml:"services"`
}

// HostMCPEntry is optional metadata for one [host.mcp.<name>] entry. Name is
// populated from the table key, not decoded from a field.
//
// EnvKeys are SECRET names: each must resolve to an op:// reference
// recorded with `pix secret set` before the op-run wrapper can hand this
// server a real value (setup's declared-requirements check, cmd/pix's
// validateDeclaredEnvironmentValues, proves this before any `[[setup]]`
// hook runs). PlainKeys are the opposite: names the SAME op-run wrapper
// must also supply, but whose value is not sensitive (an account address,
// a company domain, ...) and must never be forced into an op:// reference
// just to satisfy the wrapper's all-or-nothing env-file contract. A name
// may appear in at most one of the two lists; ParseSidecar refuses an
// entry naming it in both.
type HostMCPEntry struct {
	Name      string   `toml:"-"`
	EnvKeys   []string `toml:"env_keys"`
	PlainKeys []string `toml:"plain_keys"`
	ProbeArgs []string `toml:"probe_args"`
}

// HostService is one [[host.services]] entry: an optional non-MCP process the
// host must run alongside the sandbox.
type HostService struct {
	Name    string   `toml:"name"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Port    int64    `toml:"port"`
	Probe   string   `toml:"probe"`
}

// InferenceSection declares environment-local Pi provider definitions: custom
// backends and the models they serve.
type InferenceSection struct {
	Backends map[string]InferenceBackend `toml:"backends"`
	Models   []InferenceModel            `toml:"models"`
}

// InferenceBackend is one [inference.backends.<name>] entry.
//
// The three credential_* fields describe an `auth = "sbx-session"` backend:
// the sandbox never holds the long-lived token, it sends a sentinel and the
// sbx proxy swaps in the real value of the named host credential service on
// the way out. They are declarations of WIRING, never of a secret value —
// credential_service names an sbx credential service, key_env names the
// environment variable the sentinel travels in, and the header/format pair
// is the literal request shape. All three are host-affecting, so all three
// are fingerprinted and rendered on the trust bill (workflow/env/bom.go's
// InferenceFact); editing any of them re-gates the environment.
//
// Without these an environment could not describe a credentialed gateway at
// all: the launcher's kit synthesis (inference/live.go) has always read them
// from machine config.toml, and refuses an sbx-session backend that supplies
// no identity. Carrying them here is what makes "the environment carries its
// own auth" true for an environment repository rather than only for a
// hand-edited machine file.
type InferenceBackend struct {
	Driver   string `toml:"driver"`
	Protocol string `toml:"protocol"`
	BaseURL  string `toml:"base_url"`
	Auth     string `toml:"auth"`
	KeyEnv   string `toml:"key_env"`
	// CredentialService names the sbx credential service whose session token
	// the proxy injects. Required (with KeyEnv) when Auth is "sbx-session".
	CredentialService string `toml:"credential_service"`
	// CredentialHeader defaults to "Authorization" when empty.
	CredentialHeader string `toml:"credential_header"`
	// CredentialFormat defaults to "Bearer %s" when empty.
	CredentialFormat string `toml:"credential_format"`
}

// InferenceModel is one [[inference.models]] entry.
type InferenceModel struct {
	ID              string `toml:"id"`
	Backend         string `toml:"backend"`
	UpstreamID      string `toml:"upstream_id"`
	ContextWindow   int64  `toml:"context_window"`
	MaxOutputTokens int64  `toml:"max_output_tokens"`
	Reasoning       bool   `toml:"reasoning"`
}

// ModelReference names one place pix.toml references a model by id. This
// package only collects the fact; resolving it against the effective
// inference catalog (machine config plus this environment's own
// [inference.*] definitions, narrowed further when Exclusive is set) is a
// downstream validator's job.
type ModelReference struct {
	// Field is the dotted source key, e.g. "models.main" or "agents.engineer".
	Field string
	Model string
}

// ModelReferences returns every model-id reference in the sidecar:
// models.main (when set) followed by each [agents] entry, agent names sorted
// for a stable, reproducible result.
func (s *Sidecar) ModelReferences() []ModelReference {
	var refs []ModelReference
	if s.Models.Main != "" {
		refs = append(refs, ModelReference{Field: "models.main", Model: s.Models.Main})
	}
	names := make([]string, 0, len(s.Agents))
	for name := range s.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		refs = append(refs, ModelReference{Field: "agents." + name, Model: s.Agents[name]})
	}
	return refs
}

// Error is a strict pix.toml validation failure: exact file, line, and key.
// Error() always renders "<file>:<line>: <reason>: <key>" — AC-18's shape —
// so a caller can point a user at the exact offending line, never a default.
type Error struct {
	File   string
	Line   int
	Key    string
	Reason string
}

// Error() always names a key when there is one (AC-18's shape). Some real
// TOML syntax errors carry no key at all — they are purely positional (a
// blank key before '=', a stray token before anything was parsed) — and
// %q-quoting an empty Key would print a bare `: ""` on the end, which reads
// as "the key is the empty string" rather than "there was no key". Key ==
// "" therefore drops the trailing `: %q` clause instead of rendering it
// empty.
func (e *Error) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Reason)
	}
	return fmt.Sprintf("%s:%d: %s: %q", e.File, e.Line, e.Reason, e.Key)
}

const ownedByEnvFile = "native-owned; declare it in .sbxenv.yaml, not pix.toml"

// arrayOfTableRoots names sidecar top-level fields the Go struct decodes as
// a map (agents.<name> = <model>), not a slice — so `[[agents]]` (array-of-
// tables syntax) is a structural type mismatch the TOML decoder does NOT
// itself refuse: BurntSushi silently decodes each array element's keys as
// Undecoded (agents stays empty) rather than erroring on the shape
// mismatch, because a Go map is a perfectly valid decode target for either
// TOML shape considered in isolation. Reported as a plain "unknown key" for
// e.g. "agents.name", that message sends the author to fix the wrong thing
// (as if `name` were a typo'd field) instead of the real, structural
// problem named by arrayOfTableMisuseReason below: `agents` itself must be
// authored as a table, not an array of tables.
var arrayOfTableRoots = map[string]bool{
	"agents": true,
}

// nativeOwnedRoot names the top-level fields .sbxenv.yaml owns exclusively.
// A pix.toml that declares any of these fails closed with a reason pointing
// at the right file, rather than the generic "unknown key".
var nativeOwnedRoot = map[string]bool{
	"kits":           true,
	"workspaces":     true,
	"workspace":      true,
	"env":            true,
	"secrets":        true,
	"bindings":       true,
	"mcp":            true,
	"resources":      true,
	"sandboxOptions": true,
	"ports":          true,
}

// nativeOwnedReason classifies an offending dotted key path: a top-level
// native-owned field, a host.mcp.<name>.url/command command definition
// (also native-owned — sbx already owns that server's transport), or a
// plain unknown key/typo. key is authoritative — each element is one
// resolved TOML key segment, quoted-dotted segments already collapsed to a
// single element by the decoder — so this never re-splits on '.'.
func nativeOwnedReason(key toml.Key) string {
	if len(key) == 1 && nativeOwnedRoot[key[0]] {
		return ownedByEnvFile
	}
	if len(key) == 4 && key[0] == "host" && key[1] == "mcp" && (key[3] == "url" || key[3] == "command") {
		return ownedByEnvFile
	}
	return "unknown key"
}

// arrayOfTableMisuseReason recognizes the ONE shape arrayOfTableRoots
// exists for: an Undecoded key rooted at a map-typed field with at least
// one further segment. That shape can ONLY arise from `[[root]]` (array-of-
// tables) syntax against a map field — the correct `[root]\nkey = value`
// form decodes cleanly with no Undecoded entries at all, so there is no
// ambiguity to guess at. ok is false for every other key, including a
// bare, single-segment "agents" (which cannot happen: a wholly unknown
// bare `agents = ...` of the wrong TOML type is a decode type error, not an
// Undecoded key).
func arrayOfTableMisuseReason(key toml.Key) (root, reason string, ok bool) {
	if len(key) < 2 || !arrayOfTableRoots[key[0]] {
		return "", "", false
	}
	root = key[0]
	return root, fmt.Sprintf("%q must be declared as a table ([%s]), not an array of tables ([[%s]])", root, root, root), true
}

// resolveLine finds the diagnostic line for key in locs, degrading to its
// containing table/key when the exact dotted path was never itself
// recorded as a line. That gap is real: locateKeyLines is a line-oriented
// scanner over TOML's own `[section]`/`[[section]]` headers and top-level
// `key = value` assignments, and it never walks into a `{ ... }` inline
// table literal's own sub-keys — so an unknown key nested only inside one
// (a REAL Undecoded entry; toml.MetaData.Undecoded does drill into inline
// tables) has nothing recorded on its own path. Progressively shortening
// the path to its containing table, and ultimately the whole file (line 1),
// is how a partial line answer degrades — NEVER to 0, which no editor line
// number can mean.
func resolveLine(locs map[string]int, key toml.Key) int {
	for n := len(key); n > 0; n-- {
		if line, ok := locs[toml.Key(key[:n]).String()]; ok {
			return line
		}
	}
	return 1
}

// ParseSidecar reads and strictly validates the pix.toml sidecar at path,
// returning typed facts for exactly the design-of-record sections. Any key
// outside that schema — a typo or a native-owned field sbx already owns —
// fails the parse with an *Error naming the exact key and line; nothing is
// silently defaulted or merged. Named ParseSidecar (not Parse) because this
// package also parses the native .sbxenv.yaml document via the unrelated
// Parse(path string) (*Document, error) in parse.go; the two are distinct
// strict parsers over distinct files and must never be confused at the call
// site.
//
// Strictness comes from the real TOML decoder's own metadata
// (toml.MetaData.Undecoded), never from a hand-rolled regex line-scanner: a
// scanner that only recognizes bare, unquoted keys would silently wave a
// quoted unknown or native-owned key straight through to the final decode,
// which BurntSushi does not error on for fields the target struct doesn't
// declare. Undecoded also resolves TOML's own quoting rules, so a quoted
// dotted identity like host.mcp."github.copilot" is one key segment, never
// split at the embedded dot.
func ParseSidecar(path string) (*Sidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("envinfo: read %s: %w", path, err)
	}
	text := string(data)
	base := filepath.Base(path)

	var s Sidecar
	md, err := toml.Decode(text, &s)
	if err != nil {
		if pe, ok := err.(toml.ParseError); ok {
			line := pe.Position.Line
			if line <= 0 {
				line = 1
			}
			return nil, &Error{File: base, Line: line, Key: pe.LastKey, Reason: "invalid TOML: " + pe.Message}
		}
		return nil, fmt.Errorf("envinfo: %s: %w", base, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		key := undecoded[0]
		locs := locateKeyLines(text)
		if root, reason, ok := arrayOfTableMisuseReason(key); ok {
			return nil, &Error{
				File:   base,
				Line:   resolveLine(locs, toml.Key{root}),
				Key:    root,
				Reason: reason,
			}
		}
		return nil, &Error{
			File:   base,
			Line:   resolveLine(locs, key),
			Key:    key.String(),
			Reason: nativeOwnedReason(key),
		}
	}

	for name, entry := range s.Host.MCP {
		entry.Name = name
		s.Host.MCP[name] = entry
	}
	if err := validateHostMCPKeys(base, text, s.Host.MCP); err != nil {
		return nil, err
	}
	if err := validateSetupHooks(base, text, s.Setup); err != nil {
		return nil, err
	}
	return &s, nil
}

// validateHostMCPKeys refuses a [host.mcp.<name>] entry that lists the same
// env var name in both env_keys and plain_keys — the two lists are mutually
// exclusive classifications (a secret that must resolve to an op:// ref, or
// a plain value that must never be one), so a name in both is an authoring
// mistake, not something either list's own reader could silently resolve.
func validateHostMCPKeys(base, text string, mcp map[string]HostMCPEntry) error {
	for _, name := range sortedHostMCPEntryKeys(mcp) {
		entry := mcp[name]
		plain := map[string]bool{}
		for _, k := range entry.PlainKeys {
			plain[k] = true
		}
		for _, k := range entry.EnvKeys {
			if plain[k] {
				key := fmt.Sprintf("host.mcp.%s", name)
				line := locateKeyLines(text)[key]
				if line == 0 {
					line = 1
				}
				return &Error{File: base, Line: line, Key: key, Reason: fmt.Sprintf("%q is listed in both env_keys and plain_keys; it must be a secret (env_keys) or a plain value (plain_keys), never both", k)}
			}
		}
	}
	return nil
}

func sortedHostMCPEntryKeys(m map[string]HostMCPEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateSetupHooks enforces the whole `[[setup]]` grammar at parse time,
// with no filesystem access at all: identity, argv shape, and character
// safety. Everything that depends on the disk — the command resolving to a
// REGULAR, executable, non-symlink file whose content can be hashed — is
// deliberately NOT decided here: that belongs to whatever computes the
// trust bill of materials (workflow/env.ComputeBoM) and, again, to the
// re-proof taken immediately before execution. A parse must stay a pure
// function of the file's bytes so `pix env show` can render a sidecar that
// names a tool this host has not installed yet.
//
// Every refusal is an *Error naming the `setup` key and its line, because
// the author needs the offending entry, not a generic "invalid pix.toml".
func validateSetupHooks(base, text string, hooks []SetupHook) error {
	if len(hooks) == 0 {
		return nil
	}
	line := locateKeyLines(text)["setup"]
	if line == 0 {
		line = 1
	}
	bad := func(format string, args ...any) error {
		return &Error{File: base, Line: line, Key: "setup", Reason: fmt.Sprintf(format, args...)}
	}
	seen := map[string]bool{}
	for i, h := range hooks {
		where := fmt.Sprintf("setup[%d]", i)
		if !setupIDRE.MatchString(h.ID) {
			return bad("%s: id %q must match [A-Za-z0-9._-]+", where, h.ID)
		}
		if seen[h.ID] {
			return bad("%s: duplicate id %q; each setup hook needs its own identity", where, h.ID)
		}
		seen[h.ID] = true
		if h.Command == "" {
			return bad("%s (%s): command is required", where, h.ID)
		}
		if hasControlChars(h.Command) {
			return bad("%s (%s): command contains a control character", where, h.ID)
		}
		if !strings.ContainsRune(h.Command, '/') {
			return bad("%s (%s): command %q must be a path (./tool or /abs/tool); a bare name resolves through PATH, which is ambiguous and cannot be fingerprinted", where, h.ID, h.Command)
		}
		if hasDotDotSegment(h.Command) {
			return bad("%s (%s): command %q must not contain a %q segment", where, h.ID, h.Command, "..")
		}
		for _, spec := range []struct {
			key  string
			argv []string
		}{{"check_args", h.CheckArgs}, {"apply_args", h.ApplyArgs}} {
			if len(spec.argv) == 0 {
				return bad("%s (%s): %s is required and must name at least one argument", where, h.ID, spec.key)
			}
			for _, arg := range spec.argv {
				if hasControlChars(arg) {
					return bad("%s (%s): %s contains a control character", where, h.ID, spec.key)
				}
			}
		}
		switch h.Kind {
		case "", SetupKindInstall, SetupKindAuth:
		default:
			return bad("%s (%s): kind %q must be %q or %q", where, h.ID, h.Kind, SetupKindInstall, SetupKindAuth)
		}
		seenInput := map[string]bool{}
		for _, in := range h.Inputs {
			if in == "" {
				return bad("%s (%s): inputs entries must not be empty", where, h.ID)
			}
			if hasControlChars(in) {
				return bad("%s (%s): inputs entry %q contains a control character", where, h.ID, in)
			}
			if filepath.IsAbs(in) {
				return bad("%s (%s): inputs entry %q must be relative to the environment root, not absolute", where, h.ID, in)
			}
			if hasDotDotSegment(in) {
				return bad("%s (%s): inputs entry %q must not contain a %q segment", where, h.ID, in, "..")
			}
			if filepath.Clean(filepath.ToSlash(in)) != filepath.ToSlash(in) {
				return bad("%s (%s): inputs entry %q must already be a clean relative path (no %q, %q, or a trailing slash)", where, h.ID, in, "./", "//")
			}
			if seenInput[in] {
				return bad("%s (%s): duplicate inputs entry %q; each companion path is declared once", where, h.ID, in)
			}
			seenInput[in] = true
		}
	}
	return nil
}

// hasControlChars reports whether s carries any C0/C7F control character —
// a NUL an exec boundary would truncate at, or an ESC/CR a consent screen
// could be repainted with. Rejected at parse time so neither the renderer
// nor os/exec ever sees one.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// hasDotDotSegment reports whether a command path contains a ".." element.
// A relative hook command resolves against the environment root; a `..`
// segment would walk out of the directory the reviewer actually read.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// ValidateSkillWorkspaces confirms every [pi].skills path resolves, relative
// to the pix.toml directory, to a REAL (symlink-resolved) location inside
// one of the supplied workspace roots. workspaces need only be absolute —
// the environment adapter's job, not this package's — since this function
// resolves symlinks on both sides of the comparison itself:
//
//   - each workspace root is passed through filepath.EvalSymlinks once. A
//     root that does not exist, or whose symlink chain is broken, cannot
//     legitimately contain anything, so it silently drops out of the
//     containment set rather than aborting the whole call — one intact
//     workspace root among several still gets a real check.
//   - each skills path is passed through filepath.EvalSymlinks too. A path
//     that does not exist, or is an unresolvable/looping symlink, fails
//     closed with its own precise reason rather than being compared at all.
//   - containment is then checked on the two RESOLVED (real) paths, not the
//     authored ones. This is what refuses a skills entry that is itself a
//     symlink pointing outside every workspace: its literal path may sit
//     inside a workspace while its real target does not, and the real
//     target is what Pi actually loads.
//
// An environment with no declared workspaces — or none that resolve — fails
// every skills path closed rather than defaulting to "allowed".
func ValidateSkillWorkspaces(sidecarPath string, s *Sidecar, workspaces []string) error {
	base := filepath.Base(sidecarPath)
	dir := filepath.Dir(sidecarPath)
	line := 0
	if data, err := os.ReadFile(sidecarPath); err == nil {
		line = locateKeyLines(string(data))["pi.skills"]
	}

	var realWorkspaces []string
	for _, ws := range workspaces {
		real, err := filepath.EvalSymlinks(filepath.Clean(ws))
		if err != nil {
			continue
		}
		realWorkspaces = append(realWorkspaces, real)
	}

	for _, rel := range s.Pi.Skills {
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, rel)
		}
		abs = filepath.Clean(abs)

		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return &Error{
				File:   base,
				Line:   line,
				Key:    "pi.skills",
				Reason: fmt.Sprintf("%q does not exist or could not be resolved: %v", rel, err),
			}
		}

		inside := false
		for _, ws := range realWorkspaces {
			if real == ws || strings.HasPrefix(real, ws+string(filepath.Separator)) {
				inside = true
				break
			}
		}
		if !inside {
			return &Error{
				File:   base,
				Line:   line,
				Key:    "pi.skills",
				Reason: fmt.Sprintf("%q resolves outside every declared workspace", rel),
			}
		}
	}
	return nil
}

// locateKeyLines walks pix.toml text line by line purely to find the first
// line each dotted key path is defined at, for diagnostics. It is
// TOML-quote-aware — a quoted segment (e.g. "github.copilot") is kept as one
// key element, matching how the real decoder resolves it — but it is
// cosmetic only: Parse's accept/reject decision is made from
// toml.MetaData.Undecoded against the already-validated, already-decoded
// document, never from this scan. An imperfect match here can only cost a
// diagnostic's line number, never create or hide a validation bypass.
func locateKeyLines(text string) map[string]int {
	locs := map[string]int{}
	var curPath []string
	for i, raw := range strings.Split(text, "\n") {
		line := i + 1
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if inner, ok := trimBrackets(t, "[[", "]]"); ok {
			path := parseDottedKey(inner)
			curPath = path
			recordLoc(locs, path, line)
			continue
		}
		if inner, ok := trimBrackets(t, "[", "]"); ok {
			path := parseDottedKey(inner)
			curPath = path
			recordLoc(locs, path, line)
			continue
		}
		if keyText, ok := splitKeyValueLine(t); ok {
			full := append(append([]string{}, curPath...), parseDottedKey(keyText)...)
			recordLoc(locs, full, line)
		}
	}
	return locs
}

// trimBrackets strips a table-header line's open/close delimiters (and any
// trailing comment), returning the inner dotted-key text. The close search
// is quote-aware so a literal "]" inside a quoted segment or a trailing
// comment never gets mistaken for the header's own delimiter.
func trimBrackets(t, open, close string) (string, bool) {
	if !strings.HasPrefix(t, open) {
		return "", false
	}
	if open == "[" && strings.HasPrefix(t, "[[") {
		return "", false
	}
	rest := t[len(open):]
	idx := findUnquotedIndex(rest, close)
	if idx < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:idx]), true
}

// findUnquotedIndex returns the index of sub's first occurrence in s outside
// any quoted span, or -1.
func findUnquotedIndex(s, sub string) int {
	var inQuote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == '\\' && inQuote == '"' && i+1 < len(s) {
				i++
				continue
			}
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if strings.HasPrefix(s[i:], sub) {
			return i
		}
	}
	return -1
}

// splitKeyValueLine finds a line's "key = value" split point, tracking quote
// state so an '=' inside a quoted key or value is never mistaken for the
// assignment operator.
func splitKeyValueLine(t string) (string, bool) {
	var inQuote byte
	for i := 0; i < len(t); i++ {
		c := t[i]
		if inQuote != 0 {
			if c == '\\' && inQuote == '"' && i+1 < len(t) {
				i++
				continue
			}
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
		case '=':
			return strings.TrimSpace(t[:i]), true
		}
	}
	return "", false
}

// recordLoc records the first line a dotted key path is seen at, keyed by
// the same quoting-aware rendering toml.Key.String() produces, so a lookup
// against an *Error's toml.Key-derived Key field always hits.
func recordLoc(locs map[string]int, path []string, line int) {
	if len(path) == 0 {
		return
	}
	key := toml.Key(path).String()
	if _, ok := locs[key]; !ok {
		locs[key] = line
	}
}

// parseDottedKey splits a dotted-key text into its resolved segments,
// honoring TOML quoting: a dot inside a quoted segment (basic "..." or
// literal '...') never splits that segment. This is what makes
// host.mcp."github.copilot" one identity instead of two.
func parseDottedKey(s string) []string {
	raw := splitDottedRaw(strings.TrimSpace(s))
	out := make([]string, len(raw))
	for i, seg := range raw {
		out[i] = unquoteKeySegment(seg)
	}
	return out
}

// splitDottedRaw splits on top-level '.' characters only — one still inside
// an open quote is kept as part of the current segment.
func splitDottedRaw(s string) []string {
	var segs []string
	var cur strings.Builder
	var inQuote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			cur.WriteByte(c)
			if c == '\\' && inQuote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
			cur.WriteByte(c)
		case '.':
			segs = append(segs, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	segs = append(segs, strings.TrimSpace(cur.String()))
	return segs
}

// unquoteKeySegment strips and unescapes one raw key segment: a TOML basic
// string ("...", Go-compatible escapes), a literal string ('...', no
// escapes), or a bare key (returned unchanged).
func unquoteKeySegment(seg string) string {
	if len(seg) >= 2 && strings.HasPrefix(seg, `"`) && strings.HasSuffix(seg, `"`) {
		if u, err := strconv.Unquote(seg); err == nil {
			return u
		}
		return strings.Trim(seg, `"`)
	}
	if len(seg) >= 2 && strings.HasPrefix(seg, "'") && strings.HasSuffix(seg, "'") {
		return seg[1 : len(seg)-1]
	}
	return seg
}
