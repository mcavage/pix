# Repo-less host setup — review synthesis

Five-lens review (product, architect, dx-consultant, growth-marketing, review
gate) of the plan to make the HOST side of pi-stack installable without cloning
the repo, symmetric with the sandbox (`sbx run pi-stack --kit git+...`).

## Verdict: GO-SMALLER

The original plan (prebuilt binary via `curl|sh`, fold every Make target into
the binary incl. `run`/`mcp-register`, XDG config where repo-local wins) is a
**NO-GO as written**. All five reviewers converge:

- Symmetry is aesthetic, not user value. The clone was never the hard part of
  running a host daemon — **lifecycle** and **version skew** are.
- One fatal, unresolved contradiction (below) that the plan never names.
- Several folds (`run`, `mcp-register`, `curl|sh`) actively make things worse.

Ship the small slice; kill the ambitious folds until the contract is defined.

## The load-bearing decision (do this FIRST)

**Prebuilt host = public-only and version-coupled to a released kit. Overlay
users clone and build.** State it explicitly in README + AGENTS.

Why it's forced: overlay host plugins are **compile-time Go source**
(`services/host/overlay_*.go` symlinked in, self-registering via `init()` into
`extraCommands`/`extraServiceFactories` — `main.go:30-35`, `serve.go:35-36`). A
frozen prebuilt binary cannot carry them. So "repo-less host" can only ever mean
"public-stack host" (memory, gws-token, slack). The intended adopt-at-work user
(the one with the MOST host surface) still builds from source. That is fine — but
it must be named, or the open-core headline quietly breaks. Do NOT try to fix
this with Go dynamic plugins (platform/signing/EDR pain, worse security story
than the current compile-time boundary).

## Version skew is the real risk

CI stamps a new `0.0.<run_number>` image on every push and commits the tag back
into `pi-kit/spec.yaml` (`.github/workflows/publish.yml`), so the git-kit image
moves fast. A host binary installed once does not move with it. Failure mode: a
`:0.0.40` host binary against a `:0.0.83` image → the image's memory extension
calls an RPC the old host answers with `method not found`
(`memory.go:587-589`) → user sees "memory is weird", not "host too old".

Contract required before shipping a standalone binary:
- `pi-stack-host version` reporting a build + protocol version.
- `doctor` compares host version against the running image/kit version and
  **hard-fails on incompatible** (not just a warning).
- Extraction-from-image is a dead end: the image is Linux, the host is
  macOS/Windows, and host services must run on the host to exec host creds
  (`gwstoken.go:50-56` execs `gws auth export`; MCP spawns `op run`).

## MVP scope (the slice worth building)

1. **Rename the Go module** `pi-stack/host` → a real path. Near-zero internal
   blast radius (single `package main`, no cross-package imports; overlay
   symlink build survives). Relocate `go:embed` defaults under `services/host/`
   (can't reach `../config`).
2. **Release artifact + checksums.** CI cross-compiles `pi-stack-host` for
   darwin/linux × amd64/arm64, attaches to the GitHub Release with SHA256s.
   Treat the prebuilt binary as **primary**; `go install` is best-effort only
   (a subdir module forces `services/host/vX.Y.Z` tags that collide with the
   single-`VERSION`/CI scheme — don't contort CI for it).
3. **Fold the pure-logic subcommands:** `doctor`, `version`, `init` (seeds XDG
   config, `--no-clobber`, **off by default**). Folding `doctor` also removes
   the `nc`/`hostname` deps the DHI base lacks and fixes repo-path leaks.
   `serve` already exists; switch it to read the config file.
4. **One canonical config schema** at `~/.config/pi-stack/config.env`, strict
   `KEY=value` (no spaces) so Make-`include`, shell `source`, and a Go dotenv
   reader all parse it identically (today's `local.mk` uses spaces — trap).
   Precedence is FLAT, no cwd-sniffing:
   `env > $PI_STACK_CONFIG or ~/.config/pi-stack/config.env > embedded`.
   Repo-local only via explicit `PI_STACK_CONFIG=`.
5. **A real supervision recipe** — launchd (macOS) / systemd-user (Linux) unit
   templates, not a bare `serve &`. Document start/stop/upgrade/uninstall.
6. **`doctor` leads with a one-line verdict**, then detail ordered by dependency
   (keys → ollama → memory → mcp). Keep its copy-pasteable `TODO: <exact cmd>`
   lines verbatim — best-in-class error surface, port unchanged.

## Explicit CUTS (do not build in v1)

- **`pi-stack-host run`.** Redundant with `bin/pi-stack`, and folding forces the
  binary to re-release to track every `sbx` flag change. `bin/pi-stack` is
  repo-relative by design (resolves ROOT/KIT/OVERLAY from its own location,
  stacks kits, mounts dev skills — `bin/pi-stack:25-58`); a prebuilt binary
  can't preserve that without reinventing repo+overlay discovery in Go. Keep
  launching in shell/Make.
- **Folding `mcp-register`** — blocked until the creation-time `--mcp` problem is
  solved. Registration ≠ attachment: local stdio servers (slack) are NOT
  surfaced by dynamic discovery and this sbx can't attach to a running sandbox,
  so slack only works if the sandbox was CREATED with `--mcp slack`
  (`AGENTS.md:118-123`). Cutting `run` while folding `mcp-register` leaves the
  repo-less Slack story broken (the README `sbx run` line has no `--mcp`). Also
  `mcp-register` bakes an absolute binary + `op-refs.env` path into the
  registration (`Makefile:185-188`); versioned install dirs would strand it on
  upgrade — needs a stable shim path if ever folded.
- **`curl|sh` that touches 1Password / starts services / registers MCP.**
  Clashes with the project's own "a network daemon spawning child processes is
  backdoor-shaped" threat model, and starts unauthenticated `gws-token`
  (`gwstoken.go:128-143`) + memory (`memory.go:7-12`) by default. A plain
  download-binary-and-verify-checksum installer is fine; anything that brokers
  creds or daemonizes is not.
- **Renaming everything to one human `pi-stack` binary.** DX wants it; the review
  gate refutes it — `bin/pi-stack` is a repo-relative launcher that can't become
  a prebuilt binary without a second product surface. Keep `pi-stack-host` as
  the host-services binary; keep `bin/pi-stack` as the launcher.

## GTM framing (do this regardless, it's cheaper than the feature)

The honest barrier is **`sbx` + cloud keys**, not the clone. Repo-less host
setup shifts a second-order gate from "annoying" to "gone", not a fatal one.
Higher-leverage: (1) film the already-stubbed demo gif — it IS the pitch;
(2) put the 5-command quickstart above the fold; (3) drop the "personal setup"
hedge from the top. Don't build a landing page, comparison docs, or a no-keys
free tier.

## Open decisions needing an owner call

1. **Boundary** (blocking everything): accept "prebuilt = public-only, overlay =
   source build" and document it? (Recommend: yes.)
2. **Compat contract**: ship `version` + a hard `doctor` skew check now, or punt
   the standalone binary until the contract exists? (Recommend: contract is part
   of the MVP, not optional.)
3. **Lifecycle**: ship launchd/systemd units in v1, or accept `serve &` rot?
   (Recommend: ship the units; it's the real work the plan under-scoped.)
