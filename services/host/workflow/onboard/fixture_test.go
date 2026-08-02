package onboard

import (
	"io"

	"pix/host/config"
	"pix/host/hostenv"
)

// defaultCfg is a minimal Config carrying the fields these tests read.
func defaultCfg() *config.Config {
	return &config.Config{Services: []string{"memory"}}
}

// testDeps is the Deps for tests that are not about composition: registration
// and the catalog gate both succeed, and the paired host binary resolves.
// Onboarding takes all three as parameters now, so a test that does not care
// says so once instead of faking an sbx gateway.
func testDeps() Deps {
	return Deps{
		HostBinary: func() (string, error) { return "/usr/bin/pix-host", nil },
		Register: func(*config.Config, hostenv.Env, io.Writer, []string,
			func() (string, error), map[string]config.MCPContainer) error {
			return nil
		},
		VerifyCatalog: func(hostenv.Env, []string) error { return nil },
	}
}
