package stack

import (
	"fmt"
	"regexp"
	"strings"
)

// pixPrefix is the ONE unscoped namespace root every pix-owned resource
// name (scoped or not) still lives under: "pix-". It is redeclared here
// rather than imported from sandbox.Prefix, because stack is a foundation
// package (L0) and sandbox is a capability (L1) — imports point down, never
// up (docs/design/architecture.md), so the capability depends on this
// package's names, never the reverse.
const pixPrefix = "pix-"

// stampRe is the grammar LocalTemplateTag requires of its stamp argument: at
// least one character, drawn only from a shell/argv-safe charset. The same
// posture sandbox's own nameRe applies to a sandbox name, applied here to a
// value that also ends up composed into a `docker`/`sbx template` argv.
var stampRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$`)

// SandboxPrefix returns this stack's own sandbox namespace: "pix-<id>-".
// A caller (sandbox naming, once wired) composes a full sandbox name by
// appending its own basename/digest suffix to this prefix, so two different
// PIX_HOME stacks never produce the same sandbox name even for the same
// workspace.
func SandboxPrefix(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return pixPrefix + id + "-", nil
}

// MemoryContainerName returns this stack's own pix-memory Docker container
// name: "pix-memory-<id>". Replaces the single global "pix-memory" name
// (container.Name) once a consumer is wired to it — see stack/doc.go: this
// package only PRODUCES the name, it does not reconcile the container.
func MemoryContainerName(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return "pix-memory-" + id, nil
}

// MCPMemoryName returns this stack's own reserved memory MCP server name:
// "pix-memory-<id>". Replaces the single global "pix-memory" reservation
// (envinfo.MCPMemoryName) once a consumer is wired to it.
func MCPMemoryName(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return "pix-memory-" + id, nil
}

// MCPSessionName returns this stack's own reserved session-control MCP
// server name: "pix-session-<id>". Replaces the single global "pix-session"
// reservation (envinfo.MCPSessionName) once a consumer is wired to it.
func MCPSessionName(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return "pix-session-" + id, nil
}

// LocalTemplateTagPrefix returns this stack's own local-image-tag
// namespace: "local-<id>-". A caller composes the full tag by appending its
// own stamp (see LocalTemplateTag), so two stacks building a local image at
// the same instant never race onto the same `sbx template` tag.
func LocalTemplateTagPrefix(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return "local-" + id + "-", nil
}

// LocalTemplateTag returns this stack's own locally-loaded image tag:
// "local-<id>-<stamp>" — the scoped replacement for the current bare
// "local-<unix-timestamp>" grammar `make load`/out/.local-image-tag write
// today. stamp is validated against the same argv-safe grammar a sandbox
// name enforces (stampRe): a caller-supplied timestamp, build number, or
// short hash all satisfy it, but nothing that could break out of a
// `docker`/`sbx` argv does.
func LocalTemplateTag(id, stamp string) (string, error) {
	prefix, err := LocalTemplateTagPrefix(id)
	if err != nil {
		return "", err
	}
	if !stampRe.MatchString(stamp) {
		return "", fmt.Errorf("stack: invalid stamp %q: must match %s", stamp, stampRe.String())
	}
	return prefix + stamp, nil
}

// IsScopedSandboxName reports whether name is a sandbox name scoped to the
// stack identified by id — i.e. it carries SandboxPrefix(id) as a prefix and
// has at least one character beyond it. A malformed id can never match
// anything, so this returns false rather than propagating id's error: a
// predicate has no unsafe name to construct, only a yes/no question to
// answer.
func IsScopedSandboxName(id, name string) bool {
	prefix, err := SandboxPrefix(id)
	if err != nil {
		return false
	}
	return strings.HasPrefix(name, prefix) && len(name) > len(prefix)
}

// IsAnyPixSandboxName reports whether name falls under the pix-owned
// namespace AT ALL, regardless of which stack (if any) scoped it: a bare
// "pix-" prefix with at least one character beyond it. This is the coarser
// check a coexistence sweep needs to recognize a sandbox created before
// per-stack scoping existed, or one belonging to a DIFFERENT stack's ID —
// IsScopedSandboxName is the finer check for "belongs to THIS stack".
func IsAnyPixSandboxName(name string) bool {
	return strings.HasPrefix(name, pixPrefix) && len(name) > len(pixPrefix)
}
