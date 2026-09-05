# Contributing to Pix

Pix is a small launcher and agent distribution. Read [AGENTS.md](AGENTS.md) for
the current architecture, invariants, and development workflow. The
[README](README.md) is written for people using Pix, not building it.

## Where contributions belong

- Launcher behavior belongs in the Go module under `services/host/`.
- Memory is a separate Go MCP service under `services/memory/`.
- Pi extensions and shared runtime helpers are TypeScript in `extensions/` and
  `lib/`. Put ambient declarations in `types/`, never `extensions/`.
- Skills and agent presets live under `skills/` and `agents/`. Keep them generic;
  company-specific workflows and accounts belong in a separate environment repo.
- Sandbox configuration belongs in native sbx environments and kits. Do not add
  a second registry, plugin framework, daemon, or model-selection router.

A skill describes a reusable process. An agent can inherit its model or name one
explicitly; environment `[agents]` bindings supply defaults. There is no intent
router or agent-management CLI. Consult
`services/host/inference/catalog/models.json` for Pix's known model identities.

## Local development

Use the Go versions declared by the modules, the Node toolchain from
`images/agent/Dockerfile`, and the pinned npm lockfile.

```bash
git clone https://github.com/mcavage/pix.git
cd pix
npm ci
(cd services/host && go build ./... && go test ./...)
(cd services/memory && go build ./... && go test ./...)
npm test
npx tsc --noEmit
```

Build and launch the complete local bundle on a host with Docker, sbx, and access
to the pinned base images:

```bash
make load
make run
```

Use a separate `PIX_HOME` for UAT. `make load` updates the launcher, runtime,
images, and manifest together and loads the agent into sbx's separate image
store. A running sandbox retains its old image. `make run` loads skills live;
`/reload` picks up those skill edits. Image-baked edits need a new sandbox.

## Before opening a PR

1. Exercise the changed behavior through its real caller. A helper's unit test
   alone does not prove the CLI, extension, or Gateway tool is wired correctly.
2. Run appropriate tests and `bash scripts/gate.sh`. Documentation-only work
   needs checked links, examples, and existing docs tests, not a new image build.
3. For setup, lifecycle, authentication, or image changes, run isolated host UAT
   and report what actually passed. If host access is unavailable, state the gap.
4. Review the diff for unnecessary complexity, secret exposure, and stale docs.
5. Sign commits, check the PR targets the intended branch, and describe the final
   behavior, validation, and remaining limitations. Confirm CI after pushing.

Normal product copy should be direct and actionable. Keep implementation detail
in maintainer docs or verbose output. Public commits and PRs must not include
private integration details, credentials, or business data.

## Contribution license

Pix is a Docker, Inc. project distributed under the MIT license. By opening a PR,
you license your contribution under the MIT license and represent that you have the
right to do so, including any required employer authorization. There is no
separate CLA and no copyright assignment: inbound license equals outbound license. See [LICENSE](LICENSE), [NOTICE.md](NOTICE.md),
and [authorization notes](docs/legal/AUTHORIZATIONS.md).

Use the issue templates for bugs and requests. Report vulnerabilities through
[SECURITY.md](SECURITY.md), not a public issue.
