package pack

import (
	"io"
	"testing"
)

// TestMain neutralizes the ONE side effect in this package that reaches outside
// the test process: propagateConfig's real implementation runs `launchctl
// kickstart -k` against the developer's own pix LaunchAgent (via
// service.DefaultReloader). Every test here that adopts a pack went through it,
// so a full run restarted the live daemon on the machine running the tests,
// serially, blocking on each — ~507s of a ~578s `go test ./...` on a laptop
// with the agent loaded, and ~0s in CI where there is no agent to kick. That
// gap is why the fast gate's documented 32.8s go-test baseline stopped
// describing any real machine.
//
// Neutralized here rather than per-test on purpose: the failure mode is a test
// FORGETTING to stub it, and a package-wide default cannot be forgotten. A test
// that wants to assert the propagation happened sets the var itself.
func TestMain(m *testing.M) {
	propagateConfig = func(io.Writer) {}
	m.Run()
}
