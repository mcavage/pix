package launch

import (
	"strings"
	"testing"

	"pix/host/sandbox"
)

// TestBuildEntrypointAttachArgv pins the v2 attach shape: everything the
// sandbox runs comes after `--`, and model/resume are entrypoint arguments
// carried on EVERY attach, not creation-time state.
func TestBuildEntrypointAttachArgv(t *testing.T) {
	argv, err := BuildEntrypointAttachArgv("pix-proj-1234abcd", true, PixEntrypoint(), "anthropic/claude-sonnet-5", "sess-7")
	if err != nil {
		t.Fatalf("BuildEntrypointAttachArgv: %v", err)
	}
	want := "exec -it pix-proj-1234abcd -- pi --model anthropic/claude-sonnet-5 --resume sess-7"
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("attach argv =\n  %s\nwant\n  %s", got, want)
	}
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("every attach must separate in-sandbox argv with `--`: %v", argv)
	}
	for _, a := range argv[:sep] {
		if a == "--model" || a == "--resume" {
			t.Fatalf("session options must never precede `--` (sbx would claim them): %v", argv)
		}
	}
}

func TestEntrypointArgsOmitsUnsetOptions(t *testing.T) {
	if got := strings.Join(EntrypointArgs(PixEntrypoint(), "", ""), " "); got != "pi" {
		t.Fatalf("a bare session must exec just the entrypoint, got %q", got)
	}
	if got := strings.Join(EntrypointArgs(nil, "m", ""), " "); got != "pi --model m" {
		t.Fatalf("a nil entrypoint must default to the Pix entrypoint, got %q", got)
	}
	if got := strings.Join(EntrypointArgs(PixEntrypoint(), "", "s"), " "); got != "pi --resume s" {
		t.Fatalf("resume alone must be carried, got %q", got)
	}
}

// TestNameForIsDeterministicPerWorkspaceAndEnv proves two environments on
// one workspace are two sandboxes, and that naming is stable across
// spellings.
func TestNameForIsDeterministicPerWorkspaceAndEnv(t *testing.T) {
	a := sandbox.NameFor("/home/u/proj", "work")
	b := sandbox.NameFor("/home/u/./proj", "work")
	c := sandbox.NameFor("/home/u/proj", "home")
	if a != b {
		t.Fatalf("the same workspace+env must derive one name: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("two environments on one workspace must not share a sandbox: %s", a)
	}
	if !strings.HasPrefix(a, "pix-") {
		t.Fatalf("derived names stay pix-* scoped, got %s", a)
	}
}
