# Security

pi-stack's whole premise is running an autonomous agent without a stream of
approval prompts, so the trust boundary matters more than usual. Read this before
you rely on it.

## The trust boundary

The safety boundary is the **Docker Sandbox (sbx) VM**, not a per-command
confirmation. pi runs full-auto inside a disposable, network-limited VM.

What the sandbox protects:

- **Your host filesystem.** The agent only sees the mounted workspace, not the
  rest of your machine.
- **Your provider credentials.** Anthropic, OpenAI, and Google keys are injected
  by the host proxy at the network layer. The VM never holds them; it only sees
  model responses. GitHub uses the same proxy injection.
- **The network.** Egress is limited to the allowlist in `pi-kit/spec.yaml`. A
  new external host has to be added there explicitly.
- **Host data tools.** Google Workspace, Slack, and pack-provided connectors
  (containerized MCP servers or host daemons) run host-side, reached through the
  sbx gateway. Tokens stay on the host; the sandbox talks to a gateway, not to the
  service.

## What the sandbox does NOT protect

Be clear-eyed about these:

- **The mounted workspace is writable.** The agent can modify or delete anything
  in the repo you launched it on. Commit often; the sandbox is disposable, your
  uncommitted work is not.
- **GitHub credentials authorize real actions.** Proxy-injected `gh`/git
  credentials can push branches and open PRs against repos your token can reach.
  Scope the token you give sbx accordingly.
- **Untrusted content is a prompt-injection vector.** Text the agent reads from
  Slack, email, web pages, or a repo can contain instructions aimed at the agent.
  The `gog` MCP server runs read-only and wraps fetched content as untrusted; the
  Slack server stamps message results with an untrusted-content guard. Treat any
  capability that returns third-party text as a channel an attacker can write to,
  and prefer read-only, least-privilege configuration.
- **Local models run on your machine.** Ollama models the memory loop uses run on
  the host, outside the VM boundary.

## Host-side MCP servers run with your trust, not the sandbox's

A local-command MCP server (Slack, `gog`, a pack's host wrapper) is a process
the sbx gateway spawns on your **host**, not inside the sandbox. Registering
one (`sbx mcp add`, `pi-stack mcp register`) is a host-level trust decision:
the command you register runs with whatever access the gateway's spawn
environment has, resolved credentials included. Review a server's registered
command before trusting it (`sbx mcp get <name>`), and treat a pack that ships
a host-executing integration as running code on your machine, not just in the
sandbox; `pi-stack pack use` gates that with an explicit bill-of-materials
prompt before adoption. A remote MCP server (notion/atlassian/granola-style,
added by URL) authenticates through hosted OAuth handled entirely host-side by
the gateway; the sandbox never sees the token.

**Remote content is untrusted content.** Anything a capability reads back
from the outside world, an email body, a Slack message, a doc, a wiki
page, becomes part of the prompt sent to your model provider once it's
recalled or returned. A server that fences its results as untrusted (`gog`'s
`--wrap-untrusted`, Slack's message guard) reduces the odds the agent treats
that text as an instruction, but it is a mitigation, not a guarantee: assume
fetched content can attempt prompt injection and keep write-capable tools off
by default.

**Revoking and rotating access.** An OAuth grant (Google Workspace, a remote
catalog server) is revoked from that provider's own account security page,
not from pi-stack; `pi-stack gog setup` re-authorizes cleanly afterward if you
need the integration back. A 1Password-backed MCP credential (a Slack token,
a keyring password) is rotated in 1Password itself; the gateway only resolves
an `op://` ref at spawn time, so the new value takes effect once you
re-register the server (`pi-stack mcp register`), which triggers a fresh
spawn. `pi-stack secret sync` is the equivalent for the cloud model provider
keys (Anthropic/OpenAI/Google), not MCP credentials.

## Reporting a vulnerability

Do not open a public issue for a security problem.

Email the maintainer (see the GitHub profile for
[@mcavage](https://github.com/mcavage)) with:

- what the issue is and where (file, command, or component),
- how to reproduce it,
- the impact you see.

You will get an acknowledgement. Fixes to the public tree ship as normal
releases with a note in [CHANGELOG.md](CHANGELOG.md).

## Hardening notes

- Give sbx the narrowest provider and GitHub tokens that still let the agent do
  its job.
- Keep the network allowlist tight; add hosts only when a task needs them.
- Run host MCP connectors read-only unless a workflow genuinely needs writes.
- Host MCP credentials resolve from 1Password (`op run`) at spawn time, so secret
  values never land on disk or in a registration.
