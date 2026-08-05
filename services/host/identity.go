package main

import (
	"os"
	"strconv"

	"pix/host/config"
)

// identity.go is the APPLICATION-LEVEL readiness surface of the host services.
//
// A TCP dial proves something holds a port. It does not prove the thing holding
// it is ours, that it is the version we shipped, or that it can answer a request.
// Every readiness verdict about memory (:11435) goes through this RPC instead, so
// a foreign listener — most realistically a surviving daemon from an older
// install — renders as "port held by an unidentified process", never as ready.
//
// The shape is fixed: {name, version, port, db_path, ready, degraded_reason}, and
// the launcher's probe matches on name and version.

// identityMemory is the wire name, matched EXACTLY by the launcher: change it and
// every readiness verdict for memory turns into "unidentified process" until both
// sides ship together.
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
// daemon (no embedder, so recall is keyword-only) is still ready, with the reason
// stated — reporting not-ready for a working service trains users to ignore it.
func memoryIdentity(hasEmbeddings bool) serviceIdentity {
	id := serviceIdentity{
		Name:    identityMemory,
		Version: version,
		Port:    servicePort("MEMORY_PORT", 11435),
		DBPath:  config.MemoryDBPath(),
		Ready:   true,
	}
	if !hasEmbeddings {
		id.DegradedReason = "no embedder: recall is keyword-only"
	}
	return id
}
