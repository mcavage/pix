package cli

import "testing"

// TestGrepWord moved here with its subject: it was stranded in the doctor
// workflow's test file, which was written when GrepWord lived there.
func TestGrepWord(t *testing.T) {
	if !GrepWord("anthropic openai", "openai") {
		t.Error("should match whole word")
	}
	if GrepWord("openaikey", "openai") {
		t.Error("should not match substring")
	}
	if !GrepWord("a,b:c/d", "c") {
		t.Error("should split on punctuation")
	}
}
