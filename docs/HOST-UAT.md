# Host UAT script

An agent working inside a pix sandbox cannot run `make load` / `make run` —
those need the HOST's Docker + `sbx` CLI, which the sandbox has no access to
(see AGENTS.md, "Build → load → run"). This is the exact script and prompt to
hand to a **human**, on their own Docker-Hardened-Image-entitled host, to
verify a release end to end before it ships. Nothing here runs unattended;
each step names what a human confirms.

## Prerequisites (the human's own, never pix's)

- A Docker account with its own DHI entitlement, obtained directly from
  Docker. Building the image pulls a DHI base layer; pix never requests,
  stores, or asserts a third party's entitlement on their behalf — see
  `NOTICE.md`. Redistribution of the PUBLISHED `docker.io/<ns>/pix` image is
  authorized separately and durably in `docs/legal/AUTHORIZATIONS.md` (A-1);
  that record covers this project's own publish, not your account. Record who
  ran this UAT and against which entitlement in your own release notes.
- `sbx` installed and authenticated.
- A clean checkout of the release commit/tag.

## Script

```bash
# 1. Build and load the image this release ships.
make gate                      # fast local gate: build, vet, go test, node test, tsc
make build                     # docker build, tagged VERSION from Makefile
make load                      # docker save + sbx template load (heavy, ~1GB)

# 2. Confirm the version actually loaded (not a stale cached pull).
docker run --rm --entrypoint pi docker.io/mcavage/pix:$(make -s print-version 2>/dev/null || grep -m1 '^VERSION' Makefile | sed 's/.*= *//') --version

# 3. Load-check every extension with no API key present.
docker run --rm docker.io/mcavage/pix:<version> bash -lc 'pi -p hi'
# expect: "No API key" (extensions loaded). "Failed to load extension" fails the UAT.

# 4. Fresh sandbox, interactive TTY, bare positional launch.
cd /path/to/some/test/repo
pix run --name pix-uat-test
```

**Confirm by hand, in the sandbox:**

- [ ] `pix status` shows the sandbox as running.
- [ ] `pix doctor` reports evidence, not a bare "ok"/"configured" claim.
- [ ] `pix task new uat-check` creates an isolated checkout + branch; `pix
      task ls` shows it; `pix task rm uat-check` persists the branch and
      tears the checkout down.
- [ ] `pix monitor --json` shows NDJSON events for the session just run (if
      `pix serve` has monitor enabled).
- [ ] `pix mcp register && pix mcp ls` shows the configured servers as
      registered (Slack/Google Workspace are external — confirm they resolve
      through the gateway, not a built-in `pix-host` subcommand: `pix-host
      mcp --list` must print nothing).
- [ ] A retired verb answers correctly: `pix host` prints a `PIX_RETIRED`
      line and exits 2 — confirm nothing else happened (no sandbox, no file).
- [ ] From a NON-interactive shell (e.g. `pix run/ 2>&1 | cat` from a
      directory named `run`, or any pipe), a bare `pix DIR` refuses with exit
      2 and creates nothing; the explicit `pix run DIR` still works from the
      same non-interactive shell.
- [ ] Exit every shell attached to the test sandbox; confirm it tears itself
      down automatically (`pix ls` no longer lists it) with no `pix rm` call.

## The exact prompt for an in-sandbox agent verification pass

Paste this into a fresh session in the loaded sandbox once the checklist
above passes, to get an independent second opinion before signing off:

```
Run the healthcheck skill. Then, without assuming anything from this
conversation, verify from scratch: (1) `pix help --all` lists no retired
verb — cross-check against
services/host/cmd/pix/corpus/retirement.jsonl; (2) `pix task new`,
`pix task ls`, `pix task rm` complete cleanly against this checkout; (3)
`pix monitor --json | head -5` prints valid NDJSON; (4) `pix mcp ls` and
`pix-host mcp --list` agree that Slack/Google Workspace are gateway-
registered, not host-binary-served. Report each as pass/fail with the exact
command and output, not a summary claim.
```

## After UAT

Record the result (pass/fail per checklist item, who ran it, against which
DHI entitlement) in your own release tracking — this is a per-user,
per-release artifact and deliberately does not live in this public repo (see
AGENTS.md's open-core boundary and `NOTICE.md`'s affiliation disclaimer).
