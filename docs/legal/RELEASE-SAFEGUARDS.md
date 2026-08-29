# Release legal/compliance safeguards (W0)

Implements AC-REL-01..04. The mechanisms live in `scripts/legal/`,
`scripts/release/`, `tests/legal-*.test.mjs` + `tests/slow/`,
`.github/workflows/legal.yml`, and the release path in
`.github/workflows/publish.yml`. No behavior of the agent, its extensions, or
its skills changed.

## `legal.yml` is the PRE-PUBLISH gate, not a workflow beside it

The first cut of this landed `legal.yml` as a deliberately standalone workflow
so it could evolve without touching `publish.yml`'s job graph. That was wrong
in one specific, load-bearing way: **GitHub does not order two workflows
against each other.** A push to `main` started `legal` and `publish`
concurrently, so `publish` could push `pix:<version>` while the full-history
secret scan, the third-party/license gate, and the SBOM were still running —
or after they had already gone red. A gate that runs beside the thing it gates
is a report, not a gate.

`legal.yml` now also declares `on: workflow_call`, and `publish.yml` calls it
as the `legal-gate` job. **Both** `build` (which pushes layers by digest) and
`merge` (which creates the versioned tag) list it in `needs`, so no bytes
reach the registry until it is green. It still runs on its own for pull
requests — one definition, two entry points.
`tests/legal-workflow.test.mjs` parses both workflows and proves the
dependency edges, so nobody can quietly drop the `needs` and keep the file.

## AC-REL-01 — generated THIRD_PARTY_NOTICES + fail-closed license gate

- `scripts/legal/dependencies.json` — hand-maintained ledger (same convention
  as `scripts/arch-metrics/budgets.json` / `services/host/inference/catalog/models.json`):
  every Go module actually reachable from `services/host`'s build graph (**34**,
  derived via `go list -deps` across the release GOOS/GOARCH set, see
  `scripts/legal/list-go-modules.sh`), every
  npm package baked into the image, the **MPL-2.0** entries for
  `github.com/hashicorp/go-plugin` and `github.com/hashicorp/yamux`, and the
  **MIT** entry for `github.com/thejerf/suture/v4` (live in `go.mod` since
  U07's host `serve` supervision tree, not a placeholder — license verified
  against the vendored module cache at the pinned `v4.0.6`).
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
- **The live-vs-ledger check runs BOTH directions now.** It only ever ran
  forward (a live module with no ledger row fails), which means ledger
  staleness could only grow: 24 rows for the `charmbracelet`/`glamour`
  dependency tree survived the monitor TUI's deletion and were still being
  published as attribution for code pix does not ship, while
  `RELEASE-SAFEGUARDS.md` claimed a module count (49) nobody could reproduce
  from `go list`. `validateLedgerLiveness()` is the reverse gate: a ledger row
  that is not in the live build graph fails closed. It refuses to judge
  against an EMPTY live list, so a broken `go list` reads as "undecidable",
  never as "every row is stale".
- **Global npm pins are gated too.** `typescript` was installed as a bare
  `npm install -g typescript`, i.e. whatever the registry served that minute,
  while the ledger recorded an exact version and license — a claim about an
  unreproducible build. The Dockerfile pins `ARG TYPESCRIPT_VERSION` and
  `--check-npm-pins` cross-checks it against the ledger row (and
  `check-third-party-notices.sh` also requires `package.json`'s devDependency
  to agree, and fails if the install line loses its `@${TYPESCRIPT_VERSION}`).
- `scripts/legal/generate-third-party-notices.mjs` — renders
  `THIRD_PARTY_NOTICES.md` from the ledger; `--check-live <file>` validates a
  live `module@version` list against the ledger + policy.
- `scripts/check-third-party-notices.sh` — the CI check: regenerate + diff
  (no stale notices), live license-class gate, the `bakedTools` version gate
  (`--check-baked-tools Dockerfile`, ruff/fd/go pins match the ledger),
  required-attribution assertions (go-plugin/yamux MPL-2.0, the live Suture
  entry, the patched pi-tui, ruff/fd/go), and **inclusion** checks
  (Dockerfile `COPY`, the Homebrew tarball in `publish.yml`).

### MPL-2.0 disclosure (B1)

- `licenses/MPL-2.0.txt` — the **full, verbatim** MPL-2.0 text, shipped in
  the image and the Homebrew tarball. The notices previously said license
  texts were "reproduced below" in one paragraph and "not reproduced
  verbatim here" in another; that contradiction is gone, and
  `check-third-party-notices.sh` fails if the blanket "not reproduced" claim
  ever comes back while the file ships.
- Each MPL-2.0 ledger entry carries a `sourceUrl` pinned to the **exact
  version linked** (`.../tree/v1.8.0`, `.../tree/v0.1.2`) and a
  `licenseTextFile`. `--check-copyleft-disclosure` (new,
  `validateCopyleftDisclosure()`) fails closed if a weak-copyleft entry has
  no https source URL, a URL that does not pin the ledger version, or names
  a license text that is not actually present in the tree.

## AC-REL-02 — tarball/image inclusion

- `Dockerfile` now `COPY`s `THIRD_PARTY_NOTICES.md`, `NOTICE.md`, `LICENSE`
  and `licenses/` into the image (`/home/agent/.pi/agent/`). `LICENSE` is
  there because MIT §2 requires the notice to travel with copies, and
  `licenses/` because MPL-2.0 §3.1 requires recipients be told the terms.
- `.github/workflows/publish.yml`'s Homebrew darwin tarball step now bundles
  all four alongside `pix`/`pix-host`. (The man page, `pix.1`, was retired
  along with `pix man`/`--man`, so it is no longer part of this tarball.)
- **The bare `pix-darwin-<arch>` / `pix-host-darwin-<arch>` release assets are
  gone.** Bundling the notices into the tarball did not help the artifact most
  non-Homebrew users actually installed: `install.sh` fetched the two loose
  binaries, which are a distribution of pix (MIT s2) and of the MPL-2.0 code
  linked into `pix-host` (MPL-2.0 s3.1) with no notices attached at all. The
  release now publishes only the notice-bearing tarballs plus `SHA256SUMS`;
  `install.sh` downloads the tarball, verifies its sha256 against
  `SHA256SUMS`, refuses to install if any binary OR any required notice is
  missing from it, and installs the notices to
  `${XDG_DATA_HOME:-~/.local/share}/pix` next to the binaries. A release step
  additionally unpacks each tarball and asserts the notices are inside it — a
  check on the bytes, not on the workflow text that made them.
- All of it is asserted by `scripts/check-third-party-notices.sh`,
  `tests/legal-inclusion-and-docker-base.test.mjs`, and
  `tests/install.test.mjs` (which runs the installer end to end against a
  synthetic local release: a good tarball installs, a checksum mismatch
  installs nothing, a notice-less tarball is refused).

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
  assembles the multi-arch manifest and captures its digest. Scope, stated
  precisely: it fails closed when a version's digest would change **given a
  record it can read**. In `publish.yml` that record is written into a fresh,
  ephemeral runner workspace, so a later run has nothing to compare against
  — **this script does not, and cannot, enforce immutability across runs.**
  Anything that says otherwise is wrong; see the tag guard below for what
  actually does.
- **Cross-run tag immutability is enforced against the REGISTRY, because the
  registry is the only durable state.** The failure was real and easy to hit:
  `version` picks the next version with no `v<version>` git tag, but that tag
  is created by `bump` — *after* `merge` has already pushed
  `pix:<version>`. A run that published and then died (or was cancelled)
  before `bump` left the git tag free and the Docker tag taken, so the next
  push to `main` selected the same version and
  `docker buildx imagetools create` silently overwrote a published tag with
  different bytes. `scripts/release/tag-availability.sh` asks the registry
  directly, in two places: the `version` job advances the patch past any
  version whose tag already exists (with a loud warning naming the partial
  release), and `merge` re-asks immediately **before** `imagetools create` and
  FAILS rather than mutate — the second check exists because a full multi-arch
  build separates the two, and `concurrency: publish` serializes runs without
  undoing an earlier one's push. The verdict is tri-state and **fails closed**:
  auth failure, network failure, or an unrecognized error is `UNDECIDED`
  (exit 2), never "free". Its whole decision procedure is exposed as
  `--classify <exit-code> <stderr-file>` so `tests/legal-provenance.test.mjs`
  proves the classification offline — including that an `unauthorized` reply
  mentioning "not found" is NOT read as free.
- **It is wired.** `publish.yml`'s `merge` job now exports the multi-arch
  manifest digest as a job output, and a new **blocking** `provenance` job
  (`needs: [version, merge]`, and `bump` now `needs` it) runs
  `verify-provenance.sh <version> <published-digest> <git-sha>` and uploads
  the record. A release cannot proceed without an immutable version→digest
  record of what was actually published.
- `scripts/legal/sbom-config.json` — pins the SBOM tool (`docker/scout-action`,
  SHA-pinned) and states it reuses the SAME license-class policy as the
  notices gate, rather than drifting. It now runs in two places, both
  **blocking for generation**: the `provenance` job scans the **published
  image by digest** (`IMAGE@sha256:…`) and `legal.yml`'s `sbom` job scans the
  repo tree (`fs://.`) on every PR, each asserting a non-empty result.

  The published-image job additionally asserts the SBOM does not describe some
  OTHER image. Scout resolves a multi-arch reference to the platform child it
  analyzes and records that digest in a `pkg:oci` purl, so when a purl is
  present the check is that its digest is the published index or one of its
  children, read from `docker buildx imagetools inspect --raw`.

  The purl is not always present: Scout emits it when it can read the image's
  provenance attestation, and scanning a digest seconds after the push that
  created it produced an SBOM with 652 packages and no purl, while the same
  digest scanned later had one. A missing purl is therefore not a failure.
  **The limit, stated rather than implied:** with no purl the binding between
  the SBOM and the published digest rests on the workflow having passed
  `IMAGE@DIGEST` to Scout, which is by construction and not independently
  verified. A purl naming the WRONG image still fails the release.

  Anchore's action was replaced because its first act is to download the syft
  binary from a third-party release CDN. That download returned 503 twice in
  one afternoon, once here and once in `publish.yml`, where it failed a release
  whose every other gate was green. `continue-on-error` is gone — the
  gate asserts `legal.yml` contains none.
- What is still **not** claimed: SBOM *diffing* (failing a release because
  the component set changed) is unimplemented and stated as such in
  `docs/legal/FINDINGS.md` #7. Generation being blocking is not a
  supply-chain diff gate, and this doc does not pretend otherwise.

## Durable legal basis + privacy (B2/B4)

- `docs/legal/AUTHORIZATIONS.md` — the record of who authorized what, in
  what capacity: DHI base redistribution for the published image (A-1) and
  the employer-IP/copyright posture (A-2). `LICENSE` names Docker, Inc. and
  the pix contributors; `NOTICE.md` states ownership, the inbound = outbound
  MIT contribution license, and points at the record instead of re-deriving
  it per build. Each entry lists what it explicitly does **not** cover
  (trademark license, third-party DHI entitlement, other contributors'
  employment agreements).
- `docs/legal/PRIVACY.md` — what data leaves the machine and to whom: not
  just the model providers and MCP servers you configured, but also the
  DEFAULT destinations pix ships with and never asks about (the zero-config
  `web_search` backend, `api.parallel.ai` and the other keyed search
  fallbacks, the npm version check, and package/toolchain downloads) — no
  pix backend, no telemetry, but not "no traffic unless configured" either.
  Also states what stays loopback-local (memory; the monitor transcript tap
  was removed outright, not merely made local), how credentials are handled
  (`op://` refs, never written to disk), and the honest limit: the real
  exposure is the data you route through it, and lawful basis/retention for
  that data is the operator's obligation, not pix's.

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

## Ownership + affiliation notice

- `NOTICE.md` (root-level): pix is a Docker, Inc. project (MIT), with an
  inbound = outbound contribution sentence, a third-party non-affiliation
  disclaimer (HashiCorp, Anthropic, OpenAI, Google, Mozilla — descriptive
  trademark use only), and a pointer to the DHI authorization of record. The
  earlier "pix is an independent project, not affiliated with Docker, Inc."
  wording contradicted the recorded ownership and is gone. Baked into the
  image and the Homebrew tarball alongside `THIRD_PARTY_NOTICES.md`,
  `LICENSE`, and `licenses/`. `README.md` was deliberately left untouched (a
  shared, frequently-edited file) — the disclaimer lives entirely in the new
  file so this change stays disjoint.

## What's still open

See `docs/legal/FINDINGS.md` — unresolved items that need a human (counsel or
the DHI account holder), recorded separately rather than asserted as done.
