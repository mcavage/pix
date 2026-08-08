package workspace

import "strings"

// sandboxls.go — parsing `sbx ls` output. It lives here, below its callers
// (launch's ls/rm), as the one canonical parser every reader of `sbx ls` shares.

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
