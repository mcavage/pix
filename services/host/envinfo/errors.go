package envinfo

import "errors"

// ErrLiteralValueRefused is wrapped into a %w-formatted error naming the
// exact `secrets.<name>.value` or `registries.<host>.value` key path
// docs/design/environments.md §5.1's restriction 1 refuses: "Literal
// `value` entries are refused in both `secrets` and `registries`. Secret
// values do not belong in authored files."
var ErrLiteralValueRefused = errors.New("envinfo: literal value refused")

// ErrUnsupportedSchemaVersion is wrapped into a %w-formatted error naming
// the exact document source and the unsupported/missing schemaVersion
// value it declared.
var ErrUnsupportedSchemaVersion = errors.New("envinfo: unsupported schemaVersion")

// ErrDuplicateIdentity is wrapped into a %w-formatted error naming the
// exact identity-addressed key path (see doc.go, "Stable identity") that
// two entries both claim. BuildTree refuses to guess which one wins.
var ErrDuplicateIdentity = errors.New("envinfo: duplicate identity")

// ErrTrailingDocument is wrapped into a %w-formatted error naming the exact
// document source when a `.sbxenv.yaml` file authors more than one YAML
// document (a second `---`-separated document, even an empty one, a
// host-exec payload, or a second copy of `value`/`ref`/`command` fields).
// Only the first document is ever decoded; a strict parser that silently
// stops at the first document without checking for EOF right after would
// let a reviewer approve one document while a second, unreviewed one rides
// along in the same file undetected.
var ErrTrailingDocument = errors.New("envinfo: trailing YAML document")

// ErrAmbiguousKitReference is wrapped into a %w-formatted error naming the
// exact `kits[<i>]` entry that contains a colon but does not match a
// recognized URL scheme (`scheme://...`) — the scp-style shorthand git
// itself accepts (`git@host:path`, `host:path`) and any other bare
// colon-bearing reference. resolveLocalKits (parse.go) refuses to guess
// which of "local path" or "remote reference" was meant; the author must
// use an explicit scheme or a plain local path instead.
var ErrAmbiguousKitReference = errors.New("envinfo: ambiguous kit reference")

// ErrEmptyBindingDomains is wrapped into a %w-formatted error naming the
// exact `bindings.<service>.apiKey.domains` key path a service declared
// with an effective, post-merge domain list of zero. A binding that can
// never inject into any domain is never a valid declaration to carry
// forward silently — see tree.go's BuildTree for the full rationale.
var ErrEmptyBindingDomains = errors.New("envinfo: binding declared with zero domains")

// ErrReadOnlyPrimaryWorkspace and ErrClonedAdditionalWorkspace are
// RenderEffective's two workspace refusals. Both exist because upstream's
// schema has no field for the fact in question, so the only alternatives
// to refusing are silently dropping a bit that RESTRICTS or ISOLATES host
// access:
//
//   - a read-only primary workspace has no `readOnly` key under
//     `workspace:`; dropping it mounts the tree read-write.
//   - a cloned additional workspace has no `clone` key under
//     `additionalWorkspaces[]` (upstream mounts them directly "even when
//     the primary workspace uses clone mode"); dropping it direct-mounts a
//     tree the caller asked to be a private clone.
//
// Either silent drop expands what the sandbox can write on the host, which
// is precisely the class of change §9.1 requires to be visible.
var (
	ErrReadOnlyPrimaryWorkspace = errors.New(
		"envinfo: a read-only primary workspace is not expressible in sbx's schema (workspace has no readOnly field); refusing to render it read-write")
	ErrClonedAdditionalWorkspace = errors.New(
		"envinfo: an additional workspace cannot be cloned in sbx's schema (additionalWorkspaces has no clone field); refusing to render it as a direct mount")
)

// errNoDocuments and errNoSchemaVersion are Merge's own two refusals: no
// input at all, or an input set whose every document somehow declared an
// empty schemaVersion (Parse already refuses that per file, so this is a
// belt-and-suspenders guard against a hand-built Document bypassing Parse).
var (
	errNoDocuments     = errors.New("envinfo: merge requires at least one document")
	errNoSchemaVersion = errors.New("envinfo: no document declared a schemaVersion")
)
