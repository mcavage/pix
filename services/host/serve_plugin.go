// serve_plugin.go — the out-of-process half of `serve`: the supervision tree's
// `serve`-side face, plus the HTTP shims that back the stable listeners with a
// dispensed plugin client. The sandbox never sees any of this — it POSTs
// JSON-RPC to :11435; this process owns that listener and adapts it to the
// plugin's typed interface, so a restart or reattach is invisible to callers.
//
// The CredentialBroker seam (brokerProxyMux / servePluginBroker) is DORMANT:
// the public tree ships no built-in broker, but an external SHA-pinned binary
// ([plugins.broker]) plugs into exactly this shim.

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
// handlers below read the dispensed client from it per request.
type pluginHolder = supervise.Holder

// supervisor is the thin `serve`-side face of the supervision tree: it keeps
// serve.go's launch/shutdown vocabulary and nothing else. There is no watchdog,
// backoff, fail-counter or holder logic here — that is supervise's.
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
// an external broker's PIX_BROKER_AUTH, which no other unit may see (F2).
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

// pluginEnvAllowlist is the set of env names a plugin subprocess may inherit,
// so it never picks up a secret the parent carries (cloud creds, API keys, the
// ssh-agent socket). PIX_BROKER_AUTH is deliberately absent: the broker gets
// its bearer only via extraEnv in brokerService(), and no other unit sees it.
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
	// Shared Ollama endpoint
	"OLLAMA_HOST": true,
	// Dynamic linker paths for CGO-built external plugins that link shared libraries.
	"LD_LIBRARY_PATH":   true, // Linux
	"DYLD_LIBRARY_PATH": true, // macOS
	// Port vars the supervisor communicates to plugins
	"PIX_MEMORY_PORT": true,
	"PIX_BROKER_PORT": true,
}

// pluginEnvAllowNames is the allowlist as a slice, the shape a UnitSpec carries
// (supervise.FilterEnv does the filtering, per unit, in the child's env).
func pluginEnvAllowNames() []string {
	out := make([]string, 0, len(pluginEnvAllowlist))
	for k := range pluginEnvAllowlist {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// verifyPluginSHA is the operator-facing pre-check on an EXTERNAL plugin binary
// (F5): an empty pin is a hard refusal, a mismatch names both hashes. The check
// that actually gates exec is supervise.StageExecutable, which re-hashes the
// bytes it stages on every (re)start.
func verifyPluginSHA(spec config.PluginSpec) error {
	if spec.SHA == "" {
		return fmt.Errorf("plugin %s: refusing to launch an unpinned external plugin (no sha in config); external plugins must be sha-pinned", spec.Path)
	}
	got, err := supervise.FileSHA256(spec.Path)
	if err != nil {
		return fmt.Errorf("hash plugin binary: %w", err)
	}
	if want := strings.TrimSpace(spec.SHA); !strings.EqualFold(got, want) {
		return fmt.Errorf("plugin %s sha256 mismatch: got %s, want %s (refusing to launch)", spec.Path, got, strings.ToLower(want))
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
	// with resolves the dispensed store per CALL, so a restart or reattach is
	// invisible to the caller and a down unit is a clean JSON-RPC error.
	with := func(fn func(plugin.MemoryStore, jsonObj) (any, error)) func(jsonObj) (any, error) {
		return func(p jsonObj) (any, error) {
			s, _ := h.Get().(plugin.MemoryStore)
			if s == nil {
				return nil, errors.New("memory plugin unavailable")
			}
			return fn(s, p)
		}
	}
	return jsonrpcMux(map[string]func(jsonObj) (any, error){
		// Same application-level identity the built-in path serves, so a
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
				list = append(list, jsonObj{"id": c.ID, "content": c.Content, "frequency": c.Frequency, "project": projOrNil(c.Project)})
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

// servePluginBroker is the self-exec entry `pix-host plugin broker` would serve
// a built-in CredentialBroker from. The public tree ships none — the seam is
// dormant — so this always exits cleanly. An external broker ships its own
// main() and is exec'd directly; name is the kind it dispenses under.
func servePluginBroker(name string) {
	fmt.Fprintf(os.Stderr, "pix-host plugin %s: no built-in broker registered (set [plugins.broker] path+sha to an external plugin binary)\n", name)
	os.Exit(2)
}
