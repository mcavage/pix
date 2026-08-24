package envinfo

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("envinfo: %s: empty document", source)
		}
		return nil, fmt.Errorf("envinfo: %s: %w", source, err)
	}
	doc.Source = source

	if doc.SchemaVersion != SchemaVersionV1 {
		return nil, fmt.Errorf("%w: %s: got %q, want %q", ErrUnsupportedSchemaVersion, source, doc.SchemaVersion, SchemaVersionV1)
	}

	resolveLocalKits(&doc, sourceDir)

	if err := refuseLiteralValues(&doc); err != nil {
		return nil, err
	}

	return &doc, nil
}

// resolveLocalKits fills Resolved/Local on every KitEntry. An entry is
// LOCAL when it contains no URI scheme (no "://"): a bare relative or
// absolute filesystem path. A local relative path resolves against
// sourceDir, never the process's working directory. A remote reference
// (git/https/etc.) is left byte-for-byte as its own Resolved value —
// there is nothing on this host to resolve it against.
func resolveLocalKits(doc *Document, sourceDir string) {
	for i, k := range doc.Kits {
		if strings.Contains(k.Raw, "://") {
			doc.Kits[i].Local = false
			doc.Kits[i].Resolved = k.Raw
			continue
		}
		doc.Kits[i].Local = true
		if filepath.IsAbs(k.Raw) {
			doc.Kits[i].Resolved = filepath.Clean(k.Raw)
		} else {
			doc.Kits[i].Resolved = filepath.Clean(filepath.Join(sourceDir, k.Raw))
		}
	}
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
