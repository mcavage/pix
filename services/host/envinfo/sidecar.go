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
}

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
type HostMCPEntry struct {
	Name      string   `toml:"-"`
	EnvKeys   []string `toml:"env_keys"`
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
type InferenceBackend struct {
	Driver   string `toml:"driver"`
	Protocol string `toml:"protocol"`
	BaseURL  string `toml:"base_url"`
	Auth     string `toml:"auth"`
	KeyEnv   string `toml:"key_env"`
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

func (e *Error) Error() string {
	return fmt.Sprintf("%s:%d: %s: %q", e.File, e.Line, e.Reason, e.Key)
}

const ownedByEnvFile = "native-owned; declare it in .sbxenv.yaml, not pix.toml"

// Parse reads and strictly validates the pix.toml at path, returning typed
// facts for exactly the design-of-record sections. Any key outside that
// schema — a typo or a native-owned field sbx already owns — fails the parse
// with an *Error naming the exact key and line; nothing is silently defaulted
// or merged.
func Parse(path string) (*Sidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("envinfo: read %s: %w", path, err)
	}
	text := string(data)
	base := filepath.Base(path)

	// Confirm the file is syntactically valid TOML using the real parser
	// before trusting the lightweight line-scanner's well-formed-line
	// assumptions below.
	var probe map[string]any
	if _, derr := toml.Decode(text, &probe); derr != nil {
		if pe, ok := derr.(toml.ParseError); ok {
			return nil, &Error{File: base, Line: pe.Position.Line, Key: pe.LastKey, Reason: "invalid TOML: " + pe.Message}
		}
		return nil, fmt.Errorf("envinfo: %s: %w", base, derr)
	}

	if _, serr := scanFile(text); serr != nil {
		serr.File = base
		return nil, serr
	}

	var s Sidecar
	if _, err := toml.Decode(text, &s); err != nil {
		return nil, fmt.Errorf("envinfo: %s: %w", base, err)
	}
	for name, entry := range s.Host.MCP {
		entry.Name = name
		s.Host.MCP[name] = entry
	}
	return &s, nil
}

// ValidateSkillWorkspaces confirms every [pi].skills path resolves, relative
// to the pix.toml directory, to a location inside one of the supplied
// workspace roots. workspaces must already be canonical absolute paths — the
// environment adapter's job, not this package's — since this function does
// not itself resolve symlinks. An environment with no declared workspaces
// fails every skills path closed rather than defaulting to "allowed".
func ValidateSkillWorkspaces(sidecarPath string, s *Sidecar, workspaces []string) error {
	base := filepath.Base(sidecarPath)
	dir := filepath.Dir(sidecarPath)
	line := 0
	if data, err := os.ReadFile(sidecarPath); err == nil {
		if locs, _ := scanFile(string(data)); locs != nil {
			line = locs["pi.skills"]
		}
	}
	for _, rel := range s.Pi.Skills {
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, rel)
		}
		abs = filepath.Clean(abs)

		inside := false
		for _, ws := range workspaces {
			ws = filepath.Clean(ws)
			if abs == ws || strings.HasPrefix(abs, ws+string(filepath.Separator)) {
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

// schemaNode is one location in the strict pix.toml key tree.
type schemaNode struct {
	// fields are leaf keys allowed directly at this node.
	fields map[string]bool
	// forbiddenLocal are leaf keys that are explicitly refused at this node,
	// with the reason to report (used for native-owned fields).
	forbiddenLocal map[string]string
	// children are fixed-name child tables (e.g. host.mcp, host.services).
	children map[string]*schemaNode
	// dynamicChild, when set, is the node any single additional path segment
	// resolves to (e.g. the server name under host.mcp, or the backend name
	// under inference.backends).
	dynamicChild *schemaNode
	// freeform means any leaf key name is accepted here without further
	// checks (the [agents] table: agent names are user-chosen).
	freeform bool
}

func newSchema() *schemaNode {
	mcpEntry := &schemaNode{
		fields: map[string]bool{"env_keys": true, "probe_args": true},
		forbiddenLocal: map[string]string{
			"url":     ownedByEnvFile,
			"command": ownedByEnvFile,
		},
	}
	mcpContainer := &schemaNode{dynamicChild: mcpEntry}
	servicesEntry := &schemaNode{
		fields: map[string]bool{"name": true, "command": true, "args": true, "port": true, "probe": true},
	}
	host := &schemaNode{
		children: map[string]*schemaNode{"mcp": mcpContainer, "services": servicesEntry},
	}

	backendEntry := &schemaNode{
		fields: map[string]bool{"driver": true, "protocol": true, "base_url": true, "auth": true, "key_env": true},
	}
	backendsContainer := &schemaNode{dynamicChild: backendEntry}
	modelsEntry := &schemaNode{
		fields: map[string]bool{
			"id": true, "backend": true, "upstream_id": true,
			"context_window": true, "max_output_tokens": true, "reasoning": true,
		},
	}
	inference := &schemaNode{
		children: map[string]*schemaNode{"backends": backendsContainer, "models": modelsEntry},
	}

	models := &schemaNode{fields: map[string]bool{"main": true, "exclusive": true}}
	agents := &schemaNode{freeform: true}
	memory := &schemaNode{fields: map[string]bool{"scope": true}}
	pi := &schemaNode{fields: map[string]bool{"skills": true}}

	return &schemaNode{
		fields: map[string]bool{"schema": true},
		forbiddenLocal: map[string]string{
			"kits":           ownedByEnvFile,
			"workspaces":     ownedByEnvFile,
			"workspace":      ownedByEnvFile,
			"env":            ownedByEnvFile,
			"secrets":        ownedByEnvFile,
			"bindings":       ownedByEnvFile,
			"mcp":            ownedByEnvFile,
			"resources":      ownedByEnvFile,
			"sandboxOptions": ownedByEnvFile,
			"ports":          ownedByEnvFile,
		},
		children: map[string]*schemaNode{
			"models": models, "agents": agents, "memory": memory, "pi": pi,
			"host": host, "inference": inference,
		},
	}
}

// check walks path from the schema root. When allowLeaf is true the final
// segment may match a fields entry (a "key = value" line); when false every
// segment must resolve through children/dynamicChild (a "[table]" or
// "[[table]]" header). It returns nil when path is legal, else an *Error
// naming the exact offending key (the full dotted path as authored).
func (n *schemaNode) check(path []string, allowLeaf bool) *Error {
	cur := n
	for i, seg := range path {
		if cur.freeform {
			return nil
		}
		if reason, bad := cur.forbiddenLocal[seg]; bad {
			return &Error{Key: strings.Join(path, "."), Reason: reason}
		}
		last := i == len(path)-1
		if allowLeaf && last && cur.fields[seg] {
			return nil
		}
		if child, ok := cur.children[seg]; ok {
			cur = child
			continue
		}
		if cur.dynamicChild != nil {
			cur = cur.dynamicChild
			continue
		}
		return &Error{Key: strings.Join(path, "."), Reason: "unknown key"}
	}
	return nil
}

var (
	reArrayHeader = regexp.MustCompile(`^\[\[\s*([^\]]+?)\s*\]\]\s*(#.*)?$`)
	reHeader      = regexp.MustCompile(`^\[\s*([^\]]+?)\s*\]\s*(#.*)?$`)
	reKey         = regexp.MustCompile(`^([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*)\s*=`)
)

// scanFile walks pix.toml text line by line, validating every header and key
// path against the strict schema and recording the first line each dotted
// path is seen at. It assumes syntactically valid TOML (Parse confirms that
// with the real parser first) and does not itself handle quoted keys or
// dots embedded inside quoted path segments.
func scanFile(text string) (map[string]int, *Error) {
	root := newSchema()
	locs := map[string]int{}
	var curPath []string
	for i, raw := range strings.Split(text, "\n") {
		line := i + 1
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		switch {
		case reArrayHeader.MatchString(t):
			path := splitDotted(reArrayHeader.FindStringSubmatch(t)[1])
			if err := root.check(path, false); err != nil {
				err.Line = line
				return locs, err
			}
			curPath = path
			recordLoc(locs, path, line)
		case reHeader.MatchString(t):
			path := splitDotted(reHeader.FindStringSubmatch(t)[1])
			if err := root.check(path, false); err != nil {
				err.Line = line
				return locs, err
			}
			curPath = path
			recordLoc(locs, path, line)
		case reKey.MatchString(t):
			keyPath := splitDotted(reKey.FindStringSubmatch(t)[1])
			full := append(append([]string{}, curPath...), keyPath...)
			if err := root.check(full, true); err != nil {
				err.Line = line
				return locs, err
			}
			recordLoc(locs, full, line)
		}
	}
	return locs, nil
}

func recordLoc(locs map[string]int, path []string, line int) {
	key := strings.Join(path, ".")
	if _, ok := locs[key]; !ok {
		locs[key] = line
	}
}

func splitDotted(s string) []string {
	parts := strings.Split(s, ".")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return out
}
