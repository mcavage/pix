package main

import (
	"fmt"
	"strings"
	"time"
)

// readiness_service.go proves a host service is OURS before calling it ready.
//
// The failure this exists to catch: something is listening on 11435, a dial
// succeeds, and every surface renders ✓ — while the process holding the port
// is a surviving daemon from an older install, or an unrelated program. A dial
// answers "is the port held", which is not the question. So no readiness
// verdict here derives from a dial alone: the port must answer the `identity`
// JSON-RPC method (services/host/identity.go) with OUR name at OUR version.
//
// Bounded on purpose: one attempt, one retry, ~1s each. A readiness probe that
// can loop is a readiness probe that can hang the command it is reporting for.

const (
	// serviceIdentityTimeout bounds a single identity call.
	serviceIdentityTimeout = 1 * time.Second
	// serviceIdentityRetries is the number of RETRIES after the first attempt
	// (so 2 calls total, worst case ~2s). Deliberately not a loop.
	serviceIdentityRetries = 1
)

// serviceIdentity is the launcher's view of the identity payload.
type serviceIdentityResult struct {
	Name           string
	Version        string
	Port           int
	DBPath         string
	Ready          bool
	DegradedReason string
}

// identityProber calls the identity method on a port. Injected so tests drive
// the classification without a live daemon.
type identityProber func(port int) (serviceIdentityResult, error)

// rpcIdentityProbe is the real prober: the shared JSON-RPC client, bounded,
// with exactly one retry.
func rpcIdentityProbe(port int) (serviceIdentityResult, error) {
	c := rpcClient{Port: port, Timeout: serviceIdentityTimeout}
	var lastErr error
	for attempt := 0; attempt <= serviceIdentityRetries; attempt++ {
		res, err := c.Call("identity", nil)
		if err == nil {
			return serviceIdentityResult{
				Name:           idStr(res["name"]),
				Version:        idStr(res["version"]),
				Port:           intOf(res["port"]),
				DBPath:         idStr(res["db_path"]),
				Ready:          boolOf(res["ready"]),
				DegradedReason: idStr(res["degraded_reason"]),
			}, nil
		}
		lastErr = err
	}
	return serviceIdentityResult{}, lastErr
}

func idStr(v any) string {
	s, _ := v.(string)
	return s
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// serviceAxisSpec describes one identifiable host service.
type serviceAxisSpec struct {
	axis      Axis
	label     string
	port      int
	wantName  string // the exact identity.name this port must report
	enabled   bool   // in the configured services set
	startCmd  string
	selfVer   string // the launcher's own version; a mismatch is reported
	dialOnly  func(port int) bool
	probeFunc identityProber
}

// serviceReadinessCheck renders one service axis. The decision table:
//
//	nothing listening + enabled    -> todo, `pix serve`
//	nothing listening + disabled   -> note (expected absence)
//	listening, no identity answer  -> todo, "held by an unidentified process"
//	listening, foreign name        -> todo, "held by an unidentified process"
//	listening, version mismatch    -> todo, naming both versions
//	identity says not ready        -> todo, with the daemon's own reason
//	identity ours + ready          -> ready
//
// There is no path from a successful dial to `ready`.
func serviceReadinessCheck(spec serviceAxisSpec) check {
	endpoint := fmt.Sprintf("127.0.0.1:%d", spec.port)
	base := check{label: spec.label, endpoint: endpoint}
	// A configured-but-down service is a VERIFIED todo, and it is optional:
	// the harness still runs without recall. `--requested` promotion (an
	// invocation that explicitly asked for the service) is what makes it
	// block, and that lives in the snapshot, not here.
	base.requirement = requirementOptional

	listening := true
	if spec.dialOnly != nil {
		listening = spec.dialOnly(spec.port)
	}
	if !listening {
		if !spec.enabled {
			base.note = true
			base.verdict = verdictUnverifiable
			base.detail = fmt.Sprintf(":%d down (not in configured services)", spec.port)
			base.evidence = fmt.Sprintf("nothing listening on %s; %s is not in the configured services", endpoint, spec.label)
			return base
		}
		base.verdict = verdictTodo
		base.detail = fmt.Sprintf(":%d down", spec.port)
		base.evidence = fmt.Sprintf("nothing listening on %s", endpoint)
		base.todo = spec.startCmd
		return base
	}

	probe := spec.probeFunc
	if probe == nil {
		// No identity prober was supplied at all (production always wires
		// env.identityProbe -> rpcIdentityProbe; this only happens in tests
		// that fake a listening port without also faking identity). A dial
		// alone is not proof of identity, so this renders unverifiable —
		// never a silent real network call, and never a todo for a
		// capability this environment never claimed to be able to check.
		base.verdict = verdictUnverifiable
		base.detail = fmt.Sprintf(":%d up but identity could not be confirmed from here", spec.port)
		base.evidence = fmt.Sprintf("something is listening on %s but no identity prober is available in this environment", endpoint)
		return base
	}
	start := time.Now()
	id, err := probe(spec.port)
	base.duration = time.Since(start)

	unidentified := func(why string) check {
		base.verdict = verdictTodo
		base.detail = fmt.Sprintf("port %d held by an unidentified process", spec.port)
		base.evidence = fmt.Sprintf("port %d held by an unidentified process: %s", spec.port, why)
		base.todo = spec.startCmd
		return base
	}
	switch {
	case err != nil:
		return unidentified("it did not answer the identity request")
	case id.Name == "":
		return unidentified("it answered without an identity")
	case id.Name != spec.wantName:
		return unidentified(fmt.Sprintf("it identifies as %q, not %q", id.Name, spec.wantName))
	case spec.selfVer != "" && id.Version != "" && id.Version != spec.selfVer:
		base.verdict = verdictTodo
		base.detail = fmt.Sprintf("port %d holds %s %s, this launcher is %s", spec.port, spec.wantName, id.Version, spec.selfVer)
		base.evidence = fmt.Sprintf("port %d held by %s version %s; expected %s", spec.port, id.Name, id.Version, spec.selfVer)
		base.todo = "pix serve stop && pix serve"
		return base
	case !id.Ready:
		base.verdict = verdictTodo
		reason := id.DegradedReason
		if strings.TrimSpace(reason) == "" {
			reason = "the service reports it is not ready"
		}
		base.detail = fmt.Sprintf(":%d up but not ready — %s", spec.port, reason)
		base.evidence = fmt.Sprintf("%s on %s reports ready=false: %s", id.Name, endpoint, reason)
		base.todo = spec.startCmd
		return base
	}

	base.verdict = verdictReady
	base.detail = fmt.Sprintf(":%d up", spec.port)
	base.evidence = fmt.Sprintf("%s %s answered identity on %s (db %s)", id.Name, id.Version, endpoint, id.DBPath)
	if id.DegradedReason != "" {
		base.detail += " (" + id.DegradedReason + ")"
		base.evidence += "; degraded: " + id.DegradedReason
	}
	return base
}

// serviceReadinessAxes builds the memory and knowledge axes. Both are lazy:
// a caller that requests neither pays for no probe at all.
func serviceReadinessAxes(env shellEnv, memoryEnabled, knowledgeEnabled bool, probe identityProber) map[Axis]axisBuilder {
	dial := env.DialLocal
	return map[Axis]axisBuilder{
		axisServiceMemory: func() []check {
			return []check{serviceReadinessCheck(serviceAxisSpec{
				axis: axisServiceMemory, label: "memory",
				port: memoryClient().Port, wantName: identityMemoryName,
				enabled: memoryEnabled, startCmd: "pix serve",
				selfVer: version, dialOnly: dial, probeFunc: probe,
			})}
		},
		axisServiceKnowledge: func() []check {
			return []check{serviceReadinessCheck(serviceAxisSpec{
				axis: axisServiceKnowledge, label: "knowledge",
				port: knowledgeClient().Port, wantName: identityKnowledgeName,
				enabled: knowledgeEnabled, startCmd: "pix serve",
				selfVer: version, dialOnly: dial, probeFunc: probe,
			})}
		},
	}
}

// The identity names the daemons report. Duplicated from
// services/host/identity.go on purpose: these two binaries are separate
// packages, and the launcher must state the name it EXPECTS rather than
// importing whatever the local build happens to say. A mismatch between them
// is exactly the "unidentified process" case this probe exists to report.
const (
	identityMemoryName    = "pix-memory"
	identityKnowledgeName = "pix-knowledge"
)
