package mcp

import "pix/host/hostenv"

// Credentials is everything registration needs to know about the host's
// 1Password setup: whether `op` is installed, which op-refs.env the gateway
// will actually be handed, where one would be seeded if none exists, and
// whether the gog file-keyring password is a filled ref.
//
// It is a PARAMETER, not something this package resolves. Answering these
// questions is the secret capability's job, and a capability may not call a
// sibling — so the workflow asks secret and passes the answers down. That is
// the whole L1-may-not-import-L1 rule in one struct: mcp still gets the facts,
// it just stops being the one to go and get them.
type Credentials struct {
	OpPath     string // resolved `op` binary ("" = not installed)
	OpRefsPath string // an EXISTING op-refs.env ("" = none found)
	SeedPath   string // where a template op-refs.env would be seeded
	GogKeyring bool   // GOG_KEYRING_PASSWORD is present as a FILLED op:// ref
}

// CredentialsFunc resolves Credentials for an env. Declared here so callers
// can name the seam; cmd/pix supplies the implementation over secret.
type CredentialsFunc func(hostenv.Env) Credentials
