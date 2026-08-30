package envinfo

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse strictly decodes one authored `.sbxenv.yaml` file at path: unknown
// fields at any nesting level are refused (KnownFields(true)), the schema
// version is gated to SchemaVersionV1, every local `kits:` entry is
// resolved against path's own directory (never the process's working
// directory — docs/design/environments.md §4/§6.2), and a literal `value`
// in secrets/registries is refused. Source is set to path.
func Parse(path string) (*Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envinfo: %w", err)
	}
	defer f.Close()
	return decode(f, path, filepath.Dir(path))
}

// ParseBytes strictly decodes in-memory `.sbxenv.yaml` bytes. source is a
// caller-supplied label recorded as Document.Source and used for
// provenance and error messages; sourceDir is the directory local `kits:`
// entries resolve against (the directory the bytes would live in if they
// were the file named by source).
func ParseBytes(data []byte, source, sourceDir string) (*Document, error) {
	return decode(bytes.NewReader(data), source, sourceDir)
}

func decode(r io.Reader, source, sourceDir string) (*Document, error) {
	// The whole file is read up front (these are small authored config
	// files, never a stream worth chunking) so it can be decoded twice from
	// independent Decoders: once as a raw yaml.Node to inspect the exact
	// authored type of schemaVersion (see checkSchemaVersionIsAuthoredString
	// below), and once through the strict, KnownFields(true) struct decode
	// this package has always done. yaml.Node's own Decode method does not
	// honor KnownFields, so it cannot replace the struct decode; it can only
	// supplement it.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("envinfo: %s: %w", source, err)
	}

	var probe yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&probe); err != nil && err != io.EOF {
		return nil, fmt.Errorf("envinfo: %s: %w", source, err)
	}
	if err := checkSchemaVersionIsAuthoredString(&probe, source); err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("envinfo: %s: empty document", source)
		}
		return nil, fmt.Errorf("envinfo: %s: %w", source, err)
	}
	doc.Source = source

	// A second YAML document in the same file (even an empty one — a bare
	// trailing `---` decodes to a nil value with no error, not io.EOF) is
	// refused outright: this package only ever reviews and decodes the
	// first document, so a second one — carrying an empty payload, a
	// host-exec fact (secrets/registries command), or another value
	// payload — would ride along completely unreviewed. Decoding the
	// trailing content into `interface{}` rather than *Document deliberately
	// skips this file's own KnownFields/schema checks for that content: the
	// point here is only to detect that MORE than one document exists, not
	// to characterize what the second one contains.
	var trailing interface{}
	if terr := dec.Decode(&trailing); terr != io.EOF {
		if terr == nil {
			return nil, fmt.Errorf("%w: %s: a second YAML document follows the first; only one authored document per file is accepted (found after decoding the first)", ErrTrailingDocument, source)
		}
		return nil, fmt.Errorf("%w: %s: a second YAML document follows the first and failed to parse: %v", ErrTrailingDocument, source, terr)
	}

	if doc.SchemaVersion != SchemaVersionV1 {
		return nil, fmt.Errorf("%w: %s: got %q, want %q", ErrUnsupportedSchemaVersion, source, doc.SchemaVersion, SchemaVersionV1)
	}

	if err := resolveLocalKits(&doc, sourceDir); err != nil {
		return nil, err
	}

	resolveWorkspaces(&doc, sourceDir)

	if err := refuseLiteralValues(&doc); err != nil {
		return nil, err
	}

	return &doc, nil
}

// checkSchemaVersionIsAuthoredString refuses a `schemaVersion` scalar that
// was NOT authored as a YAML string — most importantly a bare unquoted
// integer like `schemaVersion: 1`. gopkg.in/yaml.v3 silently coerces an
// `!!int`-tagged scalar into a Go string field on ordinary struct decode
// (Document.SchemaVersion is a string), so without this check
// `schemaVersion: 1` and the only intentionally accepted spelling,
// `schemaVersion: "1"`, would decode identically. Inspecting the raw node's
// own Tag — set by yaml.v3's resolver from the scalar's syntax, not from
// the target field's Go type — is the one way to tell them apart; a target
// of `interface{}` would show the same divergence (int 1 vs string "1") but
// only after already having a materialized value to compare, which is a
// strictly weaker check than refusing the source syntax itself. A missing
// schemaVersion is left to the existing post-decode ErrUnsupportedSchemaVersion
// check; this function only fires when the key is present but mistyped.
func checkSchemaVersionIsAuthoredString(probe *yaml.Node, source string) error {
	if probe.Kind != yaml.DocumentNode || len(probe.Content) == 0 {
		return nil
	}
	mapping := probe.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "schemaVersion" {
			continue
		}
		val := mapping.Content[i+1]
		if val.Tag != "!!str" {
			return fmt.Errorf("%w: %s: schemaVersion must be authored as a quoted YAML string (e.g. schemaVersion: \"1\"); got %s %s", ErrUnsupportedSchemaVersion, source, val.Tag, val.Value)
		}
		return nil
	}
	return nil
}

// uriSchemeRE matches a recognized URL form: a leading scheme conforming to
// RFC 3986's grammar (a letter, then letters/digits/+/-/.) followed by
// "://" — "https://", "git+ssh://", "ssh://", "file://", and so on. Only a
// reference matching this is classified remote; see classifyKit.
var uriSchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// classifyKit decides whether one authored `kits:` entry is a local
// filesystem path or a remote reference, and refuses to guess when it is
// neither clearly. A recognized URL form (uriSchemeRE) is always remote. A
// raw string with no colon OUTSIDE any authored `${VAR}`/`${VAR:-default}`
// interpolation expression (interpolationRE, interpolation.go) is always
// local — a bare relative or absolute filesystem path; the colon in a
// `${VAR:-default}` expression's own default-value grammar is not this
// ambiguity, so it is stripped before the check (docs/design/
// environments.md §9.1's default-value syntax vs. this finding are
// unrelated colons). Anything else — a colon present outside interpolation,
// but not part of a recognized "scheme://" — is refused outright rather
// than silently treated as local: that shape is exactly the scp-style
// shorthand git itself accepts as a remote (`git@host:path`, `host:path`),
// and treating it as a local path would resolve it against sourceDir into
// a filesystem path nobody authored and that most likely does not exist,
// all while the author's actual remote reference silently vanished.
func classifyKit(raw string) (local bool, err error) {
	if uriSchemeRE.MatchString(raw) {
		return false, nil
	}
	stripped := interpolationRE.ReplaceAllString(raw, "")
	if strings.Contains(stripped, ":") {
		return false, fmt.Errorf("%w: %q has a colon but does not match a recognized URL scheme (scheme://...). This is the scp-style shorthand git itself accepts as remote (e.g. git@host:path). Use an explicit scheme (e.g. git+ssh://) or a plain local path", ErrAmbiguousKitReference, raw)
	}
	return true, nil
}

// resolveLocalKits fills Resolved/Local on every KitEntry using
// classifyKit. A local relative path resolves against sourceDir, never the
// process's working directory. A remote reference is left byte-for-byte as
// its own Resolved value — there is nothing on this host to resolve it
// against. An ambiguous entry (classifyKit's error) aborts the whole parse:
// see ErrAmbiguousKitReference.
func resolveLocalKits(doc *Document, sourceDir string) error {
	for i, k := range doc.Kits {
		local, err := classifyKit(k.Raw)
		if err != nil {
			return fmt.Errorf("kits[%d]: %w", i, err)
		}
		doc.Kits[i].Local = local
		if !local {
			doc.Kits[i].Resolved = k.Raw
			continue
		}
		if filepath.IsAbs(k.Raw) {
			doc.Kits[i].Resolved = filepath.Clean(k.Raw)
		} else {
			doc.Kits[i].Resolved = filepath.Clean(filepath.Join(sourceDir, k.Raw))
		}
	}
	return nil
}

// resolveWorkspaces fills Resolved on the authored primary workspace and
// on every additional workspace, against sourceDir — upstream's own rule
// ("Relative paths resolve from the directory of the first environment
// file"), and the same source-directory discipline resolveLocalKits
// already applies.
//
// A path still carrying an authored `${VAR}` expression is left VERBATIM,
// unresolved. Joining sourceDir onto `${HOME}/src` would fabricate a path
// nobody authored and that cannot exist (the expansion is very often
// absolute), and this package resolves no interpolation of its own
// (doc.go: "Interpolation is surfaced, never resolved"). Carrying the
// expression through instead keeps it visible to BuildTree and lets
// ComputeFingerprint cover it as expression-plus-keyed-HMAC. Kits keep
// their older, unchanged behavior here; widening that is not this fix.
func resolveWorkspaces(doc *Document, sourceDir string) {
	if doc.Workspace.Present {
		doc.Workspace.Resolved = resolveWorkspacePath(doc.Workspace.Raw, sourceDir)
	}
	for i, ws := range doc.AdditionalWorkspaces {
		doc.AdditionalWorkspaces[i].Resolved = resolveWorkspacePath(ws.Path, sourceDir)
	}
}

func resolveWorkspacePath(raw, sourceDir string) string {
	if raw == "" || interpolationRE.MatchString(raw) {
		return raw
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(sourceDir, raw))
}

// refuseLiteralValues implements docs/design/environments.md §5.1
// restriction 1. It runs once per authored file, before merge — a later
// file can only ever contribute a record that already passed this check
// (see merge.go's whole-record-replace policy for secrets/registries), so
// BuildTree never has to repeat it.
func refuseLiteralValues(doc *Document) error {
	for name, s := range doc.Secrets {
		if s.Value != "" {
			return fmt.Errorf("%w: %s: secrets.%s.value", ErrLiteralValueRefused, doc.Source, name)
		}
	}
	for host, r := range doc.Registries {
		if r.Value != "" {
			return fmt.Errorf("%w: %s: registries.%s.value", ErrLiteralValueRefused, doc.Source, host)
		}
	}
	return nil
}
