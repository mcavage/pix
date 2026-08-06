# Privacy and data handling

What pix does with data, stated at the level of "what leaves the machine, to
whom, and for how long". Written against a data-minimization baseline (GDPR
Art. 5(1)(c), CCPA/CPRA §1798.100(c)): the question for each item below is
whether pix collects, retains, or transmits more than the stated purpose
requires.

pix is a developer tool you run yourself. There is **no pix backend**: no
telemetry endpoint, no analytics service, no account, no usage reporting to
Docker, Inc. or anyone else. Every network destination pix talks to is one
you configured (a model provider, an MCP server, a registry).

## What leaves your machine

| Data | Goes where | Why | Retention |
| --- | --- | --- | --- |
| Prompts, file contents, tool output the agent reads | The model provider you selected (Anthropic / OpenAI / Google), or nowhere for a local Ollama model | Inference | Governed by that provider's terms, not by pix |
| Prompts routed to an MCP server | That server (local stdio subprocess, or a remote catalog server you registered) | Tool calls you invoked | Governed by that server |
| Image pulls/pushes | The registry in your config (`docker.io`, `dhi.io`) | Build/run the sandbox | Registry's terms |
| Nothing else | — | — | — |

Sandbox egress is allowlisted in `pi-kit/spec.yaml`
(`permissions.network.allow`). A destination not on that list cannot be
reached from inside the sandbox, which is the enforcement point for the
table above.

## What stays local

- **Memory** (`pix-host memory`, `:11435`): the self-learning store. Binds
  loopback, file-backed on your machine, never synced anywhere.
- **Monitor ingest** (`:11437`): loopback by default. `--bind 0.0.0.0` is an
  explicit LAN opt-in with a warning and no auth token — do not use it on an
  untrusted network.
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
