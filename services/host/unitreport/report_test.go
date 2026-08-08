package unitreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScrubErrorRedactsCredentialShapes(t *testing.T) {
	cases := map[string]string{
		"dial failed: ANTHROPIC_API_KEY=sk-ant-1234567890 rejected": "sk-ant-1234567890",
		"unit broker: header Authorization: Bearer eyJhbGciOi.abc":  "eyJhbGciOi.abc",
		"resolve op://vault/item/credential failed":                 "op://vault/item/credential",
		"env grant SLACK_TOKEN=xoxb-99 was refused":                 "xoxb-99",
		"MY_SECRET: hunter2 is not valid":                           "hunter2",
	}
	for in, secret := range cases {
		got := ScrubError(in)
		if strings.Contains(got, secret) {
			t.Errorf("ScrubError(%q) still leaks %q: %q", in, secret, got)
		}
		if !strings.Contains(got, "<redacted>") {
			t.Errorf("ScrubError(%q) = %q: redaction left no marker", in, got)
		}
	}
	if got := ScrubError("unit memory: plugin process (pid 4242) exited"); got != "unit memory: plugin process (pid 4242) exited" {
		t.Errorf("an innocent error must survive verbatim, got %q", got)
	}
	if ScrubError("") != "" {
		t.Error("empty stays empty")
	}
}

func TestWriteReportIsAtomicPrivateAndReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "serve.units.json")
	in := Report{SchemaVersion: SchemaVersion, PID: 7, GeneratedUnix: 1700000000,
		Units: []Unit{{Name: "memory", State: "running"}}}
	if err := WriteReport(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("snapshot must be owner-only (0600), got %v (%v)", fi.Mode().Perm(), err)
		}
	}
	// No temp file left behind: a status dir that fills with .units-*.json is a
	// leak an operator finds months later.
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".units-") {
			t.Fatalf("temp file survived the rename: %s", e.Name())
		}
	}
	got, found, err := ReadReport(path)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if got.PID != 7 || len(got.Units) != 1 || got.Units[0].Name != "memory" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	// Rewriting replaces, never appends.
	if err := WriteReport(path, in); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	b, _ := os.ReadFile(path)
	var probe Report
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("second write left invalid json: %v", err)
	}
}

func TestReadReportMissingIsNotAnError(t *testing.T) {
	_, found, err := ReadReport(filepath.Join(t.TempDir(), "nope.json"))
	if found || err != nil {
		t.Fatalf("a missing snapshot means 'serve is not running', not an error: found=%v err=%v", found, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	if _, _, err := ReadReport(bad); err == nil {
		t.Fatal("a corrupt snapshot must be reported, never treated as an empty tree")
	}
}
