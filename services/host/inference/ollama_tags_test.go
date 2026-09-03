package inference

import (
	"reflect"
	"testing"
)

// TestNormalizeOllamaTag: the two spellings that made a present model
// report missing — a bare name (what a human writes, and what `ollama pull`
// takes) against the daemon's own `:latest`, and a catalog id's `ollama/`
// provider prefix against the daemon's unprefixed name.
func TestNormalizeOllamaTag(t *testing.T) {
	cases := map[string]string{
		"nomic-embed-text":         "nomic-embed-text:latest",
		"nomic-embed-text:latest":  "nomic-embed-text:latest",
		"ollama/qwen3.5:9b":        "qwen3.5:9b",
		"Qwen3.5:9B":               "qwen3.5:9b",
		"  nomic-embed-text  ":     "nomic-embed-text:latest",
		"hf.co/user/model":         "hf.co/user/model:latest",
		"hf.co/user/model:Q4_K_M":  "hf.co/user/model:q4_k_m",
		"library/nomic-embed-text": "library/nomic-embed-text:latest",
		"":                         "",
	}
	for in, want := range cases {
		if got := NormalizeOllamaTag(in); got != want {
			t.Errorf("NormalizeOllamaTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOllamaStatusHasModelAcrossTagSpellings: the bug this normalization
// exists for. `ollama pull nomic-embed-text` lists as
// `nomic-embed-text:latest`, so an exact-match lookup against the
// configured name reported the model missing and setup offered to pull a
// model that was already there.
func TestOllamaStatusHasModelAcrossTagSpellings(t *testing.T) {
	st := OllamaStatus{Reachable: true, Models: map[string]OllamaModelInfo{
		"nomic-embed-text:latest": {Tag: "nomic-embed-text:latest"},
		"qwen3.5:9b":              {Tag: "qwen3.5:9b"},
	}}
	for _, want := range []string{"nomic-embed-text", "nomic-embed-text:latest", "ollama/qwen3.5:9b", "qwen3.5:9b"} {
		if !st.HasModel(want) {
			t.Errorf("HasModel(%q) = false, want true", want)
		}
	}
	if st.HasModel("mxbai-embed-large") {
		t.Error("HasModel reported a model the endpoint never listed")
	}
	// ResolveModel returns the DAEMON's spelling, which is the one worth
	// printing and recording.
	if got, ok := st.ResolveModel("nomic-embed-text"); !ok || got != "nomic-embed-text:latest" {
		t.Errorf("ResolveModel = (%q, %v), want the listed tag", got, ok)
	}
}

// TestOllamaStatusChatModels: an embedding-only model cannot hold a
// conversation, so it must never appear in a session-model listing —
// however prominent it is in `ollama list`.
func TestOllamaStatusChatModels(t *testing.T) {
	st := OllamaStatus{Reachable: true, Models: map[string]OllamaModelInfo{
		"qwen3.5:9b":                {Tag: "qwen3.5:9b"},
		"nomic-embed-text:latest":   {Tag: "nomic-embed-text:latest"},
		"mxbai-embed-large:latest":  {Tag: "mxbai-embed-large:latest"},
		"snowflake-arctic-embed2:l": {Tag: "snowflake-arctic-embed2:l"},
		"glm-5.2:cloud":             {Tag: "glm-5.2:cloud"},
	}}
	want := []string{"glm-5.2:cloud", "qwen3.5:9b"}
	if got := st.ChatModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("ChatModels() = %v, want %v", got, want)
	}
}
