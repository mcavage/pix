package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"time"

	"pix/host/config"
)

// serviceCheck reports a host service's port state. A down service that is in
// the configured SERVICES set gets a `pix serve` TODO; one that isn't
// enabled is merely informational.
func serviceCheck(label string, port int, up bool, startCmd string, isEnabled bool) readiness.Check {
	if up {
		return readiness.Check{Label: label, Verdict: readiness.VerdictReady, Detail: fmt.Sprintf(":%d up", port)}
	}
	if isEnabled {
		return readiness.Check{Label: label, Verdict: readiness.VerdictTodo, Detail: fmt.Sprintf(":%d down", port), Todo: startCmd}
	}
	return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictUnverifiable, Detail: fmt.Sprintf(":%d down (not in configured services)", port)}
}

// memCaptureCheck asks the running memory daemon (:11435) whether automatic fact
// capture is live. It reads the daemon's own health.capture flag (which re-probes
// the watcher model), so it catches the latched-off case a plain `ollama list`
// check misses. Off => the exact `ollama pull` fix.
func memCaptureCheck() readiness.Check {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"health","params":{}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:11435", bytes.NewReader(body))
	if err != nil {
		return readiness.Check{Label: "fact capture", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "could not query daemon health"}
	}
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return readiness.Check{Label: "fact capture", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "could not query daemon health"}
	}
	defer res.Body.Close()
	var parsed struct {
		Result struct {
			Capture       bool   `json:"capture"`
			CaptureReason string `json:"captureReason"`
			WatcherModel  string `json:"watcherModel"`
		} `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&parsed) != nil {
		return readiness.Check{Label: "fact capture", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "could not read daemon health"}
	}
	m := parsed.Result.WatcherModel
	if parsed.Result.Capture {
		return readiness.Check{Label: "fact capture", Verdict: readiness.VerdictReady, Detail: fmt.Sprintf("on (watcher %s)", m)}
	}
	// Prefer the daemon's own live reason (e.g. a watcher inference timeout while
	// Ollama is wedged) over the generic "unavailable" text — that's the whole
	// point of surfacing captureReason.
	detail := fmt.Sprintf("OFF — watcher %q unavailable (recall still works)", m)
	if parsed.Result.CaptureReason != "" {
		detail = fmt.Sprintf("OFF — %s (recall still works)", parsed.Result.CaptureReason)
	}
	return readiness.Check{
		Label:   "fact capture",
		Verdict: readiness.VerdictTodo,
		Detail:  detail,
		Todo:    "ollama pull " + m,
	}
}

// memoryGroup builds the memory-service cluster: the :11435 daemon plus (when
// it is up) the live fact-capture flag read from its health endpoint.
func memoryGroup(cfg *config.Config, env hostenv.Env) readiness.Group {
	memory := readiness.Group{Title: "Memory service (recall + capture)", Axis: readiness.AxisServiceMemory}
	// Readiness comes from the APPLICATION-LEVEL identity probe, never from a
	// dial: a port held by a foreign process renders "unidentified", not ✓.
	s := readiness.Build(
		readiness.Request{Axes: []readiness.Axis{readiness.AxisServiceMemory, readiness.AxisServiceKnowledge}},
		axis.ServiceReadinessAxes(env, config.ServiceEnabled(cfg, "memory"), config.ServiceEnabled(cfg, "knowledge"), env.IdentityProbe),
	)
	memory.Checks = append(memory.Checks, s.All()...)
	memUp := false
	if c, ok := s.Checks(readiness.AxisServiceMemory); ok && c[0].Result() == readiness.VerdictReady {
		memUp = true
	}
	// Live capture status straight from the daemon's health, not just "is the
	// model in ollama": this is the flag that decides whether observe() actually
	// stores anything. A latched-off watcher (daemon booted before the model was
	// pulled) shows here even when `ollama list` now has the model.
	if memUp {
		memory.Checks = append(memory.Checks, memCaptureCheck())
	}
	return memory
}
