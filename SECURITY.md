# Security

pix's whole premise is running an autonomous agent without a stream of
approval prompts, so the trust boundary matters more than usual. Read this before
you rely on it.

## The trust boundary

The safety boundary is the **Docker Sandbox (sbx) VM**, not a per-command
confirmation. pi runs full-auto inside a disposable, network-limited VM.

What the sandbox protects:

- **Your host filesystem.** The agent sees the workspace and additional mounts
  declared by the environment, not arbitrary host files.
- **Your provider credentials.** Anthropic, OpenAI, and Google keys are injected
  by the host proxy at the network layer. The VM never holds them; it only sees
  model responses. GitHub uses the same proxy injection.
- **The network.** Egress is limited to the allowlist in `pi-kit/spec.yaml`. Environments may extend that policy through native kits; such changes
  participate in environment approval.
- **Host data tools.** Google Workspace, Slack, and environment-declared connectors
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
- **Memory inference runs outside the VM.** The memory service uses the configured
  Ollama endpoint. A local endpoint stays on your host; a remote endpoint or
  cloud-backed model can send content off-host. Recalled memory also becomes
  part of the active model's prompt.

## Host-side MCP servers run with your trust, not the sandbox's

A local-command MCP server (a mail bridge, `gog`) is a process the sbx
gateway spawns on your **host**, not inside the sandbox. Environments
declare their integrations in `.sbxenv.yaml`; Pix also supplies its own
memory service through the Gateway. Registering one (`sbx mcp
add`) is a host-level trust decision: the command you register runs with
whatever access the gateway's spawn environment has, resolved credentials
included. Review a server's registered command before trusting it (`sbx mcp
inspect <name>`), and treat an environment that declares a host-executing
integration as running code on your machine, not just in the sandbox. `pix
env trust NAME` gates that with a plain-language consent prompt that
defaults to No; `--verbose` shows the technical review. A remote MCP server (notion/atlassian/granola-style, added
by URL) authenticates through hosted OAuth handled entirely host-side by the
gateway; the sandbox uses the authenticated Gateway connection.

**Environment setup hooks run on your host.** An environment's `pix.toml` may
include `[[setup]]` hooks for preparing tools and authenticating accounts.
Setup runs reviewed hook snapshots containing the executable and declared
companion inputs, after verifying their hashes. Undeclared files are not copied
into the snapshot. Hooks still run with the host user's privileges and can
access that user's filesystem; a snapshot is a consistency mechanism, not a
sandbox for the hook.

`pix setup --env NAME` is the explicit environment setup path. Bare interactive
`pix` can invoke setup on a new installation. Ordinary launch does not replay
environment install/authentication steps. Approved diagnostic `probe_args` are
also host execution and must be written to be read-only. Review an unfamiliar
environment's source before approving it; technical details are available with
`pix env trust NAME --verbose`.

**Remote content is untrusted content.** Anything a capability reads back
from the outside world, an email body, a Slack message, a doc, a wiki
page, becomes part of the prompt sent to your model provider once it's
recalled or returned. A server that fences its results as untrusted (`gog`'s
`--wrap-untrusted` is the canonical case) reduces the odds the agent treats
that text as an instruction, but it is a mitigation, not a guarantee: assume
fetched content can attempt prompt injection and keep write-capable tools off
by default.

**Revoking and rotating access.** Revoke an OAuth grant at the provider, then
reconnect through environment setup or `sbx mcp auth NAME` as appropriate. Update
1Password items to rotate referenced credentials. Each Pix create/attach
re-resolves provider and GitHub references into sandbox-scoped secrets; local
MCP credentials are resolved when the Gateway spawns their process. A running
MCP process can retain its previous credential until restarted. Pix does not
create, depend on, or automatically delete host-global sbx secrets.

## The memory service is scoped, not sealed

Every Pix-owned runtime resource carries a stack id derived from your
`PIX_HOME`, so two installations on one host never take each other's
container, port, sandbox, or MCP registration by accident. That is a
collision guarantee, not a confidentiality one.

The memory registration's endpoint URL carries that stack's bearer token as a
query parameter, because sbx has no way to declare a secret authorization
header for a registered MCP server. sbx's registry is host-global and owned by
your user account, so any other process running as the SAME host user can read
the token-bearing URL back out of it and call your memory service. Closing
that needs an upstream sbx capability (a header-bearing MCP declaration, or
per-registration ACLs) this project does not own. Until then: a shared login is
a shared memory service, and nothing in memory should be a secret you would not
hand to any process on that account.

## Provider-key process exposure

A resolved provider value never enters an argument vector. Pix writes each
sandbox-scoped credential with `sbx secret set -f --sandbox <name> <service>`
and feeds the value to that command's stdin, so the host's process table
carries the flags and the service name only. Pix also never logs or persists
the value and scrubs it from subprocess errors, which stays in place as
defence in depth: `sbx` is free to echo back whatever it read.

What remains: the pipe is readable by the two processes holding it, and
anything that can already read this user's memory or ptrace its processes can
read the value there. A host where another user's code runs as your user is
outside the supported credential boundary either way.

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
