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
// DORMANT: the public tree ships no built-in broker, but an external broker
// binary (SHA-pinned via [plugins.broker]) plugs into exactly this shim.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/config"
	"pix/host/plugin"
	"pix/host/supervise"
)

// pluginHolder is the supervision tree's holder for one unit: the proxy
// handlers below read the dispensed client from it per request, so a restart
// (or a reattach) is transparent to :11435 callers.
type pluginHolder = supervise.Holder

// supervisor is the thin `serve`-side face of the supervision tree
// (services/host/supervise): ONE root supervisor, one child supervisor per
// unit, Suture-owned restart policy. It exists so serve.go keeps its
// launch/shutdown vocabulary; there is no hand-rolled watchdog, backoff or
// fail-counter here any more — that policy is Suture's.
type supervisor struct {
	mu   sync.Mutex
	tree *supervise.Tree
}

// unitHealth is a unit's OWN health probe, by kind. "the process is up" is not
// health: the supervisor evicts a unit that stops answering (see Budgets).
func unitHealth(kind string) supervise.HealthFunc {
	switch kind {
	case "memory":
		return func(impl any) error {
			m, _ := impl.(plugin.MemoryStore)
			if m == nil {
				return errors.New("memory plugin unavailable")
			}
			_, err := m.Health()
			return err
		}
	case "broker":
		return func(impl any) error {
			b, _ := impl.(plugin.CredentialBroker)
			if b == nil {
				return errors.New("broker plugin unavailable")
			}
			return b.Check()
		}
	default:
		return nil
	}
}

// ensure builds and starts the tree on first use.
func (s *supervisor) ensure() *supervise.Tree {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tree == nil {
		stage, state := supervisorDirs()
		s.tree = supervise.NewTree(supervise.Config{
			StageDir:  stage,
			StateDir:  state,
			Budgets:   supervise.DefaultBudgets(),
			Plugins:   plugin.PluginMap,
			Handshake: plugin.Handshake,
			EventSink: func(e supervise.Event) {
				log.Printf("supervise: %s %s %s %s", e.Unit, e.Type, e.Message, e.Err)
			},
		})
		s.tree.Start(context.Background())
	}
	return s.tree
}

// supervisorDirs resolves the supervisor-owned staging + reattach state dirs
// under the STATE dir (never the config dir), so `pix reset` cannot orphan a
// running unit from the state that identifies it.
func supervisorDirs() (stage, state string) {
	dir, err := config.StateDir()
	if err != nil {
		log.Printf("supervise: no state dir (%v): staging in a temp dir, reattach disabled", err)
		return filepath.Join(os.TempDir(), "pix-supervise-stage"), ""
	}
	return filepath.Join(dir, "supervise", "stage"), filepath.Join(dir, "supervise")
}

// launch supervises one unit and blocks until its first generation is healthy
// (or its first start attempt fails, which fails `serve` loudly at startup).
// extraEnv is a small set of KEY=VALUE vars granted to THIS unit only, on top
// of the allowlisted base env — e.g. an external broker's PIX_BROKER_AUTH,
// which no other unit may see (F2).
func (s *supervisor) launch(name, kind string, spec config.PluginSpec, selfPath string, extraEnv []string) (*pluginHolder, error) {
	// Pre-check the pin against the configured path for the operator-facing
	// error message; the supervisor then re-verifies the bytes it STAGES on
	// every (re)start, which is the check that actually gates exec.
	if spec.Path != "" {
		if err := verifyPluginSHA(spec); err != nil {
			return nil, err
		}
	}
	unit := supervise.UnitSpec{
		Name: name, Kind: kind, SelfExec: spec.Path == "", Path: spec.Path, SHA: spec.SHA,
		EnvAllow: pluginEnvAllowNames(), EnvGrant: extraEnv,
	}
	tree := s.ensure()
	tree.SetSelfPath(selfPath)
	return tree.Add(unit, unitHealth(kind))
}

// shutdown stops every supervised unit (drain, then kill, inside the pinned
// budgets). Safe to call with nothing launched.
func (s *supervisor) shutdown() {
	s.mu.Lock()
	tree := s.tree
	s.mu.Unlock()
	if tree != nil {
		tree.Stop()
	}
	goplugin.CleanupClients()
}

// status is the typed unit status the supervision tree tracks.
func (s *supervisor) status() []supervise.UnitStatus {
	s.mu.Lock()
	tree := s.tree
	s.mu.Unlock()
	if tree == nil {
		return nil
	}
	return tree.Status()
}

// pluginEnvAllowlist is the set of environment variable names that plugin
// subprocesses may inherit from the parent process. An allowlist approach
// prevents plugins from picking up sensitive secrets (cloud credentials, API
// keys, SSH agent sockets, etc.) the parent may carry. PIX_BROKER_AUTH is
// deliberately absent — the broker gets its bearer exclusively via extraEnv in
// brokerService(), and no other plugin may receive it (F2).
var pluginEnvAllowlist = map[string]bool{
	// Runtime essentials
	"PATH": true, "HOME": true, "USER": true,
	"TMPDIR": true, "TEMP": true, "TMP": true,
	// Config locations
	"PIX_CONFIG":      true,
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
	// Dynamic linker paths for CGO-built external plugins that link shared libraries.
	"LD_LIBRARY_PATH":   true, // Linux
	"DYLD_LIBRARY_PATH": true, // macOS
	// Port vars the supervisor communicates to plugins
	"PIX_MEMORY_PORT":    true,
	"PIX_KNOWLEDGE_PORT": true,
	"PIX_BROKER_PORT":    true,
}

// pluginEnv builds the environment for a plugin subprocess using an allowlist
// approach: only variables in pluginEnvAllowlist are inherited from the parent
// process, preventing plugins from picking up sensitive secrets (F2). The
// broker's PIX_BROKER_AUTH is absent from the allowlist and is passed
// exclusively via the extraEnv argument (only the broker gets it back).
func pluginEnv(extra []string) []string {
	return supervise.FilterEnv(pluginEnvAllowNames(), extra)
}

// pluginEnvAllowNames is the allowlist as a slice, the shape a UnitSpec carries.
func pluginEnvAllowNames() []string {
	out := make([]string, 0, len(pluginEnvAllowlist))
	for k := range pluginEnvAllowlist {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	f.Close()
	got, err := supervise.FileSHA256(spec.Path)
	if err != nil {
		return fmt.Errorf("hash plugin binary: %w", err)
	}
	want := strings.ToLower(strings.TrimSpace(spec.SHA))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("plugin %s sha256 mismatch: got %s, want %s (refusing to launch)", spec.Path, got, want)
	}
	return nil
}

// brokerCheck is the serve preflight for an out-of-process CredentialBroker.
func brokerCheck(h *pluginHolder) error {
	b, _ := h.Get().(plugin.CredentialBroker)
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
		s, _ := h.Get().(plugin.MemoryStore)
		if s == nil {
			return nil, errors.New("memory plugin unavailable")
		}
		return s, nil
	}
	methods := map[string]func(jsonObj) (any, error){
		// Same application-level identity the built-in path serves, so a
		// plugin-backed :11435 is identifiable by exactly the same probe.
		"identity": func(jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Health()
			if err != nil {
				return nil, err
			}
			return memoryIdentity(r.Vector).obj(), nil
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
			return jsonObj{"ok": r.OK, "vector": r.Vector, "capture": r.Capture,
				"captureReason": r.CaptureReason, "watcherModel": r.WatcherModel}, nil
		},
		"stats": func(p jsonObj) (any, error) {
			s, err := store()
			if err != nil {
				return nil, err
			}
			r, err := s.Stats(plugin.StatsReq{Profile: profileFromParams(p)})
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
					"kind": hit.Kind, "durability": hit.Durability, "project": projOrNil(hit.Project),
					"createdAt": hit.CreatedAt})
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
		"identity": func(jsonObj) (any, error) {
			if _, err := store(); err != nil {
				return nil, err
			}
			return knowledgeIdentity().obj(), nil
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
		s, _ := h.Get().(plugin.KnowledgeStore)
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
// CredentialBroker client. auth is the required bearer. This is the DORMANT
// broker seam: no built-in broker constructs it in the public tree, but an
// external broker plugin plugs into exactly this shim. FAIL CLOSED: an empty bearer
// rejects every request rather than serving /token unauthenticated (brokerService
// already refuses to start in that state; this is defense in depth).
func brokerProxyMux(h *pluginHolder, auth string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if auth == "" || r.Header.Get("Authorization") != "Bearer "+auth {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		b, _ := h.Get().(plugin.CredentialBroker)
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

// servePluginBroker is the self-exec entry `pix-host plugin broker` would
// serve a built-in CredentialBroker from. The public tree ships no built-in
// broker — the seam is permanently dormant — so this always exits cleanly. An
// external broker plugin (see examples/broker-example) ships its own main()
// and never reaches this path: supervisor.spawn execs it directly whenever
// config sets a path+sha, and only falls back to this self-exec entry when no
// path is set (i.e. never, for broker, in the public tree).
// name is the plugin map key the supervisor would dispense under (e.g. "broker").
func servePluginBroker(name string) {
	fmt.Fprintf(os.Stderr, "pix-host plugin %s: no built-in broker registered (set [plugins.broker] path+sha to an external plugin binary)\n", name)
	os.Exit(2)
}
