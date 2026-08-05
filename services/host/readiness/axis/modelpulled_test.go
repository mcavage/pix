package axis

import "testing"

// TestModelPulled handles :tag suffixes. Moved here with its subject from the
// doctor workflow's test file.
func TestModelPulled(t *testing.T) {
	list := "NAME              ID\ngemma4:latest     abc\n"
	if !ModelPulled(list, "gemma4") {
		t.Error("gemma4 should match gemma4:latest")
	}
	if ModelPulled(list, "gemma") {
		t.Error("gemma should not match gemma4")
	}
}
