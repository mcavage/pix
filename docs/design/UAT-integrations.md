# UAT — integrations remediation

> **HISTORICAL — pre-v2 design note.** This document predates the accepted
> Pix v2 surface and architecture (`docs/design/pix-v2-surface.md`,
> `docs/design/pix-v2-architecture.md`), which supersede it. Commands,
> files, and components described here may no longer exist. Nothing in it
> is a description of current behavior; read it as history only.


Acceptance criteria for the pix + gm-pix-pack integrations work. Written to be
executed by someone with **no context on how it was implemented**: every item
below is a behaviour a user can observe, not an implementation detail.

Repos: `/Users/mcavage/dev/pix`, `/Users/mcavage/dev/gm-pix-pack`.
Binaries: build with `make launcher` → `out/pix`, `out/pix-host`.

**Report what you observe, not what you expect.** A criterion that cannot be
tested on this host (needs a second machine, a fresh Google account, an
unlocked vault at 3am) is **BLOCKED**, not passed. Guessing is the failure mode
this whole change exists to eliminate — do not reproduce it in the QA.

---

## Background: what was wrong

Read `docs/design/integrations-remediation.md` §1 for the full audit. In short:

- The pack's setup hooks called `pix` verbs that had been deleted, so setup
  could never complete — not "was hard", *could not succeed*.
- Docs prescribed a verification (`gog mcp --list-tools`) that passes on a
  completely broken install.
- `pix doctor` treated "registered with the gateway" as "working", so two
  registrations pointing at commands that no longer existed both read as ready.
- Google Workspace worked on the maintainer's machine only because a shell
  profile exported a keyring password into the gateway's environment.

## Background: what changed

Every MCP server is now declared by the active pack, with exactly one transport
(`command`, `image`, `manifest`, `url`), optional credentials (`env`,
`env_keys`) and an optional health `probe`. Pack setup is declarative
(`[[setup.require]]` / `[[setup.apply]]`). Pix ships no MCP servers and
special-cases no vendor.

---

## A. Build and suite

| # | Criterion | How |
|---|---|---|
| A1 | Go builds clean | `cd services/host && go build ./... && go vet ./...` |
| A2 | Go tests pass | `cd services/host && go test ./...` |
| A3 | Race-clean | `cd services/host && go test -race ./...` |
| A4 | Formatted | `cd services/host && gofmt -l .` prints nothing |
| A5 | JS suite passes | `node --test tests/` |
| A6 | Fast CI gate passes | `bash scripts/gate.sh` |
| A7 | No flake | `go test ./cmd/pix/ -run TestLocalOllamaProbesAreSerialized -count=200` |

## B. The deleted surface is really gone

| # | Criterion |
|---|---|
| B1 | `grep -rn "GogAccount\|GWServerName\|GWInstallCmd\|LocalMCPNames\|NonSecretOpRefsKeys" services/host --include="*.go"` returns nothing (comments explaining the removal are acceptable; declarations are not) |
| B2 | `out/pix config get google_workspace_account` fails with an unknown-key error, and the key list it prints contains no Google/Slack keys |
| B3 | `out/pix gworkspace` and `out/pix slack` report no such command |
| B4 | No `.go` file outside `docs/` mentions `gog` as a special case |

## C. Docs tell the truth

| # | Criterion |
|---|---|
| C1 | `node --test tests/verb-references.test.mjs` passes — this resolves every `pix <verb>` written in `skills/**` and `docs/**` against the real CLI tree |
| C2 | No doc recommends `gog mcp --list-tools` as a verification |
| C3 | No doc states a literal `GOG_HOME` path |
| C4 | Every `[[integrations]]` example in the docs declares a transport (an example with only `mcp = "..."` and `env = "..."` is now invalid and would not load) |
| C5 | Pick any three commands from `README.md` and `docs/getting-started.md` and run them. They exist and behave as described |

## D. Pack loads and is honest

| # | Criterion | How |
|---|---|---|
| D1 | The pack loads | `out/pix pack show /Users/mcavage/dev/gm-pix-pack` |
| D2 | Its own guard passes | `cd /Users/mcavage/dev/gm-pix-pack && bash tests/check-onboarding-naming.sh` |
| D3 | `pack show` names the *whole* argv for the Google Workspace server, including the hardening flags — a reviewer must be able to see `--readonly` without opening the manifest |
| D4 | Declarative setup steps render what they need, not an empty field |
| D5 | The pack declares NO Slack integration, and `capabilities.json` routes `chat` → `none` |
| D6 | `setup/slack`, `setup/google-workspace`, `setup/bamboohr` no longer exist (only `setup/snowflake` survives, deliberately) |

## E. Validation fails closed — the important half

Build a scratch pack in a temp dir for each of these and confirm `out/pix pack
show <dir>` **refuses it with a message that names the problem**. A pack that
loads when it should not is a P1.

| # | Invalid pack | Must be refused because |
|---|---|---|
| E1 | `[[integrations]]` with `mcp = "x"` and no `command`/`image`/`manifest`/`url` | nothing could ever start it |
| E2 | `[[integrations]]` with both `command` and `url` | transports are mutually exclusive |
| E3 | `command = "/usr/local/bin/thing"` (an absolute path) | must be a bare name resolved on PATH |
| E4 | `command = "x"` plus `env_values = { A = "b" }` | a host command has no channel for a literal |
| E5 | `[[setup.require]]` with `kind = "typo"` | the vocabulary is closed |
| E6 | `[[setup.require]]` with `kind = "bin"` and no `install` | a missing binary with no install hint is a dead end |
| E7 | `[[setup.apply]]` with `kind = "typo"` | closed vocabulary |
| E8 | A setup step with both `path` and `require` | one form or the other |
| E9 | A setup step with neither `path` nor `require` | declares nothing |

## F. Doctor distinguishes registered from working

This is the heart of the change. **F1 and F2 are the two that matter most.**

| # | Criterion |
|---|---|
| F1 | A server registered with the gateway that NO active pack declares is reported as a **gap**, with a fix naming `sbx mcp rm` or re-activating the pack — even though `sbx mcp ls` lists it as ready |
| F2 | A `command` server whose binary is not on PATH is reported as a **gap**, not as ready |
| F3 | A server with no declared `probe` is reported as **unverified**, never as healthy |
| F4 | A server whose `probe` exits non-zero is a gap |
| F5 | A declared health probe runs through the same `op run` wrapper the gateway uses — verify by reading the registered command (`sbx mcp inspect <name>`) and confirming the probe is wrapped the same way |
| F6 | A probe that cannot run (locked 1Password vault, timeout) is reported as **unknown**, not as pass or fail, and the message says a locked vault is a likely cause |
| F7 | One slow/hanging server does not starve the others: with several servers configured, each still gets its own answer |

To exercise F1/F2 without breaking the host permanently, register a throwaway:
`sbx mcp add uat-fake --command /bin/echo --args hi`, add it to the config,
run `out/pix doctor`, then `sbx mcp rm uat-fake` and unset it.

## G. Credential handling

| # | Criterion |
|---|---|
| G1 | A server that declares credentials registers wrapped in `op run --env-file` (`sbx mcp inspect <name>`) |
| G2 | A server that declares NO credentials registers **bare** — it must not share fate with unrelated refs in op-refs.env |
| G3 | `pix secret set X <literal>` is refused for a key no pack authorized, and nothing is written |
| G4 | `pix secret set X <literal>` succeeds only when the active pack lists X in an integration's `env_keys` |
| G5 | No secret VALUE is ever printed by `pix secret ls`, `pix secret check`, or `pix doctor` |

## H. Trust and consent

| # | Criterion |
|---|---|
| H1 | The Tier-1 adoption screen shows the full argv of a `command` server before anything runs |
| H2 | It shows each declarative setup step's requirements and the argv of each remediation, with interactive ones marked |
| H3 | **Fingerprint compatibility:** a pack using the OLD executable setup form and only remote/image integrations produces the SAME fingerprint as before this change. A spurious re-consent prompt teaches users to click through the one security gate they have. If you cannot verify this empirically, say so — do not assume it |
| H4 | Editing a declarative `require`/`apply` re-gates (it is executable intent) |

## I. Failure messages are actionable

For each, read the message as if you had never seen this codebase. It must name
what is wrong AND what to do.

| # | Scenario |
|---|---|
| I1 | Registering a server whose command is missing |
| I2 | A config listing a server no pack declares |
| I3 | A setup step whose `op-ref` requirement is unmet — must say pix cannot fix it for you and give the exact `pix secret set` command, and must NOT run any remediation (nothing pix runs can put a secret in your vault) |
| I4 | A setup step whose `bin` requirement is unmet — must show the pack's install hint |
| I5 | A pack refused by any E-series validation |

## J. Regression sweep

| # | Criterion |
|---|---|
| J1 | `out/pix status`, `out/pix ls`, `out/pix models`, `out/pix memory stats`, `out/pix pack ls` all still work |
| J2 | `out/pix help --all` renders and every verb in it is real |
| J3 | `out/pix doctor` exit code is 0 when nothing is broken and non-zero when something is |
| J4 | Nothing in `~/.config/pix/` was corrupted; `config.toml` is still valid TOML and still parses |

---

## Reporting

For each criterion: **PASS / FAIL / BLOCKED**, with the command you ran and the
actual output for anything that is not a clean pass. Rank failures by user
impact, and lead with anything in section F or H — those are the two places
where a bug reintroduces the exact class of problem this change was made to fix.
