# Contributing to pi-stack

Thanks for looking. pi-stack is an opinionated distribution of the
[pi](https://github.com/badlogic/pi-mono) coding agent, so most contributions
are skills, agents, extensions, docs, or host-service code, not changes to pi
itself.

Read [AGENTS.md](AGENTS.md) before you start. It is the harness's own memory:
repo layout, the build loop, extension and skill conventions, and the mistakes
worth not repeating.

## Ground rules

- **Two languages, one convention.** Everything that runs on the HOST is Go
  (`services/host/`, compiled into `pi-stack-host`). Everything that runs INSIDE
  the sandbox is TypeScript (`extensions/`). Do not add a host-side Node or
  Python service. See AGENTS.md for why.
- **Keep the open-core boundary clean.** Nothing company-specific belongs in this
  repo: no channel names, account emails, internal hostnames, connector-specific
  env, or private skills. Those live in a private overlay repo. `make serve`
  compiles overlay plugins locally, but they are never committed here.
  `scripts/check-open-core.sh` runs in CI and fails if an overlay file or an
  internal marker is ever tracked.
- **A skill is pure mechanism.** Never bake one person's specifics (their
  channels, accounts, thresholds) into a `SKILL.md`. Read those from memory or
  the overlay at runtime; the skill only knows the shape.
- **Write like a human.** Direct, concrete, no em-dashes, no AI filler. See the
  `anti-slop` and `writing-voice` skills.

## Setup

You can develop the host binaries and skills from a normal checkout. Building
the Docker image needs a DHI-entitled Docker account; the hosted `sbx run` path
does not.

```bash
git clone https://github.com/mcavage/pi-stack
cd pi-stack
cd services/host && go build ./... && go test ./...   # host code
```

## Changing things

- **Host code (Go):** edit under `services/host/`, then
  `go build ./... && go test ./... && go vet ./...`. Add table-driven tests for
  new logic.
- **Skills:** edit `skills/<name>/SKILL.md`. In a dev sandbox (`make run`) skills
  load live from the tree, so `/reload` picks up edits with no rebuild.
- **Agents:** edit `agents/<name>.md`. Declare an `intent:`, not a pinned
  `model:`; the router resolves it. If you must pin, use a fully-qualified id that
  exists in `services/host/routing/defaults/models.json` (`pi-stack agent ls`
  flags an unknown pin).
- **Extensions (TypeScript):** edit `extensions/*.ts`. An extension that throws
  at load breaks pi startup, so guard defensively. Never put a `.d.ts` in
  `extensions/`.
- **Image / baked files:** need `make load` on a DHI host to take effect in a new
  sandbox.

## Before you open a PR

1. `cd services/host && go build ./... && go test ./... && go vet ./...`
2. If you touched the image or baked files, note that a maintainer must
   `make load` to verify.
3. Run the `code-review` skill (or ask a second model) over your diff.
4. Fill in the PR template. Say what changed, why, and how you verified it.

## Reporting bugs and asking for features

Use the issue templates. For anything security-sensitive, follow
[SECURITY.md](SECURITY.md) instead of opening a public issue.
