// Package hostenv is the launcher's dependency bundle: every OS seam
// (sys.System) plus the live probes that are domain-specific and therefore do
// not belong in sys.
//
// It is TRANSITIONAL, and should shrink rather than grow. The end state is that
// a function takes what it uses — `sys.Exec`, or a domain's own prober — and
// this bundle disappears. Each probe below is annotated with the package it
// leaves for. Do not add a field here to avoid threading a dependency.
package hostenv

import (
	"time"

	"pix/host/sys"
)

// Env is the bundle. The embedded System is never nil in production (sys.Real
// has no nullable state), which is why nothing here is guarded.
type Env struct {
	sys.System

	// Quiet suppresses progress chatter on machine-readable paths.
	Quiet bool

	// DirectInference makes one bounded, model-specific provider call. The key
	// is held only in memory and must never appear in a returned error.
	// → inference package.
	DirectInference func(provider, model, key string) error

	// OllamaInference makes one bounded, model-specific request against the
	// RESOLVED Ollama endpoint (never a spelled-out address). numCtx is the
	// rung's declared context budget, 0 for cloud. → inference package.
	OllamaInference func(endpoint, model string, numCtx int, timeout time.Duration) error
}
