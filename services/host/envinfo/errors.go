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

// ErrEmptyBindingDomains is wrapped into a %w-formatted error naming the
// exact `bindings.<service>.apiKey.domains` key path a service declared
// with an effective, post-merge domain list of zero. A binding that can
// never inject into any domain is never a valid declaration to carry
// forward silently — see tree.go's BuildTree for the full rationale.
var ErrEmptyBindingDomains = errors.New("envinfo: binding declared with zero domains")

// errNoDocuments and errNoSchemaVersion are Merge's own two refusals: no
// input at all, or an input set whose every document somehow declared an
// empty schemaVersion (Parse already refuses that per file, so this is a
// belt-and-suspenders guard against a hand-built Document bypassing Parse).
var (
	errNoDocuments     = errors.New("envinfo: merge requires at least one document")
	errNoSchemaVersion = errors.New("envinfo: no document declared a schemaVersion")
)
