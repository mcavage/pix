# Host UAT

An agent working inside a pix sandbox cannot run `make load` / `make run` —
those need the HOST's Docker + `sbx` CLI, which the sandbox has no access to
(see AGENTS.md, "Build → load → run"). This is what a **human** runs on their
own Docker-Hardened-Image-entitled host to verify a release end to end before
it ships.

UAT is two artifacts, and they do different jobs:

| artifact | what it is | verdict |
| --- | --- | --- |
| `scripts/macos/verify-pix-lifecycle.sh` | the machine-checkable half: real assertions against the real `pix`/`sbx`/`docker` on your host | exits 0 / 1 / 2, prints per-check PASS·FAIL·SKIP |
| this document | the half a script cannot judge — image provenance, interactive TTY behaviour, agent-facing quality | your signature |

Neither is optional, and neither prints a verdict it did not earn: the script
counts skipped checks and refuses to report a clean pass when any check could
not run.

## Prerequisites (the human's own, never pix's)

- A Docker account with its own DHI entitlement, obtained directly from
  Docker. Building the image pulls a DHI base layer; pix never requests,
  stores, or asserts a third party's entitlement on their behalf — see
  `NOTICE.md`. Redistribution of the PUBLISHED `docker.io/<ns>/pix` image is
  authorized separately and durably in `docs/legal/AUTHORIZATIONS.md` (A-1);
  that record covers this project's own publish, not your account. Record who
  ran this UAT and against which entitlement in your own release notes; pix
  keeps no such record itself.
- `sbx` installed and authenticated, `docker` running, `pix` on PATH
  (`make install`).
- A clean checkout of the release commit/tag.

## Step 1 — build and load the image (human, ~10 min)

```bash
make gate                      # build, vet, go test, node test, tsc
make build                     # docker build, tagged VERSION from the Makefile
make load                      # docker save + sbx template load (heavy, ~1GB)
```

Then confirm the version that loaded is the one you built, not a stale pull,
and that every extension still loads with no API key present:

```bash
VERSION=$(grep -m1 '^VERSION' Makefile | sed 's/.*= *//')
docker run --rm --entrypoint pi "docker.io/mcavage/pix:$VERSION" --version
docker run --rm "docker.io/mcavage/pix:$VERSION" bash -lc 'pi -p hi'
# expect "No API key" (extensions loaded). "Failed to load extension" FAILS UAT.
```

## Step 2 — run the lifecycle verification script (machine)

```bash
bash scripts/macos/verify-pix-lifecycle.sh                # lifecycle + host services
bash scripts/macos/verify-pix-lifecycle.sh --with-oauth   # + the interactive OAuth pass
bash scripts/macos/verify-pix-lifecycle.sh --no-services  # lifecycle only (CI-ish)
```

What it asserts, and why each one is in a RELEASE gate rather than a unit test
(each needs the real sbx runtime, a real daemon, or a real process kill):

1. **Command + flag surface.** `pix help --all` and every verb's help exit 0,
   and each flag the script itself uses is declared by that verb — a UAT script
   passing because it typed a flag that no longer exists is the failure mode
   this closes. A deleted verb gets the ordinary unknown-command answer: pix has
   no released users, so there is no retirement notice and no `PIX_RETIRED`
   sentinel to assert on.
2. **Digest naming.** Two workspaces sharing a basename produce two DISTINCT
   `pix-<basename>-<digest>` sandboxes. Aliasing here would let one repo's
   sandbox answer for another's.
3. **Instance identity.** The lease record for a launched sandbox carries an
   `instance_id`.
4. **Attach fingerprint.** Re-running against an existing sandbox with a
   changed MCP set is REFUSED, not silently attached.
5. **Exit propagation.** A failing inner command surfaces as a non-zero `pix
   run`; a bare `pix <not-a-dir>` refuses with exit 2 and creates nothing.
6. **Multi-shell teardown.** Two shells attach; the FIRST to leave does not
   tear the sandbox down, the LAST one does. The create/attach readiness
   waits are bounded appropriately for a COLD post-`make load` image pull
   (180s to observe the first shell's create, 90s to observe the second
   shell's attach, by default), not a flat 30s that a real pull can exceed;
   both windows (and the polling interval) are overridable via
   `UAT_CREATE_WAIT_SECS` / `UAT_ATTACH_WAIT_SECS` / `UAT_POLL_INTERVAL`.
7. **`--keep` and the orphan reaper.** A kept sandbox survives its last shell,
   `pix rm --orphans` refuses to reap it, and an explicit `pix rm NAME` still
   removes it.
8. **Host services.** `pix serve status --json` publishes the supervision tree
   (identity, state, restarts, generation, reattached, last probe latency),
   `pix doctor --json` carries the same `supervisor` object, and neither leaks
   anything credential-shaped. Before any of this, the script PREFLIGHTS an
   already-running serve: it resolves the running pid's executable and
   compares it to the `pix-host` this run just built. Both paths are resolved
   through their FINAL symlink component (`resolve_symlink_final`, a portable
   readlink loop — macOS has no `readlink -f`/`realpath` guarantee), so a
   make-install symlink and the real executable `lsof` reports for the
   running pid compare equal instead of false-mismatching. Any pre-existing
   daemon stops the run with the exact `pix serve stop`/`kill <pid>` remedy:
   even a matching unmanaged daemon cannot prove launchd install/respawn
   semantics. With a clear host, the script installs and starts the current
   build itself, reversibly, then uninstalls it on exit.
9. **Memory unit restart.** The memory CHILD is SIGKILLed: `:11435` must never
   stop accepting connections, the unit's generation must advance, and `pix
   memory stats` must answer again.
10. **launchd restart + mode-aware stop.** A managed serve killed with -9 is
    respawned by launchd; `pix serve stop` then actually stops it and it stays
    stopped (a bare SIGTERM would be undone by `KeepAlive` — invariant #3).
11. **External OAuth hooks** (`--with-oauth`): the catalog bundle registers
    only when one or more shipped servers are missing; the script tracks and
    removes exactly the individual registrations it added, preserving every
    pre-existing same-name registration. Before forcing ANY browser flow, it
    reads CURRENT `pix doctor --json` evidence for notion/atlassian/granola
    first: a server already registered and authenticated (the common case on
    a host that has run this before) is certified a PASS right there, with
    zero `pix mcp auth` invocation for it — a rerun on an already-authorized
    host never re-forces a browser flow. Only servers doctor cannot already
    certify are gaps, and it then authorizes ONLY those gap servers
    INDIVIDUALLY — `pix mcp auth notion`, `pix mcp auth atlassian`, `pix mcp
    auth granola` — ASSERTING each one's own exact exit code and output (never
    just firing `pix mcp auth --all` and hoping): a sweep-all call would rope
    in every OTHER server registered on the host too, so one unrelated,
    broken 8th server (a private pack's, or leftover state from a prior
    session) can never fail a release check for a server this release never
    shipped and never asked to authorize. Completion is then, per server,
    CERTIFIED against a machine-readable probe (`pix doctor --json`'s
    per-server registered/authenticated evidence) rather than an operator's
    say-so: a
    PASS requires the exact registered-and-authenticated evidence line, "not
    registered" and "registered, not authenticated" are each their own
    explicit FAIL, and anything else (unclassified, unknown, or no evidence
    line at all) is an honest SKIP, never a silent PASS. An operator
    confirmation is optional and additive: it reads a
    bounded `read -t` from `/dev/tty` specifically (never the script's own
    stdin), through a real fd OPEN attempted first with its own stderr
    suppressed — `/dev/tty` can fail to open (ENXIO, no controlling terminal)
    even when its `-r`/`-w` permission bits pass, e.g. under a backgrounded or
    fully non-interactive invocation, and an unsuppressed failed open would
    otherwise print raw device-open noise into the release log. A closed,
    absent, silent, or declined optional confirmation is
    only an informational note, never FAIL or SKIP; the machine probe is the
    real verdict. Finally, `pix mcp ls` is
    checked for the honest host-registration disclaimer (a POSITIVE claim it
    must contain) and for the absence of any present-tense session-attachment
    claim (a precise NEGATIVE regex) — not a bare substring search for
    "attached", which the disclaimer's own honest prose ("not what's attached
    to...") would always trip.
12. **Snapshot secret scan is honest about empty units.** The `serve status
    --json` credential-shape scan does not pass vacuously when the published
    `units[]` is empty during a service-enabled (`--with-services`, the
    default) full run: an empty snapshot has nothing in it to have scanned, so
    a negative regex trivially "finding no secrets" there is not proof of
    cleanliness. Memory always runs as a supervised unit in that mode, so zero
    units is a real gap; the check FAILS explicitly instead of reporting a
    free PASS.
13. **`serve: not running` cannot be misread as running.** The install-if-down
    gate reads `pix serve status --json`'s own boolean `running` field, never
    a text substring match: the down state's human-readable line is literally
    `serve: not running`, which CONTAINS the substring "running" — a bare
    `grep -q running` matches (and reports "found") in BOTH states, so `! ...`
    was permanently false and silently skipped installing serve when it was
    actually down.

Safety properties of the script itself, asserted or enforced:

- It refuses to run inside a sandbox, and refuses to start if a sandbox from a
  previous run of itself is still present.
- It only removes sandboxes it created (its own `pix-uat-<pid>-*` names, plus
  names it watched appear during its own run). It contains no `pix rm --all`
  and no `--force`, and greps ITSELF for those shapes before doing anything.
- It works in a temp tree and asserts `$PWD` is unchanged at exit.
- It uninstalls a launchd service it installed (which also covers a serve it
  stopped mid-run), and removes only MCP catalog servers it registered, in a trap
  that runs on every exit path.
- Every backgrounded `pix run` reads from `/dev/null`, never an inherited
  terminal or the script's own stdin, and every `wait` on one is bounded
  (`bounded_wait`): a wedged background run is killed and reported, never left
  to hang the rest of the script.
- Host-service checks preflight WHICH `pix-host` binary an already-running
  `serve` is before trusting it, so the checks can never silently grade a
  stale, unmanaged daemon left over from before this build.

**Exit codes:** `0` every check passed · `1` a check failed · `2` incomplete
(missing prerequisite, refused to start, or any SKIP — an incomplete run is not
a release verdict).

## Step 3 — what only a human can judge (interactive)

Launch a real interactive session and confirm by hand:

```bash
cd /path/to/some/test/repo
pix run --name pix-uat-manual
```

- [ ] The TUI renders correctly while streaming: the input box and powerbar
      stay pinned (this is the vendored `tui-bottom-pin` patch; a jittering
      input box means the patch did not apply to this pi version).
- [ ] `pix status` shows the sandbox as running; `pix doctor` reports
      EVIDENCE, not a bare "ok"/"configured" claim.
- [ ] `pix task new uat-check` creates an isolated checkout + branch; `pix task
      ls` shows it; `pix task rm uat-check` persists the branch and tears the
      checkout down.
- [ ] `pix mcp ls` shows the configured servers; `pix-host mcp --list` prints
      nothing (pix ships no general/vendor MCP server: every one is pack-declared and
      gateway-run, never host-binary-served. uat-mcp is the ephemeral exception).
- [ ] `/help` and `/getting-started` render the capability map.
- [ ] Exit every shell attached to the test sandbox; it tears itself down
      (`pix ls` no longer lists it) with no `pix rm` call.
- [ ] `pix rm pix-uat-manual` if anything survived.

## Step 4 — self-UAT and browser bootstrap

Run the new automated self-UAT locally to verify Chrome/profile integration:

```bash
pix uat status
pix uat browser bootstrap
```
(Wait for the browser to launch on the persistent 0700 profile, then Ctrl-C to stop).

## Step 5 — independent in-sandbox verification pass (legacy oracle)

Paste this into a fresh session in the loaded sandbox once steps 1-3 pass, for
a second opinion that assumes nothing from your own session:

```
Run the healthcheck skill. Then, without assuming anything from this
conversation, verify from scratch: (1) `pix help --all` is the whole verb set, and
every command string in docs/ and skills/ resolves against it (`node --test
tests/verb-references.test.mjs` is the mechanical form of this); (2) `pix
task new`, `pix task ls`, `pix task rm` complete cleanly against this checkout;
(3) `pix mcp ls` and `pix-host mcp --list` agree that every registered server is
gateway-run from a pack declaration, not host-binary-served — `pix-host mcp
--list` must be empty. Report each as pass/fail with the exact command and
output, not a summary claim.
```

## After UAT

Record the result (the script's exit code and per-check output, your answers to
step 3, who ran it, against which DHI entitlement) in your own release
tracking — this is a per-user, per-release artifact and deliberately does not
live in this public repo (see AGENTS.md's open-core boundary and `NOTICE.md`'s
affiliation disclaimer).

If a host service misbehaved during UAT, `docs/runbooks/host-services.md` is
the on-call runbook: SLIs, the alert-to-response table, and recovery order.
