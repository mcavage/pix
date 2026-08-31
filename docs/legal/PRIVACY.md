# Privacy and data handling

What pix does with data, stated at the level of "what leaves the machine, to
whom, and for how long". Written against a data-minimization baseline (GDPR
Art. 5(1)(c), CCPA/CPRA §1798.100(c)): the question for each item below is
whether pix collects, retains, or transmits more than the stated purpose
requires.

pix is a developer tool you run yourself. There is **no pix backend**: no
telemetry endpoint, no analytics service, no account, no usage reporting to
Docker, Inc. or anyone else. That is not the same as "no network traffic
unless you configure something": pix ships with default third-party
destinations for updates, package downloads, and web search, and they are
enumerated below rather than left to the phrase "only what you configured".

## What leaves your machine

| Data | Goes where | Why | Retention |
| --- | --- | --- | --- |
| Prompts, file contents, tool output the agent reads | The model provider you selected (Anthropic / OpenAI / Google), or nowhere for a local Ollama model | Inference | Governed by that provider's terms, not by pix |
| Prompts routed to an MCP server | That server (local stdio subprocess, or a remote catalog server you registered) | Tool calls you invoked | Governed by that server |
| Image pulls/pushes | The registry in your config (`docker.io`, `dhi.io`) | Build/run the sandbox | Registry's terms |
| **Your `web_search` query text** (and the fetched pages' contents come back through it) | The baked `~/.pi/web-search.json` **pins `api.parallel.ai`** (Parallel search/extract) as the provider for every search when `PARALLEL_API_KEY` is set — not a fallback, the preferred backend outright. Without that key, resolution falls through to `api.openai.com` (OpenAI search) if wired, then the keyless `api.exa.ai` / `mcp.exa.ai` default, then any other allowed/configured backend in order: `api.perplexity.ai`, `generativelanguage.googleapis.com` (Gemini) | The `web_search`/`fetch_content` tools in `pi-web-access` | Governed by that search provider |
| **A version check**: pi asks the npm registry whether a newer `@earendil-works/pi-coding-agent` exists (this is what the in-sandbox "Update available" banner is) | `registry.npmjs.org` | Update notification | npm's terms; pix stores no result |
| **Package + toolchain downloads**: pi extensions and npm packages, the pinned `fd`/`ruff`/Go binaries at image build, git fetches, release assets fetched by `install.sh`/`brew` | `registry.npmjs.org`, `nodejs.org`, `pi.dev`, `github.com`, `codeload.github.com`, `objects.githubusercontent.com`, `raw.githubusercontent.com`, `go.dev` | Install what the sandbox runs | Those hosts' terms |
| GitHub API calls you make (`gh`, PRs, issues) | `api.github.com`, `uploads.github.com` | Commands you ran | GitHub's terms |
| Whatever an environment's `[[setup]]` hook itself contacts (a package registry, an identity provider's device-login endpoint) | Wherever that hook's own command goes — pix neither chooses nor proxies it | Host install/authentication you explicitly requested with `pix setup --env NAME`, after accepting that hook's argv and executable hash in `pix env trust` | Governed by whatever the hook talks to; pix retains nothing from it |
| Loopback traffic to services on your own machine: the stack-scoped `pix-memory-<stack-id>` container (a loopback port `pix setup` allocates per PIX_HOME, 18080 by default) and `ollama` (:11434) | Your own machine, over `host.docker.internal`/`localhost` | Recall, local inference | Local only; see below |

Every sandbox-egress destination is disclosed above: sandbox egress is
allowlisted in `pi-kit/spec.yaml` (`permissions.network.allow`), and a
destination not on that list cannot be reached from inside the sandbox. The
table is not limited to sandbox egress, though — it also names a few
host-side and build-time destinations that never go through that allowlist
at all: `go.dev` (the Go toolchain, fetched while the image is built, not at
sandbox runtime) and the loopback host services (`memory`, `ollama`), which
the sandbox reaches over `host.docker.internal`, a route the egress
allowlist governs but that never leaves your machine. If you need the
default search backend or the update check off, remove its host from the
sandbox egress allowlist and recreate the sandbox; nothing else in pix
depends on it.

## What stays local

- **Memory** (`pix-memory-<stack-id>`, one container per `PIX_HOME`, reached through the sbx MCP Gateway over loopback on that stack's allocated port): the self-learning store. It binds
  loopback, is file-backed on your machine, and is never synced anywhere. "Local"
  describes the store and its extraction/embedding path, not everything
  memory touches: once a row is **recalled** (auto-injected each turn, or via
  `/recall`/`memory_recall`), its content goes into the prompt sent to
  whichever model provider is active — the same row that never left this
  machine to get stored now leaves it to get answered. The daemon requires
  the bearer token mounted from this stack's state; the host-global Gateway
  registration carries that token in its stack-scoped endpoint URL because
  sbx cannot express a custom authorization header. The stack-scoped name
  prevents two `PIX_HOME` installations from colliding; it does not keep the
  token private from another process running as the same host user, which can
  read the stored URL out of sbx's own registry. See
  [../memory.md](../memory.md) for the full trust model and how capture and
  recall actually work.
- **No transcript of its own.** pix used to ship a monitor: an in-sandbox tap
  POSTed every model request, reply and raw tool result to a loopback ingest
  listener, which appended them under `~/.local/state/pix/monitor/`. That whole
  subsystem was REMOVED, tap included, so pix no longer writes any transcript
  anywhere. What remains on disk is what `pi` itself writes: its session
  transcripts under `.pi-sessions/*.jsonl` in your workspace.
  **If you ran an earlier version, that data is still there and nothing will
  ever touch it again** — no reader, no eviction pass, no bounds. Delete it:
  `rm -rf ~/.local/state/pix/monitor` (that orphaned path predates PIX_HOME;
  everything Pix writes today lives under `${PIX_HOME:-$HOME/.pix}`).
- **Session transcripts / todos / provenance records**: files under your home
  and `out/`, never uploaded by pix.
- **Config and state**: everything Pix owns lives under `PIX_HOME` (default `~/.pix`, overridable with `$PIX_HOME`): `~/.pix/config.toml`, `~/.pix/secrets.env`, `~/.pix/state/`. There is no XDG split and no second config or secrets location.

## Credentials

Direct provider keys are `op://` references resolved from 1Password on each
run. `pix secret` never writes a secret value to disk: it seeds, opens, and
validates the reference file only. Pix refreshes configured provider, tool,
and GitHub values into sbx's sandbox-scoped secret store after the sandbox has
a verified receipt and before attaching. Values are not copied into the
sandbox image, Pix config, MCP registrations, or provenance records. Pix
ignores host-global sbx secrets and never removes them automatically.

## Minimization posture, and its limits

- pix collects no personal data of its own, so there is no pix-side
  controller/processor relationship, no retention schedule to publish, and
  no deletion endpoint to offer.
- **The real exposure is what you feed it.** If you point pix at a mailbox,
  a CRM, or an HR system through an MCP server, personal data flows from
  that system into a model provider's inference path. pix does not filter
  that, and it cannot decide lawful basis, purpose limitation, or retention
  for your employer's data. Those are your obligations under whatever
  processing agreement covers that source system and that model provider.
- The full-history secret scan (`scripts/check-secret-history.sh`) guards
  against credentials in the repo. It is not a personal-data scanner.

## Not covered here

Model-provider terms, your employer's data-processing agreements, and any
sector-specific regime (health, financial, EU AI Act obligations for a
deployer) are out of scope for this file and for this project's CI gates.
This document is transparency, not legal advice; see
`docs/legal/FINDINGS.md` for what still needs a human.
