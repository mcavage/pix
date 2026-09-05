package provision

import (
	"fmt"
	"strings"
	"testing"
)

type prereqRecorder struct {
	calls []string
	sbx   string
}

func (r *prereqRecorder) Check(name string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if name == "sbx" {
		return r.sbx, nil
	}
	return name + " version ok", nil
}

func TestCheckPrereqsUsesSupportedSbxVersionGrammar(t *testing.T) {
	r := &prereqRecorder{sbx: "sbx version: v0.39.0 abc123"}
	if err := CheckPrereqs(r); err != nil {
		t.Fatalf("CheckPrereqs: %v", err)
	}
	if got := strings.Join(r.calls, "\n"); !strings.Contains(got, "sbx version") || strings.Contains(got, "sbx --version") {
		t.Fatalf("prerequisite argv =\n%s\nwant `sbx version`, never the rejected root --version flag", got)
	}
}

func TestCheckPrereqsRefusesOldOrUnparseableSbxBeforeMutation(t *testing.T) {
	for _, banner := range []string{"sbx version: v0.38.9 old", "not a version"} {
		t.Run(fmt.Sprintf("%q", banner), func(t *testing.T) {
			r := &prereqRecorder{sbx: banner}
			err := CheckPrereqs(r)
			if err == nil || !strings.Contains(err.Error(), "sbx is required") {
				t.Fatalf("CheckPrereqs(%q) = %v, want sbx refusal", banner, err)
			}
		})
	}
}
