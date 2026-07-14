// Package okf reads and parses OKF (Open Knowledge Format) knowledge bundles.
//
// An OKF bundle is a directory tree of UTF-8 markdown files, each optionally
// carrying YAML frontmatter delimited by `---` lines. Consumption is
// deliberately permissive: unknown frontmatter types, missing optional fields,
// broken cross-links, and a missing index.md never cause a bundle to be
// rejected.
package okf

import (
	"bufio"
	"path/filepath"
	"regexp"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// Reserved filenames that are not treated as concepts.
const (
	indexFile = "index.md"
	logFile   = "log.md"
)

// Concept is a single knowledge file within a bundle.
type Concept struct {
	ID          string         // path within the bundle minus ".md" (e.g. "tables/users")
	Type        string         // required frontmatter "type" ("" if no frontmatter)
	Title       string         // optional
	Description string         // optional
	Resource    string         // optional URI
	Tags        []string       // optional
	Timestamp   string         // optional ISO8601
	Body        string         // markdown after the frontmatter
	Path        string         // bundle-relative path (e.g. "/tables/orders.md")
	Extra       map[string]any // unknown/arbitrary frontmatter keys
	Citations   []string       // entries parsed from a "# Citations" section
	Links       []string       // bundle-relative targets of markdown links in the body
}

// knownFields are the frontmatter keys mapped onto typed Concept fields; every
// other key is captured into Concept.Extra.
var knownFields = map[string]bool{
	"type":        true,
	"title":       true,
	"description": true,
	"resource":    true,
	"tags":        true,
	"timestamp":   true,
}

var (
	// markdown inline links: [text](target)
	linkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
	// a "# Citations" heading (any level), case-insensitive.
	citationHeadingRe = regexp.MustCompile(`(?i)^#{1,6}\s+citations\s*$`)
	// any ATX heading, used to detect the end of the citations section.
	headingRe = regexp.MustCompile(`^#{1,6}\s+`)
)

// ParseConcept parses a single OKF markdown file. path is the bundle-relative
// path (leading "/" optional). It never returns a nil Concept on success. A
// file with no frontmatter is treated as an all-body concept with empty type.
func ParseConcept(path string, data []byte) (*Concept, error) {
	rel := normalizePath(path)
	c := &Concept{
		ID:    strings.TrimSuffix(strings.TrimPrefix(rel, "/"), ".md"),
		Path:  rel,
		Extra: map[string]any{},
	}

	front, body := splitFrontmatter(string(data))
	c.Body = body

	if front != "" {
		raw := map[string]any{}
		if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
			return nil, err
		}
		for k, v := range raw {
			if knownFields[k] {
				continue
			}
			c.Extra[k] = v
		}
		c.Type = asString(raw["type"])
		c.Title = asString(raw["title"])
		c.Description = asString(raw["description"])
		c.Resource = asString(raw["resource"])
		c.Timestamp = asString(raw["timestamp"])
		c.Tags = asStringSlice(raw["tags"])
	}

	c.Citations = extractCitations(body)
	c.Links = extractLinks(body)
	return c, nil
}

// splitFrontmatter separates YAML frontmatter (between the first two `---`
// lines) from the markdown body. CRLF line endings are tolerated. If there is
// no leading frontmatter block, the whole input is returned as the body.
func splitFrontmatter(s string) (front, body string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Frontmatter must be the very first line (allowing a leading BOM).
	trimmed := strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(trimmed, "---\n") && trimmed != "---" {
		return "", s
	}
	rest := trimmed[len("---"):]
	rest = strings.TrimPrefix(rest, "\n")
	// Find the closing delimiter line.
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == "---" {
			front = strings.Join(lines[:i], "\n")
			body = strings.Join(lines[i+1:], "\n")
			body = strings.TrimPrefix(body, "\n")
			return front, body
		}
	}
	// No closing delimiter: treat the whole thing as body.
	return "", s
}

// extractCitations pulls list/line entries under a "# Citations" heading until
// the next heading or end of file.
func extractCitations(body string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	in := false
	for sc.Scan() {
		line := sc.Text()
		if citationHeadingRe.MatchString(strings.TrimSpace(line)) {
			in = true
			continue
		}
		if !in {
			continue
		}
		if headingRe.MatchString(line) {
			break // next section ends citations
		}
		entry := strings.TrimSpace(line)
		entry = strings.TrimPrefix(entry, "-")
		entry = strings.TrimPrefix(entry, "*")
		entry = strings.TrimPrefix(entry, "+")
		entry = strings.TrimSpace(entry)
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// extractLinks returns bundle-relative targets of markdown links in the body.
// External links (http://, https://, mailto:, etc.) and pure anchors are
// skipped. Relative and absolute ("/...") bundle targets are preserved as
// written.
func extractLinks(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
		target := strings.TrimSpace(m[1])
		if target == "" {
			continue
		}
		if strings.HasPrefix(target, "#") {
			continue // in-page anchor
		}
		if isExternal(target) {
			continue
		}
		// Drop any anchor fragment for the recorded target.
		if i := strings.IndexByte(target, '#'); i >= 0 {
			target = target[:i]
		}
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

func isExternal(target string) bool {
	// A scheme like "http:" / "mailto:" before any "/" means external.
	if i := strings.IndexByte(target, ':'); i > 0 {
		scheme := target[:i]
		if !strings.ContainsAny(scheme, "/.") {
			return true
		}
	}
	return strings.HasPrefix(target, "//")
}

func normalizePath(path string) string {
	p := filepath.ToSlash(path)
	p = strings.TrimPrefix(p, "./")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
