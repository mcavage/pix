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
| **Your `web_search` query text** (and the fetched pages' contents come back through it) | `api.exa.ai` / `mcp.exa.ai` — the **zero-config default** backend. Fallbacks, used when you supply their key: `generativelanguage.googleapis.com` (Gemini), `api.perplexity.ai` | The `web_search`/`fetch_content` tools in `pi-web-access` | Governed by that search provider |
| **A version check**: pi asks the npm registry whether a newer `@earendil-works/pi-coding-agent` exists (this is what the in-sandbox "Update available" banner is) | `registry.npmjs.org` | Update notification | npm's terms; pix stores no result |
| **Package + toolchain downloads**: pi extensions and npm packages, the pinned `fd`/`ruff`/Go binaries at image build, git fetches, release assets fetched by `install.sh`/`brew` | `registry.npmjs.org`, `nodejs.org`, `pi.dev`, `github.com`, `codeload.github.com`, `objects.githubusercontent.com`, `raw.githubusercontent.com`, `go.dev` | Install what the sandbox runs | Those hosts' terms |
| GitHub API calls you make (`gh`, PRs, issues) | `api.github.com`, `uploads.github.com` | Commands you ran | GitHub's terms |
| Loopback traffic to host services you started (`memory` :11435, `monitor` ingest :11437, `ollama` :11434) | Your own machine, over `host.docker.internal`/`localhost` | Recall, the wiretap, local inference | Local only — see below |

Those are all the destinations, not a sample: sandbox egress is allowlisted in
`pi-kit/spec.yaml` (`permissions.network.allow`), a destination not on that
list cannot be reached from inside the sandbox, and the table above is the
same set. If you need the default search backend or the update check off,
remove its host from that allowlist and recreate the sandbox; nothing else in
pix depends on it.

## What stays local

- **Memory** (`pix-host memory`, `:11435`): the self-learning store. Binds
  loopback, file-backed on your machine, never synced anywhere.
- **Monitor ingest** (`:11437`): loopback by default. `--bind 0.0.0.0` is an
  explicit LAN opt-in with a warning and no auth token — do not use it on an
  untrusted network.
  **It persists a transcript, and that is the point of it.** The wiretap is
  not in-memory: when the monitor service is running (it is part of the
  default `services` set that `pix serve` starts), every event the in-sandbox
  tap ships is appended to a file on your machine under
  `~/.local/state/pix/monitor/`, one `events.ndjson` per
  (sandbox, session) plus a `blobs.ndjson` for full payload bodies. That
  content is your prompts, the model's replies, and raw tool output.
  Concretely:
  - **Location/permissions**: `<state-dir>/monitor/` (`$XDG_STATE_HOME/pix`,
    else `~/.local/state/pix`), directories `0700`, files `0600`, no symlink
    followed.
  - **Redaction**: every event and every blob is passed through the
    secret-redaction pass before it is written (a stored blob can therefore
    differ from the hash it is referenced by; the record marks that with
    `redacted`). Redaction targets credentials, not personal data.
  - **Bounds, not a retention schedule**: each stream is trimmed
    drop-oldest at 4000 events / 8 MB, and the number of retained streams is
    capped at 200 (oldest by mtime evicted). Nothing is deleted on a
    schedule or by age — a stream that stays under those caps stays on disk
    until you remove it.
  - **Deleting it**: `rm -rf ~/.local/state/pix/monitor`. Turning it off:
    `pix config set services memory` (any explicit `services` list that
    omits `monitor`), then restart `pix serve`.
  - It is never uploaded, and `pix monitor` reads it offline from the same
    files.
- **Session transcripts / todos / provenance records**: files under your home
  and `out/`, never uploaded by pix.
- **Config**: `~/.config/pix/config.toml`, `~/.local/state/pix/`.

## Credentials

Direct provider keys are `op://` references resolved from 1Password at
spawn. `pix secret` never writes a secret value to disk — it seeds, opens,
and validates the reference file only. Values are not copied into the
sandbox image, the config, MCP registrations, or provenance records.

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
