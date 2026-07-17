package main

import "testing"

func TestParsePiStackBoxes(t *testing.T) {
	// A realistic `sbx ls` with a header, mixed columns, non-pi-stack rows, and
	// column drift (state word in different positions, dir last).
	out := `NAME                 IMAGE            STATE     WORKSPACE
pi-stack-pi-stack    pi-stack:0.0.27  running   /Users/mcavage/dev/pi-stack
pi-stack-tact        pi-stack:0.0.27  stopped   /Users/mcavage/dev/tact
some-other-box       ubuntu           running   /tmp/x
pi-stack-t-001-ab    pi-stack:0.0.27  exited    /Users/mcavage/dev/pi-stack/tasks/001/co
`
	boxes := parsePiStackBoxes(out)
	if len(boxes) != 3 {
		t.Fatalf("want 3 pi-stack boxes, got %d: %+v", len(boxes), boxes)
	}
	if boxes[0].Name != "pi-stack-pi-stack" || boxes[0].State != "running" ||
		boxes[0].Dir != "/Users/mcavage/dev/pi-stack" {
		t.Errorf("box0 = %+v", boxes[0])
	}
	if boxes[1].State != "stopped" {
		t.Errorf("box1 state = %q, want stopped", boxes[1].State)
	}
	// Non-pi-stack row must be filtered out.
	for _, b := range boxes {
		if b.Name == "some-other-box" {
			t.Error("non-pi-stack box leaked into the list")
		}
	}
}

func TestParsePiStackBoxes_Empty(t *testing.T) {
	if got := parsePiStackBoxes("NAME  STATE\n"); len(got) != 0 {
		t.Fatalf("want 0, got %+v", got)
	}
}
