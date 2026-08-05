package service

// fixture_test.go carries this package's own portProbe fixture: the two
// concrete answers (which ports are open, which env vars are set) the status
// and upgrade paths read, as data.

// fakeEnv is a service-shaped host: open ports and environment variables.
type fakeEnv struct {
	ports   map[int]bool
	envVars map[string]string
}

func (f fakeEnv) Getenv(name string) string { return f.envVars[name] }
func (f fakeEnv) DialLocal(port int) bool   { return f.ports[port] }

// env returns the fixture as the narrow probe the code under test takes.
func (f fakeEnv) env() portProbe { return f }
