package env

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// ── ComputeLs / RenderLs: deterministic, marks the default, no status taxonomy ──

func TestComputeLs_EmptyRegistry(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	r, err := ComputeLs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Default != "" || len(r.Entries) != 0 {
		t.Errorf("ComputeLs on an empty registry = %+v, want zero value", r)
	}
}

func TestRenderLs_EmptyRegistryUsesBuiltInDefaultsProse(t *testing.T) {
	var out bytes.Buffer
	RenderLs(&out, LsResult{})
	got := out.String()
	if !strings.Contains(got, "built-in defaults") {
		t.Errorf("empty ls output = %q, want the D17 built-in-defaults prose", got)
	}
	if strings.Contains(got, "default environment") {
		t.Errorf("empty ls output = %q, must never say the banned 'default environment'", got)
	}
}

func TestComputeLs_DeterministicAndMarksDefault(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", root)

	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfg, "home", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("work"); err != nil {
		t.Fatal(err)
	}

	// Deterministic across repeated calls, over three different cwds
	// (mirroring AC-10's "resolves identically from any cwd" over a
	// listing rather than a single lookup).
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	var results []LsResult
	for _, cwd := range []string{t.TempDir(), t.TempDir(), t.TempDir()} {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
		r, err := ComputeLs(cfg)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	for i := 1; i < len(results); i++ {
		if !equalLsResults(results[0], results[i]) {
			t.Errorf("ComputeLs is not deterministic across cwd: %+v vs %+v", results[0], results[i])
		}
	}

	r := results[0]
	if r.Default != "work" {
		t.Errorf("Default = %q, want %q", r.Default, "work")
	}
	if len(r.Entries) != 2 {
		t.Fatalf("Entries = %+v, want 2 rows", r.Entries)
	}
	// Known() sorts, so "home" precedes "work".
	if r.Entries[0].Name != "home" || r.Entries[1].Name != "work" {
		t.Errorf("Entries order = %+v, want sorted [home, work]", r.Entries)
	}
	if !r.Entries[1].Default {
		t.Errorf("work entry Default = false, want true")
	}
	if r.Entries[0].Default {
		t.Errorf("home entry Default = true, want false")
	}
	// Neither has been reviewed yet.
	if r.Entries[0].Accepted || r.Entries[1].Accepted {
		t.Errorf("Entries = %+v, want both unaccepted (nothing was reviewed)", r.Entries)
	}
}

func TestRenderLs_MarksDefaultAndReviewState(t *testing.T) {
	var out bytes.Buffer
	RenderLs(&out, LsResult{
		Default: "work",
		Entries: []LsEntry{
			{Name: "home", Root: "/h", Default: false, Accepted: true, ReviewState: ReviewAccepted},
			{Name: "work", Root: "/w", Default: true, Accepted: false, ReviewState: ReviewUnaccepted},
		},
	})
	got := out.String()
	for _, want := range []string{"home", "work", "accepted", "unaccepted", "/h", "/w"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls output missing %q:\n%s", want, got)
		}
	}
}

// TestRenderLs_AllFourReviewStatesPlusInvalid pins the ONE ls-column
// spelling for every ReviewState computeReviewState (or ComputeLs's own
// invalid-degrade) can produce — the exact four-plus-one taxonomy this
// unit adds, never a silently blank column for a state a future caller
// introduces.
func TestRenderLs_AllFourReviewStatesPlusInvalid(t *testing.T) {
	var out bytes.Buffer
	RenderLs(&out, LsResult{
		Entries: []LsEntry{
			{Name: "a", Root: "/a", ReviewState: ReviewNotRequired},
			{Name: "b", Root: "/b", ReviewState: ReviewUnaccepted},
			{Name: "c", Root: "/c", ReviewState: ReviewAccepted},
			{Name: "d", Root: "/d", ReviewState: ReviewChanged},
			{Name: "e", Root: "/e", ReviewState: ReviewInvalid},
		},
	})
	got := out.String()
	for _, want := range []string{"n/a", "unaccepted", "accepted", "changed", "invalid"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderLsJSON_SchemaVersionAndNoneWhenUnselected(t *testing.T) {
	var out bytes.Buffer
	if err := RenderLsJSON(&out, LsResult{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"schema_version": 1`) {
		t.Errorf("ls --json = %s, want schema_version", got)
	}
	if !strings.Contains(got, `"environment": "none"`) {
		t.Errorf("ls --json with no default = %s, want environment \"none\" (D17)", got)
	}
	if !strings.Contains(got, `"environments": []`) {
		t.Errorf("ls --json with an empty registry = %s, want an empty array, not null", got)
	}
}

// TestRenderLsJSON_CarriesReviewStateAndBackwardAcceptedBool proves the
// `review_state` addition never drops the pre-existing `accepted` bool a
// script may already read.
func TestRenderLsJSON_CarriesReviewStateAndBackwardAcceptedBool(t *testing.T) {
	var out bytes.Buffer
	if err := RenderLsJSON(&out, LsResult{
		Default: "work",
		Entries: []LsEntry{{Name: "work", Root: "/w", Default: true, Accepted: true, ReviewState: ReviewAccepted}},
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`"accepted": true`, `"review_state": "accepted"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ls --json = %s, want %q", got, want)
		}
	}
}

func equalLsResults(a, b LsResult) bool {
	if a.Default != b.Default || len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		if a.Entries[i] != b.Entries[i] {
			return false
		}
	}
	return true
}
