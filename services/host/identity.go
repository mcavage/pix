package main

import (
	"os"
	"strconv"

	"pix/host/config"
)

// identity.go is the APPLICATION-LEVEL readiness surface of the host services.
// A TCP dial proves something holds a port, not that it is ours, our version,
// or answerable — every readiness verdict about memory (:11435) goes through
// this RPC instead, so a foreign listener (e.g. a surviving older daemon)
// renders as "port held by an unidentified process", never as ready. Shape:
// {name, version, port, db_path, ready, degraded_reason}; the launcher's probe
// matches on name and version.

// identityMemory is the wire name, matched EXACTLY by the launcher: change it
// and every verdict for memory becomes "unidentified process" until both ship together.
const identityMemory = "pix-memory"

// serviceIdentity is the payload of the `identity` JSON-RPC method.
type serviceIdentity struct {
	Name           string
	Version        string
	Port           int
	DBPath         string
	Ready          bool
	DegradedReason string
}

// obj renders the identity as the JSON-RPC result object.
func (s serviceIdentity) obj() jsonObj {
	return jsonObj{
		"name":            s.Name,
		"version":         s.Version,
		"port":            s.Port,
		"db_path":         s.DBPath,
		"ready":           s.Ready,
		"degraded_reason": s.DegradedReason,
	}
}

// servicePort reads the port a service is actually bound to from the same env
// knobs serve uses, so the reported port is the real one and not a default the
// operator may have overridden.
func servicePort(envVar string, def int) int {
	if v := os.Getenv(envVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// memoryIdentity answers "who holds :11435". ready is the service's own
// judgement: the store is open and can serve recall. A degraded but serving
// daemon (Ollama's embed model unreachable, so recall is keyword-only) is
// still ready, with the reason stated — reporting not-ready for a working
// service trains users to ignore it. vector is the CURRENT tri-state embed
// health (memembed.go's embedHealthState, read live by the caller's Health()
// call): nil means "never exercised yet", not a value probed or cached at
// startup, and — critically — NOT a confirmed failure, so it must not report
// a degraded reason either. Only a confirmed false (a real /api/embed
// failure) does.
func memoryIdentity(vector *bool) serviceIdentity {
	id := serviceIdentity{
		Name:    identityMemory,
		Version: version,
		Port:    servicePort("MEMORY_PORT", 11435),
		DBPath:  config.MemoryDBPath(),
		Ready:   true,
	}
	if vector != nil && !*vector {
		id.DegradedReason = "embed model unreachable: recall is keyword-only"
	}
	return id
}
