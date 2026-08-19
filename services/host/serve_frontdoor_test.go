package main

// serve_frontdoor_test.go proves the front door NEVER LIES: from the instant its
// listener is bound to the instant its supervised unit is dispensed, the port
// answers an honest `identity` — right name, right version, ready=false, with a
// reason — instead of accepting a connection nothing reads.
//
// The bug this pins: `serve` bound every front door in Phase 1 and started
// http.Serve only after the child-spawn phase returned. A pack daemon whose port
// was held by a leaked orphan spent its full 15s preflight budget in that phase,
// so for 15 seconds a TCP dial to :11435 succeeded and every HTTP request hung.
// `pix serve install`, whose verification budget is 10s, reported
// "memory answered its port but not its identity check (service unreachable)"
// for a daemon that was healthy the whole time.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// frontDoorCall posts one JSON-RPC request at a handler and returns the decoded body.
func frontDoorCall(t *testing.T, h http.Handler, method string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s response %q: %v", method, rec.Body.String(), err)
	}
	return got
}

// TestStartingMuxAnswersIdentityNotReady is the core contract: the starting
// handler must be IDENTIFIABLE (right name, right version — a probe that cannot
// match those reads the port as held by a foreign process) and must report
// ready=false with a reason, so the caller waits instead of concluding the
// binary is stale or the service is wedged.
func TestStartingMuxAnswersIdentityNotReady(t *testing.T) {
	got := frontDoorCall(t, startingMux(identityMemory, 11435), "identity")
	res, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("identity returned no result object: %v", got)
	}
	if res["name"] != identityMemory {
		t.Errorf("name = %v, want %q — a probe matches on this", res["name"], identityMemory)
	}
	if res["version"] != version {
		t.Errorf("version = %v, want %q — a mismatch reads as a stale binary", res["version"], version)
	}
	if res["ready"] != false {
		t.Errorf("ready = %v, want false: the unit is not up yet", res["ready"])
	}
	if reason, _ := res["degraded_reason"].(string); reason == "" {
		t.Error("degraded_reason is empty; a not-ready answer must say why")
	}
	if p, _ := res["port"].(float64); int(p) != 11435 {
		t.Errorf("port = %v, want 11435", res["port"])
	}
}

// TestStartingMuxOtherMethodsReportStarting: a method the unit WILL support in a
// moment must not come back as "method not found". That sends a caller looking
// for a version skew that does not exist.
func TestStartingMuxOtherMethodsReportStarting(t *testing.T) {
	got := frontDoorCall(t, startingMux(identityMemory, 11435), "recall")
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("recall on a starting front door returned no error envelope: %v", got)
	}
	msg, _ := e["message"].(string)
	if strings.Contains(msg, "method not found") {
		t.Errorf("recall error = %q, want it to name the starting unit, not deny the method exists", msg)
	}
	if !strings.Contains(msg, "starting") {
		t.Errorf("recall error = %q, want it to say the service is starting", msg)
	}
}

// TestJSONRPCMuxKeepsMethodNotFoundWithoutFallback: adding the fallback seam must
// not change the steady-state surface, where an unknown method IS a client error.
func TestJSONRPCMuxKeepsMethodNotFoundWithoutFallback(t *testing.T) {
	h := jsonrpcMux(map[string]func(jsonObj) (any, error){
		"ping": func(jsonObj) (any, error) { return "pong", nil },
	})
	got := frontDoorCall(t, h, "nope")
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown method returned no error envelope: %v", got)
	}
	if msg, _ := e["message"].(string); msg != "method not found" {
		t.Errorf("message = %q, want %q", msg, "method not found")
	}
	if code, _ := e["code"].(float64); int(code) != -32601 {
		t.Errorf("code = %v, want -32601", e["code"])
	}
}

// TestSwapHandlerSwapsLive: the real mux replaces the starting one in place,
// behind the SAME already-serving http.Server, which is the whole mechanism that
// lets Phase 2 serve before Phase 3 spawns.
func TestSwapHandlerSwapsLive(t *testing.T) {
	sh := newSwapHandler(startingMux(identityMemory, 11435))
	if res, _ := frontDoorCall(t, sh, "identity")["result"].(map[string]any); res["ready"] != false {
		t.Fatalf("before the swap, ready = %v, want false", res["ready"])
	}
	sh.set(jsonrpcMux(map[string]func(jsonObj) (any, error){
		"identity": func(jsonObj) (any, error) { return jsonObj{"ready": true}, nil },
	}))
	res, _ := frontDoorCall(t, sh, "identity")["result"].(map[string]any)
	if res["ready"] != true {
		t.Fatalf("after the swap, ready = %v, want true", res["ready"])
	}
}

// TestBindFrontDoorsIsImmediatelyAnswerable: bindFrontDoors must hand back a
// front door that ALREADY has a handler. A nil handler here is the regression —
// http.Server would fall through to DefaultServeMux, and every probe would get a
// 404 from a service that is merely starting.
func TestBindFrontDoorsIsImmediatelyAnswerable(t *testing.T) {
	t.Setenv("MEMORY_BIND", "127.0.0.1")
	t.Setenv("MEMORY_PORT", "0") // ephemeral: this test binds for real
	all, err := bindFrontDoors(func(name string) bool { return name == "memory" })
	if err != nil {
		t.Fatalf("bindFrontDoors: %v", err)
	}
	defer func() {
		for _, s := range all {
			_ = s.ln.Close()
		}
	}()
	if len(all) != 1 {
		t.Fatalf("got %d front doors, want 1", len(all))
	}
	if all[0].h == nil {
		t.Fatal("front door has a nil handler right after bind: the port would be open behind nothing")
	}
	res, _ := frontDoorCall(t, all[0].h, "identity")["result"].(map[string]any)
	if res["name"] != identityMemory || res["ready"] != false {
		t.Errorf("freshly-bound front door identity = %v, want name=%q ready=false", res, identityMemory)
	}
}
