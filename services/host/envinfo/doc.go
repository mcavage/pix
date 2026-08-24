// Package envinfo is the L1 capability that owns strict native
// `.sbxenv.yaml` v1 parsing (docs/design/environments.md §5.1, Story 1). It
// is the ONE package in this module allowed to declare a decode struct for
// the schemaVersion field a native environment document carries —
// services/host/arch_test.go's TestOnlyEnvinfoDecodesNativeEnvYAML fails the
// build the day a second package grows that struct tag, and uatenvmatrix
// (Story 0's own fixture-owning
// probe) is explicitly forbidden from importing this package at all
// (TestArchitecture_UatenvmatrixNeverImportsEnvinfo), so an agreement
// between the two is evidence the renderer is faithful, never a tautology.
//
// It does four things, one per file:
//
//   - document.go — the strict typed schema. gopkg.in/yaml.v3's
//     KnownFields(true) decoder refuses any key this package does not
//     model, recursively, so a typo in an authored file is a load error,
//     not silent data loss.
//   - parse.go     — Parse/ParseBytes: strict per-file decode, schemaVersion
//     gate, local relative kit-path resolution against the SOURCE FILE'S
//     OWN directory (never the process cwd), and the literal-`value`
//     refusal in secrets/registries (docs/design/environments.md §5.1,
//     restriction 1).
//   - merge.go     — Merge: upstream's documented multi-file composition
//     semantics (docs/design/environments.md §4: "nested maps merge by
//     key, lists concatenate, and later files replace scalar values"),
//     with per-node provenance (which source file contributed each value)
//     carried through so the tree below can attribute it.
//   - tree.go       — BuildTree: the PRE-COMPOSITION semantic tree. "Pre-
//     composition" names a real boundary: this is the authored/merged
//     document's own shape, before any Pix-owned runtime fact (a
//     generated `pix-*` sandbox name, a mixin kit, resume arguments —
//     docs/design/environments.md §6.2) is added. Composing those belongs
//     to a later story's renderer, not here — this package takes no
//     dependency on services/host/sandbox and computes no sandbox name of
//     its own; a caller that needs one supplies it as a parameter to
//     whatever it builds from this tree, never the reverse.
//
// # Stable identity
//
// Every list this schema declares gets ONE of two addressing modes in the
// tree, chosen by whether the schema gives the list a natural key:
//
//   - identity-addressed: `mcp.servers[<name>]` (keyed by the server's own
//     `name`), `bindings.<service>.apiKey.domains[<domain>]` (keyed by the
//     domain string itself), `ports[<sandboxPort>]` (keyed by the
//     declared sandbox-side port). A second entry claiming an identity
//     already in use is a stable-identity VIOLATION and BuildTree refuses
//     it, rather than silently letting the last one win — that would make
//     "which entry does `mcp.servers[github]` mean" depend on merge order.
//   - index-addressed: `kits[<i>]`. `kits` has no field upstream treats as
//     a name, so two kit entries can be identical strings with no
//     ambiguity to detect; index is the only stable address available and
//     collision detection does not apply.
//
// # Interpolation is surfaced, never resolved
//
// docs/design/environments.md §9.1 states the Story 1 security obligation
// this package carries forward from Story 0: every authored `${VAR}` (or
// `${VAR:-default}`) reference must be reportable by source variable name
// and destination key path, and the resolved value must never be
// displayed or persisted. BuildTree's Interpolations field is exactly
// that: Var, an optional literal Default, and KeyPath — there is no
// resolved-value field on the type, and this package never calls
// os.Getenv or anything that would read a host environment variable (see
// TestNoHostEnvironmentResolution in interpolation_test.go, a grep-based
// fitness function over this package's own production source).
package envinfo
