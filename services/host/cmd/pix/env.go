// env.go — the composition root's env constructor: the ONE place the real OS
// seams and live probes are wired together. Everything below takes a
// hostenv.Env as a parameter and may not construct one, or it would have to
// know which capability supplies each probe.
package main

import (
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/sys"
)

// defaultShellEnv returns a hostenv.Env backed by the real OS.
func defaultShellEnv() hostenv.Env {
	return hostenv.Env{
		System: sys.Real{}, DirectInference: inference.LiveDirectInferenceProbe, OllamaInference: inference.LiveOllamaInferenceProbe}
}
