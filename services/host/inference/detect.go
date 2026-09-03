package inference

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"pix/host/hostenv"
)

// detect.go is the ONE Ollama integration every caller (doctor, setup, and
// the memory-container wiring) shares — never a second, disagreeing probe.
// It answers three questions, all by DETECTION, never by interview (docs/
// design/pix-v2-surface.md §7 — "There is no setup interview" — applies to
// Ollama exactly as it does to llmman):
//
//  1. is the `ollama` CLI on PATH at all?
//  2. what endpoint does this host's existing config/runtime already name
//     (OllamaEndpointFor: OLLAMA_HOST, or the default loopback address), and
//     is that endpoint LOCAL (this host's own disk — pulling is safe) or
//     REMOTE (someone else's daemon, or a proxied Ollama Cloud account —
//     Pix has no business starting a multi-gigabyte download there)?
//  3. what models does that endpoint currently list?
//
// This deliberately does NOT reproduce workflow/models' per-model local/cloud
// classification (classifyOllamaTag) — that machinery answers a DIFFERENT
// question ("which of these already-listed tags may this binding treat as
// free-and-local for the RAM gate"), for a flow that is not wired to any
// command. This file answers the simpler, structural question a single
// endpoint address already settles: is Pix talking to a daemon it can pull
// into, or one it can only read from.

// OllamaMode is the one fact that decides whether Pix may offer a pull.
type OllamaMode string

const (
	// OllamaModeLocal: the resolved endpoint is a loopback address — the
	// default, or an explicit OLLAMA_HOST naming 127.0.0.1/localhost/::1.
	// A pull here downloads onto disk this host owns.
	OllamaModeLocal OllamaMode = "local"
	// OllamaModeRemote: OLLAMA_HOST names some other machine — a shared team
	// daemon, or a host proxying an Ollama Cloud account. Pix reports what it
	// finds and never offers to pull.
	OllamaModeRemote OllamaMode = "remote"
)

// OllamaModelInfo is one row of the daemon's own /api/tags answer.
type OllamaModelInfo struct {
	Tag        string
	RemoteHost string
	Size       int64
}

// OllamaStatus is the whole integration's answer for this host. Every field
// is a detected fact; none is a user choice.
type OllamaStatus struct {
	// CLIPresent reports whether an `ollama` binary was found on PATH.
	CLIPresent bool
	// Endpoint is the resolved address (OllamaEndpointFor).
	Endpoint OllamaEndpoint
	// Mode is derived from Endpoint.Host alone.
	Mode OllamaMode
	// Reachable reports whether /api/tags actually answered.
	Reachable bool
	// ListErr is the reachability failure, set only when !Reachable.
	ListErr error
	// Models is every tag the endpoint listed, keyed by tag. Empty (never
	// nil) when Reachable but nothing is pulled; nil when !Reachable.
	Models map[string]OllamaModelInfo
}

// HasModel reports whether tag is on the resolved endpoint's list.
func (s OllamaStatus) HasModel(tag string) bool {
	_, ok := s.Models[tag]
	return ok
}

// CanPull reports whether this integration may offer to pull a missing
// model: only ever true for a reachable LOCAL endpoint. A remote endpoint —
// cloud or a shared daemon — is reported, never pulled into.
func (s OllamaStatus) CanPull() bool {
	return s.Reachable && s.Mode == OllamaModeLocal
}

// isLoopbackHost reports whether host — as OllamaEndpoint.Host resolves it,
// brackets already stripped by splitHostPort — names this machine's own
// loopback interface.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// DetectOllama is the one entry point every caller uses. It never asks the
// user anything: the CLI check, the endpoint, the mode, and the model
// listing are all read from this host's own existing config/runtime.
func DetectOllama(env hostenv.Env) OllamaStatus {
	st := OllamaStatus{Endpoint: OllamaEndpointFor(env)}
	if _, err := env.LookPath("ollama"); err == nil {
		st.CLIPresent = true
	}
	st.Mode = OllamaModeRemote
	if isLoopbackHost(st.Endpoint.Host) {
		st.Mode = OllamaModeLocal
	}
	models, err := ListOllamaModels(env)
	if err != nil {
		st.ListErr = err
		return st
	}
	st.Reachable = true
	st.Models = models
	return st
}

// ContainerOllamaHost renders the OLLAMA_HOST value the pix-memory
// CONTAINER should be given, translating this HOST's own loopback address
// into the one address a container can actually reach it at
// (host.docker.internal) — a container's own 127.0.0.1 is itself, never the
// host. A remote endpoint passes through unchanged: it already names an
// address reachable from anywhere on the network, container or not.
func ContainerOllamaHost(s OllamaStatus) string {
	if s.Mode == OllamaModeRemote {
		return fmt.Sprintf("%s:%d", s.Endpoint.Host, s.Endpoint.Port)
	}
	return fmt.Sprintf("host.docker.internal:%d", s.Endpoint.Port)
}

// ollamaDetectTimeout bounds the /api/tags request so a wedged or absent
// daemon can never hang a caller. A var, not a const, so a hermetic test can
// shrink it.
var ollamaDetectTimeout = 5 * time.Second

// ollamaDetectBodyCap bounds how much of the /api/tags response this
// integration will read — generous for a real install (see workflow/models'
// identical cap for the measurement this is sized from), but never
// unbounded against a wedged or malicious listener.
const ollamaDetectBodyCap = 16 << 20 // 16 MiB

// ollamaTagNamePattern is Ollama's own legal name grammar — namespace/
// model:tag — and the ingestion boundary for every tag this function ever
// returns. A rogue listener, or a redirected OLLAMA_HOST, controls every
// byte of the response; without this a name carrying \r, \n, or an ANSI
// erase sequence could forge whatever a caller renders from it. Checked
// once, here, mirroring workflow/models' identical boundary.
var ollamaTagNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*$`)

func validOllamaTagName(name string) bool {
	return name != "" && len(name) <= 256 && ollamaTagNamePattern.MatchString(name)
}

type ollamaTagsResponse struct {
	Models []struct {
		Name       string `json:"name"`
		RemoteHost string `json:"remote_host"`
		Size       int64  `json:"size"`
	} `json:"models"`
}

// ListOllamaModels calls the resolved endpoint's /api/tags directly — the
// daemon's own listing, never a shelled-out `ollama list` text parse — so
// this doubles as the daemon reachability probe: "is Ollama up" has exactly
// one spelling.
func ListOllamaModels(env hostenv.Env) (map[string]OllamaModelInfo, error) {
	endpoint := strings.TrimRight(OllamaEndpointFor(env).URL, "/")
	req, err := http.NewRequest(http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	resp, err := (&http.Client{Timeout: ollamaDetectTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not list Ollama models")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("could not list Ollama models (HTTP %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ollamaDetectBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("could not list Ollama models (could not read the response)")
	}
	if len(raw) > ollamaDetectBodyCap {
		return nil, fmt.Errorf("the Ollama daemon answered, but its tag listing exceeded the %dMiB safety cap — this is a cap, not a sign the daemon is down", ollamaDetectBodyCap>>20)
	}
	var body ollamaTagsResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("the Ollama daemon answered, but its tag listing was not valid JSON")
	}
	seen := make(map[string]OllamaModelInfo, len(body.Models))
	for _, m := range body.Models {
		if validOllamaTagName(m.Name) {
			seen[m.Name] = OllamaModelInfo{Tag: m.Name, RemoteHost: m.RemoteHost, Size: m.Size}
		}
	}
	return seen, nil
}
