// serve_plugin.go — the out-of-process half of the `serve` supervisor.
//
// The default config runs every capability BUILT-IN and IN-PROCESS (memoryMux()
// exactly as before), so nothing here executes. Only when config selects a
// non-builtin impl for a slot does `serve` launch a go-plugin subprocess
// (startup-only, never per request), keep it alive with a basic health/backoff
// watchdog, and back the stable HTTP shim (:11435 JSON-RPC) with a handler that
// proxies to the dispensed plugin client.
//
// The sandbox never sees any of this: it still POSTs JSON-RPC to :11435; the
// supervisor owns that listener and adapts it to the plugin's typed interface.
//
// The CredentialBroker seam (brokerProxyMux / servePluginBroker below) is
// DORMANT: the public tree ships no built-in broker, but an overlay broker
// (external binary or extraBrokerFactory) plugs into exactly this shim.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"pi-stack/host/config"
	"pi-stack/host/plugin"
)

// pluginHolder holds the currently-dispensed client for one slot. The watchdog
// swaps it on restart; proxy handlers read it per request (so a restart is
// transparent to in-flight-free callers).
type pluginHolder struct {
	mu     sync.RWMutex
	impl   interface{}
	client *goplugin.Client
}

func (h *pluginHolder) get() interface{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.impl
}

func (h *pluginHolder) set(impl interface{}, c *goplugin.Client) {
	h.mu.Lock()
	h.impl, h.client = impl, c
	h.mu.Unlock()
}

func (h *pluginHolder) cur() *goplugin.Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

// supervisor owns the out-of-process plugin subprocesses: a startup-only spawn,
// a simple bounded-restart watchdog, and clean shutdown via CleanupClients().
type supervisor struct {
	mu       sync.Mutex
	stopping bool
}

// launch spawns the plugin for a slot once at startup and starts its watchdog.
// extraEnv is a small set of KEY=VALUE vars granted to THIS plugin's subprocess
// only (on top of the filtered base env; see pluginEnv) — e.g. an overlay
// broker's PI_STACK_BROKER_AUTH bearer, which no other plugin may see (F2).
func (s *supervisor) launch(name, kind string, spec config.PluginSpec, selfPath string, extraEnv []string) (*pluginHolder, error) {
	h := &pluginHolder{}
	if err := s.spawn(h, name, kind, spec, selfPath, extraEnv); err != nil {
		return nil, err
	}
	go s.watch(h, name, kind, spec, selfPath, extraEnv)
	return h, nil
}

// pluginEnvAllowlist is the set of environment variable names that plugin
// subprocesses may inherit from the parent process. An allowlist approach
// prevents plugins from picking up sensitive secrets (cloud credentials, API
// keys, SSH agent sockets, etc.) the parent may carry. PI_STACK_BROKER_AUTH is
// deliberately absent — the broker gets its bearer exclusively via extraEnv in
// brokerService(), and no other plugin may receive it (F2).
var pluginEnvAllowlist = map[string]bool{
	// Runtime essentials
	"PATH": true, "HOME": true, "USER": true,
	"TMPDIR": true, "TEMP": true, "TMP": true,
	// Config locations
	"PI_STACK_CONFIG": true,
	"XDG_CONFIG_HOME": true,
	"XDG_DATA_HOME":   true,
	"XDG_STATE_HOME":  true,
	// Memory service configuration
	"MEMORY_PORT":               true,
	"MEMORY_BIND":               true,
	"MEMORY_DB":                 true,
	"MEMORY_WATCHER_MODEL":      true,
	"MEMORY_EMBED_MODEL":        true,
	"MEMORY_EMBED_TIMEOUT_MS":   true,
	"MEMORY_WATCHER_TIMEOUT_MS": true,
	"MEMORY_SYNTH_MS":           true,
	// Knowledge service configuration
	"KNOWLEDGE_PORT":    true,
	"KNOWLEDGE_BIND":    true,
	"KNOWLEDGE_DB":      true,
	"KNOWLEDGE_BUNDLES": true,
	// Shared Ollama endpoint
	"OLLAMA_HOST": true,
	// Port vars the supervisor communicates to plugins
	"PI_STACK_MEMORY_PORT":    true,
	"PI_STACK_KNOWLEDGE_PORT": true,
	"PI_STACK_BROKER_PORT":    true,
}

// pluginEnv builds the environment for a plugin subprocess using an allowlist
// approach: only variables in pluginEnvAllowlist are inherited from the parent
// process, preventing plugins from picking up sensitive secrets (F2). The
// broker's PI_STACK_BROKER_AUTH is absent from the allowlist and is passed
// exclusively via the extraEnv argument (only the broker gets it back).
func pluginEnv(extra []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(pluginEnvAllowlist)+len(extra))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if pluginEnvAllowlist[key] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// verifyPluginSHA enforces the pinned checksum of an EXTERNAL plugin binary
// before it is executed (F5). External plugins MUST be sha-pinned: an empty
// spec.SHA is a hard refusal (never launch an unpinned binary), and a configured
// spec.SHA that does not match the binary at spec.Path is likewise refused. The
// built-in self-exec path (spec.Path == "") is exempt and never reaches here.
func verifyPluginSHA(spec config.PluginSpec) error {
	if spec.SHA == "" {
		return fmt.Errorf("plugin %s: refusing to launch an unpinned external plugin (no sha in config); external plugins must be sha-pinned", spec.Path)
	}
	f, err := os.Open(spec.Path)
	if err != nil {
		return fmt.Errorf("open plugin binary: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("hash plugin binary: %w", err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(spec.SHA))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("plugin %s sha256 mismatch: got %s, want %s (refusing to launch)", spec.Path, got, want)
	}
	return nil
}

// spawn launches one go-plugin subprocess and dispenses its client. An external
// binary (spec.Path) is preferred — and its pinned SHA is enforced first;
// otherwise the host binary re-execs itself as the built-in plugin server
// (`pi-stack-host plugin <kind>`). The subprocess env is always filtered (F2).
func (s *supervisor) spawn(h *pluginHolder, name, kind string, spec config.PluginSpec, selfPath string, extraEnv []string) error {
	var cmd *exec.Cmd
	if spec.Path != "" {
		if err := verifyPluginSHA(spec); err != nil {
			return err
		}
		cmd = exec.Command(spec.Path)
	} else {
		cmd = exec.Command(selfPath, "plugin", kind)
	}
	cmd.Env = pluginEnv(extraEnv)
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: plugin.Handshake,
		Plugins:         plugin.PluginMap,
		Cmd:             cmd,
		Managed:         true,
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return err
	}
	raw, err := rpc.Dispense(kind)
	if err != nil {
		client.Kill()
		return err
	}
	h.set(raw, client)
	log.Printf("plugin %s (%s) launched", name, kind)
	return nil
}

// watch polls the subprocess (go-plugin's Client.Exited() is a bool, not a
// channel) and restarts a crashed plugin with exponential backoff, capped at 5
// total restarts; past that it logs loudly and leaves the slot degraded (a
// crashed plugin never takes down the kernel — the proxy handler then returns an
// "unavailable" error, mirroring memory's "degrade loudly" ethos). After a
// successful restart, if the plugin runs stably for one full polling interval the
// fail counter is reset so the next crash is treated as a fresh failure (L-2).
func (s *supervisor) watch(h *pluginHolder, name, kind string, spec config.PluginSpec, selfPath string, extraEnv []string) {
	fails := 0
	backoff := time.Second
	var lastSpawnAt time.Time // time of the most recent successful spawn
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}
		c := h.cur()
		if c == nil || !c.Exited() {
			// Plugin is running. If it has survived at least 30 s since the last
			// restart, reset the fail counter — the restart is stable, so the next
			// crash starts fresh. 30 s is significantly larger than the 2 s polling
			// interval to avoid trivially satisfying the condition (L-2).
			if !lastSpawnAt.IsZero() && time.Since(lastSpawnAt) >= 30*time.Second {
				if fails > 0 {
					log.Printf("plugin %s stable after restart; resetting fail counter", name)
					fails = 0
				}
				lastSpawnAt = time.Time{} // clear to avoid repeated resets
			}
			continue
		}
		// Plugin exited — increment the fail counter and clear the stable marker.
		fails++
		lastSpawnAt = time.Time{}
		if fails > 5 {
			log.Printf("plugin %s exited %d times; degrading (no more restarts)", name, fails)
			return
		}
		log.Printf("plugin %s exited; restarting (attempt %d/5) after %v", name, fails, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if err := s.spawn(h, name, kind, spec, selfPath, extraEnv); err != nil {
			log.Printf("plugin %s restart failed: %v", name, err)
		} else {
			lastSpawnAt = time.Now()
		}
	}
}

// shutdown marks the supervisor stopping (so the watchdog does not relaunch) and
// kills every managed plugin subprocess. Safe to call with no plugins launched.
func (s *supervisor) shutdown() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	goplugin.CleanupClients()
}

// brokerCheck is the serve preflight for an out-of-process CredentialBroker.
func brokerCheck(h *pluginHolder) error {
	b, _ := h.get().(plugin.CredentialBroker)
	if b == nil {
		return errors.New("broker plugin unavailable")
	}
	return b.Check()
}

// --- HTTP shims backed by a plugin client -----------------------------------

// projOrNil mirrors nullStr(): an empty project string surfaces as JSON null,
// matching the built-in memoryMux() output shape.
func projOrNil(p string) any {
	if p == "" {
		return nil
	}
	return p
}

// memoryProxyMux serves the same JSON-RPC surface as memoryMux() but delegates
// every method to the dispensed MemoryStore client. Response shapes are
// byte-identical to the built-in path so the sandbox recall extension is
// unaffected by which impl backs :11435.
func memoryProxyMux(h *pluginHolder) http.Handler {
	store := func() (plugin.MemoryStore, error) {
		s, _ := h.get().(plugin.MemoryStore)
		if s == nil {
			return nil, errors.New("memory plugin unavailable")
		}
		return s, nil
	}
	methods := map[string]func(jsonObj) (any, error){
		"health": func(jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Health()
			if err != nil {
				return nil, err
			}
			return jsonObj{"ok": r.OK, "vector": r.Vector, "capture": r.Capture, "watcherModel": r.WatcherModel}, nil
		},
		"stats": func(jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			// NOTE: builtin (in-process) stats is profile-scoped; plugin-backed stats
			// is NOT (the plugin.MemoryStore.Stats() signature is intentionally left
			// unscoped to avoid breaking the external contract). Documented limitation.
			r, err := s.Stats()
			if err != nil {
				return nil, err
			}
			return jsonObj{"active": r.Active, "durable": r.Durable, "perishable": r.Perishable,
				"facts": r.Facts, "learnings": r.Learnings, "deleted": r.Deleted}, nil
		},
		"recall": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Recall(plugin.RecallReq{
				Query: getStr(p, "query"), Limit: clampInt(p["limit"], 0, 0, 1000),
				CharBudget: clampInt(p["charBudget"], 0, 0, 1000000), Kind: getStr(p, "kind"), Project: getStr(p, "project"),
				Profile: profileFromParams(p),
			})
			if err != nil {
				return nil, err
			}
			list := []jsonObj{}
			for _, hit := range r.Hits {
				list = append(list, jsonObj{"id": hit.ID, "content": hit.Content, "score": hit.Score,
					"kind": hit.Kind, "durability": hit.Durability, "project": projOrNil(hit.Project)})
			}
			return jsonObj{"hits": list}, nil
		},
		"remember": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			in := rememberFromParams(p)
			r, err := s.Remember(plugin.RememberReq{
				Content: in.content, Kind: in.kind, Durability: in.durability, Source: in.source,
				Project: in.project, HasProject: in.hasProject, TTLDays: in.ttlDays,
				Confidence: in.confidence, Reward: in.reward, Tags: in.tags,
				Dedupe: in.dedupe, HasDedupe: in.hasDedupe, Profile: in.profile,
			})
			if err != nil {
				return nil, err
			}
			return jsonObj{"id": r.ID, "reaffirmed": r.Reaffirmed}, nil
		},
		"forget": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Forget(plugin.ForgetReq{ID: getStr(p, "id"), Profile: profileFromParams(p)})
			if err != nil {
				return nil, err
			}
			return jsonObj{"ok": r.OK}, nil
		},
		"synthesize": func(jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Synthesize(plugin.SynthesizeReq{})
			if err != nil {
				return nil, err
			}
			return jsonObj{"merged": r.Merged, "expired": r.Expired}, nil
		},
		"promotable": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Promotable(plugin.PromotableReq{MinFrequency: clampInt(p["minFrequency"], 3, 1, 1000000), Profile: profileFromParams(p)})
			if err != nil {
				return nil, err
			}
			list := []jsonObj{}
			for _, c := range r.Candidates {
				list = append(list, jsonObj{"id": c.ID, "content": c.Content, "frequency": c.Frequency, "project": projOrNil(c.Project)})
			}
			return jsonObj{"candidates": list}, nil
		},
		"observe": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			project, hasProj := projectFromParams(p)
			r, err := s.Observe(plugin.ObserveReq{User: getStr(p, "user"), Project: project, HasProject: hasProj, Profile: profileFromParams(p)})
			if err != nil {
				return nil, err
			}
			out := jsonObj{"accepted": r.Accepted}
			if r.Reason != "" {
				out["reason"] = r.Reason
			}
			return out, nil
		},
	}
	return jsonrpcMux(methods)
}

// --- knowledge shims --------------------------------------------------------

// knowledgeMethods is the JSON-RPC method table for the knowledge service,
// shared by the in-process (knowledgeMux) and plugin (knowledgeProxyMux) paths
// so both surfaces are byte-identical. It exposes query / reindex / health and
// resolves the backing KnowledgeStore per call (a restart is transparent).
func knowledgeMethods(store func() (plugin.KnowledgeStore, error)) map[string]func(jsonObj) (any, error) {
	return map[string]func(jsonObj) (any, error){
		"query": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			// bundles is a SET filter. Prefer the `bundles` array; tolerate a
			// single legacy `bundle` string by wrapping it into a 1-elem set.
			bundles := strSliceParam(p, "bundles")
			if len(bundles) == 0 {
				if single := getStr(p, "bundle"); single != "" {
					bundles = []string{single}
				}
			}
			r, err := s.Query(plugin.QueryArgs{
				Query: getStr(p, "query"), Bundles: bundles, Limit: clampInt(p["limit"], 0, 0, 1000),
			})
			if err != nil {
				return nil, err
			}
			list := []jsonObj{}
			for _, c := range r.Concepts {
				list = append(list, jsonObj{
					"id": c.ID, "type": c.Type, "title": c.Title, "description": c.Description,
					"path": c.Path, "snippet": c.Snippet, "score": c.Score,
					"citations": strSliceOrEmpty(c.Citations), "bundle": c.Bundle,
				})
			}
			return jsonObj{"concepts": list}, nil
		},
		"reindex": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Reindex(plugin.ReindexArgs{BundlePaths: strSliceParam(p, "bundle_paths")})
			if err != nil {
				return nil, err
			}
			return jsonObj{"indexed": r.Indexed, "bundles": strSliceOrEmpty(r.Bundles)}, nil
		},
		"health": func(jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Health()
			if err != nil {
				return nil, err
			}
			return jsonObj{"ok": r.OK, "vector": r.Vector, "bundles": strSliceOrEmpty(r.Bundles), "concepts": r.Concepts}, nil
		},
	}
}

// knowledgeMux serves the knowledge JSON-RPC surface directly over the
// in-process built-in store (the fast path — no subprocess), mirroring memoryMux()
// for memory. The store is wrapped in the same adapter the plugin path dispenses.
func knowledgeMux(store *knowledgeStore) http.Handler {
	adapter := newKnowledgeStoreAdapter(store)
	return jsonrpcMux(knowledgeMethods(func() (plugin.KnowledgeStore, error) { return adapter, nil }))
}

// knowledgeProxyMux serves the same surface but delegates to the dispensed
// KnowledgeStore plugin client (mirrors memoryProxyMux). Shapes are identical to
// the in-process path so the sandbox is unaffected by which impl backs :11436.
func knowledgeProxyMux(h *pluginHolder) http.Handler {
	return jsonrpcMux(knowledgeMethods(func() (plugin.KnowledgeStore, error) {
		s, _ := h.get().(plugin.KnowledgeStore)
		if s == nil {
			return nil, errors.New("knowledge plugin unavailable")
		}
		return s, nil
	}))
}

// strSliceOrEmpty normalizes a nil string slice to a non-nil empty one so the
// JSON encodes as [] rather than null (matching the built-in output shape).
func strSliceOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// strSliceParam extracts a []string from a JSON-RPC params array of strings.
func strSliceParam(p jsonObj, key string) []string {
	var out []string
	if arr, ok := p[key].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// jsonrpcMux wraps a method table in the same JSON-RPC 2.0 envelope handling
// memoryMux() uses (single + batch, parse-error, method-not-found).
func jsonrpcMux(methods map[string]func(jsonObj) (any, error)) http.Handler {
	handleOne := func(msg jsonObj) jsonObj {
		id := msg["id"]
		method, _ := msg["method"].(string)
		fn := methods[method]
		if fn == nil {
			return jsonObj{"jsonrpc": "2.0", "id": id, "error": jsonObj{"code": -32601, "message": "method not found"}}
		}
		params, _ := msg["params"].(map[string]any)
		if params == nil {
			params = jsonObj{}
		}
		res, err := fn(params)
		if err != nil {
			return jsonObj{"jsonrpc": "2.0", "id": id, "error": jsonObj{"code": -32603, "message": err.Error()}}
		}
		return jsonObj{"jsonrpc": "2.0", "id": id, "result": res}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, jsonObj{"error": "POST JSON-RPC only"})
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			writeJSON(w, http.StatusOK, jsonObj{"jsonrpc": "2.0", "id": nil, "error": jsonObj{"code": -32700, "message": "parse error"}})
			return
		}
		switch v := parsed.(type) {
		case []any:
			out := []jsonObj{}
			for _, mm := range v {
				if m, ok := mm.(map[string]any); ok {
					out = append(out, handleOne(m))
				}
			}
			writeJSON(w, http.StatusOK, out)
		case map[string]any:
			writeJSON(w, http.StatusOK, handleOne(v))
		}
	})
	return mux
}

// brokerToken is the JSON shape a broker /token shim returns (a short-lived
// bearer). It moved here from the deleted host token service; the generic broker
// proxy (brokerProxyMux) is its only user.
type brokerToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// brokerProxyMux serves a /token surface that mints via the dispensed
// CredentialBroker client. auth is the required bearer (empty disables the
// check). This is the DORMANT broker seam: no built-in broker constructs it in
// the public tree, but an overlay broker plugs into exactly this shim.
func brokerProxyMux(h *pluginHolder, auth string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if auth != "" && r.Header.Get("Authorization") != "Bearer "+auth {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		b, _ := h.get().(plugin.CredentialBroker)
		if b == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token_error", "message": "broker plugin unavailable"})
			return
		}
		t, err := b.Mint("", nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token_error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, brokerToken{AccessToken: t.AccessToken, ExpiresIn: t.ExpiresIn, TokenType: t.TokenType})
	})
	return mux
}

// servePluginBroker runs a built-in CredentialBroker as a go-plugin self-exec
// entry (`pi-stack-host plugin broker`). The public tree registers no built-in
// broker — the seam is overlay-only and dormant — so this exits cleanly unless
// an overlay wired one via extraBrokerFactory. An EXTERNAL overlay broker binary
// (see examples/broker-example) ships its own main() and does not use this path.
// name is the plugin map key the supervisor dispenses under (e.g. "broker").
func servePluginBroker(name string) {
	if extraBrokerFactory == nil {
		fmt.Fprintln(os.Stderr, "pi-stack-host plugin broker: no built-in broker registered (overlay-only seam)")
		os.Exit(2)
	}
	plugin.Serve(map[string]goplugin.Plugin{name: &plugin.BrokerPlugin{Impl: extraBrokerFactory()}})
}
