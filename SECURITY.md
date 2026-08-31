# Security

pix's whole premise is running an autonomous agent without a stream of
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
- **Local models run on your machine.** Ollama models the memory loop uses run on
  the host, outside the VM boundary.

## Host-side MCP servers run with your trust, not the sandbox's

A local-command MCP server (a mail bridge, `gog`) is a process the sbx
gateway spawns on your **host**, not inside the sandbox. Pix ships none of
them: every server is declared by the environment you launch, in its own
`.sbxenv.yaml`, which is why adoption is gated. Registering one (`sbx mcp
add`) is a host-level trust decision: the command you register runs with
whatever access the gateway's spawn environment has, resolved credentials
included. Review a server's registered command before trusting it (`sbx mcp
get <name>`), and treat an environment that declares a host-executing
integration as running code on your machine, not just in the sandbox. `pix
env trust NAME` gates that with an explicit bill-of-materials prompt that
defaults to No. A remote MCP server (notion/atlassian/granola-style, added
by URL) authenticates through hosted OAuth handled entirely host-side by the
gateway; the sandbox never sees the token.

**Environment setup hooks run on your host, on purpose.** An environment's
`pix.toml` may declare `[[setup]]` hooks — the replacement for the removed
pack install/auth hook. They are the only environment-authored code pix
executes on your machine, and they run **only** when you type `pix setup
--env NAME`, never during `pix run`, `pix doctor`, or any implicit launch.
Before that, `pix env trust NAME` shows you each hook's id, kind, the exact
check and apply argv, and the sha256 of the executable, and the fingerprint
you accept covers all of it, so a hook whose script or arguments change is
refused until you review it again. Immediately before running one, pix
re-hashes the file on disk and refuses on any mismatch. Hooks execute as
argv with no shell, and pix injects no environment variables or secret
values into them. Treat a hook you did not write the way you would treat
`curl | sh`: read the script, then decide.

**Remote content is untrusted content.** Anything a capability reads back
from the outside world, an email body, a Slack message, a doc, a wiki
page, becomes part of the prompt sent to your model provider once it's
recalled or returned. A server that fences its results as untrusted (`gog`'s
`--wrap-untrusted` is the canonical case) reduces the odds the agent treats
that text as an instruction, but it is a mitigation, not a guarantee: assume
fetched content can attempt prompt injection and keep write-capable tools off
by default.

**Revoking and rotating access.** An OAuth grant (Google Workspace, a remote
catalog server) is revoked from that provider's own account security page,
not from pix; re-authorize afterward through that environment's own
`[[setup]]` auth hook (`pix setup --env NAME`) or the server's own login
flow, then register it again with `sbx mcp add <name>`. A 1Password-backed
MCP credential (an API token, a keyring password) is rotated in 1Password itself;
the gateway only resolves an `op://` ref at spawn time, so the new value takes
effect once you re-register the server (`sbx mcp add <name>`), which triggers a
fresh spawn. `pix secret set` is the equivalent for the cloud model provider
keys (Anthropic/OpenAI/Google), not MCP credentials.

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
