package uatenvmatrix_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/uatenvmatrix"
)

func TestParseArgs_ListChecksNeedsNothingElse(t *testing.T) {
	got, err := uatenvmatrix.ParseArgs([]string{"--list-checks"})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if !got.ListChecks {
		t.Fatalf("expected ListChecks=true, got %#v", got)
	}
}

func TestParseArgs_RejectsPositionalArgs(t *testing.T) {
	dir := t.TempDir()
	if _, err := uatenvmatrix.ParseArgs([]string{
		"--out-dir", dir, "--steps-dir", dir,
		"--image-tag", "docker.io/mcavage/pix:uat-run1",
		"extra-positional",
	}); err == nil {
		t.Fatal("expected an error for a positional argument")
	}
	if _, err := uatenvmatrix.ParseArgs([]string{"--list-checks", "extra-positional"}); err == nil {
		t.Fatal("expected an error for a positional argument in --list-checks mode too")
	}
}

func TestParseArgs_RequiresAbsoluteExistingOutDir(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		outDir string
	}{
		{"empty", ""},
		{"relative", "relative/path"},
		{"nonexistent", filepath.Join(dir, "does-not-exist")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := uatenvmatrix.ParseArgs([]string{
				"--out-dir", tc.outDir, "--steps-dir", dir,
				"--image-tag", "docker.io/mcavage/pix:uat-run1",
			})
			if err == nil {
				t.Fatalf("expected an error for --out-dir=%q", tc.outDir)
			}
		})
	}
}

func TestParseArgs_OutDirMustBeADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := uatenvmatrix.ParseArgs([]string{
		"--out-dir", file, "--steps-dir", dir,
		"--image-tag", "docker.io/mcavage/pix:uat-run1",
	})
	if err == nil {
		t.Fatal("expected an error when --out-dir is a file, not a directory")
	}
}

func TestParseArgs_RequiresAbsoluteExistingStepsDir(t *testing.T) {
	dir := t.TempDir()
	_, err := uatenvmatrix.ParseArgs([]string{
		"--out-dir", dir, "--steps-dir", "relative/steps",
		"--image-tag", "docker.io/mcavage/pix:uat-run1",
	})
	if err == nil {
		t.Fatal("expected an error for a relative --steps-dir")
	}
	_, err = uatenvmatrix.ParseArgs([]string{
		"--out-dir", dir, "--steps-dir", filepath.Join(dir, "missing"),
		"--image-tag", "docker.io/mcavage/pix:uat-run1",
	})
	if err == nil {
		t.Fatal("expected an error for a nonexistent --steps-dir")
	}
}

func TestParseArgs_ImageTagMustBeFullyQualifiedInUatNamespace(t *testing.T) {
	dir := t.TempDir()
	badTags := []string{
		"",
		"docker.io/mcavage/pix:latest",
		"docker.io/mcavage/pix:",
		"docker.io/mcavage/pix:test-candidate",
		"mcavage/pix:uat-run1",
		"docker.io/mcavage/pix",
	}
	for _, tag := range badTags {
		t.Run(tag, func(t *testing.T) {
			_, err := uatenvmatrix.ParseArgs([]string{
				"--out-dir", dir, "--steps-dir", dir, "--image-tag", tag,
			})
			if err == nil {
				t.Fatalf("expected an error for image tag %q", tag)
			}
		})
	}
}

func TestParseArgs_ValidInvocation(t *testing.T) {
	outDir := t.TempDir()
	stepsDir := t.TempDir()
	got, err := uatenvmatrix.ParseArgs([]string{
		"--out-dir", outDir, "--steps-dir", stepsDir,
		"--image-tag", "docker.io/mcavage/pix:uat-run-20260901-000000-abcd1234",
	})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if got.OutDir != outDir || got.StepsDir != stepsDir || got.ImageTag != "docker.io/mcavage/pix:uat-run-20260901-000000-abcd1234" {
		t.Fatalf("ParseArgs = %#v", got)
	}
}

func TestRunCLI_ListChecksPrintsCandidateCheckNamesWithoutHostTools(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := uatenvmatrix.RunCLI([]string{"--list-checks"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	printed := strings.Fields(stdout.String())
	want := uatenvmatrix.CheckNames()
	if len(printed) != len(want) {
		t.Fatalf("printed %v checks, want %v", printed, want)
	}
	for i := range want {
		if printed[i] != want[i] {
			t.Errorf("printed[%d] = %q, want %q", i, printed[i], want[i])
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output, got %q", stderr.String())
	}
}

func TestRunCLI_ParseErrorExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := uatenvmatrix.RunCLI([]string{"--image-tag", "docker.io/mcavage/pix:latest"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Error("expected a diagnostic on stderr")
	}
}

func TestRunCLI_RunFailureExitsOne(t *testing.T) {
	outDir := t.TempDir() // deliberately empty: no candidate pix/pix-host binaries
	stepsDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := uatenvmatrix.RunCLI([]string{
		"--out-dir", outDir, "--steps-dir", stepsDir,
		"--image-tag", "docker.io/mcavage/pix:uat-run1",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "candidate binary missing") {
		t.Errorf("stderr = %q, want it to name the missing candidate binary", stderr.String())
	}
}
