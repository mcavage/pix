# Onboarding v3: inference-first progressive setup

Status: IMPLEMENTED. This document is the delivery contract; readiness claims
must follow live probes, not configuration presence. See also
`docs/design/models-cli.md`, which renamed the launcher verb this doc calls
the "route compiler" to `pix models` (`make routing` compiles it) and adds
the `pix models add <provider>` later-path this doc's setup flow currently
lacks.

## Goal

A new user reaches a verified first task without learning Pix's internal model
catalog, backend bindings, route compiler, generated kits, or credential
transport. Advanced concepts remain available through verbose diagnostics.

## Supported path

```bash
brew install mcavage/tap/pix
pix setup
```

Packs are explicit and may be repeated. Setup never advertises them:

```bash
pix setup \
  --pack 'git+https://github.com/owner/team-pack.git#ref=main' \
  --pack 'git+https://github.com/owner/project-pack.git#ref=v2'
```

The full `git+https` reference plus an explicit branch, tag, or commit is the
canonical form. It makes provenance visible and works for public and private
repositories using the caller's normal Git credentials. Local paths remain the
development form; the `owner/repo` shorthand is accepted but not advertised.

Packs are applied before interactive inference questions. A pack can provide
and enforce an inference backend, in which case setup skips questions it has
already answered. Session/ambient credentials are verified on the sandbox data
plane; setup reports those bindings as configured candidates until that live
probe succeeds rather than claiming host-side verification it cannot perform.

## Inference flow

When no pack has settled inference, setup first asks which runtimes to use:

```text
How should Pix run models? Select one or more:
  Direct model APIs (default)
  Ollama                 # only when a healthy daemon was detected
  Custom gateway
```

Direct API secrets are sourced through 1Password. A keyless gateway using the
session established by `sbx login` does not install, invoke, or require
1Password. Ollama is opt-in; Pix talks only to its local daemon, regardless of
whether Ollama executes a model locally or forwards it to Ollama Cloud.

Setup then shows the catalog models those runtimes actually expose and asks
which models agents may use. This is a roster, not per-agent configuration: the
intent router optimizes only inside it. Selecting one model deliberately routes
every agent through that model; selecting GLM and Kimi from an Ollama login, for
example, keeps the entire harness on those two. `pix setup --models <id,id>` is
the explicit noninteractive or later-change form. An exclusive pack owns its
inference roster and skips this personal question without deleting the saved
personal roster underneath it.

## Catalog, bindings, and routes

Pix ships a reviewed model-catalog snapshot seeded from models.dev. The catalog
contains identity and capabilities, never host availability. Configuration and
packs contribute backend bindings. Setup probes what is reachable from the host
and carries session-only bindings as candidates, then compiles:

```text
catalog × available bindings × pack policy → runtime routes
```

No model produces no routes and blocks setup. With one model, every scored
intent uses it and diversity is reported as degraded. Two-, three-, and
full-catalog topologies optimize only among callable models. A static fallback
may never select an unavailable model.

## Ollama and memory

Without Ollama, memory is disabled and setup stays quiet. With Ollama, setup
checks the watcher and embedding models. If both exist, memory is enabled and
verified. Missing models are offered as one explicit download with size/cost
disclosure; declining leaves memory disabled. Pix never silently sends memory
capture to a cloud model.

## Packs

A pack may declare public endpoint metadata, authentication mode, canonical
model mappings, and routing policy. Private details remain in the private pack;
the public repository uses only fictional generic fixtures. An exclusive pack
backend removes direct credentials and models from the runtime, model cycle,
explicit selection, and compiled routes.

Pack setup hooks remain typed, resumable, trust-gated, and probe-after-mutation.
Required hooks run automatically; optional hooks run only through repeated
`--with <id>` selections.

## Personal context

Durable personal context lives at:

```text
$XDG_DATA_HOME/pix/context/
  AGENTS.md
  skills/<name>/SKILL.md
```

Default: `~/.local/share/pix/context`. It loads for runs, tasks, host mode, and
subagents above pack context; workspace instructions remain most specific.
Personal context replaces the auto-created personal/default pack.

## Verification matrix

Every topology exercises setup, top-level inference, all intents, subagents,
model switching, pack enforcement, and personal-context precedence:

- no model;
- exactly one model;
- two models, same and different families/backends;
- three models, same and mixed families/backends;
- full supported catalog;
- Ollama absent, present, and sole inference backend;
- keyless gateway;
- exclusive pack gateway with direct keys already present;
- model disappearance after setup;
- repeated pack composition and removal.

## Release gates

Technical completion is followed, in order, by:

1. Agent-experience review: agents see only callable models and intelligible
   degradation.
2. User-experience review: setup requires no architecture knowledge.
3. Developer-experience review: automation and diagnostics are coherent.
4. A removal/simplification/hiding audit of every surfaced Pix concept.

Success words are earned only by post-mutation live probes.
