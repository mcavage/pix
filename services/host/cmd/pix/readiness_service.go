package main

import (
	"fmt"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/rpc"
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
// serviceIdentityResult and identityProber alias their hostenv counterparts;
// both moved so Env could leave package main, and both come back with the
// readiness extraction.
type serviceIdentityResult = hostenv.ServiceIdentity

// identityProber calls the identity method on a port. Injected so tests drive
// the classification without a live daemon.
type identityProber = hostenv.IdentityProber

// The identity probe and its JSON field extractors moved to rpc.IdentityProbe: it is a JSON-RPC call, and
// leaving it here made every package that needs an identity probe depend on
// readiness.

// serviceAxisSpec describes one identifiable host service.
type serviceAxisSpec struct {
	Axis      readiness.Axis
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
func serviceReadinessCheck(spec serviceAxisSpec) readiness.Check {
	endpoint := fmt.Sprintf("127.0.0.1:%d", spec.port)
	base := readiness.Check{Label: spec.label, Endpoint: endpoint}
	// A configured-but-down service is a VERIFIED todo, and it is optional:
	// the harness still runs without recall. `--requested` promotion (an
	// invocation that explicitly asked for the service) is what makes it
	// block, and that lives in the snapshot, not here.
	base.Requirement = readiness.RequirementOptional

	listening := true
	if spec.dialOnly != nil {
		listening = spec.dialOnly(spec.port)
	}
	if !listening {
		if !spec.enabled {
			base.Note = true
			base.Verdict = readiness.VerdictUnverifiable
			base.Detail = fmt.Sprintf(":%d down (not in configured services)", spec.port)
			base.Evidence = fmt.Sprintf("nothing listening on %s; %s is not in the configured services", endpoint, spec.label)
			return base
		}
		base.Verdict = readiness.VerdictTodo
		base.Detail = fmt.Sprintf(":%d down", spec.port)
		base.Evidence = fmt.Sprintf("nothing listening on %s", endpoint)
		base.Todo = spec.startCmd
		return base
	}

	probe := spec.probeFunc
	if probe == nil {
		// No identity prober was supplied at all (production always wires
		// env.IdentityProbe -> rpc.IdentityProbe; this only happens in tests
		// that fake a listening port without also faking identity). A dial
		// alone is not proof of identity, so this renders unverifiable —
		// never a silent real network call, and never a todo for a
		// capability this environment never claimed to be able to check.
		base.Verdict = readiness.VerdictUnverifiable
		base.Detail = fmt.Sprintf(":%d up but identity could not be confirmed from here", spec.port)
		base.Evidence = fmt.Sprintf("something is listening on %s but no identity prober is available in this environment", endpoint)
		return base
	}
	start := time.Now()
	id, err := probe(spec.port)
	base.Duration = time.Since(start)

	unidentified := func(why string) readiness.Check {
		base.Verdict = readiness.VerdictTodo
		base.Detail = fmt.Sprintf("port %d held by an unidentified process", spec.port)
		base.Evidence = fmt.Sprintf("port %d held by an unidentified process: %s", spec.port, why)
		base.Todo = spec.startCmd
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
		base.Verdict = readiness.VerdictTodo
		base.Detail = fmt.Sprintf("port %d holds %s %s, this launcher is %s", spec.port, spec.wantName, id.Version, spec.selfVer)
		base.Evidence = fmt.Sprintf("port %d held by %s version %s; expected %s", spec.port, id.Name, id.Version, spec.selfVer)
		base.Todo = "pix serve stop && pix serve"
		return base
	case !id.Ready:
		base.Verdict = readiness.VerdictTodo
		reason := id.DegradedReason
		if strings.TrimSpace(reason) == "" {
			reason = "the service reports it is not ready"
		}
		base.Detail = fmt.Sprintf(":%d up but not ready — %s", spec.port, reason)
		base.Evidence = fmt.Sprintf("%s on %s reports ready=false: %s", id.Name, endpoint, reason)
		base.Todo = spec.startCmd
		return base
	}

	base.Verdict = readiness.VerdictReady
	base.Detail = fmt.Sprintf(":%d up", spec.port)
	base.Evidence = fmt.Sprintf("%s %s answered identity on %s (db %s)", id.Name, id.Version, endpoint, id.DBPath)
	if id.DegradedReason != "" {
		base.Detail += " (" + id.DegradedReason + ")"
		base.Evidence += "; degraded: " + id.DegradedReason
	}
	return base
}

// serviceReadinessAxes builds the memory and knowledge axes. Both are lazy:
// a caller that requests neither pays for no probe at all.
func serviceReadinessAxes(env hostenv.Env, memoryEnabled, knowledgeEnabled bool, probe identityProber) map[readiness.Axis]readiness.AxisBuilder {
	dial := env.DialLocal
	return map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisServiceMemory: func() []readiness.Check {
			return []readiness.Check{serviceReadinessCheck(serviceAxisSpec{
				Axis: readiness.AxisServiceMemory, label: "memory",
				port: rpc.MemoryClient().Port, wantName: rpc.MemoryName,
				enabled: memoryEnabled, startCmd: "pix serve",
				selfVer: version, dialOnly: dial, probeFunc: probe,
			})}
		},
		readiness.AxisServiceKnowledge: func() []readiness.Check {
			return []readiness.Check{serviceReadinessCheck(serviceAxisSpec{
				Axis: readiness.AxisServiceKnowledge, label: "knowledge",
				port: rpc.KnowledgeClient().Port, wantName: rpc.KnowledgeName,
				enabled: knowledgeEnabled, startCmd: "pix serve",
				selfVer: version, dialOnly: dial, probeFunc: probe,
			})}
		},
	}
}

// The identity names the daemons report now live beside the probe that checks
// them, as rpc.MemoryName / rpc.KnowledgeName. They remain deliberately
// DUPLICATED from services/host/identity.go: those are separate binaries, and
// the launcher must state the name it EXPECTS rather than importing whatever
// the local build happens to say. A mismatch is exactly the "unidentified
// process" case the probe exists to report.
