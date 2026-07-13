package okf

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bundle is a parsed OKF knowledge bundle.
type Bundle struct {
	// Dir is the root directory the bundle was read from.
	Dir string
	// IndexBody is the contents of the root index.md, if present.
	IndexBody string
	// LogBody is the contents of the root log.md, if present.
	LogBody string
	// Warnings records non-fatal problems (e.g. unreadable files) encountered
	// while reading the bundle.
	Warnings []string

	concepts map[string]*Concept
}

// ReadBundle walks dir and parses every non-reserved ".md" file into a
// Concept. Reserved files (index.md, log.md) at any level are captured
// separately (root-level contents are exposed via Index/Log). Unreadable or
// unparseable files are skipped with a recorded warning rather than failing
// the whole bundle.
func ReadBundle(dir string) (*Bundle, error) {
	b := &Bundle{
		Dir:      dir,
		concepts: map[string]*Concept{},
	}

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			b.Warnings = append(b.Warnings, fmt.Sprintf("walk %s: %v", path, err))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Skip symlinked entries: os.ReadFile follows links, so a symlink inside
		// the bundle pointing at a file OUTSIDE it (e.g. secrets.md -> ~/.ssh/id_rsa)
		// would otherwise be read and indexed as a concept. Only real files within
		// the bundle tree are read.
		if d.Type()&fs.ModeSymlink != 0 {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = path
			}
			b.Warnings = append(b.Warnings, fmt.Sprintf("skip symlink %s", filepath.ToSlash(rel)))
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			b.Warnings = append(b.Warnings, fmt.Sprintf("rel %s: %v", path, relErr))
			return nil
		}
		rel = filepath.ToSlash(rel)

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			b.Warnings = append(b.Warnings, fmt.Sprintf("read %s: %v", rel, readErr))
			return nil
		}

		name := d.Name()
		if name == indexFile || name == logFile {
			// Only the root-level reserved files are surfaced directly; nested
			// ones are still skipped as concepts.
			if filepath.Dir(rel) == "." {
				if name == indexFile {
					b.IndexBody = string(data)
				} else {
					b.LogBody = string(data)
				}
			}
			return nil
		}

		c, parseErr := ParseConcept(rel, data)
		if parseErr != nil {
			b.Warnings = append(b.Warnings, fmt.Sprintf("parse %s: %v", rel, parseErr))
			return nil
		}
		b.concepts[c.ID] = c
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return b, nil
}

// Concepts returns all parsed concepts sorted by ID.
func (b *Bundle) Concepts() []*Concept {
	out := make([]*Concept, 0, len(b.concepts))
	for _, c := range b.concepts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Concept returns the concept with the given ID, or nil if absent.
func (b *Bundle) Concept(id string) *Concept {
	return b.concepts[id]
}

// Index returns the root index.md contents, or "" if the bundle has none.
func (b *Bundle) Index() string {
	return b.IndexBody
}

// Log returns the root log.md contents, or "" if the bundle has none.
func (b *Bundle) Log() string {
	return b.LogBody
}
