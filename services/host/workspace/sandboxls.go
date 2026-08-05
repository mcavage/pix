package workspace

import "strings"

// sandboxls.go — parsing `sbx ls` output. It lives here, below both callers,
// because status renders these lines and reset deletes what they name; a type
// two callers exchange belongs under both of them.

type SandboxLine struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// ParseSandboxes extracts pix-* sandbox lines from `sbx ls` output. It is
// lenient about column layout by reusing the canonical sbx parser. In
// particular, the final column may be a pack mount rather than the state.
func ParseSandboxes(sbxLsOut string) []SandboxLine {
	var out []SandboxLine
	for _, box := range ParsePixBoxes(sbxLsOut) {
		out = append(out, SandboxLine{Name: box.Name, State: box.State})
	}
	return out
}

// SbxBox is one parsed `sbx ls` row for a pix sandbox.
type SbxBox struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Dir   string `json:"dir,omitempty"`
}

// ParsePixBoxes filters `sbx ls` output to pix-* rows and pulls name, state, and
// (best-effort) the workspace dir. The state is whichever field is a lifecycle word.
func ParsePixBoxes(sbxLsOut string) []SbxBox {
	var out []SbxBox
	for _, ln := range strings.Split(sbxLsOut, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 1 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, "pix-") {
			continue
		}
		b := SbxBox{Name: name}
		var absolutePaths []string
		for _, f := range fields[1:] {
			switch {
			case KnownSbxStates[strings.ToLower(f)]:
				b.State = strings.ToLower(f)
			case strings.HasPrefix(f, "/"):
				absolutePaths = append(absolutePaths, f)
			}
		}
		if len(absolutePaths) == 1 {
			b.Dir = absolutePaths[0]
		}
		if b.State == "" {
			b.State = "?"
		}
		out = append(out, b)
	}
	return out
}

// KnownSbxStates are the sbx lifecycle words we recognize when parsing a row,
// so we can pick the state column out regardless of sbx's exact layout.
var KnownSbxStates = map[string]bool{
	"running": true, "stopped": true, "exited": true,
	"created": true, "paused": true, "restarting": true, "dead": true,
}
