# Onboarding v3: one supported path

Status: PROPOSED. The immediate correctness fixes described in section 2 are implemented on `fix/onboarding`; the productized installer and routing work remain future work.

## 1. Why this exists

A Docker employee followed the Pix and `gm-pix-pack` instructions and still ended up with Gmail unavailable. `pix mcp ls` showed the Google Workspace server, but that command only showed host registration. The running sandbox had been created before the server was registered, so the tools were not attached. A live `pix mcp load` fixed it.

The same audit found broader problems:

- the pack README used a removed `pix gog setup` command;
- Pix, the pack, and agent skills disagreed on `gog` versus `google-workspace`;
- the pack installer registered new MCP servers and then told users to run `pix run`, which reattached to stale create-time state;
- optional doctor checks rendered with hard-failure glyphs;
- Slack instructions treated a personal `xoxp-` user token like a team credential;
- `pix setup` assumes more model providers than `pix run` actually requires;
- installation, host setup, pack activation, OAuth, model pulls, and verification are split across two READMEs and several commands.

This is not ready for broad employee rollout. The target is a clean Mac to a verified first task without help from the pack author.

## 2. Immediate repair

The first release fixes truthfulness before adding automation:

1. Use one Google Workspace contract everywhere: CLI `pix gworkspace`, MCP server `google-workspace`, config key `google_workspace_account`, implementation binary `gog`, Homebrew formula `openclaw/tap/gogcli`.
2. Make `pix mcp ls` say that it reports host registration, not attachment to the current sandbox. Point users at `pix status`, `pix doctor`, `pix mcp load`, and `pix run --replace`.
3. End pack activation with `pix run --replace` because integrations affect sandbox creation.
4. Render optional doctor failures as warnings and exclude note-only checks from actionable TODOs.
5. Treat Slack and Google Workspace as optional pack capabilities.
6. Forbid shared Slack user tokens. Every employee needs a distinct OAuth grant, and setup must prove the token identity through `auth.test`.
7. Add anti-drift tests for commands, server names, Homebrew formulae, and agent repair guidance.

## 3. Product decision

Homebrew is the primary and only supported macOS distribution. The curl
installer (`install.sh`) is a fallback for Linux or a machine without Homebrew;
there is no signed-release path.

The Homebrew formula owns only the `pix` and `pix-host` binaries and the manpage.
It deliberately declares no dependencies. `pix setup` owns prerequisite
installation plus stateful work: authentication, configuration, model selection,
service lifecycle, pack activation, and verification. Do not turn the shell
downloader into a second setup engine.

The supported path becomes:

```bash
brew install mcavage/tap/pix
pix setup --pack docker/gm-pix-pack
pix run
```

`pix setup` may open browser or 1Password prompts. It resumes safely after interruption and prints one final verdict.

A pack contributes setup through typed `[[setup]]` entries in `pack.toml`.
Each entry names a repo-relative executable, bounded read-only probe arguments,
idempotent apply arguments, and whether the step is required for first use.
Setup executable bytes and argv are included in the launcher-owned Tier-1 trust
fingerprint. Pix runs required steps only, probes before applying, and requires
the same probe to pass afterward. Optional integrations authorize on first use
or through an explicit later setup action. A pack must not ship a second public
installer; a legacy installer may only delegate to `pix setup --pack`.

## 4. Setup phases

Setup is an idempotent state machine. Each phase records observed evidence, makes one bounded change, verifies that change, and can be rerun.

1. **Inventory:** Docker Desktop, `sbx`, Homebrew, 1Password CLI, Ollama, GitHub CLI, configured providers, active pack, MCP registrations, running sandbox, and host services.
2. **Dependencies:** offer one Homebrew transaction for missing supported packages. Non-Homebrew systems receive exact manual instructions without partial mutation.
3. **Authentication:** run `sbx login`, provider setup, remote MCP OAuth, Google Workspace OAuth, Snowflake SSO, and Slack user OAuth one at a time. Never start a second browser flow while one is unresolved.
4. **Secrets:** discover likely 1Password items by metadata, let the user choose, and store only `op://` references. Never read every concealed field from an item and never write a resolved secret to Pix config.
5. **Models:** ask which providers the user has, probe what is actually callable, offer local model pulls with download sizes, and compile routes for that exact availability set.
6. **Services:** when memory is enabled, install or start `pix serve` and verify health. Disabled memory is not a failure.
7. **Pack:** activate the selected pack, apply its required and optional capabilities, register integrations, and complete their auth.
8. **Sandbox sync:** live-load new MCP servers when safe or recreate the target sandbox once. Do not leave registered-but-unattached state as a successful result.
9. **Smoke test:** call one cheap read-only operation for every enabled capability from the target sandbox. Registration alone never passes this phase.
10. **First task:** start Pix and run a useful workflow. Setup success is measured by a working task, not a green config file.

Interactive setup has one consent per category, not one prompt per package or provider. `--yes` remains deterministic and never invents missing credentials.

## 5. Doctor vocabulary

Doctor reports observed truth. The default view shows subsystem verdicts and actionable fixes; `--verbose` adds evidence.

- **Blocked:** a required, verified failure. Exit 1.
- **Action required:** an enabled or explicitly requested capability needs user action. It blocks only when required or requested.
- **Degraded:** an optional capability is unavailable and a fallback is active. Exit 0.
- **Disabled:** deliberately off. Informational, exit 0.
- **Unverifiable:** Pix could not establish truth. Use `?` and say where to rerun the check.
- **Ready:** verified by a live probe.

An unconfigured knowledge bundle, optional skill, local model, or MCP integration is not a failure. Doctor must never print an “all checks pass” headline alongside an actionable TODO.

## 6. 1Password discovery

Manual `op://` entry remains the escape hatch, not the primary path.

Safe discovery reads item and vault metadata, presents candidate items, asks the user to choose a field, constructs the reference, and validates that one reference with `op read`. It does not fetch whole item payloads or print concealed values.

A pack may declare secret requirements with labels and expected token types:

```toml
[[secrets]]
name = "BAMBOOHR_API_KEY"
required = true
kind = "api-key"
label = "BambooHR API key"

[[secrets]]
name = "SLACK_TOKEN"
required = false
kind = "slack-user-token"
label = "Personal Slack user grant"
```

Pix validates shape and identity where the provider supports it. For Slack, `xoxp-` plus `auth.test` is required; another employee's token is a hard failure.

## 7. Slack OAuth

`SLACK_TOKEN` is a personal user token. Every Slack call runs as its owner, including searches, private-channel reads, and DMs the owner can access. It must never come from a shared vault item.

Use one Docker-owned Slack app with the minimal user scopes documented in `docs/design/slack-setup.md`. A controlled HTTPS callback service holds the app client secret and exchanges each employee's authorization code. Employees do not receive the client secret. Each employee stores only their own returned token in their own 1Password vault.

`pix slack setup|auth|status|disable` must:

- launch or link to the approved user authorization flow;
- write only the employee's `op://` reference;
- verify the token with `auth.test` and display the acting identity;
- verify registration and sandbox attachment separately;
- revoke or remove Pix wiring without claiming that deleting a reference revokes Slack access.

Until this exists, Slack remains optional. Missing Slack data is safer than using somebody else's identity.

## 8. Packs and model routing

Packs declare requirements and recommendations, not secret values or unreachable model IDs.

```toml
[requirements]
providers = "any"
min_providers = 1
cross_vendor_review = "preferred"

[routing]
run_intent = "strategy"
mode = "merge"

[[capability]]
name = "chat"
required = false
smoke_test = "slack.health"
```

Resolution order:

1. explicit `pix run --model` or `--intent`;
2. explicit user config;
3. active pack recommendation;
4. Pix default.

The router consumes observed provider availability, not a static assumption that Anthropic, OpenAI, Google, and Ollama all exist.

On a single-provider host, setup also selects a compatible top-level intent
when the shipped OpenAI-specific default would be unusable: `strategy` for
Anthropic, `review` for Google, and `overlord` for OpenAI. An explicit
non-default user intent is never overwritten.

- With several providers, preserve cross-vendor review where possible.
- With one cloud provider, use same-provider review and report the degradation once.
- With Ollama only, every enabled intent resolves to an available local model or is explicitly disabled. It never falls back to a cloud model that cannot authenticate.
- A pack may require cross-vendor review, but then activation fails clearly when fewer than two suitable providers exist.

Provider-subset matrix tests cover every non-empty supported provider combination and every intent.

## 9. Rollout

1. Land the immediate repair in Pix and `gm-pix-pack`.
2. Measure the current path with three employees on clean Macs. Record commands, browser handoffs, elapsed time, and where help was needed.
3. Publish the Homebrew formula and dependency inventory.
4. Add 1Password discovery and provider-aware routing.
5. Add per-user Slack OAuth and pack requirement declarations.
6. Add sandbox capability smoke tests.
7. Make the new path the default only after three consecutive unassisted setups succeed.

## 10. Success criteria

- Four or fewer terminal commands from a supported clean Mac to first `pix run`.
- No dependency on a named employee for credentials or setup help.
- Zero false failure glyphs for disabled or optional features.
- `pix mcp ls` cannot be mistaken for sandbox attachment truth.
- Setup succeeds only after enabled capabilities pass live read-only probes from the target sandbox.
- Slack always acts as the current employee, proven by `auth.test`.
- All-provider, subset-provider, and Ollama-only routing pass the same intent matrix.
- At least 80 percent of pilot users complete setup unassisted in 30 minutes and run a second command without prompting.
