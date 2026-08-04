# Release legal/compliance safeguards (W0)

Implements AC-REL-01..04. Everything here is disjoint from application
source: new scripts under `scripts/legal/` and `scripts/release/`, new tests
under `tests/legal-*.test.mjs` + `tests/slow/`, a new standalone CI workflow
(`.github/workflows/legal.yml`), and small, additive edits to `Dockerfile`
(a build ARG + two `COPY` lines) and `.github/workflows/publish.yml` (bundle
the notices into the Homebrew tarball). No behavior of the agent, its
extensions, or its skills changed.

## AC-REL-01 — generated THIRD_PARTY_NOTICES + fail-closed license gate

- `scripts/legal/dependencies.json` — hand-maintained ledger (same convention
  as `scripts/arch-metrics/budgets.json` / `services/host/routing/scorecard.json`):
  every Go module actually reachable from `services/host`'s build graph (49,
  derived via `go list -deps`, see `scripts/legal/list-go-modules.sh`), every
  npm package baked into the image, the **MPL-2.0** entries for
  `github.com/hashicorp/go-plugin` and `github.com/hashicorp/yamux`, and a
  **planned** entry for `github.com/thejerf/suture` (not yet vendored —
  license verified via its published `LICENSE` file, MIT, but flagged as a
  placeholder to re-verify at add-time).
- `bakedTools` (same file) — the **directly-downloaded static binaries** the
  Dockerfile `curl`s straight from a GitHub Releases page (or, for Go,
  `go.dev/dl`) and bakes into the image, rather than installing via npm/`go.mod`
  or apt: `ruff` (astral-sh/ruff, MIT), `fd` (sharkdp/fd, dual MIT OR
  Apache-2.0), and the `go` toolchain itself (golang/go, BSD-3-Clause). Each
  entry's `licenseEvidence` is a live fetch of the upstream `LICENSE` file AT
  THE EXACT PINNED TAG (not a generic "well-known license" claim), and each
  carries a `dockerfileArg` (`RUFF_VERSION`/`FD_VERSION`/`GO_VERSION`) that
  `validateBakedTools()` cross-checks against the live Dockerfile — a version
  bump in one without the other fails closed (see AC-REL-01 gate below).
  Every OTHER apt-installed tool in the Dockerfile (`git`, `gh`, `ripgrep`,
  `hostname`, `gzip`, `curl`, `which`, `build-essential`, `xxd`, the `python3`
  trio) is deliberately OUT of this ledger: they are Debian/DHI-patched
  packages pulled from the base OS's own package repository, not a bespoke
  binary pix fetches and bundles itself, and Debian's own package/copyright
  system already carries that attribution obligation at the OS layer. This is
  a documented scoping decision, not an oversight — re-open it if pix ever
  starts vendoring one of those binaries directly instead of using apt.
- `scripts/legal/notices-policy.json` — the fail-closed gate: only
  `permissive` and (explicitly allowlisted) `weak-copyleft` classes pass;
  everything else, including an **undeclared** live dependency, fails closed.
- `scripts/legal/generate-third-party-notices.mjs` — renders
  `THIRD_PARTY_NOTICES.md` from the ledger; `--check-live <file>` validates a
  live `module@version` list against the ledger + policy.
- `scripts/check-third-party-notices.sh` — the CI check: regenerate + diff
  (no stale notices), live license-class gate, the `bakedTools` version gate
  (`--check-baked-tools Dockerfile`, ruff/fd/go pins match the ledger),
  required-attribution assertions (go-plugin/yamux MPL-2.0, the Suture
  planned entry, the patched pi-tui, ruff/fd/go), and **inclusion** checks
  (Dockerfile `COPY`, the Homebrew tarball in `publish.yml`).

## AC-REL-02 — tarball/image inclusion

- `Dockerfile` now `COPY`s `THIRD_PARTY_NOTICES.md` and `NOTICE.md` into the
  image (`/home/agent/.pi/agent/`).
- `.github/workflows/publish.yml`'s Homebrew darwin tarball step now bundles
  both files alongside `pix`/`pix-host`/`pix.1`.
- Both are asserted by `scripts/check-third-party-notices.sh` and
  `tests/legal-inclusion-and-docker-base.test.mjs`.

## AC-REL-03 — Docker base image: explicit digest/build-arg path

- `Dockerfile`: `ARG BASE_IMAGE=dhi.io/node:25-debian13-dev` replaces the bare
  `FROM dhi.io/node:25-debian13-dev` (default behavior unchanged — `make
  load`/CI still resolves the same tag). An immutable build now pins
  `--build-arg BASE_IMAGE=dhi.io/node:25-debian13-dev@sha256:<digest>`.
- `scripts/release/resolve-base-digest.sh` resolves that digest via a LIVE
  `docker buildx imagetools inspect` against your own DHI-entitled session —
  it refuses to guess or fabricate one, and its JSON-parsing logic is unit
  tested against a fixture so the *parsing* is provably correct without
  needing Docker/network in CI.
- A documented **public fallback** (`docker.io/library/node:25-bookworm`) is
  named for contributors without DHI entitlement — explicitly NOT claimed as
  a validated hardening-equivalent, and NOT a claim of any right (resolved or
  unresolved) to DHI beyond what the operator building the image has
  separately obtained from Docker, Inc.

## AC-REL-04 — SBOM/provenance for an immutable version/digest

- `scripts/release/verify-provenance.sh` — records `out/provenance/<version>.json`
  (version, digest, git sha, timestamp) **after** `publish.yml`'s `merge` job
  assembles the multi-arch manifest and captures its digest. A version's
  digest is immutable once recorded: a re-run with a *different* digest for
  an already-recorded version fails closed (a same-digest re-run is a no-op).
- `scripts/legal/sbom-config.json` — pins the SBOM tool (`anchore/sbom-action`,
  SHA-pinned) and states it reuses the SAME license-class policy as the
  notices gate, rather than drifting. Wired as a **non-blocking** job in
  `legal.yml` (no human has reviewed an SBOM-diff gating policy yet — see
  `docs/legal/FINDINGS.md`).

## Full-history secret scan

- `scripts/legal/secret-scan.mjs` — pattern rules (AWS/GitHub/Slack/Google
  tokens, PEM private keys, embedded Basic-Auth URLs, generic Bearer tokens),
  a `--self-test`, and `--scan <repo> --out <report.json>` which walks
  `git rev-list --objects --all` — every blob reachable from every ref, not
  just the current tree — so a secret buried in an amended-away commit is
  still caught (proven by `tests/legal-secret-scan.test.mjs`).
- **Traversal is a TRULY BATCHED git-plumbing pipeline, not a per-object
  spawn loop.** The original version ran three `spawnSync("git", ...)` calls
  PER OBJECT (`cat-file -t`, `cat-file -s`, `cat-file blob`) — O(objects)
  process spawns, ~24s on this repo's ~700-commit history and scaling
  linearly with history size. It now does ONE `git rev-list --objects --all`,
  ONE `git cat-file --batch-check` (fed every object at once, via stdin) to
  get type+size, and a small, FIXED number of `git cat-file --batch` calls
  (fed only the wanted blob shas, chunked by cumulative content size so
  memory stays bounded — never one call per object) to fetch content. Full
  scan of this repo's real history: **~0.75s**, down from ~24s (~30x). The
  call count is exported (`gitCallCount`/`resetGitCallCount()`) so
  `tests/legal-secret-scan.test.mjs` PROVES it stays flat as history size
  grows (3-file vs. 40-file/40-commit fixture repos both scan in <= 5 git
  invocations), not just that the output is unchanged. Fail-closed behavior
  and every existing fixture/allowlist semantic (self-exclusion of the
  allowlist's own path, content-addressed fingerprinting, a different secret
  value anywhere still failing closed) are unchanged and re-verified by the
  same test suite plus new unit tests for the batched plumbing helpers
  (`batchCheck`, `batchContent`, `parseBatchOutput`).
- Run for real against this repo's actual history: confirmed clean (0
  findings) with the reviewed allowlist. Every non-zero raw match seen along
  the way was confirmed to be this repo's OWN test fixtures for its
  secret-redaction logic (AWS's public `AKIAIOSFODNN7EXAMPLE`, Slack's
  documented `xoxb-1234567890-...` example shape, synthetic
  `user:token@host` URLs, and this safeguard's own `--self-test` fixture
  constants) — never a live credential. Each is allowlisted by
  content-addressed fingerprint (`sha256(blob:path:match)`) in
  `scripts/legal/secret-scan-allowlist.txt`, with a comment recording why. A
  **different** secret value anywhere in history is NOT covered by this list
  and still fails closed. (Editing `secret-scan.mjs`/its test file itself
  changes THEIR OWN blob hash and therefore their fingerprints — this
  rewrite closed that exact gap, which a prior commit had left open; see the
  allowlist file's own comments for the fixed-point mechanics.)
- `scripts/check-secret-history.sh` wraps it with the repo's allowlist and
  writes `out/secret-scan/report.json`; `.github/workflows/legal.yml`'s
  `secret-scan` job fetches every ref before running it and uploads the
  report as an artifact even on failure.

## Affiliation disclaimer

- `NOTICE.md` (new, root-level, disjoint file): pix is an independent
  project, not affiliated with/endorsed by/sponsored by Docker, Inc.,
  HashiCorp, or any other referenced vendor; trademark references are
  descriptive only. Baked into the image and the Homebrew tarball alongside
  `THIRD_PARTY_NOTICES.md`. `README.md` was deliberately left untouched (a
  shared, frequently-edited file) — the disclaimer lives entirely in the new
  file so this change stays disjoint.

## What's still open

See `docs/legal/FINDINGS.md` — unresolved items that need a human (counsel or
the DHI account holder), recorded separately rather than asserted as done.
