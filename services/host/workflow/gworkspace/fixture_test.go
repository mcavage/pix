package gworkspace

import (
	"pix/host/hostenv"
	"pix/host/mcp"
)

// noCreds is the Creds for tests that are not about 1Password: no `op`, no
// refs, no gog keyring, so registration builds the bare hardened invocation.
// Credentials are a parameter now, so a test that does not care says so.
func noCreds(hostenv.Env) mcp.Credentials { return mcp.Credentials{} }

func bareGog(acct string) string {
	return "gog --account " + acct +
		" --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read"
}

// opWrappedCreds is the Creds for tests whose subject IS the op-wrapped
// registration: op installed, an op-refs.env present, and GOG_KEYRING_PASSWORD
// a filled ref. These used to be read off the real filesystem by
// BuildGogRegistrar; they are a parameter now, so the test states them.
func opWrappedCreds(refs string) Creds {
	return func(hostenv.Env) mcp.Credentials {
		return mcp.Credentials{OpPath: "/usr/bin/op", OpRefsPath: refs, SeedPath: refs, GogKeyring: true}
	}
}
