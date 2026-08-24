//go:build windows

package uat

// StopWorker has no supported implementation on Windows: the UAT
// self-development runner (docs/design/self-development-uat.md) is a
// macOS/Linux host feature, same as ListenSocket/DialSocket (socket_windows.go).
// This stub exists only so lifecycle.go — which calls StopWorker
// unconditionally and carries no build tag itself — keeps compiling here; a
// windows uat package has no registration to have ever recorded a worker for.
func StopWorker(runnerState, sessionID string) error {
	return nil
}
