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
- `scripts/legal/notices-policy.json` — the fail-closed gate: only
  `permissive` and (explicitly allowlisted) `weak-copyleft` classes pass;
  everything else, including an **undeclared** live dependency, fails closed.
- `scripts/legal/generate-third-party-notices.mjs` — renders
  `THIRD_PARTY_NOTICES.md` from the ledger; `--check-live <file>` validates a
  live `module@version` list against the ledger + policy.
- `scripts/check-third-party-notices.sh` — the CI check: regenerate + diff
  (no stale notices), live license-class gate, required-attribution
  assertions (go-plugin/yamux MPL-2.0, the Suture planned entry, the patched
  pi-tui), and **inclusion** checks (Dockerfile `COPY`, the Homebrew tarball
  in `publish.yml`).

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
- Run for real against this repo's actual history: 340 raw matches, all
  confirmed to be this repo's OWN test fixtures for its secret-redaction
  logic (AWS's public `AKIAIOSFODNN7EXAMPLE`, Slack's documented
  `xoxb-1234567890-...` example shape, synthetic `user:token@host` URLs) —
  never a live credential. Each is allowlisted by content-addressed
  fingerprint (`sha256(blob:path:match)`) in
  `scripts/legal/secret-scan-allowlist.txt`, with a comment recording why. A
  **different** secret value anywhere in history is NOT covered by this list
  and still fails closed.
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
