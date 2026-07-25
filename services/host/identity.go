package main

import (
	"os"
	"strconv"

	"pix/host/config"
)

// identity.go is the APPLICATION-LEVEL readiness surface of the host services.
//
// A TCP dial proves that something holds a port. It does not prove that the
// thing holding it is ours, that it is the version we shipped, or that it can
// answer a request. Every readiness verdict about memory (:11435) or knowledge
// (:11436) goes through this RPC instead, so a foreign listener — most
// realistically a surviving daemon from an older install — renders as
// "port held by an unidentified process", never as ready.
//
// The shape is fixed: {name, version, port, db_path, ready, degraded_reason}.
// The launcher's probe (readiness_service.go) matches on name and version.

const (
	// identityMemory / identityKnowledge are the wire names. They are matched
	// EXACTLY by the launcher, so they are part of the compatibility surface:
	// change one and every readiness verdict for that service turns into
	// "unidentified process" until both sides ship together.
	identityMemory    = "pix-memory"
	identityKnowledge = "pix-knowledge"
)

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
// knobs serve uses, so the reported port is the real one rather than a
// hard-coded default the operator may have overridden.
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
// daemon (no embedder, so recall is keyword-only) is still ready, with the
// reason stated — a readiness probe that reported not-ready for a working
// service would train the user to ignore it.
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

// knowledgeIdentity answers "who holds :11436".
func knowledgeIdentity() serviceIdentity {
	return serviceIdentity{
		Name:    identityKnowledge,
		Version: version,
		Port:    servicePort("KNOWLEDGE_PORT", 11436),
		DBPath:  config.KnowledgeDBPath(),
		Ready:   true,
	}
}
