package knowledge

import (
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// Small helpers copied from cmd/pix for this package's tests. Copying three
// lines beats exporting a production symbol so a test elsewhere can see it.

// containsStr reports whether list contains s.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func countStr(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

func defaultCfg() *config.Config {
	c := &config.Config{}
	// apply defaults via Load's helper by round-tripping through the exported
	// fields the doctor reads.
	c.Services = []string{"memory"}
	c.MemoryWatcherModel = "gemma4"
	c.MemoryEmbedModel = "nomic-embed-text"
	return c
}

// fakeEnv is a knowledge-shaped host: which ports answer, and nothing else.
// The launcher's version fakes a dozen seams for doctor; these tests need one.
// Copying it beats importing test code across a package boundary.
type fakeEnv struct{ ports map[int]bool }

func (f fakeEnv) env() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		DialLocalFn: func(port int) bool { return f.ports[port] },
	}}
}
