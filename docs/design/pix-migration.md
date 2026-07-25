# Rename pi-stack to Pix and fix first-run reliability

This proposal turns `pi-stack` into `pix`, shortens the local feedback loop, and makes setup produce evidence instead of optimistic configuration.

**Status:** proposed  
**Branch:** `pix/rename-and-onboarding-audit`  
**Baseline:** `v0.0.47` / `e126a5c`  
**Scope:** repository, binaries, host services, image and kit, runtime paths, setup, doctor, MCP, gog, tests, and user documentation

## Recommendation

Proceed with the Pix rename, but treat it as a versioned migration rather than a global search and replace.

The first Pix release should be `v0.1.0`. It should:

- install `pix` and `pix-host`;
- publish `docker.io/mcavage/pix:<version>` and use the renamed GitHub repository;
- write new host state under Pix paths;
- keep bounded compatibility for existing pi-stack config, processes, sandboxes, workspace state, environment variables, and MCP registrations;
- run an explicit, lock-aware migration instead of moving live state on first access;
- use one readiness model for setup, status, doctor, onboarding, and run warnings;
- make gog setup a single idempotent transaction;
- reduce the normal test loop from roughly 53 seconds to under 10 seconds, with a target below 5 seconds;
- fix the current `task ls` regression before the larger rename starts;
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

This is not one mechanical rename. Some old names must remain as compatibility fixtures or historical records.

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

### Gog

There are multiple writers for one integration:

- `gog setup` performs the correct transaction: validate, authorize, prove headless tools work, register, then save with rollback;
- general setup/onboard can save `gog_account` and `mcp = ["gog"]` without completing OAuth, then best-effort register a broken server;
- the Makefile repeats registration and runs a weaker health check.

The repository-local OAuth client file is named `gwork.json`. It is excluded through `.git/info/exclude`, is currently mode `0644`, and contains an OAuth client secret. The new flow must never copy or print its contents. It should warn on broad source permissions, import through the existing secure snapshot path, and tell the user the source file can be removed after successful import.

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
- MCP server names such as `gog`, `slack`, `notion`, and `atlassian`;
- provider service names;
- skill, agent, capability, and model-intent names;
- `.pi-sessions/`;
- `pi-kit/` as the repository directory, because old version-pinned kit URLs reference it;
- persisted custom message types such as `pi-stack-todo-cleared` unless both names can be read safely.

### Compatibility policy

Compatibility falls into three classes.

**Permanent read compatibility:**

- discover and manage old `pi-stack-*` sandboxes;
- read workspace `.pi-stack/` state as well as `.pix/`;
- exclude both `refs/pi-stack/*` and `refs/pix/*` from task containment checks;
- recognize persisted session/custom message markers that contain the old name.

These names live outside the install and may outlast any deprecation window.

**Two-minor-release compatibility:**

- old environment variables, with `PIX_*` taking precedence and conflicts reported;
- old executable basenames where ownership checks need to recognize prior installs;
- old config/state/data locations;
- old launchd/systemd identities;
- old theme and generated marker names;
- old MCP registrations, reported with an exact migration command.

**Historical text:**

Do not rewrite old changelog entries or upstream issue reports where `pi-stack` is historically correct. Add them to a checked-in rename allowlist.

## Architecture proposal

### 1. Explicit state migration

Add `pix migrate` and make it safe to run more than once.

The command must:

1. inventory old and new paths, services, sandboxes, registrations, and environment overrides without mutation;
2. support `--dry-run` and print paths and actions only;
3. stop the old managed or lazy serve process through its supervisor;
4. acquire old lock paths and refuse to proceed while old processes or locks remain;
5. move, never copy, config, state, data, memory databases, refs-only secret files, and trust state;
6. preserve file modes, especially `0600` secret-reference files;
7. leave pack trust fingerprints unchanged so changed host-exec commands re-gate;
8. install the new login service and remove both old service identities;
9. re-register MCP servers so absolute host-binary and `op-refs.env` paths are current;
10. verify one daemon per port and run the same readiness snapshot used by doctor;
11. report old-prefix sandboxes and offer explicit cleanup without deleting them automatically.

`pix run`, `pix status`, and `pix doctor` detect old-only state and print the same instruction: `Old pi-stack state detected. Run 'pix migrate --dry-run' to preview, then 'pix migrate'.` `run` refuses to create new Pix state until migration completes; status and doctor remain read-only.

Migration writes a journal in the old state directory before its first move. Each journal entry records a planned source, destination, precondition, and completion state. File moves use same-filesystem atomic rename where possible. Cross-filesystem moves use copy-to-temporary, fsync, atomic destination rename, source removal, then journal completion. SQLite databases move with their WAL and SHM files while the daemon is stopped. An injected failure after any step must be recoverable by re-running the command. A lock refusal prints the owning PID and the exact `pix serve stop` or legacy-service removal command.

Do not lazily move SQLite databases or secret files from a path resolver. A live daemon can split a SQLite database from its WAL, and separate old/new lock paths do not serialize access.

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
- gog CLI, OAuth account, hardened headless tool listing, and gateway registration;
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

Every non-ready check carries non-empty evidence and at least one exact recovery command. Explicit requests such as `--pull-models` or `--mcp gog` must not exit zero when nothing was completed. Golden-output tests cover each exit code and the rendered evidence/fix pair.

### 3. One gog transaction

Make the existing hardened `gogSetup` implementation the only writer of gog account, OAuth, MCP registration, and config state.

Gog is an advanced integration, not a prerequisite for the primary install. Its documentation starts with the missing prerequisite: create or obtain a Google Cloud **Desktop OAuth client**, download its JSON, and keep it outside a repository. For the current development host that file is `gwork.json`; public examples use an explicit user path.

The public flow becomes:

```bash
brew install gog
chmod 600 ~/Downloads/gwork.json
pix gog setup --account you@example.com --credentials ~/Downloads/gwork.json
pix gog status
```

Rules:

- general `pix setup` may call the same transaction, but may not duplicate its state changes;
- account without credentials is incomplete and must not save or register anything;
- missing values prompt on a TTY and fail with an exact command in non-interactive mode;
- auth must prove the hardened read-only, no-send, untrusted-content-wrapped headless server returns tools before config is saved;
- registration occurs before save and rolls back on save failure;
- `pix gog status` is a read-only rendering of the same readiness checks doctor uses;
- Makefile targets delegate to Pix instead of rebuilding gog registration logic in shell;
- after successful import, warn if the original OAuth JSON has broad permissions and tell the user it can be removed;
- never print, diff, back up, or migrate OAuth JSON contents.

### 4. Prescriptive README

The README should have one primary path and push raw `sbx run` usage into an advanced section.

The opening path should be:

1. Install Docker Desktop.
2. Install Docker Sandboxes nightly using the command published by `docker/sbx-releases`:

   ```bash
   brew install docker/tap/sbx@nightly
   sbx version
   ```

3. Install and authenticate 1Password CLI:

   ```bash
   brew install 1password-cli
   op signin
   op whoami
   ```

4. Install Ollama and the configured defaults:

   ```bash
   brew install ollama
   brew services start ollama
   ollama pull qwen3.5:9b
   ollama pull nomic-embed-text
   curl -fsS http://127.0.0.1:11434/api/tags >/dev/null
   ```

   The single `qwen3.5:9b` pull satisfies both watcher and bridge roles on a fresh install. An anti-drift test must compare these documented tags with `config.DefaultMemoryWatcherModel`, `config.DefaultMemoryEmbedModel`, and `config.DefaultOllamaBridgeModel`.

5. Install GitHub CLI and wire its host token into sbx for push and PR workflows:

   ```bash
   brew install gh
   gh auth login
   sbx secret set -g github -t "$(gh auth token)"
   ```

6. Install Pix and ensure `~/.local/bin` is on `PATH`.
7. Run `pix setup`.
8. Run `pix doctor` and show literal expected output for core checks, plus how optional and unverifiable axes appear.
9. Run `pix run` in a project.
10. Put gog in an optional advanced section that explains how to obtain a Desktop OAuth client before showing the setup command.
11. Explain daily conventions: status, run/reattach, replace after create-time changes, plan/build/deliver, model switching, memory commands, task workspaces, update, backup, and troubleshooting.

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
# fresh-host migration and setup smoke
```

Never use `-short` to hide correctness tests from pull requests. Separate tests by dependency class, not by importance.

### 6. Compile and budget agent context

Treat always-on prompt space as a versioned build artifact with a byte budget.

The first pass does not need retrieval infrastructure:

1. Move memory and knowledge recall out of the changing system-prompt suffix into a stable, model-visible turn-context channel supported by pi. Preserve provenance, prompt-injection labels, and tool behavior. Add a multi-turn test proving the system-prompt hash stays constant while recall content changes.
2. Cut skill frontmatter descriptions to a 200-character trigger that distinguishes the skill. Keep full instructions in `SKILL.md`, loaded only when selected.
3. Remove CLI reference prose, historical rationale, and migration matrices from always-on `AGENTS.md`; leave one-line pointers to `pix help --all`, `docs/reference.md`, and design docs.
4. Separate authored context from delivered context. `pix context compile` generates the root `AGENTS.md` from a source document with `always`, `on-demand`, and `reference` sections. Pi continues loading the standard `AGENTS.md`; no custom resource-loader behavior is required.
5. Add `pix context show --segments` and `pix doctor --context` to report project context, ancestor context, skill catalog, extension guidelines, tool schemas, recall payload, total bytes, and system-prompt hash stability.
6. Warn when an ancestor `AGENTS.md` dominates the budget. The repository cannot silently rewrite host-owned ancestor instructions.

Initial budgets:

- generated project `AGENTS.md`: at most 8 KB;
- skill catalog: at most 8 KB;
- project-owned extension prompt snippets/guidelines: at most 2 KB;
- project-owned always-on context: at most 18 KB;
- cold turn including external ancestor context and default tools: target at most 48 KB;
- memory plus knowledge recall: at most 2 KB and never part of the system-prompt hash.

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

### Wave 2: make setup and gog transactional

1. Refactor setup into inventory, gate, mutate, verify, report, and handoff phases.
2. Make post-mutation verification the only source of success messages.
3. Route all gog writes through `gogSetup`.
4. Add `gog status`.
5. Remove duplicate Makefile registration and health logic.
6. Add secure source-permission guidance for `gwork.json` and preserve the existing snapshot hardening.

**Gate:** repeated setup is idempotent; interrupted setup reports exact partial progress; account-only gog setup performs no mutation; headless zero-tool gog can never render ready.

### Wave 3: add rename compatibility before changing writes

1. Add new path resolvers with old-path read fallback.
2. Add exact dual-basename ownership checks.
3. Discover both old and new sandbox prefixes.
4. Read both workspace directories and harden both against symlinks.
5. Exclude both task-ref namespaces permanently.
6. Recognize both service identities and stop the old supervisor before starting the new one.
7. Read both environment prefixes and set both where old sandbox images need them.
8. Add the rename migration engine behind the existing `pi-stack migrate --dry-run` development entry point and seed full old-state fixtures. Do not flip writes yet.

**Gate:** the existing `pi-stack` binary operating on a full old-state fixture can inventory and verify it without mutation; old sandboxes remain visible and removable; no old database or secret file is copied.

### Wave 4: internal rename

1. Rename the Go module and imports.
2. Move `cmd/pi-stack` to `cmd/pix`, including embedded templates and man page.
3. Rename binaries, release assets, type shims, themes, package metadata, OCI labels, and in-image paths.
4. Update exact process invocation detection in both TypeScript and Go.
5. Add `scripts/check-rename.sh` with an allowlist for intentional legacy strings.

**Gate:** build, unit tests, typecheck, open-core check, and rename allowlist pass; no accidental historical or compatibility fixture rewrite.

### Wave 5: flip user-facing writes and publish artifacts

1. Complete `pix migrate`, including journaled moves, service replacement, MCP re-registration, verification, and resume after interruption.
2. Emit `pix-*` sandbox names, `.pix/` workspace state, `refs/pix/*`, and Pix config/state/data paths.
3. Install `com.pix.serve` / `pix-serve.service` only after removing the legacy service.
4. Register MCP servers against `pix-host` and the new refs path.
5. Rename GitHub repository and preserve its redirect.
6. Reserve and publish the new Docker Hub repository before changing the kit image.
7. Update kit metadata, release workflow, installer URL, checksums, and raw catalog URLs.
8. Keep frozen old image tags and the old install URL under owner control.
9. Publish `v0.1.0`.

**Gate:** CI runs a real migration on seeded old state and injects a process failure after every journal step; every retry finishes with one authoritative state tree, no split database/WAL files, current MCP argv, and exactly one daemon per port. Fresh install passes. A new launcher plus an old sandbox image still supports memory and monitor environment wiring.

### Wave 6: rewrite and verify documentation

1. Replace README quickstart with the prescriptive path above.
2. Update reference, man page, setup/gog/memory docs, contribution docs, examples, and agent context.
3. Add anti-drift tests for verbs, config keys, readiness axes, and documented prerequisites.
4. Execute every primary README command on a clean macOS host or disposable VM.
5. Record expected output and specific recovery commands.

**Gate:** a fresh user can install from the README without cloning the repository; `pix doctor` verifies keys, all configured Ollama roles, memory, gog, and every configured MCP integration.

## Cutover acceptance criteria

### Rename and migration

- `pix version`, `pix help`, `pix doctor`, `pix run`, `pix task`, and `pix state` contain no accidental old branding.
- `pix status` lists both `pix-*` and legacy `pi-stack-*` sandboxes.
- `pix migrate --dry-run` performs no writes.
- migration stops old supervisors, moves state once, re-registers MCP, and leaves one daemon per port.
- conflicts between `PIX_*` and `PI_STACK_*` fail with a clear precedence message.
- `.pix/host-state.json` and `.pi-stack/host-state.json` are both ignored in user repositories.
- pack host-exec changes re-prompt for trust.
- old changelog/upstream text remains intact and allowlisted.

### Setup

- every success line comes from a post-mutation probe;
- setup, doctor, status, onboarding, and run use the same verdict for the same evidence;
- all three configured Ollama roles are checked;
- host Ollama readiness is always tested; sandbox-to-Ollama reachability is tested from an existing or newly created sandbox and remains explicitly optional/unverifiable before one exists;
- requested but unregistered/unverified MCP servers return exit `1`, while a requested axis that cannot be probed returns exit `3`;
- optional inherited integrations report problems without preventing unrelated key repair;
- no unverifiable state is rendered as absent or ready.

### Gog

- one code path owns auth, headless proof, registration, config save, and rollback;
- `pix gog status` is read-only;
- account without credentials performs no mutation;
- the runtime remains Gmail-no-send, read-only, and wraps external content as untrusted;
- OAuth JSON contents never enter logs, backups, config, or migration output.

### Tests

- the task profile regression has end-to-end coverage;
- normal local tests complete under 10 seconds on the current development machine;
- timeout tests cancel process trees without two-second pipe waits;
- real package installation and image checks remain in release coverage;
- CI reports timing trends so performance regressions are visible.

### Prompt context

- project-owned always-on prompt content is at most 18 KB and the measured cold turn is at most 48 KB;
- skill catalog entries are capped and full skill bodies remain on demand;
- dynamic memory/knowledge recall does not change the system-prompt hash;
- monitor fixtures prove the hash is stable across at least 95% of normal turns;
- must-keep safety instructions survive compilation;
- `pix doctor --context` attributes every segment so external ancestor growth is visible.

## Risks and controls

- **Global replacement corrupts compatibility tests or history.** Use staged commits and an explicit old-name allowlist.
- **Two service supervisors fight for one port.** Stop and uninstall both old identities before installing the new one; verify one PID per port.
- **SQLite or secret state splits across directories.** Move under old locks with the daemon stopped; never copy or lazily migrate.
- **Old MCP registrations execute missing binaries or stale refs paths.** Treat re-registration as a required migration step and verify exact argv ownership.
- **Short `pix` binary collides with another package.** Installer preflight reports the existing resolved path and fails closed. Non-interactive replacement requires `PIX_FORCE_INSTALL=1`; doctor reports the first resolved binary.
- **GitHub redirects hide broken hardcoded URLs.** Keep the old repository controlled and test every raw/install/catalog URL directly.
- **Docker Hub has no repository rename redirect.** Reserve and publish the new image before the kit cutover; freeze old tags.
- **Setup refactor broadens scope during rename.** Land it in isolated, testable waves before changing public writes.

## Deliberate non-goals

- changing service ports;
- renaming MCP servers, skills, agents, capabilities, or routing intents;
- redesigning memory or knowledge storage during the rename;
- automatically deleting old sandboxes;
- automatically trusting migrated pack host execution;
- preserving old branding in new output beyond actionable migration messages.

## TODO after foundations: iterate on memory quality

Memory still does not feel right. Do not hide that behind the rename or declare it solved by better health checks.

After the rename, setup, and prompt-budget work lands, run a measured memory iteration:

1. Collect at least 30 real incidents tagged `re-teach`, `never-captured`, `wrong-recall`, `stale-recall`, `over-recall`, `wrong-scope`, `invisible`, or `degraded`.
2. Make degradation visible once per session. In particular, report when embedding or watcher operation has latched off and re-probe instead of remaining silently keyword-only.
3. Add an opt-in recall decision log with candidate score components, injected rows, drop reasons, token budget, and degradation state. Define retention and redaction before enabling it.
4. Add `/recall why` for the last turn and a lightweight good/bad/irrelevant verdict.
5. Build a frozen capture corpus and recall corpus with must-inject, may-inject, and must-not-inject labels.
6. Add an offline `pix-host memory eval` report for capture precision/recall, injection precision, needed-fact hit rate, wasted tokens, degraded-session rate, and recall latency.
7. Use the incident histogram and eval results to choose one experiment. If `never-captured` dominates, test capture scope/guards first. If wrong or stale recall dominates, test ranking terms and self-reinforcing frequency boost first.

The north-star metric is re-teach rate per active week. Do not change the schema, storage engine, embedding model, scope model, or injection budget until the incident diary and eval baseline exist.

**Exit criterion:** publish a one-page recommendation naming the dominant failure mode, baseline metrics, and the single next experiment. Memory redesign starts only after that evidence exists.

## Owner decisions recorded

- The target product, repository, and CLI name is Pix / `pix`.
- The current main baseline is `v0.0.47`; this branch was reset to that commit before planning.
- The implementation should fix setup, gog, docs, test speed, and task metadata as part of the same program, not as unrelated follow-up work.
