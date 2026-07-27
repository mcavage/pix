# Rename pi-stack to Pix and fix first-run reliability

This proposal turns `pi-stack` into `pix`, shortens the local feedback loop, and makes setup produce evidence instead of optimistic configuration.

**Status:** proposed  
**Branch:** `pix/rename-and-onboarding-audit`  
**Baseline:** `v0.0.47` / `e126a5c`  
**Scope:** repository, binaries, host services, image and kit, runtime paths, setup, doctor, MCP, Google Workspace, prompt size, tests, and user documentation

## Recommendation

Proceed with a clean Pix rename rather than a compatibility program. This has one user before launch, so preserving old binaries, state paths, sandbox names, environment variables, or service identities would add code that can be deleted instead.

The first advertised release should be `v0.1.0`. It should:

- install `pix` and `pix-host`;
- publish `docker.io/mcavage/pix:<version>` and use the renamed GitHub repository;
- write only Pix-named host and workspace state;
- use one readiness model for setup, status, doctor, onboarding, and run warnings;
- expose Google Workspace as the optional `pix gworkspace` integration while treating the external `gog` executable as an implementation detail;
- reduce the normal test loop from roughly 53 seconds to under 10 seconds, with a target below 5 seconds;
- fix the current `task ls` regression before the larger rename starts;
- reduce the cold prompt and preserve a stable provider-cache prefix;
- replace the README's competing paths with one prescriptive fresh-host path.

Two naming risks are accepted, not ignored:

1. `pix` is also the Cinnamon image viewer binary on some Linux distributions. The installer must run `command -v pix`, compare the result with its managed install path, and fail closed on a collision. Non-interactive installs require an explicit `PIX_FORCE_INSTALL=1`; they never silently shadow another binary. Doctor reports which `pix` is first on `PATH`.
2. Private repositories already use `pix` in names such as `gm-pix-pack`. That overlap is acceptable if Pix is the product name, but public docs must not imply those private repositories are part of the public distribution.

## Audit baseline

### Repository rename surface

On the `v0.0.47` baseline:

- 268 tracked files contain `pi-stack` or `PI_STACK`;
- there are about 4,004 lowercase `pi-stack` occurrences;
- there are about 443 `PI_STACK` occurrences;
- user-visible names appear in Go packages, binaries, the man page, help, installer, image metadata, kit metadata, sandbox names, task refs, launchd/systemd units, MCP registrations, extensions, tests, and docs.

This is not one blind replacement. Historical records and the external `gog` executable keep their real names; active Pix runtime surfaces do not.

### Test performance

Measured on the baseline:

```text
cd services/host && go test ./...   42.803s
node --test tests/*.test.mjs         9.680s
normal local total                  52.483s
```

The dominant causes are specific and fixable:

- `services/host/cmd/pi-stack/redrive_findings2_test.go` creates `sleep` through a shell without `exec`. Killing the shell leaves the child holding output pipes open, so each timeout pays a two-second `exec.Cmd.WaitDelay`. Nineteen tests account for about 40 seconds.
- `tests/pi-patches.test.mjs` performs real npm installs and takes about 9.6 seconds.
- `tests/monitor.test.mjs` uses wall-clock retry sleeps.
- `tests/memory-recall.test.mjs` waits three seconds to test a two-second threshold.
- several Go tests rebuild example binaries instead of sharing a package-level build.

### Prompt context size

The current cold turn carries roughly 110 KB, about 27,000 to 28,000 tokens, before normal conversation:

- project `AGENTS.md`: 48,575 bytes;
- ancestor `/Users/mcavage/dev/AGENTS.md`: 23,260 bytes;
- 39 skill catalog entries: about 17,454 bytes after pi formats name, description, and location;
- base prompt and default tool schemas: roughly 15,000 to 20,000 bytes;
- memory and knowledge recall: up to roughly 2,600 bytes per turn.

The largest project-owned block is a 15 KB launcher reference inside `AGENTS.md` that duplicates `pix help --all` and `docs/reference.md`. Skill descriptions also repeat long trigger-phrase lists that pi sends on every turn even though skill bodies load on demand.

More importantly, `extensions/memory-recall.ts` and `extensions/knowledge-recall.ts` append recall to `systemPrompt` in `before_agent_start`. That changes the system-prompt hash between turns and can invalidate provider prompt-prefix caching for the entire static prompt. `extensions/monitor.ts` already records `systemPromptBytes`, `systemPromptHash`, `toolSchemaBytes`, and estimated tokens, so the project can measure this without new wire instrumentation.

### Setup and readiness

The code has solid probe components, but each command assembles a different definition of ready:

- setup checks watcher, embedding, and bridge models, but treats all as optional and omits MCP from its final summary;
- doctor checks watcher and embedding models, but not the bridge model used by the sandbox;
- status reports MCP configuration but not Ollama readiness;
- onboarding host state reports configured values, not verified state;
- run writes the Ollama bridge model and wires MCP without proving either is usable.

The result is predictable: setup can exit successfully after an explicit `--pull-models` request did no work, or after MCP registration failed and config still says the server is enabled.

### Google Workspace

There are multiple writers for one integration:

- the current `gog setup` code performs the correct transaction: validate, authorize, prove headless tools work, register, then save with rollback;
- general setup/onboard can save an account and MCP entry without completing OAuth, then best-effort register a broken server;
- the Makefile repeats registration and runs a weaker health check.

The problem is the product flow, not the local OAuth JSON filename. Pix should make Google Workspace optional, own the whole transaction, and hide the external `gog` tool behind Google-branded CLI and status output.

### Task metadata regression

The reported messages are a current-code regression, not damaged legacy metadata:

```text
pi-stack task ls: skipping modelchange.json: metadata for "modelchange" has no profile
pi-stack task ls: skipping pixrename.json: metadata for "pixrename" has no profile
No tasks for this repo.
```

`loadResolvedConfig()` now returns an empty profile because profiles were removed. `runTaskNew` writes that empty value. `hardenTaskMeta` still rejects empty profiles even though `taskSandboxName` and `legacyTaskSandboxName` now ignore profile entirely. Every task created by current code can therefore be hidden by `task ls`.

## Product contract

### Names after cutover

**Rename now:**

- repository and product: `pix` / Pix;
- launcher: `pix`;
- host binary: `pix-host`;
- Go module: use a qualified module path under the renamed repository;
- image: `docker.io/mcavage/pix:<pinned-version>`;
- kit name and display name;
- package metadata, OCI labels, release assets, man page, help, docs, and examples;
- launchd label and systemd unit;
- new sandbox, task-ref, config, state, data, and workspace-state names.

**Keep unchanged:**

- ports 11435, 11436, and 11437;
- third-party executable names such as the external `gog` binary; Pix registers and displays that integration as `google-workspace`;
- provider service names;
- skill, agent, capability, and model-intent names;
- `.pi-sessions/`;
- `pi-kit/` as the repository directory, because old version-pinned kit URLs reference it;
- persisted custom message types such as `pi-stack-todo-cleared` unless both names can be read safely.

### Clean cutover policy

Before switching the repository and release artifacts:

1. stop and uninstall the current host service;
2. remove current pi-stack sandboxes and task sandboxes;
3. keep a final `pi-stack state backup` only as an emergency archive;
4. remove the old launcher binaries and old config/state/data directories;
5. rename the repository and publish Pix from a clean host state.

Pix does not read old paths, discover old sandbox prefixes, accept old environment variables, or ship a `pix migrate` command. Tests should fail on accidental legacy identifiers except in historical changelog entries, upstream reports, and the one-time cutover checklist.

## Architecture proposal

### 1. Clean single-user cutover

Do not build migration machinery. Add a maintainer-only cutover checklist that verifies the old service is stopped, old sandboxes are removed, an emergency backup exists, old files are deleted, and the new install starts cleanly. This checklist runs once before Pix is advertised.

### 2. One readiness snapshot

Create a typed `ReadinessSnapshot` used by setup, doctor, status, onboarding host state, and run warnings.

Each check has:

```go
type ReadinessCheck struct {
    Axis        string
    Requirement Requirement // core, requested, optional
    Verdict     Verdict     // ready, todo, unverifiable, denied
    Evidence    string
    Fix         []string
}
```

The snapshot should cover:

- provider keys and 1Password state;
- sbx presence and version/capabilities;
- Ollama API reachability at the effective endpoint;
- watcher, embedding, and bridge model tags from one parsed `/api/tags` response;
- two separate Ollama network facts: host API readiness at the effective endpoint and sandbox bridge readiness when a sandbox already exists. Doctor and status must not create a sandbox to run a probe. An existing sandbox runs a bounded in-sandbox request to `host.docker.internal`; without one, sandbox reachability is `unverifiable` and optional until `run` performs the post-create probe. Bind-address inference may supply remediation context, but never a ready verdict;
- memory and knowledge services on resolved ports;
- each configured or newly requested MCP server: registered, spawnable/authorized, and attached where attachment is meaningful;
- Google Workspace account, the external `gog` CLI, hardened headless tool listing, and gateway registration;
- active pack and knowledge state.

Setup becomes a state machine:

1. **Parse:** validate arguments and proposed values.
2. **Inventory:** bounded, read-only probes produce one snapshot.
3. **Gate:** block on missing core requirements and newly requested integrations, not stale optional config.
4. **Mutate:** keys, config, pack, MCP, knowledge, and identity in a defined order.
5. **Consent:** pull each confirmed-missing model independently.
6. **Verify:** re-probe every touched axis.
7. **Report:** render only post-mutation evidence and return a meaningful exit code.
8. **Handoff:** start or reattach the sandbox only after the report.

Exit codes are part of the contract:

- `0`: every core and requested axis is ready;
- `1`: at least one core or requested axis is verified as `todo` or `denied`;
- `2`: invalid command usage;
- `3`: at least one core or requested axis is `unverifiable` and none is verified failed.

Every non-ready check carries non-empty evidence and at least one exact recovery command. Explicit requests such as `--pull-models` or `--google-workspace` must not exit zero when nothing was completed. Golden-output tests cover each exit code and the rendered evidence/fix pair.

### 3. One optional Google Workspace transaction

Use `gworkspace` because it matches the existing capability/skill name and Google's current **Google Workspace** branding. Do not expose the retired “G Suite” name or the implementation-specific `gog` name in Pix commands.

Public surface:

- CLI noun: `pix gworkspace setup|status|disable`;
- setup flag: `pix setup --google-workspace`;
- config key: `google_workspace_account`;
- MCP registration/display name: `google-workspace`;
- capability and skill: `gworkspace`;
- implementation dependency: the external `gog` executable.

Google Workspace is optional and lives outside the primary install path. The working public flow is:

```bash
brew install openclaw/tap/gogcli
pix gworkspace setup --account you@example.com
pix gworkspace status
```

`pix gworkspace setup` owns the awkward parts:

1. Verify the supported external CLI version and show the exact Homebrew command when it is absent or stale.
2. If credentials already exist, reuse them. Otherwise offer two explicit routes: select an existing Desktop OAuth client JSON, or launch gog's guided Google Cloud setup. The guided route verifies `gcloud` first, gives `brew install --cask google-cloud-sdk` plus `gcloud auth login` when needed, runs the supported `gog auth setup <account> --gcloud-project <id> --create-project --enable-apis --open-console` flow, then resumes after the user downloads the Desktop client JSON.
3. Import the client and authorize the minimum services Pix exposes: Gmail, Calendar, Drive, Docs, Sheets, and Contacts. Use gog's current `auth setup ... --credentials ... --login` route when supported, with its documented `auth credentials` plus `auth add` fallback for older supported versions.
4. Explain the one unavoidable Google step: publish the personal OAuth app from Testing to In production so refresh tokens do not expire after seven days. This does not submit the app for verification. Print the exact Google Cloud Audience URL and wait for confirmation.
5. Verify `gog auth doctor --check`, then run `mcp --list-tools` through the exact headless environment and safety flags the sbx gateway will use.
6. Register `google-workspace` only after the headless proof succeeds, save config last, and roll registration back if save fails.

Rules:

- the existing hardened authorization implementation becomes the only writer of Google Workspace account, OAuth, MCP registration, and config state;
- general `pix setup --google-workspace` calls that same transaction and never duplicates it;
- interactive setup prompts for the account and route; non-interactive setup requires `--account` and either `--credentials` or a previously imported client;
- account without a usable client performs no mutation;
- the registered server is read-only, Gmail-no-send, non-interactive, and wraps returned content as untrusted;
- `pix gworkspace status` is a read-only rendering of the same checks doctor uses: dependency version, stored client, token health, exact headless tool list, gateway registration, and sandbox availability;
- `pix gworkspace disable` removes Pix config and gateway registration but leaves gog's user-owned OAuth credentials intact;
- normal Pix output says Google Workspace, not gog; the external binary name appears only in dependency installation and low-level troubleshooting;
- Makefile targets delegate to Pix instead of rebuilding registration logic in shell.

### 4. Prescriptive README

The README should have one primary path and push raw `sbx run` usage into an advanced section.

The opening path should be:

1. Install Docker Sandboxes nightly using the command published by `docker/sbx-releases` (Docker Desktop is not required):

   ```bash
   brew install docker/tap/sbx@nightly
   sbx version
   ```

2. Install and authenticate 1Password CLI:

   ```bash
   brew install 1password-cli
   op signin
   op whoami
   ```

3. Install Ollama and the configured defaults:

   ```bash
   brew install ollama
   brew services start ollama
   ollama pull qwen3.5:9b
   ollama pull nomic-embed-text
   curl -fsS http://127.0.0.1:11434/api/tags >/dev/null
   ```

   The single `qwen3.5:9b` pull satisfies both watcher and bridge roles on a fresh install. An anti-drift test must compare these documented tags with `config.DefaultMemoryWatcherModel`, `config.DefaultMemoryEmbedModel`, and `config.DefaultOllamaBridgeModel`.

4. Install GitHub CLI and wire its host token into sbx for push and PR workflows:

   ```bash
   brew install gh
   gh auth login
   sbx secret set -g github -t "$(gh auth token)"
   ```

5. Install Pix and ensure `~/.local/bin` is on `PATH`.
6. Run `pix setup`.
7. Run `pix doctor` and show literal expected output for core checks, plus how optional and unverifiable axes appear.
8. Run `pix run` in a project.
9. Put Google Workspace in an optional advanced section that explains how to obtain a Desktop OAuth client before showing `pix gworkspace setup`.
10. Explain daily conventions: status, run/reattach, replace after create-time changes, plan/build/deliver, model switching, memory commands, task workspaces, update, backup, and troubleshooting.

Do not tell curl-installed users to run Makefile targets. Commands in the primary path must work without a repository checkout.

### 5. Test strategy

Keep one meaningful PR gate, but remove artificial time.

**Fast local and PR gate, target under 10 seconds and preferably under 5:**

```bash
cd services/host && go test ./...
node --test tests/*.test.mjs
npx --no-install tsc --noEmit
bash scripts/check-open-core.sh
bash scripts/check-rename.sh
```

Changes required:

- use `exec sleep` in timeout fixtures so process cancellation closes pipes immediately;
- inject retry and timeout durations instead of sleeping against production constants;
- replace the normal real-npm patch test with a local fixture or cached package artifact;
- keep one real npm resolution/install smoke test in a release or scheduled job;
- build example helper binaries once per package run;
- add `"type": "module"` if it is compatible with all extension-loading tests;
- print package/test timing in CI and fail only on a deliberately chosen regression budget.

**Release gate, target under 25 seconds on warm CI:**

```bash
cd services/host && go vet ./... && go test -race ./...
npx --no-install tsc --noEmit
node --test tests/*.test.mjs
# real npm patch/install smoke
# image load-check
# fresh-host setup smoke
```

Never use `-short` to hide correctness tests from pull requests. Separate tests by dependency class, not by importance.

### 6. Compile and budget agent context

Treat always-on prompt space as a versioned build artifact with a byte budget.

The first pass does not need retrieval infrastructure:

1. Return recall through `before_agent_start.message`, the pi API's persistent model-visible custom-message channel, instead of rewriting `systemPrompt`. Tag it `pix-recalled-context`, set `display: false`, and preserve provenance plus untrusted-content labels. Keep injected recall messages append-only so each later provider request retains the earlier request as a cacheable prefix. Deduplicate by memory ID/content hash and emit only net-new or corrected rows, capped at 1 KB per user turn; normal session compaction bounds long-run growth. Do not filter prior recall messages from later requests, because removing an earlier message would move the cache divergence point back to that turn. Add serialized provider-payload tests proving the system-prompt hash and prior message prefix stay constant while new recall is appended.
2. Cut skill frontmatter descriptions to a 200-character trigger that distinguishes the skill. Keep full instructions in `SKILL.md`, loaded only when selected.
3. Remove CLI reference prose and historical rationale from always-on `AGENTS.md`; leave one-line pointers to `pix help --all`, `docs/reference.md`, and design docs.
4. Separate authored context from delivered context. `pix context compile` generates the root `AGENTS.md` from a source document with `always`, `on-demand`, and `reference` sections. Pi continues loading the standard `AGENTS.md`; no custom resource-loader behavior is required.
5. Add `pix context show --segments` and `pix doctor --context` to report project context, ancestor context, skill catalog, extension guidelines, tool schemas, recall payload, total bytes, and system-prompt hash stability.
6. Warn when an ancestor `AGENTS.md` dominates the budget. The repository cannot silently rewrite host-owned ancestor instructions.

Initial budgets:

- generated project `AGENTS.md`: at most 8 KB;
- skill catalog: at most 8 KB;
- project-owned extension prompt snippets/guidelines: at most 2 KB;
- project-owned always-on context: at most 18 KB;
- cold turn including external ancestor context and default tools: target at most 48 KB;
- memory plus knowledge recall: at most 1 KB of net-new context per user turn, append-only until compaction, and never part of the system-prompt hash.

Safety and operating invariants remain always-on. Tests assert the generated context still says: no root/sudo in the sandbox; host mode runs on the real machine; `make load`/`make run` are host-only; public code cannot contain company-specific data; secret files contain `op://` refs only; extension factories must settle; declaration files never live in `extensions/`; verify before claiming completion.

Do not add a `context_lookup` tool in the first pass. The compiled pointer index plus the existing `read` tool is cheaper and easier to verify.

## Implementation plan

### Wave 0: restore velocity and trust

1. Fix `task ls` by removing the obsolete non-empty-profile invariant from `hardenTaskMeta`.
2. Add an end-to-end task metadata round-trip test using the real empty profile produced by current code.
3. Replace shell `sleep` fixtures with `exec sleep` and verify the Go suite drops from about 43 seconds to about 5 seconds.
4. Inject Node test clocks and isolate the real npm install smoke.
5. Add CI timing output and record the new baseline.
6. Move dynamic recall out of the changing system prompt and prove prompt-hash stability.
7. Trim skill descriptions and project `AGENTS.md`, then add context segment reporting and byte-budget tests.
8. Add `pi-stack context compile` before rename work begins, then rename the command mechanically to `pix context compile`. This avoids rewriting tens of kilobytes of always-on reference prose that will be removed immediately afterward.

**Gate:** all existing tests pass; `task new` followed by `task ls` lists the task without warnings; local Go plus Node tests complete under 10 seconds; project-owned always-on context is at most 18 KB; the cold-turn target is at most 48 KB; system-prompt hash stays stable across a recorded multi-turn recall scenario.

### Wave 1: build the shared readiness model under the old name

1. Introduce `ReadinessSnapshot`, reason enums, and requirement levels.
2. Normalize resolved service ports and effective Ollama endpoint handling.
3. Use one model-role list for watcher, embedding, and bridge.
4. Use one MCP evidence builder for registration, auth/spawnability, and attachment.
5. Render the same snapshot in setup, doctor, status, onboarding host state, and run warnings.
6. Add parity tests proving equal inputs produce equal verdicts across commands.

**Gate:** setup cannot report green when doctor reports a required axis broken; `--pull-models` with Ollama down fails with an exact remediation; configured MCP registration failure appears in the final summary and affects exit status when requested.

### Wave 2: make setup and Google Workspace transactional

1. Refactor setup into inventory, gate, mutate, verify, report, and handoff phases.
2. Make post-mutation verification the only source of success messages.
3. Route all Google Workspace writes through the existing hardened authorization transaction.
4. Add `gworkspace setup|status|disable`, `--google-workspace`, the `google_workspace_account` config key, and the `google-workspace` MCP display/registration name.
5. Support the current guided `gog auth setup` route plus the documented credentials/add fallback, including the OAuth publishing reminder that prevents seven-day token expiry.
6. Remove duplicate Makefile registration and health logic.
7. Keep the external `gog` executable name out of normal Pix output.

**Gate:** repeated setup is idempotent; interrupted setup reports exact partial progress; account-only non-interactive setup performs no mutation; headless zero-tool authorization can never render ready; disable removes only Pix-owned state; Google Workspace remains absent from the default setup path unless explicitly requested.

### Wave 3: perform the hard rename

1. Rename the Go module and imports.
2. Move `cmd/pi-stack` to `cmd/pix`, including embedded templates and man page.
3. Rename binaries, config/state/data paths, workspace state, sandbox names, task refs, service identities, release assets, type shims, themes, package metadata, OCI labels, and in-image paths.
4. Update exact process invocation detection in both TypeScript and Go.
5. Rename all public Google Workspace surfaces while retaining `gog` only where code invokes the external dependency.
6. Add `scripts/check-rename.sh` with a small allowlist for historical text and the maintainer cutover checklist.

**Gate:** build, unit tests, typecheck, open-core check, and rename allowlist pass; the product does not read or emit old runtime names; Google Workspace is the only integration name visible in Pix output.

### Wave 4: cut over and publish artifacts

1. Run the maintainer cutover checklist: stop/uninstall old services, remove old sandboxes, take an emergency backup, and delete old state and binaries.
2. Rename the GitHub repository.
3. Reserve and publish the new Docker Hub repository before changing the kit image.
4. Update kit metadata, release workflow, installer URL, checksums, and raw catalog URLs.
5. Install Pix from scratch and run setup on clean state.
6. Publish `v0.1.0` only after fresh-host verification.

**Gate:** the old service and sandboxes are gone; a fresh Pix install owns exactly one process per service port; setup, doctor, run, task, Google Workspace opt-in, memory, and MCP smoke checks pass.

### Wave 5: rewrite and verify documentation

1. Replace README quickstart with the prescriptive path above.
2. Update reference, man page, setup/Google Workspace/memory docs, contribution docs, examples, and agent context.
3. Add anti-drift tests for verbs, config keys, readiness axes, model defaults, and documented prerequisites.
4. Execute every primary README command on a clean macOS host or disposable VM.
5. Record expected output and specific recovery commands.

**Gate:** a fresh user can install from the README without cloning the repository; `pix doctor` verifies keys, all configured Ollama roles, memory, and every configured MCP integration; the optional guide proves `pix gworkspace setup|status` end to end.

## Cutover acceptance criteria

### Rename and cutover

- `pix version`, `pix help`, `pix doctor`, `pix run`, `pix task`, and `pix state` contain no accidental old branding.
- Pix uses only `pix-*` sandboxes, `PIX_*` environment variables, `.pix/` workspace state, Pix service identities, and Pix config/state/data paths.
- the cutover checklist leaves no old service, sandbox, launcher binary, or active state directory.
- `.pix/host-state.json` is ignored in user repositories.
- historical changelog/upstream text remains intact and allowlisted.

### Setup

- every success line comes from a post-mutation probe;
- setup, doctor, status, onboarding, and run use the same verdict for the same evidence;
- all three configured Ollama roles are checked;
- host Ollama readiness is always tested; sandbox-to-Ollama reachability is tested from an existing or newly created sandbox and remains explicitly optional/unverifiable before one exists;
- requested but unregistered/unverified MCP servers return exit `1`, while a requested axis that cannot be probed returns exit `3`;
- optional inherited integrations report problems without preventing unrelated key repair;
- no unverifiable state is rendered as absent or ready.

### Google Workspace

- Google Workspace is explicitly optional and absent from the default setup path;
- one code path owns auth, headless proof, registration, config save, and rollback;
- `pix gworkspace status` is read-only and checks the exact headless gateway path;
- guided setup supports both a new Google Cloud project and an existing Desktop OAuth client;
- non-interactive account-only setup performs no mutation;
- `pix gworkspace disable` removes only Pix-owned registration/config;
- normal output never calls the integration gog or G Suite;
- the runtime remains Gmail-no-send, read-only, and wraps external content as untrusted.

### Tests

- the task profile regression has end-to-end coverage;
- normal local tests complete under 10 seconds on the current development machine;
- timeout tests cancel process trees without two-second pipe waits;
- real package installation and image checks remain in release coverage;
- CI reports timing trends so performance regressions are visible.

### Prompt context

- project-owned always-on prompt content is at most 18 KB and the measured cold turn is at most 48 KB;
- skill catalog entries are capped and full skill bodies remain on demand;
- dynamic memory/knowledge recall does not change the system-prompt hash or remove earlier messages from the provider prefix;
- repeated memory IDs/content hashes are not reinjected, and net-new recall stays within 1 KB per user turn;
- monitor fixtures prove the system hash and append-only prefix are stable across at least 95% of normal turns;
- must-keep safety instructions survive compilation;
- `pix doctor --context` attributes every segment so external ancestor growth is visible.

## Risks and controls

- **Global replacement corrupts history or external dependency names.** Use staged commits and an explicit allowlist for historical text plus the external `gog` executable.
- **An old service survives the hard cutover.** Run the cutover checklist before installing Pix and verify one PID per service port.
- **Short `pix` binary collides with another package.** Installer preflight reports the existing resolved path and fails closed. Non-interactive replacement requires `PIX_FORCE_INSTALL=1`; doctor reports the first resolved binary.
- **Hardcoded repository URLs break at rename.** Test every install, raw catalog, kit, badge, and release URL directly after the GitHub rename.
- **Docker Hub has no repository rename redirect.** Reserve and publish the new image before the kit cutover.
- **Dynamic recall ruins input-prefix caching.** Keep recall in append-only custom context messages, deduplicate repeated memories, assert a stable system-prompt hash and message prefix, and inspect serialized provider payloads in tests.
- **Setup refactor broadens scope during rename.** Land it in isolated, testable waves before changing public writes.

## Deliberate non-goals

- changing service ports;
- renaming unrelated skills, agents, capabilities, or routing intents;
- redesigning memory or knowledge behavior during the rename;
- preserving old runtime compatibility;
- adding migration commands or dual-read state.

## Deferred TODO: memory quality

Memory still does not feel right. This project changes only how recalled context is transported so the system prompt remains cacheable; it does not claim to improve capture, ranking, scope, staleness, or recall UX.

After Pix, setup, Google Workspace, docs, test speed, and prompt caching land, start a separate memory discovery and iteration. We will define the failure cases, evaluation method, and product behavior together then. No memory-quality implementation belongs in the current waves.

## Owner decisions recorded

- The target product, repository, and CLI name is Pix / `pix`.
- The current main baseline is `v0.0.47`; this branch was reset to that commit before planning.
- The implementation should fix setup, optional Google Workspace, docs, test speed, task metadata, and prompt caching as part of the same program.
- This is a hard cutover for one pre-launch user. No migration or compatibility layer is required.
- Memory quality is a separate TODO after this program.
