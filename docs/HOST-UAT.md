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
  stores, or asserts this entitlement on the operator's behalf — see
  `NOTICE.md`. Record who ran this UAT and against which entitlement in your
  own release notes; pix keeps no such record itself.
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
   this closes. A retired verb (`pix host`) exits 2 with `PIX_RETIRED`.
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
   tear the sandbox down, the LAST one does.
7. **`--keep` and the orphan reaper.** A kept sandbox survives its last shell,
   `pix rm --orphans` refuses to reap it, and an explicit `pix rm NAME` still
   removes it.
8. **Host services.** `pix serve status --json` publishes the supervision tree
   (identity, state, restarts, generation, reattached, last probe latency),
   `pix doctor --json` carries the same `supervisor` object, and neither leaks
   anything credential-shaped.
9. **Memory unit restart.** The memory CHILD is SIGKILLed: `:11435` must never
   stop accepting connections, the unit's generation must advance, and `pix
   memory stats` must answer again.
10. **launchd restart + mode-aware stop.** A managed serve killed with -9 is
    respawned by launchd; `pix serve stop` then actually stops it and it stays
    stopped (a bare SIGTERM would be undone by `KeepAlive` — invariant #3).
11. **External OAuth hooks** (`--with-oauth`): the catalog bundle registers,
    `pix mcp auth --all` completes per server, and `pix mcp ls` still reports
    host REGISTRATION rather than claiming session attachment.

Safety properties of the script itself, asserted or enforced:

- It refuses to run inside a sandbox, and refuses to start if a sandbox from a
  previous run of itself is still present.
- It only removes sandboxes it created (its own `pix-uat-<pid>-*` names, plus
  names it watched appear during its own run). It contains no `pix rm --all`
  and no `--force`, and greps ITSELF for those shapes before doing anything.
- It works in a temp tree and asserts `$PWD` is unchanged at exit.
- It uninstalls a launchd service it installed and restarts a serve it stopped,
  in a trap that runs on every exit path.

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
- [ ] `pix monitor --json | head -5` prints valid NDJSON for the session just
      run (needs monitor enabled in `pix serve`).
- [ ] `pix mcp ls` shows the configured servers; `pix-host mcp --list` prints
      nothing (Slack/Google Workspace are external, gateway-run, never
      host-binary-served).
- [ ] `/help` and `/getting-started` render the capability map.
- [ ] Exit every shell attached to the test sandbox; it tears itself down
      (`pix ls` no longer lists it) with no `pix rm` call.
- [ ] `pix rm pix-uat-manual` if anything survived.

## Step 4 — independent in-sandbox verification pass

Paste this into a fresh session in the loaded sandbox once steps 1-3 pass, for
a second opinion that assumes nothing from your own session:

```
Run the healthcheck skill. Then, without assuming anything from this
conversation, verify from scratch: (1) `pix help --all` lists no retired verb —
cross-check against services/host/cmd/pix/corpus/retirement.jsonl; (2) `pix
task new`, `pix task ls`, `pix task rm` complete cleanly against this checkout;
(3) `pix monitor --json | head -5` prints valid NDJSON; (4) `pix mcp ls` and
`pix-host mcp --list` agree that Slack/Google Workspace are gateway-registered,
not host-binary-served. Report each as pass/fail with the exact command and
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
