package health

import (
	"context"
	"fmt"

	"pix/host/inference"
)

// ollama.go renders `pix doctor`'s Ollama row from the ONE integration
// inference.DetectOllama already is (detect.go): the same CLI/endpoint/mode/
// listing facts the memory-container wiring uses to set OLLAMA_HOST, never a
// second probe that could disagree with it.
//
// Ollama is an OPTIONAL local capability (a cloud-only host is a fully
// supported end state — pix-v2-surface.md §7), so this probe is never
// Required(): its absence never fails `pix doctor`'s exit code, and a
// missing embedding model on a REMOTE endpoint gets no Fix at all — Pix has
// no business telling a user to run a command on hardware it does not own.

// OllamaProbe reports this host's Ollama/embeddings state.
type OllamaProbe struct {
	// Detect resolves the whole integration's status. Nil is a caller bug in
	// production (Probes always wires inference.DetectOllama); a test injects
	// a fixed OllamaStatus.
	Detect func() inference.OllamaStatus
	// EmbedModel is the tag memory embeddings need present — e.g.
	// "nomic-embed-text" (cfg.MemoryEmbedModel, already defaulted by the
	// caller). Empty means "nothing to check for", and the row reports
	// listing/reachability only.
	EmbedModel string
}

func (OllamaProbe) Name() string   { return "ollama" }
func (OllamaProbe) Required() bool { return false }

func (p OllamaProbe) Check(ctx context.Context) Result {
	if p.Detect == nil {
		return Result{Status: StatusUnknown, Detail: "no detector configured"}
	}
	st := p.Detect()
	if !st.CLIPresent && !st.Reachable {
		return Result{Status: StatusOff, Detail: "not installed — local models and memory embeddings are optional and degrade to keyword recall"}
	}
	if !st.Reachable {
		detail := fmt.Sprintf("endpoint %s did not answer", st.Endpoint.String())
		if st.ListErr != nil {
			detail = fmt.Sprintf("%s: %v", detail, st.ListErr)
		}
		// A LOCAL endpoint not answering means "start the daemon" — an
		// actionable fix on this host. A REMOTE one not answering is a
		// connectivity fact about a machine Pix does not own; naming a
		// start command there would tell the user to start a daemon that is
		// not theirs to start.
		fix := ""
		if st.Mode == inference.OllamaModeLocal {
			fix = "start Ollama, then rerun `pix doctor`"
		}
		return Result{Status: StatusAbsent, Detail: detail, Fix: fix, Evidence: st.Endpoint.String()}
	}
	modeWord := "local"
	if st.Mode == inference.OllamaModeRemote {
		modeWord = "remote"
	}
	if p.EmbedModel == "" || st.HasModel(p.EmbedModel) {
		detail := fmt.Sprintf("%s endpoint %s, %d model(s) listed", modeWord, st.Endpoint.String(), len(st.Models))
		if p.EmbedModel != "" {
			detail = fmt.Sprintf("%s, embedding model %q present", detail, p.EmbedModel)
		}
		return Result{Status: StatusReady, Detail: detail, Evidence: st.Endpoint.String()}
	}
	// The embedding model is missing. Only a LOCAL endpoint gets a pull
	// command — a remote daemon (a shared team box, or a proxied Ollama
	// Cloud account) is not this host's disk to fill.
	fix := ""
	if st.CanPull() {
		fix = "ollama pull " + p.EmbedModel
	}
	return Result{
		Status:   StatusAbsent,
		Detail:   fmt.Sprintf("%s endpoint %s reachable, %d model(s) listed, embedding model %q not pulled", modeWord, st.Endpoint.String(), len(st.Models), p.EmbedModel),
		Fix:      fix,
		Evidence: st.Endpoint.String(),
	}
}
