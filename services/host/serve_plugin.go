// serve_plugin.go — the out-of-process half of `serve`: the supervision tree's
// `serve`-side face, plus the HTTP shims that back the stable listeners with a
// dispensed plugin client. The sandbox never sees any of this: it POSTs JSON-RPC
// to :11435, this process owns that listener and adapts it to the plugin's typed
// interface, so a restart or reattach is invisible to callers.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/config"
	"pix/host/plugin"
	"pix/host/supervise"
)

// pluginHolder is the supervision tree's holder for one unit: the proxy
// handlers below read the dispensed client from it per request.
type pluginHolder = supervise.Holder

// supervisor is the thin `serve`-side face of the supervision tree: serve.go's
// launch/shutdown vocabulary and nothing else. Watchdog, backoff, fail counters
// and holder logic all belong to supervise.
type supervisor struct {
	mu   sync.Mutex
	tree *supervise.Tree
}

// unitHealth is a unit's OWN health probe, by kind: "the process is up" is not
// health, and the supervisor evicts a unit that stops answering.
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
	default:
		return nil
	}
}

// ensure builds and starts the tree on first use. selfPath is this binary, the
// executable a self-exec unit is launched from.
func (s *supervisor) ensure(selfPath string) *supervise.Tree {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tree == nil {
		stage, state := supervisorDirs()
		s.tree = supervise.NewTree(supervise.Config{
			SelfPath:  selfPath,
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

// supervisorDirs resolves the staging + reattach state dirs under the STATE dir
// (never the config dir), so `pix reset` cannot orphan a running unit from the
// state that identifies it.
func supervisorDirs() (stage, state string) {
	dir, err := config.StateDir()
	if err != nil {
		log.Printf("supervise: no state dir (%v): staging in a temp dir, reattach disabled", err)
		return filepath.Join(os.TempDir(), "pix-supervise-stage"), ""
	}
	return filepath.Join(dir, "supervise", "stage"), filepath.Join(dir, "supervise")
}

// launch supervises one unit and blocks until its first generation is healthy
// (or that attempt fails, which fails `serve` loudly at startup). extraEnv is
// KEY=VALUE granted to THIS unit only on top of the allowlisted base env — e.g.
// an external plugin's own bearer, which no other unit may see.
//
// There is deliberately no pin pre-check here: supervise owns that gate at both
// ends (UnitSpec.Validate refuses an unpinned or relative external path before
// anything spawns, and StageExecutable re-hashes the bytes it stages on every
// start — the check that actually precedes exec).
func (s *supervisor) launch(name, kind string, spec config.PluginSpec, selfPath string, extraEnv []string) (*pluginHolder, error) {
	unit := supervise.UnitSpec{
		Name: name, Kind: kind, SelfExec: spec.Path == "", Path: spec.Path, SHA: spec.SHA,
		EnvAllow: pluginEnvAllow, EnvGrant: extraEnv,
	}
	return s.ensure(selfPath).Add(unit, unitHealth(kind))
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

// pluginEnvAllow is the env policy every supervised unit inherits: the names a
// plugin subprocess may see and nothing else, so it never picks up a secret the
// parent carries (cloud creds, API keys, the ssh-agent socket).
// supervise.FilterEnv applies it per unit. A per-unit secret is deliberately
// absent: a unit that needs one gets it only through launch()'s extraEnv
// (EnvGrant), which no sibling unit sees.
var pluginEnvAllow = []string{
	// Runtime essentials
	"HOME", "PATH", "TEMP", "TMP", "TMPDIR", "USER",
	// Config locations
	"PIX_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	// Memory service configuration
	"MEMORY_BIND", "MEMORY_DB", "MEMORY_EMBED_MODEL", "MEMORY_EMBED_TIMEOUT_MS",
	"MEMORY_PORT", "MEMORY_SYNTH_MS", "MEMORY_WATCHER_MODEL", "MEMORY_WATCHER_TIMEOUT_MS",
	// Shared Ollama endpoint
	"OLLAMA_HOST",
	// Dynamic linker paths for CGO-built external plugins that link shared
	// libraries (Linux, then macOS).
	"LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH",
	// Ports the supervisor communicates to plugins
	"PIX_MEMORY_PORT",
}

// --- HTTP shims backed by a plugin client -----------------------------------

// projOrNil mirrors nullStr(): an empty project string surfaces as JSON null.
func projOrNil(p string) any {
	if p == "" {
		return nil
	}
	return p
}

// memoryProxyMux backs :11435 with the SUPERVISED memory unit: it resolves the
// dispensed client per call, so a restart or reattach is invisible to the
// sandbox and a down unit is a clean JSON-RPC error rather than a dead socket.
func memoryProxyMux(h *pluginHolder) http.Handler {
	return memoryStoreMux(func() (plugin.MemoryStore, error) {
		s, _ := h.Get().(plugin.MemoryStore)
		if s == nil {
			return nil, errors.New("memory plugin unavailable")
		}
		return s, nil
	})
}

// memoryStoreMux is THE :11435 JSON-RPC surface, expressed once over the typed
// MemoryStore interface. `get` resolves the store per call — a holder-backed
// plugin client for the supervised unit, or the in-process adapter for the bare
// `pix-host memory` daemon — so the two entry points cannot answer differently.
// Response shapes are the sandbox recall extension's contract, and every one of
// them is built here and nowhere else.
func memoryStoreMux(get func() (plugin.MemoryStore, error)) http.Handler {
	with := func(fn func(plugin.MemoryStore, jsonObj) (any, error)) func(jsonObj) (any, error) {
		return func(p jsonObj) (any, error) {
			s, err := get()
			if err != nil {
				return nil, err
			}
			return fn(s, p)
		}
	}
	return jsonrpcMux(map[string]func(jsonObj) (any, error){
		// The same application-level identity the built-in path serves, so a
		// plugin-backed :11435 answers exactly the same probe.
		"identity": with(func(s plugin.MemoryStore, _ jsonObj) (any, error) {
			r, err := s.Health()
			if err != nil {
				return nil, err
			}
			return memoryIdentity(r.Vector).obj(), nil
		}),
		"health": with(func(s plugin.MemoryStore, _ jsonObj) (any, error) {
			r, err := s.Health()
			if err != nil {
				return nil, err
			}
			return jsonObj{"ok": r.OK, "vector": r.Vector, "capture": r.Capture,
				"captureReason": r.CaptureReason, "watcherModel": r.WatcherModel}, nil
		}),
		"stats": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
			r, err := s.Stats(profileFromParams(p))
			if err != nil {
				return nil, err
			}
			return jsonObj{"active": r.Active, "durable": r.Durable, "perishable": r.Perishable,
				"facts": r.Facts, "learnings": r.Learnings, "deleted": r.Deleted}, nil
		}),
		"recall": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
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
		}),
		"remember": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
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
		}),
		"forget": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
			r, err := s.Forget(plugin.ForgetReq{ID: getStr(p, "id"), Profile: profileFromParams(p)})
			if err != nil {
				return nil, err
			}
			return jsonObj{"ok": r.OK}, nil
		}),
		"synthesize": with(func(s plugin.MemoryStore, _ jsonObj) (any, error) {
			r, err := s.Synthesize(plugin.SynthesizeReq{})
			if err != nil {
				return nil, err
			}
			return jsonObj{"merged": r.Merged, "expired": r.Expired}, nil
		}),
		"promotable": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
			r, err := s.Promotable(plugin.PromotableReq{MinFrequency: clampInt(p["minFrequency"], 3, 1, 1000000), Profile: profileFromParams(p)})
			if err != nil {
				return nil, err
			}
			list := []jsonObj{}
			for _, c := range r.Candidates {
				list = append(list, jsonObj{"id": c.ID, "content": c.Content, "frequency": c.Frequency,
					"project": projOrNil(c.Project), "createdAt": c.CreatedAt})
			}
			return jsonObj{"candidates": list}, nil
		}),
		"observe": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
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
		}),
	})
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
