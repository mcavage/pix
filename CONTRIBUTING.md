# Contributing to pix

Thanks for looking. pix is an opinionated distribution of the
[pi](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) coding agent, so most contributions
are skills, agents, extensions, docs, or host-service code, not changes to pi
itself.

Read [AGENTS.md](AGENTS.md) before you start. It is the harness's own memory:
repo layout, the build loop, extension and skill conventions, and the mistakes
worth not repeating.

## Ground rules

- **Two languages, one convention.** Everything that runs on the HOST is Go
  (`services/host/`, compiled into `pix-host`). Everything that runs INSIDE
  the sandbox is TypeScript (`extensions/`). Do not add a host-side Node or
  Python service. See AGENTS.md for why.
- **Keep the open-core boundary clean.** Nothing company-specific belongs in this
  repo: no channel names, account emails, internal hostnames, connector-specific
  env, or private skills. Those live in a private **pack** (git-backed, mounted at
  runtime with `pix pack use`) and, for host-executing integrations, in a
  separate container/host-daemon repo — never compiled into the public tree.
  `scripts/check-open-core.sh` runs in CI and fails if a company-specific file or
  an internal marker is ever tracked.
- **A skill is pure mechanism.** Never bake one person's specifics (their
  channels, accounts, thresholds) into a `SKILL.md`. Read those from memory or
  a pack at runtime; the skill only knows the shape.
- **Write like a human.** Direct, concrete, no em-dashes, no AI filler. See the
  `anti-slop` and `writing-voice` skills.

## Setup

You can develop the host binaries and skills from a normal checkout. Building
the Docker image needs a DHI-entitled Docker account; the hosted `sbx run` path
does not.

```bash
git clone https://github.com/mcavage/pix
cd pix
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
  exists in `services/host/routing/defaults/models.json` (`pix agent ls`
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

## License of contributions (inbound = outbound)

pix is a Docker, Inc. project distributed under the MIT license in
[LICENSE](LICENSE), and contributions come in on those same terms: **by
opening a pull request you license your contribution under the MIT license,
and you represent that you have the right to do so** (that it is your own
work, or that your employer has authorized it). There is no separate CLA and
no copyright assignment — inbound license equals outbound license. See
[NOTICE.md](NOTICE.md) and
[docs/legal/AUTHORIZATIONS.md](docs/legal/AUTHORIZATIONS.md).

## Reporting bugs and asking for features

Use the issue templates. For anything security-sensitive, follow
[SECURITY.md](SECURITY.md) instead of opening a public issue.
