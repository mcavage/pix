# Native environments delivery handoff

Resume the full native-environments migration from this file. The user approved
**lean execution**: one implementation pass per remaining wave, focused tests
while building, one `make gate` at wave boundaries, findings batched, and only
the mandatory final security/QA/two cross-vendor review rounds. Do not restart
product planning and do not repeat completed unit reviews.

## Repository state

- Repository: `/Users/mcavage/dev/pix`
- Branch: `design/simplify-pack-rigs`
- Current HEAD: `67e6bdf6` (`Fix native environment workspace schema`)
- Waves A-C: complete.
- Wave D: E2.1-E2.7 merged. E2.8 is blocked by the live private-environment gate.
- Wave E: complete, including E3.4 removal of shipped agent intents.
- Wave F: complete, including deletion of scored routing, `routing.json`,
  `make routing`, `--intent`, `run_intent`, and `model-refresh`.
- Waves G-H: not started. E5.1 depends on E2.8, so there is no independent Wave
  G work left to do before the host gate.

Important integrated commits:

- `ee3baf6b` E2.5 native-environment launch cutover
- `7b81b74b` E3.4 shipped-agent roster cleanup
- `c0c53ad8` E2.6 recreate diagnostics + E2.7 environment host-service union
- `ebb50219` Wave F scored-router deletion
- `54152f2b` Wave F anti-em-dash gate fix
- `67e6bdf6` first real-sbx schema correction (`workspace` /
  `additionalWorkspaces`)

All integrated gates passed. The most recent full `make gate` passed all 11
segments at `67e6bdf6` in 36.566s.

## User-owned unstaged files

Preserve these exactly and never stage or commit them:

- deleted `.claude/settings.local.json`
- deleted `CODE_OF_CONDUCT.md`
- deleted `CONTRIBUTING.md`
- modified `Dockerfile`
- modified `LICENSE`
- deleted `NOTICE.md`
- deleted `SECURITY.md`
- deleted `THIRD_PARTY_NOTICES.md`

After Wave F changed the committed Dockerfile base, the verified user-diff hash
became:

```text
fad6954c04e36ba9ea3f993dc41e71d919a62a3cef6b671d5baf3e4588bd00f4
```

The old `2dd22...` hash is obsolete. A three-way proof showed the current
Dockerfile contains both Wave F's routing removal and every user-owned Dockerfile
hunk. Around future merges/gates, stash these files, restore them, and verify the
`fad695...` hash unless another committed change legitimately changes one of
their bases. If that happens, do another explicit three-way preservation proof.

## Active worktrees

- `/tmp/pix-native-schema-parity`, branch `fix/native-schema-parity`, currently
  clean at `67e6bdf6`. A `deep` implementation attempt was dispatched and
  aborted without changes. Reuse or recreate it.
- `/tmp/pix-native-wave-f`, branch `unit/native-wave-f`, stale at `c3608f83`.
  Wave F is already merged. Remove this worktree and branch.

No stash is active.

## Host UAT evidence and current blocker

The UAT MCP is available through the `mcp-gateway` cached tools even when search
returns no hit. Call `mcp_gateway_uat_*` directly.

### Host attempt 1

- Candidate: `54152f2b`
- Run: `run-20260829-161325-d3c9a7be`
- Six candidate-owned Story 0 environment checks passed.
- Candidate launch failed before sandbox visibility:

```text
field workspaces not found in type sbxenv.Config
```

This proved E2.1 had invented an invalid top-level `workspaces:` field. Commit
`67e6bdf6` replaced it with documented sbx 0.39 `workspace:` and
`additionalWorkspaces:` forms and added strict parser/renderer tests.

### Host attempt 2

- Candidate: `67e6bdf6`
- Run: `run-20260829-211707-aa7177e1`
- Six candidate-owned Story 0 environment checks again passed.
- The corrected effective file got past the workspace field but candidate
  no-environment launch failed:

```text
invalid .../effective.sbxenv.yaml: agent is required
```

The built-in `none` document emits no `agent`. Do **not** spend another host run
fixing only this field. The same audit found the production schema still differs
from official sbx 0.39 in several places.

## Immediate next task: comprehensive native-schema parity

Use the official Docker reference as the source of truth:

`https://docs.docker.com/ai/sandboxes/configuration/environment-files/`

Fix all of these in one red-first pass before another host retry:

1. Effective RuntimeFacts always render `agent: pix` for built-in defaults and
   selected Pix environments. Strict authored parsing requires `agent`.
2. Top-level official fields:
   - `schemaVersion`, `name`, `agent`, `kits`
   - `workspace` string or `{path, clone}`
   - `additionalWorkspaces` list of `{path, readOnly}`
   - `env`, exact `sandboxOptions`, `secrets`, `bindings`, `registries`, `mcp`,
     and `ports`.
3. Replace loose `sandboxOptions map[string]string` with the strict official
   object: `template`, `memory`, integer `cpus`, `pullPolicy`, `profile`.
4. A secret source is exactly one of `value`, `ref`, or **string** `command`,
   plus optional `refresh`, `backend` (`sdk|cli`), and `noVerify`. Current Pix
   incorrectly models `command` as `[]string`. Pix policy still refuses every
   literal `value`.
5. A registry entry is `{secret: <secret source>, username: <optional secret
   source>}`. Current Pix incorrectly models a flat registry ref/command.
   Literal `value` remains refused in either nested source.
6. Bindings support both `apiKey.domains` and `oauth.domains`.
7. MCP servers require `name` and exactly one of `url` or `command`; `args` is a
   string list.
8. Ports preserve/validate `sandbox`, `host`, `protocol`, and `hostIP`.
9. Update parse/merge/tree/HMAC attribution/trust review/render so every official
   field is preserved and fingerprinted without exposing resolved values.
10. Add an independent test-only mirror of the official schema and one fixture
    containing every field. Strict-parse rendered bytes through that mirror.
    Add sentinels banning old `workspaces:`, flat registry entries, and
    `command: []` secret shapes.
11. Preserve A3/A6 non-claims. Do not claim host acceptance until the next UAT.

The aborted deep prompt already described this exact task; there are no partial
changes to salvage. Dispatch a fresh `deep` agent in
`/tmp/pix-native-schema-parity`, then merge and run one full gate.

## Host gate after schema parity

Submit `uat/scenarios/smoke.yaml` against the fixed commit through
`mcp_gateway_uat_submit`, poll `mcp_gateway_uat_status`, and inspect bounded
artifacts on failure. The smoke run proves candidate build, all six upstream
environment checks, and a real no-environment candidate launch.

Before E2.8 may land, still record live proof for:

- sbx >= 0.39
- home environment create, attach, and teardown
- private work environment conversion and live run (A8/R6)
- native MCP name/render behavior
- exact `sbx env rm` invocation and target name

The current closed UAT scenario does not know the user's private work environment.
That final private conversion may require one host-side user action after all
agent-verifiable host checks pass. Do not fake it or treat verbal confirmation as
evidence.

External cleanup debt remains: `pix-uatenv-fixture-image` from
`run-20260824-092338-d4c384f5` is still visible on the host. Verify its exact
workspace identity before removing it.

## Remaining delivery order

1. Comprehensive schema parity fix and host smoke retry.
2. Complete the host gate, including private work environment evidence.
3. E2.8: delete every selectable pack launch path. No landing before host proof.
4. Wave G, one engineer pass in order E5.1 -> E5.2 -> E5.3 -> E5.4.
   E5.1 must re-derive the live pack-trust test/assertion inventory and publish a
   named environment replacement for every assertion before deleting anything.
5. Wave H, one engineer pass E6.1 -> E6.4. Never touch or resurrect the eight
   user-owned files above. Update README, reference, getting-started, AGENTS.md,
   onboarding, and healthcheck to the environment/roster model.
6. Host Gate 2: concurrent home + work UAT.
7. Mandatory final security review, QA/UAT matrix with full AC traceability, two
   explicit cross-vendor review rounds (final clean), PM/DX/copy closeout, final
   gate, and status/evidence commit.

## Resume commands

```bash
cd /Users/mcavage/dev/pix
git status --short
git log -8 --oneline
git worktree list
git stash list

# Remove stale merged Wave F worktree.
git worktree remove /tmp/pix-native-wave-f
git branch -d unit/native-wave-f

# Confirm schema-parity branch is clean at main HEAD, then dispatch the fix.
git -C /tmp/pix-native-schema-parity status --short
git -C /tmp/pix-native-schema-parity log -1 --oneline
```
