package main

import (
	"pix/host/workspace"
	"testing"
)

func TestParsePixBoxes(t *testing.T) {
	// A realistic `sbx ls` with a header, mixed columns, non-pix rows, and
	// column drift (state word in different positions, dir last).
	out := `NAME                 IMAGE            STATE     WORKSPACE
pix-pix    pix:0.0.27  running   /Users/mcavage/dev/pix
pix-tact        pix:0.0.27  stopped   /Users/mcavage/dev/tact
some-other-box       ubuntu           running   /tmp/x
pix-t-001-ab    pix:0.0.27  exited    /Users/mcavage/dev/pix/tasks/001/co
`
	boxes := workspace.ParsePixBoxes(out)
	if len(boxes) != 3 {
		t.Fatalf("want 3 pix boxes, got %d: %+v", len(boxes), boxes)
	}
	if boxes[0].Name != "pix-pix" || boxes[0].State != "running" ||
		boxes[0].Dir != "/Users/mcavage/dev/pix" {
		t.Errorf("box0 = %+v", boxes[0])
	}
	if boxes[1].State != "stopped" {
		t.Errorf("box1 state = %q, want stopped", boxes[1].State)
	}
	// Non-pix row must be filtered out.
	for _, b := range boxes {
		if b.Name == "some-other-box" {
			t.Error("non-pix box leaked into the list")
		}
	}
}

func TestParsePixBoxes_DoesNotMistakePackMountForWorkspace(t *testing.T) {
	boxes := workspace.ParsePixBoxes("pix-demo running /tmp/project /Users/me/pack/skills\n")
	if len(boxes) != 1 {
		t.Fatalf("want one box, got %+v", boxes)
	}
	if boxes[0].Dir != "" {
		t.Fatalf("ambiguous raw paths must not be guessed, got %q", boxes[0].Dir)
	}
}

func TestParsePixBoxes_Empty(t *testing.T) {
	if got := workspace.ParsePixBoxes("NAME  STATE\n"); len(got) != 0 {
		t.Fatalf("want 0, got %+v", got)
	}
}
