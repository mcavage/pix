# Legal/release findings — unresolved human confirmations (W0)

This is the register of items this change deliberately did **not** resolve,
because resolving them requires a human — counsel, or the person holding the
relevant account/entitlement — not an agent inferring or asserting on their
behalf. Nothing in `THIRD_PARTY_NOTICES.md`, `NOTICE.md`, or the CI gates
should be read as claiming any of these are closed.

| # | Item | Why it's not resolved here | Who/what can resolve it |
| --- | --- | --- | --- |
| 1 | **MPL-2.0 compliance sign-off** for `github.com/hashicorp/go-plugin` and `github.com/hashicorp/yamux`. | NARROWED, not closed. The mechanics §3 asks for now actually ship: the verbatim license text (`licenses/MPL-2.0.txt`) travels with the image and the Homebrew tarball, and each component's Source Code Form URL is pinned to the exact linked version (s3.2(a)), with the unmodified-import posture stated (no Modifications to make available). What remains is a judgment call an agent cannot certify: whether this packaging satisfies §3 for *this* product in *its* markets. | Counsel review (see `agents/legal.md` for a structured first pass; final sign-off is human). |
| 2 | ~~**DHI redistribution/entitlement rights**~~ **RESOLVED** by a recorded human authorization, not by inference: see `docs/legal/AUTHORIZATIONS.md` A-1 (DHI base redistribution for the published image) and A-2 (employer IP / copyright holder). | The blocker was that no agent could assert this on a human's behalf. It is now an explicit, attributed, durable record from the authorizing officer, and `NOTICE.md`/`LICENSE` follow it. | Closed for the published image. Still open, and deliberately NOT claimed: a third party's own DHI entitlement (they need their own account or the public fallback base), and any trademark/brand license beyond descriptive use. |
| 3 | **Immutable base-image digest** is not actually resolved/pinned by this change — `scripts/release/resolve-base-digest.sh` exists and its parsing logic is tested, but it was never run against a live, credentialed `dhi.io` session (no such session exists in this environment), so no real digest was recorded. | Requires a live DHI-entitled Docker login, which this environment does not have and should not fabricate. | Whoever runs `make load`/the publish workflow with real DHI credentials. |
| 4 | **`gopkg.in/yaml.v3` dual-license nuance** (Apache-2.0 + MIT portions) is recorded as `"Apache-2.0 AND MIT"` / class `permissive` based on the module's `LICENSE` file, but the notices file does not reproduce which specific files fall under which of the two — a reasonable simplification for a permissive/permissive dual license, but worth a second look if this project's distribution model ever needs per-file attribution. | Low-risk simplification, not independently confirmed against the upstream NOTICE breakdown. | Whoever next touches the ledger; low priority. |
| 5 | ~~**`typescript`'s license** was not independently re-read from a local `node_modules/typescript/package.json`~~ **RESOLVED**: `npm ci` has since run in this environment, and `node -e "console.log(require('typescript/package.json').license)"` against the installed `node_modules/typescript/package.json` reads `Apache-2.0`, matching the ledger's `scripts/legal/dependencies.json` entry. | It was a re-verification gap only — the recorded license was already correct, just not freshly re-read from an installed copy. | Closed. Re-check again only if the pinned `typescript` version in `package.json` changes. |
| 6 | **`anchore/sbom-action`'s pinned commit SHA** (`e22c389904149dbc22b58101806040fa8d37a610`, tag `v0.24.0`) was resolved via a web lookup against GitHub, not a live `git ls-remote`/registry pull from inside this environment. | No outbound network policy check was run for this specific host in this environment. | Re-verify before first real use: `git ls-remote https://github.com/anchore/sbom-action v0`. |
| 7 | **SBOM-diff gating policy.** NARROWED. SBOM *generation* is now blocking in both places it runs — `legal.yml` (repo tree, every PR) and `publish.yml`'s `provenance` job (the PUBLISHED image, by manifest digest) — and both assert a non-empty result, so an SBOM that silently produced nothing now fails. What is still NOT implemented, and is not claimed anywhere, is SBOM *diffing*: failing a release because the component set changed (e.g. a copyleft dep arriving through a transitive npm/native dep the Go-module gate cannot see). | Needs a human decision on false-positive tolerance and enforcement scope, not just a script. | Whoever owns the release process next; the mechanism is in place, the diff policy is not. |
| 8 | ~~**`github.com/thejerf/suture`'s actual license, at the version eventually vendored**, was verified via a web fetch, not a local checksummed `go.sum` entry, because it wasn't a dependency yet~~ **RESOLVED**: U07 landed the host `serve` supervision tree on `github.com/thejerf/suture/v4 v4.0.6`, present in `go.mod`/`go.sum` with a checksummed entry, and `scripts/legal/dependencies.json` carries the live (not planned) ledger row with the MIT license verified against that pinned version's module cache. | It was a timing gap only — the module is now a real, checksummed dependency and the ledger reflects that. | Closed. `scripts/check-third-party-notices.sh` continues to fail closed on any future undeclared module. |

### Provenance record durability (scope note, not a claim)

`scripts/release/verify-provenance.sh` enforces that a version's digest can
never be rewritten *given a prior record it can read*. In `publish.yml` that
record is written into a fresh runner workspace and uploaded as a 90-day
artifact; the next run starts with an empty `out/provenance/`. **So the script
enforces nothing across runs, and this document previously implied more than
that.** The earlier wording rested the cross-run guarantee on "the `version`
job only ever selecting an unused `v<version>` tag", which was false in the
exact case that matters: the `v<version>` git tag is created by `bump`, AFTER
`merge` has already pushed `pix:<version>`, so a run that published and then
failed left the git tag free and the Docker tag taken — and the next run
re-selected that version and overwrote a published image.

What enforces cross-run tag immutability now is the registry itself:
`scripts/release/tag-availability.sh` is consulted in the `version` job (skip
any version whose tag exists) and again in `merge` immediately before
`imagetools create` (fail, mutating nothing). It is tri-state and fails closed
— an undecidable answer is never "free". Honest scope, restated:

* `verify-provenance.sh`: immutability **within a run**, and against any
  record restored into its out-dir. Nothing more.
* the registry pre-check: **cross-run** protection against overwriting a
  published `:<version>` tag.
* `:latest` is a deliberately moving tag and is not covered by either.
* Neither is a signature. Nothing here proves *who* published a digest — no
  cosign/attestation is wired — and no document should read as if it does.

### Still open, unchanged by the release-blocker fixes

Items **1** (MPL-2.0 compliance sign-off), **3** (a real, credentialed
base-image digest), **4** (`gopkg.in/yaml.v3`'s per-file dual-license
breakdown), **6** (`anchore/sbom-action`'s pinned SHA re-verified from a live
`git ls-remote`) and **7** (SBOM-*diff* enforcement policy) are all still
open, and deliberately so: each needs counsel, a credentialed human session,
or an owner's decision on enforcement tolerance. The release-blocker work
(pre-publish gating, cross-run tag immutability, notice-bearing release
assets, a ledger regenerated from the live module set, a pinned global
`typescript`, and the monitor/network disclosures in `PRIVACY.md`) closed
mechanism gaps only. It closed none of these, and nothing in it should be read
as doing so.

None of the above blocks the mechanisms landing (the gates, scripts, and
generated notices are all real, tested, and passing); they block *specific
factual/legal claims* this change is careful not to make on a human's
behalf.
