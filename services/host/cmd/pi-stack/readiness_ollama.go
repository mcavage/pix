package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"pi-stack/host/config"
)

// readiness_ollama.go owns the TWO Ollama facts, kept deliberately apart:
//
//   - ollama.host      — the Ollama API answers at the EFFECTIVE endpoint on
//     this machine. One resolver (effectiveOllamaEndpoint) decides what that
//     endpoint is, and the resolved endpoint is echoed in every evidence
//     string, so a non-default OLLAMA_HOST can never produce a verdict about
//     an endpoint nobody uses.
//   - ollama.sandbox   — a sandbox can reach the host daemon through
//     host.docker.internal. Diagnostics NEVER create a sandbox to answer this:
//     with no sandbox it is `unverifiable` + optional, resolved later by run's
//     post-create probe.
//
// Bind-address inference (a daemon bound to 127.0.0.1 cannot serve the
// sandbox) may add remediation context to a `todo`. It may NEVER produce a
// `ready`: inference is not a probe.

// defaultOllamaHost is the only place the default Ollama endpoint is spelled
// out. scripts/check-endpoint-literals.sh fails the build on any other
// 127.0.0.1:11434 / localhost:11434 literal in Go source, so the resolver
// cannot be bypassed.
const defaultOllamaHost = "127.0.0.1:" + defaultOllamaPortStr

const (
	defaultOllamaPort    = 11434
	defaultOllamaPortStr = "11434"
)

// ollamaEndpoint is the resolved Ollama endpoint: the URL probes talk to, plus
// the host/port split a TCP dial needs and where the value came from.
type ollamaEndpoint struct {
	URL    string // e.g. http://127.0.0.1:11434
	Host   string // e.g. 127.0.0.1
	Port   int    // e.g. 11434
	Source string // "default" | "OLLAMA_HOST"
}

// String renders the endpoint for evidence lines. An unresolved endpoint (the
// zero value, e.g. a probe built before resolution) reads as the default, so
// an evidence line can never trail off into an empty address.
func (e ollamaEndpoint) String() string {
	if e.URL == "" {
		return "http://" + defaultOllamaHost
	}
	return e.URL
}

// loopbackOnly reports whether the endpoint is bound to loopback, which is the
// INFERENCE (never a probe) behind the sandbox-reachability remediation hint:
// a daemon on 127.0.0.1 is not reachable through host.docker.internal.
func (e ollamaEndpoint) loopbackOnly() bool {
	h := strings.ToLower(e.Host)
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "[::1]"
}

// effectiveOllamaEndpoint is THE resolver. Every axis builder and every
// renderer that names an Ollama endpoint goes through it, so doctor, status,
// setup and run can never talk about different endpoints. It mirrors the
// daemon-side resolution in services/host/memembed.go: OLLAMA_HOST wins, with
// a bare host, host:port, or full URL all accepted; otherwise the default.
func effectiveOllamaEndpoint(cfg *config.Config, env shellEnv) ollamaEndpoint {
	raw := ""
	if env.getenv != nil {
		raw = strings.TrimSpace(env.getenv("OLLAMA_HOST"))
	}
	if raw == "" {
		return ollamaEndpoint{URL: "http://" + defaultOllamaHost, Host: "127.0.0.1", Port: defaultOllamaPort, Source: "default"}
	}
	e := ollamaEndpoint{Source: "OLLAMA_HOST", Port: defaultOllamaPort}
	hostport := raw
	scheme := "http"
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

// ollamaHostAxis builds the `ollama.host` checks from ONE probe: is the binary
// on PATH, and does the API answer at the resolved endpoint. Ollama is always
// optional here (a missing local model degrades capture/recall; it never
// blocks) — `--pull-models` promotes the model axes through Request.Requested,
// not by hard-coding a requirement.
func ollamaHostAxis(cfg *config.Config, env shellEnv, ep ollamaEndpoint, p ollamaProbe) []check {
	daemonUp := p.daemonUp || p.listOK
	memoryEnabled := enabled(cfg, "memory")
	switch {
	case p.installed && daemonUp:
		return []check{{
			label:    "ollama",
			verdict:  verdictReady,
			detail:   "installed, " + ep.URL + " up",
			evidence: "ollama on PATH; the API answered at " + ep.URL,
			endpoint: ep.URL,
		}}
	case p.installed:
		return []check{{
			label:    "ollama",
			verdict:  verdictTodo,
			detail:   "installed but the daemon is not answering at " + ep.URL,
			evidence: "ollama at " + ep.URL + ": connection refused; `ollama list` failed",
			endpoint: ep.URL,
			todo:     "ollama serve",
		}}
	case memoryEnabled:
		return []check{{
			label:    "ollama",
			verdict:  verdictTodo,
			detail:   "not installed (the configured memory service needs it for capture + recall)",
			evidence: "ollama not on PATH; memory is in the configured services",
			endpoint: ep.URL,
			todo:     "brew install ollama",
		}}
	default:
		return []check{{
			label:    "ollama",
			note:     true,
			verdict:  verdictUnverifiable,
			detail:   "not installed — optional; install: https://ollama.com",
			evidence: "ollama not on PATH; nothing configured depends on it",
			endpoint: ep.URL,
		}}
	}
}

// ollamaSandboxAxis answers "can a sandbox reach the host Ollama?" WITHOUT
// creating one. With no sandbox present the honest answer is `unverifiable` +
// optional, naming the exact condition that would make it verifiable — run's
// post-create probe. Bind-address inference only ever adds remediation
// context; it never upgrades anything to ready.
func ollamaSandboxAxis(env shellEnv, ep ollamaEndpoint, p ollamaProbe, sandbox string, reachable *bool) []check {
	c := check{
		label:       "ollama in sandbox",
		requirement: requirementOptional,
		endpoint:    "host.docker.internal:" + strconv.Itoa(ep.Port),
	}
	switch {
	case reachable != nil && *reachable:
		c.verdict = verdictReady
		c.detail = "sandbox " + sandbox + " reaches the host daemon"
		c.evidence = "sandbox " + sandbox + " reached host.docker.internal:" + strconv.Itoa(ep.Port)
	case reachable != nil:
		c.verdict = verdictTodo
		c.detail = "sandbox " + sandbox + " cannot reach the host daemon"
		c.evidence = "sandbox " + sandbox + " could not reach host.docker.internal:" + strconv.Itoa(ep.Port)
		c.todo = "export OLLAMA_HOST=0.0.0.0:" + strconv.Itoa(ep.Port)
		if ep.loopbackOnly() {
			// Inference, offered as remediation context on an already-verified
			// failure. It can never create a verdict of its own.
			c.detail += " (it is bound to " + ep.Host + ", which is loopback-only)"
		}
	case sandbox == "":
		// Expected absence, not a surprising probe failure: most hosts run
		// `pi-stack doctor` before their first `pi-stack run`. A note keeps
		// this ubiquitous case from perpetually blocking "all checks pass".
		c.note = true
		c.verdict = verdictUnverifiable
		c.detail = "no sandbox exists yet — verified on the next `pi-stack run`"
		c.evidence = "no sandbox for this workspace; diagnostics never create one. Verifiable once a sandbox exists: `pi-stack run`"
	default:
		c.note = true
		c.verdict = verdictUnverifiable
		c.detail = "sandbox " + sandbox + " exists but was not probed from here"
		c.evidence = "sandbox " + sandbox + " was not probed; diagnostics never exec into a sandbox. Verifiable on the next `pi-stack run`"
	}
	return []check{c}
}

// ollamaModelAxes builds one axis per model ROLE — watcher, embed AND bridge.
// The bridge model (the local model the sandbox bridge exposes, and the
// router's local option) was previously never checked anywhere, so a
// configured-but-unpulled bridge model was invisible until a session failed.
func ollamaModelAxes(cfg *config.Config, ep ollamaEndpoint, probe func() ollamaProbe) map[Axis]axisBuilder {
	roles := []struct {
		axis    Axis
		role    string
		model   string
		purpose string
	}{
		{axisModelWatcher, "watcher", cfg.MemoryWatcherModel, "fact capture"},
		{axisModelEmbed, "embed", cfg.MemoryEmbedModel, "semantic recall"},
		{axisModelBridge, "bridge", cfg.OllamaBridgeModel, "local model in the sandbox"},
	}
	out := make(map[Axis]axisBuilder, len(roles))
	for _, r := range roles {
		r := r
		out[r.axis] = func() []check {
			m := modelReadiness(r.role, r.model, r.purpose, probe(), requirementOptional)
			c := modelCheck(m)
			c.endpoint = ep.URL
			if c.evidence != "" {
				c.evidence += " (via " + ep.URL + ")"
			}
			return []check{c}
		}
	}
	return out
}
