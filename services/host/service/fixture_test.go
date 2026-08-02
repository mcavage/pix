package service

// fixture_test.go carries this package's own minimal fakeEnv rather than
// importing the launcher's.
//
// The full one in cmd/pix fakes twelve seams for doctor's benefit; these tests
// need two — which ports answer, and what the environment says. Copying the two
// is better than either importing a test helper across a package boundary (test
// code is not API) or dragging ten irrelevant seams along so a service test can
// say "nothing is listening".

import (
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// fakeEnv is a service-shaped host: open ports and environment variables.
type fakeEnv struct {
	ports   map[int]bool
	envVars map[string]string
}

func (f fakeEnv) env() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		DialLocalFn: func(port int) bool { return f.ports[port] },
		GetenvFn:    func(name string) string { return f.envVars[name] },
	}}
}
