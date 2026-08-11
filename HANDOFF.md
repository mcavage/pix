# HANDOFF — integrations remediation (overnight run, 2026-08-10/11)

**Read this first, then `docs/design/integrations-remediation.md` (the spec).**
This file is the live state of the run. Update it at every phase boundary.

## Mission

Take `pix` + `../gm-pix-pack` from "setup is impossible" to end-user ready.
Mark onboards new users tomorrow morning. He is asleep and unreachable for the
duration — **do not stop to ask questions; decide, record the decision here, and
keep going.**

## Standing constraints (from Mark, all confirmed)

| | |
|---|---|
| Git | Commit directly to **local `main`**. **Never push.** No PRs. |
| Baseline | `7218978` (= `origin/main` at run start). Local was 4 behind; fast-forwarded. |
| Stash | `stash@{0}` holds a pre-run WIP snapshot that was an OLDER draft of merged #80/#81. Nothing of value. Do not restore. |
| Host state | **Cleaning it is authorized**, including destroying the running sandbox. |
| gog | Rip out of pix core entirely. Not public. Moves to the pack. |
| Slack | Pack-owned, PKCE, **never in core**. Do LAST. If the night runs out it must degrade honestly, not half-work. |
| Slack re-auth | 30-day grant expiry: **let it fail and say why**, with the exact re-auth command. No proactive nagging, no prompts in `pix run`. |
| Plugin kinds | Stay at one (`memory`). Do NOT restore `McpServer`/`CredentialBroker`. |
| Credentials | One home: `~/.config/pix/op-refs.env`, 1Password assumed present. |
| Also owed | Fix flaky `TestLocalOllamaProbesAreSerialized` (flaked on two PRs). |

## Ralph loop protocol

Each iteration: **build → clean-slate QA → clean-slate review → triage → fix →
update this file → next loop.** QA and review agents get NO context from the
implementer beyond the spec and acceptance criteria — that is the point. Expect
3+ iterations. Record every iteration's findings below, including the ones
rejected and why.

## Phase status

| phase | what | status |
|---|---|---|
| 0 | Host cleanup (dead registrations, sandbox) | ✅ done |
| 1 | Rip gog from pix core | ✅ prod code done |
| 2 | Generic local-command MCP integration | ✅ prod code done |
| 3 | Doctor honesty (registered ≠ working) | ✅ prod code done |
| 4 | Declarative setup steps | ✅ prod code done |
| 5 | Docs + skills truth pass | ✅ done |
| 6 | gm-pix-pack rewrite | ✅ done |
| 7 | CI guards | ✅ done (the verb guard now scans all of skills/ + docs/, which is where the last round of dead verbs survived — it found ten on its first run) |
| X | Flaky test fix | ✅ done (mutation-verified both directions) |
| T | Whole suite green | ✅ go build/vet/gofmt/test/-race, 417 JS tests, `scripts/gate.sh` exit 0 |

## Verified on the real host

`pix doctor` before: `✓ mcp optional 8 registered` — with two dead registrations
and a server working only by accident. After:

```
✗ mcp        optional 1 of 6 not usable
  - google-workspace: registered and answering
  - slack: in your config, but no active pack declares it
  - bamboohr: registered and answering
  - opine/granola/notion: registered and authenticated
  - atlassian: registered, not authenticated        ← real gap, previously hidden
```

`sbx mcp inspect google-workspace` is now op-run wrapped (it was bare, working
only because ~/.bashrc exported the keyring password into the gateway's env).

Final state of that same command after three review rounds — note that the two
`⚠`-worthy findings (an expired grant, a locked vault) were both invisible
before, and the headline no longer claims ready:

```
⚠ core ready; mcp not usable
✗ mcp   optional 2 of 6 not usable, 2 not checkable
  - google-workspace: registered; its health probe did not answer in time;
      it runs through 1Password (`op run`), so unlock your vault and re-run
  - notion / atlassian: registered, its authorization expired
  - acme / pix-qa: registered with the gateway but not in your pix config
Fix:  pix mcp auth notion / pix mcp auth atlassian / op signin
```

## Verification commands

```bash
cd /Users/mcavage/dev/pix
go build ./... && go vet ./...        # from services/host
bash scripts/gate.sh                  # the fast PR gate CI runs
go test ./...                         # from services/host
node --test --test-force-exit tests/*.test.mjs   # JS suite (the glob is required)
```

## Decisions made during the run

_(append; never rewrite history here — a reversed decision gets a new row saying so)_

| # | decision | why |
|---|---|---|
| R1 | Fast-forwarded to `origin/main` instead of committing the local WIP. | The WIP was 22 files / 8 insertions / **2003 deletions** vs origin/main — an older draft of already-merged #80+#81. Committing it would have been a regression. Stashed, not discarded. |
| R2 | Deleted the whole local-MCP bridge (`pix-host mcp --list`, `LocalMCPNames`, `HostBin`, `hostResolver` threading, `LocalMCPClassifier`, `PackLocalMCP`). | The bridge returned the EMPTY SET unconditionally (`main.go:63`), yet a large fail-closed subsystem existed to handle its unanswerable cases. Servers come from the pack manifest now, so classification is a map lookup. |
| R3 | `ComputeHostBoM` is a pure function of the manifest. | It computes what a user CONSENTS to. It previously ran a subprocess, so the same pack could produce different bills of materials on different machines or moments. |
| R4 | op-run wrapping is decided by whether a server declares `EnvKeys`, not by a vendor special case. | `op run --env-file` resolves EVERY ref in the file; wrapping a credential-free server made it share fate with unrelated refs. |
| R5 | Doctor's health probe runs through the SAME `mcp.OpRunWrap` the gateway uses. | This is the original bug class: a credential that works in your terminal and not in the gateway. A probe in doctor's own shell proves nothing. Cost: a locked 1Password vault makes probes report "did not answer" — which is honest, and the message says so. |
| R6 | Per-server doctor checks run CONCURRENTLY. | The budget is per-probe, not per-exec. One `op run` blocking on biometrics starved every check after it and reported five gaps that were never checked. Measured before/after on the real host. |
| R7 | `env_values` is REFUSED at load for a `command` transport. | Found by an agent: it was copied into the spec and shown on the consent screen but never materialized — a host command has no `-e`. Consenting to a value that never reaches the server is worse than not supporting it. |
| R8 | Snowflake keeps its executable setup hook. | Installing a vendor daemon + LaunchAgent is genuinely bespoke; that is what the escape hatch is for, and the script is content-hashed into the fingerprint. Encoding it as a `bash -lc` one-liner in the manifest would have smuggled opaque shell back in. |
| R9 | Slack is NOT declared in the pack at all. | Its PKCE grant is a ROTATING token pair that op-refs (read-only resolution, no write-back) cannot hold. Declaring a name with no transport is what produced the dead `pix-host mcp slack` registration. `capabilities.json` routes chat → none so the agent says so instead of pretending. |
| R10 | **REVERSES the design doc's Phase 1:** `skills/gworkspace/SKILL.md` STAYS in pix; it does not move to the pack. | The skill is not about gog. It is the untrusted-content rule for Gmail/Doc text — the prompt-injection guard — and it belongs wherever the `gworkspace` CAPABILITY is declared, which is pix's `capabilities.json`. The pack supplies the server; pix supplies the safety rule, so a public user who wires their own Workspace server inherits it. Its false claims (registration verb, `GOG_HOME`, pix-owned flags) were corrected instead. |
| R11 | `scripts/macos/verify-pix-lifecycle.sh` asserted a `PIX_RETIRED` seam that does not exist. | Measured: `pix host` prints `no command named "host"` and exits 0, while the release gate demanded exit 2 and the string `PIX_RETIRED`. The gate itself could not pass. Fixed to assert the real contract. |
| R12 | Test fixtures (Go and JS) now force `commit.gpgsign=false`. | Fixture repos inherited the developer's `~/.gitconfig`. On a machine signing with 1Password, every fixture commit blocked on an authorization prompt that never comes in a test run — the suite hung ~60s per commit and timed out with nothing explaining why. Found independently by two agents; it is why the suite could not complete tonight. |

## Iteration log

### Iteration 1 — build (complete)

Landed as `c67a09d` (pix) and `d25f388` (gm-pix-pack). **Both UNSIGNED** — see
below. Everything green at commit time:

```
go build ./... && go vet ./...          clean
gofmt -l .                              empty
go test ./...                           all packages ok
node --test tests/*.test.mjs            417 tests, 0 fail
bash scripts/gate.sh                    exit 0
```

Five agents contributed, each working from the spec with no access to the
implementer's reasoning. Between them they found **eight real production bugs**
in work that had already been self-reviewed:

| found by | bug |
|---|---|
| test agent A | `env_values` consented to on a host command, then silently dropped |
| test agent A | `AddArgs` could panic on a server with no transport |
| test agent A | "skipping" printed for a name that actually failed the command |
| test agent B | fingerprint compat needed a real test, not an assumption (it holds — 3 proofs) |
| test agent C | `pix mcp --help` still claimed pix builds servers |
| test agent C | dead `--google-workspace` / `--credentials` flags still declared |
| test agent C + me | test fixtures inherited commit signing; suite hung ~60s per fixture commit |
| docs agent | the release gate asserted a `PIX_RETIRED` seam that does not exist |

Self-caught during review of my own diff: a catalog name in config with no pack
would have hard-errored (regression), and doctor's probes starved each other on
a shared budget.

### ⚠️ COMMITS ARE UNSIGNED

`~/.gitconfig` signs with the 1Password SSH agent, which cannot authorize
unattended. Both commits were made with `-c commit.gpgsign=false`. **Re-sign
before pushing:**

```bash
cd /Users/mcavage/dev/pix          && git commit --amend --no-edit -S
cd /Users/mcavage/dev/gm-pix-pack  && git commit --amend --no-edit -S
```

### Iteration 2 — independent review (complete)

Landed as `5b138e7` + `60c70f4` (pix) and `5134530` (gm-pix-pack). Three
clean-slate reviewers ran against `c67a09d` with no access to my reasoning.

**They were right about the thing that mattered most, and I was wrong.**
`gog auth doctor` — the probe I chose to replace the one the audit condemned —
prints `status error` on an unopenable keyring and **exits 0**. Pix judges a
probe by its exit code, so it verified exactly as much as `gog mcp
--list-tools`: nothing. I asserted its exit-code behaviour in three documents
without testing the failing case. That is the same defect as the original bug,
committed by the fix, and no amount of self-review had caught it.

| # | finding | who |
|---|---|---|
| P0 | `probe` argv executes on the host but was in neither the fingerprint nor the consent screen — a pack could change what pix runs without re-gating | security reviewer (and, in parallel, me) |
| P0 | `gog auth doctor` exits 0 when broken; the replacement probe verified nothing | product reviewer |
| P1 | `✓ ready` headline over a row that proved an integration unusable | product reviewer |
| P1 | `ActiveServerMCP` read only the last pack → doctor told users to `sbx mcp rm` servers it had just registered on a composed stack; an unloadable pack read as "declares nothing" | security reviewer |
| P1 | `pix secret check` hung forever on a locked vault, after printing FAIL for correct refs | product reviewer |
| P2 | MCP names and `probe[0]` ungated while reaching shell-paste commands (`mcp = "a; rm -rf ~"` loaded) | security reviewer |
| P2 | `NonSecretEnvNames` bypass: one integration could allowlist another's secret into plaintext | security reviewer |
| P2 | probes could outrun the budget (no `WaitDelay`) | security reviewer |
| P2 | consent screen hid credential names when a pack had prerequisites; showed apply argv but not check argv | security reviewer |
| P2 | a missing `bin` was "fixable", so the install hint was buried under an exec error | product reviewer |
| P2 | `pix mcp ls`'s ✓ means registered; `pix secret ls` claimed to resolve what it syntax-checks | product reviewer |

All closed. The security reviewer also audited for **weakened assertions** in
the rewritten tests and found none — "the gap is missing coverage, not weakened
coverage" — which is the outcome that mattered for the delegation.

**Open, deliberately deferred** (recorded so they are not lost):

- Doctor still only inspects servers in `cfg.MCP`; it does not enumerate the
  gateway, so a registration with no config entry stays invisible. On this host
  that is `pix-qa`. The commit message no longer claims otherwise.
- Snowflake has no doctor row at all (it is a proxy, not an MCP server), so a
  dead `snow-proxy` degrades silently.
- BambooHR's probe checks that a Docker image exists, not that the API key
  works. Honest but weak.
- `pix setup` aborts on the FIRST unmet requirement rather than listing all of
  them, so a fresh laptop needs three passes to learn two facts.

### Iteration 3 — adversarial verification (in progress)

`6127bb2` + `7eef572` closed the last of iteration 2's UX debt (every remedy is
listed, not just the first kind; `secret ls` no longer prints advice that leads
nowhere). A fourth clean-slate agent is now re-proving each iteration-2 fix
claim by running it, rather than reading it, and hunting for regressions the
fixes introduced.

State at the time of writing, all verified by running:

```
go build ./... && go vet ./...      clean
gofmt -l .                          empty
go test -race ./...                 clean
bash scripts/gate.sh                exit 0
pack guard (gm-pix-pack)            OK
pix pack show <pack>                loads
```

## If you are picking this up cold

1. `git log --oneline c67a09d~1..HEAD` in both repos — five commits in pix, two
   in gm-pix-pack.
2. **Re-sign every commit** (see the warning above). Nothing is pushed.
3. `docs/design/integrations-remediation.md` is the spec and the audit;
   `docs/design/UAT-integrations.md` is the acceptance criteria;
   `docs/design/CHANGE-BRIEF.md` is the symbol-level diff summary and can be
   DELETED once this lands (it is scaffolding).
4. The remaining gaps are listed below. None of them makes anything claim to
   work when it does not — that property is the whole point and it holds.

### Iteration 4 — adversarial verification (complete)

A fourth agent re-proved every iteration-2 fix **by running it** and caught two
of my commit messages claiming more than the code did — the same failure this
effort is about, so they were fixed rather than reworded (`45f88fa`):

- `6127bb2` said `op signin` "was computed and then discarded" in the past tense.
  It still was, whenever the mcp row had no verified gaps. Fixing it properly
  meant respecting an invariant I had initially broken without reading the test
  that explains it: an unknown result may never emit a copy-pasteable repair,
  because a green report with a TODO under it teaches people to ignore both. The
  remedy moved into the evidence line, as the exact command.
- `22ffad5` said a missing command "still exits non-zero" — true of `pix mcp
  add`, false of `pix pack use`, which is the verb a new employee runs.

It also found that a probe was wrapped in `op run` based on the SERVER's
credentials rather than the probe's own needs, so BambooHR's credential-free
`docker image inspect` inherited a 1Password dependency it never had.

And it independently confirmed, by building the pre-change binary in a worktree,
that fingerprint compatibility holds (`ab7d322c…` == `ab7d322c…`) and that the
cross-integration secret bypass is genuinely closed — **with a control** proving
the pack-wide exclusion is the operative mechanism rather than a name heuristic.

Its verdict: *"Yes — the nine claims substantively hold and nothing here breaks a
legitimate setup."*

Two findings from it are recorded below rather than fixed.

### Iteration 3 — clean-slate QA (complete)

A fourth agent executed the full UAT with no context. **60 of 66 criteria PASS,
1 FAIL, 3 BLOCKED, plus 6 findings outside the spec.** It found the single worst
defect of the whole effort, and it was mine:

> **`pix pack use` printed `registered mcp: <every server>` and exited 0 having
> registered NONE of them.** My own "hard failure" guard: registration resolved
> every declared command up front and returned on the first one missing from
> PATH. A pack's FIRST adoption is exactly the state where a command is not
> installed yet — installing it is what the setup step is for — so one absent
> binary silently blocked every other server, including remote and container
> transports unrelated to it. On gm-pix-pack, a machine without `gog` gets no
> BambooHR, Opine, Granola, Notion or Atlassian either, while being told it got
> all six.

Fixed in `22ffad5`: a missing command skips that server, names it, and still
exits non-zero. Everything resolvable registers. Also fixed there: the two stale
docs it caught, including `config/op-refs.env.example` — the file `make serve`
tells you to copy — which still carried the deleted `google_workspace_account`
key and the wrong `GOG_HOME` path.

It also independently verified the things that mattered most, empirically rather
than by reading:

- **Fingerprint compatibility**: it built the PRE-change binary from `7218978`
  and compared. Same pack, same fingerprint
  (`2b36ed51…`), and the new binary reading the old binary's `pack-trust.json`
  adopted with **zero** re-consent prompts.
- **All 9 invalid-pack cases refused**, plus 7 bypasses it invented itself
  (`command`+`image`, `../../bin/thing`, `thing; rm -rf /`, `mcp = "a; rm -rf ~"`,
  a shell-injected probe, `env` on a `url` transport, a dangling `setup` ref) —
  every one refused with a specific message. Only the empty-`mcp`-name case got
  through; closed in `4a52b95`.
- **One doctor run, six servers, six different honest answers**, and the
  hanging-probe server (`sleep 600`) did not starve the other five: 8s total.
- **The probe really does use the gateway's wrapper** — it sampled the process
  table and matched the prefix byte-for-byte against `sbx mcp inspect`.

## Round 5 — snow-proxy under the supervisor, and what the managed path found

The `runtime = "daemon"` feature had landed, been reviewed, been gated and been
committed. It had never started a daemon. Four separate defects sat between
"accepted" and "running", and each one was invisible to the check before it.

1. **`UnitSpec.Validate` refused the launch form the whole feature exists for.**
   It knew self-exec and a SHA-pinned path; a pack daemon is a binary the user
   compiled on their own Mac, which no shared manifest can carry a SHA for. The
   pack was accepted, packinfo validated the entry, `Tree.AddDaemon` took it, and
   the unit it was handed could not describe itself. `UnitSpec.Command` closes
   it, joins `identity()`, and is refused alongside a sha pin.
2. **The first health check used the per-probe budget.** snow-proxy binds in
   ~1.5s warm and ~3.5s cold; `HealthTimeout` is 3s. A correct install passed or
   failed by luck, and a failed start removes the unit from the tree entirely.
   `Handshake` already meant "how long may a unit take to become usable", so it
   now covers both unit kinds rather than the runtime growing a fourth timeout.
3. **`exec.LookPath` under launchd finds nothing a user installed.** launchd
   hands a login service `PATH=/usr/bin:/bin:/usr/sbin:/sbin`. Every host binary
   here lives elsewhere. The daemon worked in my foreground shell and failed
   permanently under the managed service — the only way real users run it.
4. **The same gap hit the child, and that half is worse.** A daemon inheriting
   launchd's PATH starts, binds, and answers health while unable to find the
   vendor CLI it shells out to. Green row, dead capability.

I found (3) and (4) only because I installed the managed service to leave the
machine in the state a user would have. Everything before that was verified
against a foreground process, which is not the product.

### The reason all four shipped

`supervise.DaemonService` had **no tests**. The manifest had validation tests,
the health probe had probe tests, the tree had an Add test — and the code that
execs the binary was covered by nothing. `daemon_test.go` and `hostpath_test.go`
now drive real child processes, because every one of these was a shape error in
the launch contract that a mock would have agreed with.

### Two of my own tests were vacuous, and I only knew because I checked

Every test here was verified by REINTRODUCING the bug it claims to catch. Two
passed against code with the behaviour deleted:

- the wedge test used `nc -l`, which serves one connection and exits — so the
  fixture "wedged" by dying, the supervisor caught it on the process-exited path,
  and deleting eviction entirely still passed. The fixture is python now: it
  holds its pid and its listening socket and answers nothing, which is the
  failure launchd cannot see and the only reason this runtime beats a LaunchAgent.
- the process-group test used `os.FindProcess` + `Process.Signal(nil)`. On Unix
  `FindProcess` never fails and `Signal(nil)` returns "unsupported signal type"
  for every pid alive or dead, so it reported "reaped" on the first poll.
  Liveness is `syscall.Kill(pid, 0)`.

This is the third time this session that a check I wrote could not fail. It is
the failure mode to assume, not to rule out.

### Verified end to end, under launchd, not a foreground process

- `pix doctor` → `✓ ready`, all seven rows green, including `daemons 1 answering`
- a real query: `CURRENT_USER()` = `MARK_CAVAGE`, exit 0
- the daemon's actual child PATH, read off the process: homebrew and
  `~/.local/bin` appended, not duplicated
- three consecutive cold `serve` restarts, each healthy
- `kill -9` on the pid → replaced, `restarts=1`
- `pix serve status` → `probe=571us`, a real measurement
- full gate: 10/10 segments

### Also closed in this round

- **The MCP footer called an idle lazy server a problem.** It decided by counting
  (`connectedCount != enabledCount`), which equates "not connected" with
  "broken"; a lazy or cached server is neither. Same defect as reporting
  "registered" as "working", one layer up. It now asks whether a server has a
  recorded failure or needs auth. The patch also applied its second anchor only
  `if (initChanged)`, so re-running it left the adapter half-patched with no
  error.
- **`setup/snowflake`'s check was `curl /health`.** Fair when `install.sh` loaded
  a LaunchAgent; nothing there starts anything now, and the daemon does not exist
  until `pix serve` next reads the accepted pack. With `required = true` that
  would have blocked the whole of `pix setup` for a new user — a first-day
  onboarding path. It checks the two binaries and the `[pix]` connection, and all
  three failure paths were run by hand.
- **The consent screen said "resolved on PATH at launch"**, which stopped being
  true, and had **no test** — the one fact on that screen representing a weaker
  guarantee than its neighbours was unverified. It has one, and it fails when the
  disclosure is weakened.
- **`pix serve --help` described only memory.** Corrected, but only after
  running `serve status` to confirm it really does list pack daemons.
- **Slack shipped** (`pix-integrations/slack`): read-only, PKCE with token
  rotation, credential in a 0600 file under `~/.local/state/pix/slack/`, atomic
  writes with validation before any filesystem work. Live: `auth login`
  authorized, `auth status` exits 0, doctor says "registered and answering".
- Two QA leftovers (`acme`, `pix-qa`) were polluting the real gateway. Removed.
- `pix pack use`'s "restarted managed pix services" claim — **verified this
  round**: the daemon is up and answering after that auto-restart.

### State of the three repos

All commits are **UNSIGNED** — the 1Password SSH agent cannot authorize
unattended — and **none are pushed**. Re-sign before pushing:

    git -C ~/dev/pix              rebase --exec 'git commit --amend --no-edit -S' origin/main
    git -C ~/dev/gm-pix-pack      rebase --exec 'git commit --amend --no-edit -S' origin/main
    git -C ~/dev/pix-integrations rebase --exec 'git commit --amend --no-edit -S' origin/main

At the time of writing that is 22 commits in pix (base 7218978), 5 in
gm-pix-pack (b5a6594) and 1 in pix-integrations (89e3f1d). `origin/main` is
written rather than those shas so the command stays correct if anything lands
first.

## Still open — deliberately, and recorded rather than hidden

| what | why it is acceptable for now |
|---|---|
| ~~Declarative setup has never actually RUN.~~ **CLOSED** — I ran it against isolated `PIX_CONFIG`s and it immediately found a bug (`82a9ce9`): under `--yes` the runner refused even `exec` remediations, which need no terminal, so the scripted path could not complete a step it was capable of completing. Three behaviours are now confirmed by running, not claimed: a satisfied requirement runs nothing; a failing probe runs its exec apply, prints its explain and re-verifies; an unmet `op-ref` reports the exact `pix secret set` command and runs NOTHING. | Still worth one real `pix setup --pack ~/dev/gm-pix-pack` with the vault unlocked, since the gm-pix-pack path specifically (gog install + browser OAuth) was never exercised. |
| ~~No `command`-transport server was proven to serve a real tool call.~~ **CLOSED by Mark's own in-sandbox healthcheck**, which is the only place this could be proven: **live Gmail search returned successfully** through `google-workspace`, a live BambooHR health call, a live Snowflake `SELECT 1`, 173 MCP tools, and all four remote servers authenticated. Also proved subagent dispatch across all three provider families. | Nothing left to verify here. |
| ~~`pix pack use` prints "restarted managed pix services" where the QA measured no pid change.~~ **Verified true this round**: the daemon is running and answering after that auto-restart. | Pre-existing in `service/reload.go`. Now MORE suspicious: `pix reset` hit the same class of bug — a launchd stop returns before the process exits — so this claim is probably true-but-early rather than false. Worth a look, undiagnosed. |
| **Memory recall precision**: Mark's healthcheck found 1 of 5 recalled records clearly off-topic, and some older project learnings possibly stale. | Not this work's area (memory ranking), but it is the only WARNING in an otherwise all-clear live probe, so it is the next real thing. |
| ~~Snowflake has no doctor row.~~ **CLOSED** — `runtime = "daemon"` plus the `daemons` probe. snow-proxy is supervised, health-checked, evicted when wedged, and shown in `pix doctor` and `pix serve status`. | Nothing left here. The remaining gap is narrower: a `[[proxy]]` with no accompanying `[[services]]` daemon still contributes no health check. |
| BambooHR's probe checks that a Docker image exists, not that the API key works. | Weak but honest; doctor does not overclaim. |
| `pix setup` aborts on the FIRST unmet requirement instead of listing all of them. | Three passes to learn two facts. Annoying, not misleading. |
| `pix secret ls` shows two red lines for stale `SLACK_TEAM_ID`/`SLACK_USER_ID`. | Left in place deliberately — they are IDs the operator may want when Slack returns. The message now offers removal. |
| Stray `acme` and `pix-qa` MCP registrations from test agents. **Removed this round** — doctor named both as unmanaged, which is how I found them. | Doctor names any unmanaged registration as information rather than ignoring it. That row earned its keep. |
| **`pack-trust.json` is 86 KB / 370 adoption records, 369 of them test temp dirs recording adoption of `https://example.com/attacker/pack.git`.** `TestClonePack_MarksAdoptionDurablyBeforeReturn` sets `XDG_DATA_HOME` but not `PIX_CONFIG`/`XDG_CONFIG_HOME`, and the trust-store path derives from `config.Path()`. | **Pre-existing** (from `bce2b1b`), not caused by this work, and `accepted` is unaffected. But a security boundary the test suite can write to is not a boundary — worth its own fix. |
| `pix secret check` is bounded per-ref (5s) but not overall, and does not bail early: 14 refs on a locked vault take 70s, all failing for the identical reason. | Terminates honestly, which was the bug. Early-bail is a refinement. |
| `PIX_CONFIG` isolates config but NOT host side effects — `pix pack use` with an isolated config still mutates the real sbx gateway. | Worth knowing before anyone assumes `PIX_CONFIG` makes a run safe. |
