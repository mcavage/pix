package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"pix/host/config"
	"strings"
	"time"
)

// live.go — the REAL provider probes: actually call the endpoint and report
// whether a (provider, model, key) triple answers. They lived in cmd/pix
// because doctor was the first caller; asking "does this model actually work"
// is the inference capability's own question.

// LiveOllamaInferenceProbe posts ONE minimal generate to endpoint/api/generate.
// endpoint is ALWAYS supplied by axis.EffectiveOllamaEndpoint; this function never
// spells an address of its own (scripts/check-endpoint-literals.sh). No auth
// header: the local daemon owns any cloud credential and Pix stores none.
//
// keep_alive:0 is load-bearing, not tidiness — it tells the daemon to unload
// the model as soon as the response is written, so probe n+1 starts against a
// free memory budget instead of stacking on probe n's resident weights.
func LiveOllamaInferenceProbe(endpoint, model string, numCtx int, timeout time.Duration) error {
	options := map[string]any{"num_predict": 8}
	if numCtx > 0 {
		options["num_ctx"] = numCtx
	}
	body, err := json.Marshal(map[string]any{
		"model": model, "prompt": "Reply OK", "stream": false, "keep_alive": 0, "options": options,
	})
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("probe unavailable")
	}
	defer resp.Body.Close()
	// Drained, never echoed: an Ollama error body can quote request content.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint rejected the request (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func InferenceNeedsOnePassword(cfg *config.Config) bool {
	if cfg == nil || len(cfg.Inference.Backends) == 0 {
		return true // default setup path is a direct API key
	}
	for _, binding := range cfg.Inference.Models {
		// Availability is probe evidence, not topology. Setup must still require
		// 1Password for an allowed direct binding before that first probe has
		// promoted it; exclusivity alone decides whether a backend is dormant.
		if !Allowed(cfg, binding) {
			continue
		}
		b, ok := cfg.Inference.Backends[binding.Backend]
		if ok && BackendAllowed(cfg, b, binding.Backend) && b.Auth == "1password" {
			return true
		}
	}
	return false
}

// LiveDirectInferenceProbe makes a minimal generation request through the
// provider's public API. The client has a hard wall-clock timeout and response
// bodies are never echoed, preventing provider errors from accidentally
// reflecting credential material into setup output.
func LiveDirectInferenceProbe(provider, model, key string) error {
	var endpoint string
	var body []byte
	headers := map[string]string{"Content-Type": "application/json"}
	switch provider {
	case "openai":
		endpoint = "https://api.openai.com/v1/responses"
		body, _ = json.Marshal(map[string]any{"model": model, "input": "Reply OK", "max_output_tokens": 16})
		headers["Authorization"] = "Bearer " + key
	case "anthropic":
		endpoint = "https://api.anthropic.com/v1/messages"
		body, _ = json.Marshal(map[string]any{"model": model, "max_tokens": 8, "messages": []map[string]string{{"role": "user", "content": "Reply OK"}}})
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	case "google":
		endpoint = "https://generativelanguage.googleapis.com/v1beta/models/" + url.PathEscape(model) + ":generateContent"
		body, _ = json.Marshal(map[string]any{"contents": []map[string]any{{"parts": []map[string]string{{"text": "Reply OK"}}}}, "generationConfig": map[string]int{"maxOutputTokens": 8}})
		headers["x-goog-api-key"] = key
	default:
		return fmt.Errorf("unsupported provider")
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not build probe")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: directInferenceProbeTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("probe unavailable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider rejected model request (HTTP %d)", resp.StatusCode)
	}
	return nil
}

const directInferenceProbeTimeout = 8 * time.Second
