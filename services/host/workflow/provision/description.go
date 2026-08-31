// description.go — the prose above `pix setup`'s GENERATED usage. It lives
// in this package (rather than in cmd/pix beside the command struct) so the
// workflow that performs setup owns the sentence describing it; the flag
// list is not here, since the command struct's tags are the flag list.
package provision

// Description is what `pix setup` guarantees and how a repeat behaves.
const Description = `Guided setup: initialize PIX_HOME, generate the pix-memory bearer token,
reconcile the pix-memory container, and register its reserved sbx MCP name.
Idempotent and safe to rerun — nothing is applied that a fresh probe of the
same host would not find missing.

It does not install sbx, images, or a strict kit, does not create an
environment, does not wire a model provider key, and does not launch a
sandbox: pix doctor reports the rest, and pix run starts a session.
`
