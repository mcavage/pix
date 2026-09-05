# Working on Pix

This file is for agents and developers changing Pix. [README.md](README.md) is
for users. Keep instructions current, concrete, and short enough to load on every
agent turn; put implementation history and detailed reference in `docs/`.

## Product and architecture

Build a small, maintainable launcher with guided onboarding and clear daily use.
Users need not understand containers, credential plumbing, or trust fingerprints.
Normal output explains the choice, progress, or recovery action; `--verbose`
provides bounded, redacted technical details. Preserve enforcement without
turning infrastructure details into the product experience.

Every environment uses the same flow. `default` is generated; “home” and “work”
are conventions, not modes. Keep `pix env add SOURCE [NAME]` for adopting a local
directory or cloning a Git repository without a registration database.

`pix` is the only host binary. It composes a native `.sbxenv.yaml` and uses
`sbx env create` / `sbx exec` to run the pinned Pi image. Docker manages the
separate memory container; the sbx MCP Gateway owns integration processes and is
the only sandbox-facing integration path. Do not reintroduce a plugin framework,
resident supervisor, model router, second registry, or a second sandbox grammar.

Read [the product contract](docs/design/pix-v2-surface.md) and
[architecture](docs/design/pix-v2-architecture.md) before architectural changes.
Historical design notes are evidence, not instructions to restore removed behavior.
Keep company-specific skills, accounts, endpoints, and UAT evidence in their own
environment repository, not this public repository.

## Where changes belong

| Path | Responsibility |
| --- | --- |
| `services/host/cmd/pix/` | CLI dispatch and user-facing orchestration |
| `services/host/workflow/env/` | Shared native-environment composition for preview and launch |
| `services/host/workflow/launch/` | Creation, attachment, session lifetime, and cleanup |
| `services/host/workflow/provision/`, `container/` | Runtime/image preparation and memory-container reconciliation |
| `services/host/envinfo/`, `envsetup/`, `hosttrust/` | Environment sidecar, setup snapshots, and approval receipts |
| `services/host/inference/`, `secret/` | Literal model choices, provider manifests, and credential references |
| `services/host/pixhome/`, `stack/`, `session/`, `sandbox/` | Storage paths, scoped identities, and lifetime records |
| `services/memory/` | Independent Go MCP service, SQLite store, and Dockerfile |
| `images/agent/Dockerfile`, `scripts/patches/` | Pinned Pi/toolchain image and reviewed upstream patches |
| `pi-kit/spec.yaml` | Native sbx kit, network policy, credentials, entrypoint |
| `extensions/`, `lib/`, `types/` | Pi extensions, shared helpers, ambient types |
| `agents/`, `skills/`, `settings.json`, `keybindings.json`, `themes/` | Shipped agent experience |

Host launcher and memory code are separate Go modules. Pi extensions are
TypeScript. Use existing package ownership; do not move coherent packages just
to match a proposed directory diagram. Consult the Dockerfile and module files
for current toolchain versions instead of copying pins into documentation.

## State and integration boundaries

`PIX_HOME` defaults to `~/.pix`. `context/` is personal content, `envs/` contains
environment directories or links, `runtime/<version>/` is shipped content, and
`.state/` contains machine-owned runtime data. Resolve paths through `pixhome`;
do not create an XDG split or hand-assemble alternate roots. Initialization uses
`git init -b main`; Pix never stages or commits the user's home automatically.

Credential values stay out of files, argv, logs, and model context. `secrets.env`
contains `op://` references; explicitly declared non-secret `plain_keys` may
also store values such as account names or file paths. Keep its mode `0600` and
private state directories `0700`. Use the owning config/secret writers and their
locks instead of editing files from a second code path.

`stack` derives the 16-hex id from canonical `PIX_HOME`. Sandboxes, memory, and
session MCP names always carry it. The shared environment compiler suffixes
local MCP names too; remote MCP names remain shared. The memory port lives in
`.state/memory/port`, not machine config. Never clean another home's resources.

The Gateway registry is host-global. Its memory URL currently carries a bearer
token because sbx cannot declare a secret header. Stack scoping prevents
collisions, not access by another process under the same host login. Redact the
URL in diagnostics; never claim that stack names create confidentiality.

## Command surface

`pix help --all` and `docs/reference.md` are the command reference. The nine
groups are run, ls, rm, task, env, secret, setup, doctor, and reset, plus help and
version. `cmd/pix/root.go` owns dispatch. Removed commands receive an ordinary
unknown-command error, not compatibility aliases or retirement messages.

Setup chooses a missing model with the user, collects only relevant connection
details, and checks completed steps before applying anything. Keyless or
Gateway-authenticated environments must not get a personal-provider interview.
A launch reuses the same effective-document compiler as environment preview.

## Build and verify

Run commands from the repository root unless shown otherwise:

```bash
npm ci
(cd services/host && go build ./... && go test ./...)
(cd services/memory && go build ./... && go test ./...)
npm test
npx tsc --noEmit
bash scripts/gate.sh
```

Test the affected production caller, then run the relevant gate. For a docs-only
change, check links, examples, and existing documentation tests; do not invent
implementation-mirroring tests or rebuild images without a reason.

On a host with Docker and sbx, `make load` builds the matching launcher, runtime,
images, and manifest, then loads the agent image into sbx's separate image store.
`make run` starts the development session with live skills. An image baked before
an edit does not contain that edit, and a running sandbox keeps its creation
image. Recreate an idle sandbox to test a new image. Kit changes also need a new
sandbox; live skill changes can use `/reload`.

Inside a sandbox, build/test source locally but do not claim `make load` or host
OAuth UAT passed without host access. If syncing an extension into the live Pi
agent directory for a reload, report that separately from verifying a rebuilt
image. Local identities are `X.Y.(Z+1)-beta.g<sha7>[.dirty.<12hex>]`; every artifact
in a bundle must use the same identity. Release CI uses clean semver. `make load`
uses a worktree-scoped tag and may prune only that worktree's old templates.

## Host UAT

Use an explicitly separate `PIX_HOME` and record its stack id. Never borrow the
user's current home for cleanup or silently replace its trust receipts. Use real
Pix entry points, not a parallel script that bypasses the launcher.

- Exercise fresh setup, interrupted/resumed setup, create, attach, and cleanup.
- For prompt changes, test a real PTY: long input, arrows, backspace, paste,
  Ctrl-C exit 130, and restored terminal settings. Stub browser openers in tests.
- For inference changes, check configured provider paths and keyless Ollama as
  relevant; prove responses and costs, not just a model listing.
- For integrations, make a real read-only request through the Gateway. Tool
  discovery is not authentication proof. Check the result, not just transport.
- For embeddings, recall and inspect `memory_status`; configured is not healthy.
- Resume UAT must create, remove, then resume a saved conversation. Pix passes
  `--session-dir .pi-sessions` and translates `--resume SESSION` to Pi's
  `--session SESSION`; Pi's own `--resume` opens a picker.
- Verify only the test stack's sandboxes, containers, and registrations are
  removed. Retain useful evidence without credentials or private user content.

## Safety invariants

Load-bearing properties, each pinned by a real test. Preserve every one when
you touch the surface it names.

1. **PIX_HOME is the single root, with no XDG split.** `$PIX_HOME` when
   set, else `~/.pix`, and nothing else (not `$XDG_CONFIG_HOME`, not
   `$XDG_DATA_HOME`) influences where Pix resolves its files.
2. **`config.toml` and `secrets.env` use one named writer per operation.** There is
   no generic config mutation command in v2; a field changes only through
   the verb that owns it (`pix env default`, `pix setup`, `pix secret set`).
3. **An implicit launch requires a TTY.** Bare interactive `pix` runs setup
   first when this `PIX_HOME` has no config, then behaves as `pix run .` only
   after setup succeeds. Non-interactive stdin never creates or attaches a
   sandbox and never mutates host state as a side effect of a script or pipe.
4. **An existing sandbox is never force-removed or replayed into.** `pix rm`
   verifies the current sbx instance ID before removal; unknown sbx state
   fails closed. `--force` is a named-sandbox override only, and it never
   widens the `pix-*` namespace or authorizes removing a non-Pix sandbox.
5. **`pix rm` is scoped to `pix-*` sandboxes only**; it can never reach a
   sandbox it did not create in the current stack.
6. **A process claims liveness only by holding a reference lock bound to the
   recorded sbx instance ID**, never by a bare PID. The lock releases itself
   on normal exit, signal, or crash; PIDs remain human diagnostics only.
7. **`pix rm --orphans` requires five positive proofs**, not an absence of
   evidence: a fresh sbx listing, a `pix-*` name, a matching instance ID,
   zero reference locks, and no keep marker. Any unknown answer preserves
   the sandbox.
8. **Direct provider keys come from 1Password only** (`op://` references,
   never resolved to disk or stdout); missing `op` is fatal only when the
   selected environment needs direct key resolution. Keyless and
   Gateway-authenticated backends never trigger an irrelevant 1Password flow.
9. **Environment trust is HMAC-bound and stored outside the environment**
   (`~/.pix/.state/trust`), never inside the directory being approved. A
   changed fingerprint (any host-affecting fact: kit, workspace mounts, MCP
   command/URL, secret destinations, network expansion) refuses launch and
   names `pix env trust NAME`. Trust review defaults to No; `--yes`
   suppresses the prompt only, it never skips the fingerprint check.
10. **`pix-host`, packs, scored model routing, and the custom memory RPC are
    deleted, not merely hidden.** No code path reaches any of them; there is
    no `pack.toml`, no `routing.json`, no pix-owned top-level `memory` command,
    and no unsandboxed host-agent mode. A model is chosen by name
    (`--model`, then `[models].main`, then the shipped session preference
    OpenAI/Anthropic/Google among configured providers; no choice refuses rather than using Pi's stale default);
    nothing scores or auto-selects one.
11. **Memory is operated through MCP tools, never a private protocol.**
    `memory_recall`/`memory_remember`/`memory_forget`/`memory_observe`/
    `memory_stats`/`memory_status`/`memory_snapshot`/`memory_restore` are the
    whole surface; `/recall`, `/remember`, `/forget`, and the automatic
    recall/capture hooks all call the same Gateway-registered endpoint a
    model's own tool calls use.
12. **A launch approves exactly one thing on the user's behalf.** `sbx env
    create` prints its own plan and asks its own approval for a document
    Pix already composed, fingerprinted, and put through its own trust
    gate, and that text carries the token-bearing `pix-memory` URL. Pix
    answers that duplicate prompt internally, after its own gate, captures
    the create child's output, and shows a concise error on create failure.
    `--verbose` shows the captured diagnostic, bounded and credential-redacted.
    A later session or credential failure never prints a successful create plan. The interactive `sbx exec` session
    keeps ordinary stdio; the two children are told apart by their
    `SessionDeps` seam, never by sniffing argv.
13. **An environment with no host footprint is not gated.**
    `BillOfMaterials.Tier1()` is the canonical answer and every trust
    caller asks it: a zero-footprint environment (the generated `default`)
    is never prompted for and never causes a trust-state write.
14. **`pix run` reconciles machine-owned stack artifacts after an upgrade,
    and nothing else.** A bundle/manifest mismatch runs only the shared
    `machineSetup` composition; model selection interviews and `[[setup]]` apply hooks stay in `pix setup`,
    while ordinary launch still checks trust and refreshes scoped credentials, a foreign-owned container still
    refuses, and a failure restores the previous release record so the next
    run retries.
15. **Success words are earned by a probe.** `ready`/`verified` appear only
    after a post-mutation check; `pix doctor` never repairs, registers,
    restarts, or authenticates, and never prints `configured`/`enabled` as a
    verdict.

## Models, subagents, and extensions

Model selection is explicit: CLI `--model`, environment `[models].main`, then
the literal configured-provider default. A preset's frontmatter model wins over
the environment's `[agents]` binding; otherwise inherit the parent. No scoring.
The public catalog lives in `services/host/inference/catalog/models.json`.
Generated aliases reuse the pinned Pi catalog's costs, cache/context tiers, and
capabilities. Verify rates when bumping Pi; prices are estimates, never routing
inputs. Keep time-limited pricing notes current in `docs/reference.md`.

Prove subagent model identity from assistant response metadata, not self-report,
requested config, or giving a reviewer shell access. Test single, parallel, and
chain callers when changing the tool. See
[the extension design](docs/design/subagents-extension.md).

Extension factories must return promptly and guard load failures. Settle async
work on success and error, and close listeners on `session_shutdown`. Put
ambient declarations in `types/`; Pi tries to load every `.ts` in `extensions/`
as a factory. Use Pi's pinned API and its loader for runtime verification.

Display-only messages use `deliverAs: "nextTurn"`, not `steer`. Strip disposable
display messages by `customType` in the context hook, but never strip
`pix-recalled-context` or `pix-output-style`: both are append-only prompt context.
See `extensions/timestamps.ts` and the recall context tests.

A feature is complete only when its real caller is wired and integration-tested.
If delegating, bound a task by that caller and its acceptance test; do not hand a
subagent an architectural layer and assume another agent will connect it later.

## Shipping

Keep commits signed using the configured signing key. Before pushing, check the
PR's target branch explicitly; an integration-branch merge does not land on main.
A PR describes final behavior and actual validation, not abandoned plans. Keep
organization-specific evidence out of public PRs. Report host UAT limitations
honestly and inspect CI after pushing. A clean local gate does not prove a new
remote head is green.
