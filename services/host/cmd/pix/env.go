// env.go — the composition root's env constructor.
//
// This is the ONE place the real OS seams, the real identity probe, and the
// real live probes are wired together. It lives at L4 because that is what L4
// is for: everything below takes a hostenv.Env as a parameter, and nothing
// below may construct one, or it would have to know which capability supplies
// each probe.
package main

import (
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/sys"
)

// defaultShellEnv returns a hostenv.Env backed by the real OS.
func defaultShellEnv() hostenv.Env {
	return hostenv.Env{
		System: sys.Real{}, HostBinary: func() (string, error) { return hostBinaryResolver() }, DirectInference: inference.LiveDirectInferenceProbe, OllamaInference: inference.LiveOllamaInferenceProbe}
}
