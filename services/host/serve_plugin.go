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
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/config"
	"pix/host/plugin"
	"pix/host/supervise"
	"pix/host/unitreport"
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
	// stopPublish ends the status-snapshot publisher; publishErrOnce keeps a
	// broken state dir from filling serve.log with the same line every 5s.
	stopPublish    chan struct{}
	publishErrOnce sync.Once
	// packUnits is reconcilePackUnits' desired-state ledger (pack_units.go).
	packUnits map[string]supervise.UnitSpec
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
				log.Printf("supervise: %s %s %s %s", e.Unit, e.Type, e.Message, unitreport.ScrubError(e.Err))
				// Every state change is published immediately; the ticker below
				// only covers what changes WITHOUT an event (probe latency).
				s.publish()
			},
		})
		s.tree.Start(context.Background())
		s.stopPublish = make(chan struct{})
		go s.publishLoop(s.stopPublish)
	}
	return s.tree
}

// unitsReportInterval is the ceiling on how stale `pix serve status --json` can
// be about a probe latency. Events cover every state change; this covers the
// numbers that move while nothing changes state.
const unitsReportInterval = 5 * time.Second

// publish writes the supervision-tree snapshot readers (serve status, doctor)
// consume. Publishing is best-effort and NEVER fails a running daemon: a state
// dir that cannot be written is a missing snapshot, which those readers render
// as "unknown", not as healthy.
func (s *supervisor) publish() {
	s.mu.Lock()
	tree := s.tree
	s.mu.Unlock()
	if tree == nil {
		return
	}
	if err := unitreport.WriteReport(config.ServeUnitsPath(), tree.Report()); err != nil {
		s.publishErrOnce.Do(func() {
			log.Printf("supervise: cannot publish unit status to %s: %v", config.ServeUnitsPath(), err)
		})
	}
}

// publishLoop refreshes the snapshot on a fixed interval until shutdown.
func (s *supervisor) publishLoop(stop <-chan struct{}) {
	t := time.NewTicker(unitsReportInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.publish()
		}
	}
}

// supervisorDirs resolves the staging + reattach state dirs under the STATE dir
// (never the config dir), so moving the config dir aside can never orphan a
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
	tree, stop := s.tree, s.stopPublish
	s.stopPublish = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if tree != nil {
		tree.Stop()
	}
	// The snapshot describes a LIVE tree; leaving it behind would let `serve
	// status` report units of a daemon that is gone. Removal is best-effort:
	// readers cross-check it against the pidfile anyway.
	if tree != nil {
		_ = os.Remove(config.ServeUnitsPath())
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
	"MEMORY_PORT", "MEMORY_WATCHER_MODEL", "MEMORY_WATCHER_TIMEOUT_MS",
	"MEMORY_CAPTURE_MODE",
	// Shared Ollama endpoint
	"OLLAMA_HOST",
	// Dynamic linker paths for CGO-built external plugins that link shared
	// libraries (Linux, then macOS).
	"LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH",
	// Ports the supervisor communicates to plugins
	"PIX_MEMORY_PORT",
}

// --- HTTP shims backed by a plugin client -----------------------------------

// projOrNil: an empty project string surfaces as JSON null. (nullStr(), the
// memStore-side twin this used to mirror, was deleted along with promotable(),
// its only caller; projOrNil has its own callers here and stayed.)
func projOrNil(p string) any {
	if p == "" {
		return nil
	}
	return p
}

// memoryUse is the single seam every REAL memory RPC executes through: for the
// supervised unit it is Holder.Use (so a shutdown's drain WAITS for an
// in-flight call, up to the drain budget, instead of killing the child out
// from under it — see supervise.Holder.Drain); for the bare `pix-host memory`
// daemon (no holder, nothing supervising it, nothing to drain against) it is a
// direct call. Both wrap fn so a plugin-unavailable holder surfaces as the
// same JSON-RPC error either path would give.
type memoryUse func(fn func(plugin.MemoryStore) error) error

// memoryProxyMux backs :11435 with the SUPERVISED memory unit: it resolves the
// dispensed client per call THROUGH Holder.Use, so a restart or reattach is
// invisible to the sandbox, a down unit is a clean JSON-RPC error rather than
// a dead socket, and — the property this exists for — the call is counted as
// in-flight for as long as it actually runs, not just for the instant it took
// to read the holder.
func memoryProxyMux(h *pluginHolder) http.Handler {
	return memoryStoreMux(func(fn func(plugin.MemoryStore) error) error {
		return h.Use(func(impl any) error {
			s, _ := impl.(plugin.MemoryStore)
			if s == nil {
				return errors.New("memory plugin unavailable")
			}
			return fn(s)
		})
	})
}

// memoryStoreMux is THE :11435 JSON-RPC surface, expressed once over the typed
// MemoryStore interface. `use` resolves and calls the store per request — a
// Holder.Use-wrapped plugin client for the supervised unit, or a direct call
// against the in-process adapter for the bare `pix-host memory` daemon — so
// the two entry points cannot answer differently, and every method below runs
// for its ENTIRE duration inside whichever accounting `use` provides. Response
// shapes are the sandbox recall extension's contract, and every one of them is
// built here and nowhere else.
func memoryStoreMux(use memoryUse) http.Handler {
	with := func(fn func(plugin.MemoryStore, jsonObj) (any, error)) func(jsonObj) (any, error) {
		return func(p jsonObj) (any, error) {
			var out any
			err := use(func(s plugin.MemoryStore) error {
				var ferr error
				out, ferr = fn(s, p)
				return ferr
			})
			return out, err
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
					"createdAt": hit.CreatedAt, "source": hit.Source})
			}
			return jsonObj{"hits": list}, nil
		}),
		"remember": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {
			in := rememberFromParams(p)
			r, err := s.Remember(plugin.RememberReq{
				Content: in.content, Kind: in.kind, Source: in.source,
				Project: in.project, HasProject: in.hasProject,
				Confidence: in.confidence, Tags: in.tags,
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
			return jsonObj{"merged": r.Merged}, nil
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
