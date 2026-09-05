// recreates_test.go — E2.6's doctor-side red-first tests (AC-71, AC-68
// wiring half): the default-tier line at count 0 and count > 0, the
// `--recreates` golden form (docs/design/environments.md §9.4), and the
// bounded 101-append read path.
package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"pix/host/recreatelog"
)

// At count zero, `pix doctor` says nothing about recreates.
func TestRecreateSummaryLine_ZeroCountPrintsNothing(t *testing.T) {
	dir := t.TempDir() // no recreates.log written at all
	var buf bytes.Buffer
	if err := RecreateSummaryLine(&buf, dir); err != nil {
		t.Fatalf("RecreateSummaryLine: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("count 0 must print nothing, got %q", buf.String())
	}
}

// At count > 0, `pix doctor` prints exactly one line: the count and the
// pointer, no key path, no environment name.
func TestRecreateSummaryLine_NonzeroCountPrintsExactlyOneLine(t *testing.T) {
	dir := t.TempDir()
	if err := recreatelog.Append(dir, "work", []string{"mcp.servers[github].url"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := recreatelog.Append(dir, "home", []string{"env.PIX_MEMORY_SCOPE"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var buf bytes.Buffer
	if err := RecreateSummaryLine(&buf, dir); err != nil {
		t.Fatalf("RecreateSummaryLine: %v", err)
	}
	want := "  environments   2 unplanned recreates recorded   pix doctor --recreates\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
	if bytes.Contains(buf.Bytes(), []byte("mcp.servers")) {
		t.Fatal("the default-tier line must never carry a key path")
	}
	if bytes.Contains(buf.Bytes(), []byte("work")) || bytes.Contains(buf.Bytes(), []byte("home")) {
		t.Fatal("the default-tier line must never name an environment")
	}
}

// A single record is singular: "1 unplanned recreate recorded".
func TestRecreateSummaryLine_SingularCount(t *testing.T) {
	dir := t.TempDir()
	if err := recreatelog.Append(dir, "work", []string{"env.FOO"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var buf bytes.Buffer
	if err := RecreateSummaryLine(&buf, dir); err != nil {
		t.Fatalf("RecreateSummaryLine: %v", err)
	}
	want := "  environments   1 unplanned recreate recorded   pix doctor --recreates\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// `pix doctor --recreates` golden output, byte-exact against
// docs/design/environments.md §9.4's own worked example.
func TestRenderRecreates_GoldenOutput(t *testing.T) {
	dir := t.TempDir()
	writeRecreateFixture(t, dir, recreatelog.Record{
		Timestamp: mustParseTime(t, "2026-07-14T09:02:11Z"), Environment: "work",
		ChangedKeyPaths: []string{"mcp.servers[github].url"},
	}, recreatelog.Record{
		Timestamp: mustParseTime(t, "2026-07-14T11:40:03Z"), Environment: "home",
		ChangedKeyPaths: []string{"env.PIX_MEMORY_SCOPE", "mounts[] (2 entries changed)"},
	})
	var buf bytes.Buffer
	if err := RenderRecreates(&buf, dir); err != nil {
		t.Fatalf("RenderRecreates: %v", err)
	}
	path := recreatelog.Path(dir)
	want := "recreate records: 2 (cap 100, oldest dropped)\n" +
		"file: " + path + "\n" +
		"\n" +
		"2026-07-14T09:02:11Z  work  mcp.servers[github].url\n" +
		"2026-07-14T11:40:03Z  home  env.PIX_MEMORY_SCOPE, mounts[] (2 entries changed)\n" +
		"\n" +
		"local only, never uploaded. delete the file whenever you like; that is not an error.\n"
	if buf.String() != want {
		t.Fatalf("--recreates golden mismatch:\n got: %q\nwant: %q", buf.String(), want)
	}
}

// `--recreates` at count zero still names the file, never an error.
func TestRenderRecreates_ZeroCount(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := RenderRecreates(&buf, dir); err != nil {
		t.Fatalf("RenderRecreates: %v", err)
	}
	path := recreatelog.Path(dir)
	want := "recreate records: 0 (cap 100, oldest dropped)\n" +
		"file: " + path + "\n" +
		"\n" +
		"local only, never uploaded. delete the file whenever you like; that is not an error.\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// The bounded 101-append test, through the REAL doctor read path: 101
// appends leave exactly 100 records, oldest dropped, and both doctor
// surfaces (the summary line and the full listing) see that bound, not a
// mocked one.
func TestRecreates_101AppendsBoundThroughDoctorReadPath(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 101; i++ {
		if err := recreatelog.Append(dir, "work", []string{"env.SEQ" + strconv.Itoa(i)}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
	var summary bytes.Buffer
	if err := RecreateSummaryLine(&summary, dir); err != nil {
		t.Fatalf("RecreateSummaryLine: %v", err)
	}
	want := "  environments   100 unplanned recreates recorded   pix doctor --recreates\n"
	if summary.String() != want {
		t.Fatalf("summary after 101 appends: got %q, want %q", summary.String(), want)
	}
	var full bytes.Buffer
	if err := RenderRecreates(&full, dir); err != nil {
		t.Fatalf("RenderRecreates: %v", err)
	}
	if !bytes.Contains(full.Bytes(), []byte("recreate records: 100 (cap 100, oldest dropped)")) {
		t.Fatalf("--recreates after 101 appends did not report the 100-record bound:\n%s", full.String())
	}
	// The oldest (env.SEQ0) was dropped; the newest (env.SEQ100) survives.
	if bytes.Contains(full.Bytes(), []byte("env.SEQ0\n")) {
		t.Fatal("the oldest record should have been dropped at the 100-record cap")
	}
	if !bytes.Contains(full.Bytes(), []byte("env.SEQ100")) {
		t.Fatal("the newest record should survive the 100-record cap")
	}
}

func writeRecreateFixture(t *testing.T, dir string, records ...recreatelog.Record) {
	t.Helper()
	// recreatelog.Append cannot be given a caller-chosen timestamp (by
	// design — see recreatelog/recreatelog.go), so the golden test writes
	// the exact on-disk JSON shape directly through the package's own
	// exported Record type, then reads it back through the same
	// recreatelog.Read this file's production code calls. This never
	// bypasses recreatelog's own strict-decode contract: Read still parses
	// whatever bytes land at recreatelog.Path(dir).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal fixture records: %v", err)
	}
	if err := os.WriteFile(recreatelog.Path(dir), data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}
