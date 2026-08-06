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
| 5 | **`typescript`'s license** (recorded as Apache-2.0 in `scripts/legal/dependencies.json`) was NOT independently re-read from a local `node_modules/typescript/package.json` in this environment (it isn't installed here) — it's the well-known published license, not a freshly re-verified fact. | `npm ci` was not run as part of this change (scope: legal safeguards, not touching the dependency-install step). | Re-verify with `node -e "console.log(require('typescript/package.json').license)"` after `npm ci`; trivial, just not done here. |
| 6 | **`anchore/sbom-action`'s pinned commit SHA** (`e22c389904149dbc22b58101806040fa8d37a610`, tag `v0.24.0`) was resolved via a web lookup against GitHub, not a live `git ls-remote`/registry pull from inside this environment. | No outbound network policy check was run for this specific host in this environment. | Re-verify before first real use: `git ls-remote https://github.com/anchore/sbom-action v0`. |
| 7 | **SBOM-diff gating policy.** NARROWED. SBOM *generation* is now blocking in both places it runs — `legal.yml` (repo tree, every PR) and `publish.yml`'s `provenance` job (the PUBLISHED image, by manifest digest) — and both assert a non-empty result, so an SBOM that silently produced nothing now fails. What is still NOT implemented, and is not claimed anywhere, is SBOM *diffing*: failing a release because the component set changed (e.g. a copyleft dep arriving through a transitive npm/native dep the Go-module gate cannot see). | Needs a human decision on false-positive tolerance and enforcement scope, not just a script. | Whoever owns the release process next; the mechanism is in place, the diff policy is not. |
| 8 | **`github.com/thejerf/suture`'s actual license, at the version eventually vendored**, was verified via a web fetch of its `LICENSE` file (MIT) at time of writing, not from a local, checksummed `go.sum` entry (it isn't a dependency yet). | It isn't in `go.mod`/`go.sum` — there is nothing to checksum. | Whoever adds the real dependency: re-run `scripts/check-third-party-notices.sh` (it will fail closed on the newly-live, undeclared module until the ledger is updated with the verified version). |

### Provenance record durability (scope note, not a claim)

`scripts/release/verify-provenance.sh` enforces that a version's digest can
never be rewritten *given a prior record*. In `publish.yml` the record is
written in a fresh workspace and uploaded as a 90-day artifact, so the
cross-run guarantee rests on the `version` job only ever selecting an unused
`v<version>` tag (one publish per version), not on the script re-reading an
older run's file. That is the honest scope: immutability is enforced within a
run and against any restored record; uniqueness across runs comes from the
version selector.

None of the above blocks the mechanisms landing (the gates, scripts, and
generated notices are all real, tested, and passing); they block *specific
factual/legal claims* this change is careful not to make on a human's
behalf.
