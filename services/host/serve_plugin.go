// serve_plugin.go — the out-of-process half of the `serve` supervisor.
//
// The default config runs every capability BUILT-IN and IN-PROCESS (memoryMux()
// / gwsTokenMux() exactly as before), so nothing here executes. Only when config
// selects a non-builtin impl for a slot does `serve` launch a go-plugin
// subprocess (startup-only, never per request), keep it alive with a basic
// health/backoff watchdog, and back the stable HTTP shim (:11435 JSON-RPC,
// :11441 /token) with a handler that proxies to the dispensed plugin client.
//
// The sandbox never sees any of this: it still POSTs JSON-RPC to :11435 and GETs
// :11441/token; the supervisor owns those listeners and adapts them to the
// plugin's typed interface.

package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os/exec"
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
func (s *supervisor) launch(name, kind string, spec config.PluginSpec, selfPath string) (*pluginHolder, error) {
	h := &pluginHolder{}
	if err := s.spawn(h, name, kind, spec, selfPath); err != nil {
		return nil, err
	}
	go s.watch(h, name, kind, spec, selfPath)
	return h, nil
}

// spawn launches one go-plugin subprocess and dispenses its client. An external
// binary (spec.Path) is preferred; otherwise the host binary re-execs itself as
// the built-in plugin server (`pi-stack-host plugin <kind>`).
func (s *supervisor) spawn(h *pluginHolder, name, kind string, spec config.PluginSpec, selfPath string) error {
	var cmd *exec.Cmd
	if spec.Path != "" {
		cmd = exec.Command(spec.Path)
	} else {
		cmd = exec.Command(selfPath, "plugin", kind)
	}
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
// "unavailable" error, mirroring memory's "degrade loudly" ethos).
func (s *supervisor) watch(h *pluginHolder, name, kind string, spec config.PluginSpec, selfPath string) {
	fails := 0
	backoff := time.Second
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
			continue
		}
		fails++
		if fails > 5 {
			log.Printf("plugin %s exited %d times; degrading (no more restarts)", name, fails)
			return
		}
		log.Printf("plugin %s exited; restarting (attempt %d/5) after %v", name, fails, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if err := s.spawn(h, name, kind, spec, selfPath); err != nil {
			log.Printf("plugin %s restart failed: %v", name, err)
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
				Dedupe: in.dedupe, HasDedupe: in.hasDedupe,
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
			r, err := s.Forget(plugin.ForgetReq{ID: getStr(p, "id")})
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
			r, err := s.Promotable(plugin.PromotableReq{MinFrequency: clampInt(p["minFrequency"], 3, 1, 1000000)})
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
			r, err := s.Observe(plugin.ObserveReq{User: getStr(p, "user"), Project: project, HasProject: hasProj})
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

// gwsBrokerProxyMux serves the same /token surface as gwsTokenMux() but mints via
// the dispensed CredentialBroker client. auth is the required bearer (the shared
// broker token); it mirrors gwsTokenMux()'s Authorization check exactly.
func gwsBrokerProxyMux(h *pluginHolder, auth string) http.Handler {
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
		writeJSON(w, http.StatusOK, gwsBearer{AccessToken: t.AccessToken, ExpiresIn: t.ExpiresIn, TokenType: t.TokenType})
	})
	return mux
}
