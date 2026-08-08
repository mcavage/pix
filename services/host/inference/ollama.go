package inference

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"pix/host/config"
	"pix/host/hostenv"
)

// ollama.go answers "where does this host's Ollama listen, and what does it
// call a catalog model". ONE resolver owns the endpoint, so no surface can
// report on an address the daemon does not use;
// scripts/check-endpoint-literals.sh fails the build on any other literal.

// defaultOllamaHost is the only place the default endpoint is spelled out.
const defaultOllamaHost = "127.0.0.1:" + defaultOllamaPortStr

const (
	defaultOllamaPort    = 11434
	defaultOllamaPortStr = "11434"
)

// OllamaEndpoint is the resolved endpoint: the URL a probe talks to, the
// host/port split a TCP dial needs, and where the value came from.
type OllamaEndpoint struct {
	URL    string // e.g. http://127.0.0.1:11434
	Host   string // e.g. 127.0.0.1
	Port   int    // e.g. 11434
	Source string // "default" | "OLLAMA_HOST"
}

// String renders the endpoint for evidence lines. The zero value reads as the
// default, so a line never trails off into an empty address.
func (e OllamaEndpoint) String() string {
	if e.URL == "" {
		return "http://" + defaultOllamaHost
	}
	return e.URL
}

// OllamaEndpointFor is THE resolver, mirroring services/host/memembed.go:
// OLLAMA_HOST wins (bare host, host:port or full URL); otherwise the default.
func OllamaEndpointFor(env hostenv.Env) OllamaEndpoint {
	raw := strings.TrimSpace(env.Getenv("OLLAMA_HOST"))
	if raw == "" {
		return OllamaEndpoint{URL: "http://" + defaultOllamaHost, Host: "127.0.0.1", Port: defaultOllamaPort, Source: "default"}
	}
	e := OllamaEndpoint{Source: "OLLAMA_HOST", Port: defaultOllamaPort}
	hostport, scheme := raw, "http"
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		scheme, hostport = u.Scheme, u.Host
	}
	e.Host = hostport
	if h, p, ok := splitHostPort(hostport); ok {
		e.Host, e.Port = h, p
	}
	if e.Host == "" {
		e.Host = "127.0.0.1"
	}
	e.URL = fmt.Sprintf("%s://%s:%d", scheme, e.Host, e.Port)
	return e
}

// splitHostPort splits "host:port" without rejecting a bare host. A bracketed
// IPv6 literal keeps its brackets so the rebuilt URL stays valid.
func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 || strings.HasSuffix(s, "]") {
		return s, 0, false
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil || p <= 0 {
		return s, 0, false
	}
	return s[:i], p, true
}

// OllamaTagFor strips the catalog's provider prefix, giving the tag `ollama
// pull` and `ollama list` actually speak.
func OllamaTagFor(catalogID string) string {
	return strings.TrimPrefix(catalogID, "ollama/")
}

// OllamaBindingDriver keys off the BACKEND's driver: a binding named
// "ollama/..." on some other backend is not an ollama binding.
func OllamaBindingDriver(cfg *config.Config, b config.InferenceModelBinding) bool {
	if cfg == nil {
		return false
	}
	backend, ok := cfg.Inference.Backends[b.Backend]
	return ok && backend.Driver == "ollama"
}
