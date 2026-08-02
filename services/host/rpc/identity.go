package rpc

// identity.go is the `identity` method every host service answers: the
// APPLICATION-level proof a readiness axis needs before it may render ready. A
// listening port is not evidence that the thing behind it works, which is the
// whole reason this exists.
//
// It lives in rpc rather than readiness because it IS a JSON-RPC call. Leaving
// it in readiness made every package that needs an identity probe depend on
// readiness, which is a layer above most of them.

import "time"

// ServiceIdentity is a host service's answer to the `identity` method — the
// APPLICATION-level proof a readiness axis needs before rendering ready. A
// listening port is not evidence that the thing behind it works.
type ServiceIdentity struct {
	Name           string
	Version        string
	Port           int
	DBPath         string
	Ready          bool
	DegradedReason string
}

// IdentityProber calls the identity method on a port. Injected so tests can
// drive the classification without a live daemon.
type IdentityProber func(port int) (ServiceIdentity, error)

const (
	// MemoryName and KnowledgeName are the names each service reports. A
	// service answering with the wrong name means the port is held by something
	// else, which is a different failure from "nothing is listening".
	MemoryName    = "pix-memory"
	KnowledgeName = "pix-knowledge"

	identityTimeout = 900 * time.Millisecond
	identityRetries = 1
)

// rpcIdentityProbe is the real prober: the shared JSON-RPC client, bounded,
// with exactly one retry.
func IdentityProbe(port int) (ServiceIdentity, error) {
	c := Client{Port: port, Timeout: identityTimeout}
	var lastErr error
	for attempt := 0; attempt <= identityRetries; attempt++ {
		res, err := c.Call("identity", nil)
		if err == nil {
			return ServiceIdentity{
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
	return ServiceIdentity{}, lastErr
}

// Small JSON field extractors for the identity payload. Untyped map access
// is deliberate: a service on a newer schema must degrade to zero values
// rather than fail to parse.
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
