# Native environments delivery handoff

Continue the native-environments delivery from this durable handoff.

**Repository:** `/Users/mcavage/dev/pix`
**Branch:** `design/simplify-pack-rigs`
**Wave C closure HEAD:** `83385b0a3c82a78da0c995122086257696891a62`

## Current state

Story 0 / Wave A, Story 1 foundations / Wave B, and Story 1 verbs / Wave C
are all complete. Units E0.1-E0.7, E1.0-E1.6, and E1.7-E1.15 are done. Wave D
(launch cutover, E2.1-E2.8) has not started.

The working tree carries seven unstaged, user-owned files. This is the
user's own cleanup, entirely outside this delivery's scope, and must remain
unstaged and uncommitted by any future work here:

- deleted: `.claude/settings.local.json`
- deleted: `CODE_OF_CONDUCT.md`
- deleted: `CONTRIBUTING.md`
- deleted: `NOTICE.md`
- deleted: `SECURITY.md`
- deleted: `THIRD_PARTY_NOTICES.md`
- modified: `LICENSE`

Every Wave C gate run treated this cleanup as fixed, external state: captured
byte-exact (`git diff` sha256
`c6df92b5ac482047c92b7224c6e8427cb21ae829825c33f49d4057382dc0536d`), stashed
by name for a clean-HEAD gate run, then popped and reverified byte-identical
(same sha256, same `git status --porcelain`) before and after. Nothing of it
was ever staged or committed. Do not stage it, do not "fix" the legal-doc
test failures it causes when present in the tree as-is (that failure is
attributed and expected, see Wave C evidence below) — it is not this
delivery's file to touch.

## Wave C deliverables

- **E1.7:** `workflow/env` registry, exact-name resolution from any cwd,
  location-refusal spine (containment, symlinked root/executable, repoint
  invalidates acceptance).
- **E1.8:** host bill-of-materials computation and `pix env review` trust
  gate; every host-execution facet reviewed independently, default-deny.
- **E1.9:** `cmd/pix env` dispatch skeleton, `env ls`, `env show`, wired to
  the E1.8 review gate; one typed `EffectiveMounts` set shared end-to-end
  across Load/Review/Show.
- **E1.10:** `pix env add` — register or scaffold, always reviewed,
  transactional.
- **E1.11:** `pix env use`/`forget`, plus the hidden `env rm` pointer (not a
  real verb — points at `forget`).
- **E1.12:** `pix env edit NAME pix|sbxenv` — `$VISUAL`/`$EDITOR` argv,
  strict reload, PRD §5.4 verdict taxonomy (valid/no-op, valid-but-changed,
  invalid).
- **E1.13:** env error-form enumeration, copy lint, non-TTY sweep — closes
  the wave by proving family consistency across every error and refusal.
- **E1.14:** `pix reset` environment invariants (never deletes a source,
  always invalidates trust acceptance), test-only.
- **E1.15:** quiet environment surfaces (D13): no row at all when nothing is
  registered, not even "none"; `doctor`/`status` stay silent at zero.

### Fixes landed during and after the wave (all closed)

- **H1 / review-consent TOCTOU** and **L1 / config lost-update**: a race
  window between trust-bill computation and recorded acceptance, and a
  registry commit that did not sync the entire fresh config. Fixed in
  `d232272e` (TOCTOU) and `4ac9f494` (full-config sync, A3).
- **A1 / physical containment**: containment refusal was lexical and let a
  symlinked environment root or referenced executable escape; fixed with
  `EvalSymlinks`-based physical resolution in `7cd2f775`.
- **Shell injection**: dynamic tokens (names, paths, argv) were interpolated
  into runnable `pix env` commands without shell-quoting; fixed in
  `e1b15a97` (every dynamic token quoted).
- **Terminal control injection (M1)**: the trust bill renders
  attacker-controlled `.sbxenv.yaml`/`pix.toml` content straight into the
  terminal a human reads to answer `[y/N]`; raw ESC/CSI/OSC, CR/LF, DEL, C1,
  or Unicode bidi controls could repaint the consent screen or forge a
  verdict line. Fixed in `8ab00d7e` with `sys.TerminalSafe`, a display-only
  sanitizer at the one render boundary.
- **Tier0 review-state / run-hint / actionable-copy**: explicit
  `not_required`/`accepted`/`unaccepted`/`changed` taxonomy in
  `show`/`ls`/JSON (`e731714f`); exact at-most-once, fail-open-toward-silence
  run hint; and a final small DX/copy pass (`83385b0a`) making `env use`
  help plain-language, JSON counts never `omitempty`, the non-TTY refusal
  never glue to a shared-stream prompt, and every error family use the same
  three-part (failure/fact/runnable-command) form.
- **WB-SCOPE-01** (Wave B foundation branch not independently releasable
  because config refusals named planned verbs): resolved once E1.9 landed
  the real `cmd/pix env` dispatch/read verbs.

## Verification

Fresh top-level verification at closure HEAD `83385b0a`:

- Independent QA/UAT pass (the `qa-lead` preset failed to start twice, local
  Ollama unavailable, infrastructure-only, non-product; a QA executor
  substituted directly with no coverage loss): **36/36 matrix rows PASS** at
  committed HEAD `8ab00d7e`, evidence committed as `b02b8586`.
- A final small DX/copy nits patch (`83385b0a`) landed on top, re-verified
  with its own focused/red-first tests plus a full `-race` re-run of
  `workflow/env`/`workflow/reset` (no regressions), then a fresh full
  `make gate`: **all 11 segments PASS, 35.622s**, log
  `.pi-agent/deliver/native-environments/logs/wave-c-final-gate.log`
  (mirrored at
  `.pi-agent/deliver/native-environments/uat/wave-c/logs/gate-final-closure-head.log`).
- User-cleanup handling reverified at this same run: patch sha256
  `c6df92b5ac482047c92b7224c6e8427cb21ae829825c33f49d4057382dc0536d`
  identical before and after, `git status --porcelain` empty at the stashed
  clean-HEAD gate, nothing staged or committed.
- Security: **final APPROVE**.
- Product checkpoint: **PM APPROVE**; **DX APPROVE** with the final small
  nits above fixed in `83385b0a`; **UX** findings adjudicated and fixed, or
  explicitly D10/out-of-scope.
- Official cross-vendor reviews: round1 **BLOCK** (3 original findings plus
  later review/security findings surfaced across unit merges, all fixed
  through `8ab00d7e`/`83385b0a`); round2 **LGTM** at `0bc0333b` (a security
  review run separately against that same head found M1, fixed next round);
  round3 **LGTM** at `8ab00d7e`; final-patch **LGTM** at `83385b0a`.
- No claim anywhere in this evidence depends on an unavailable host `sbx`.
- UAT: `.pi-agent/deliver/native-environments/uat/wave-c/wave-c-summary.json`
  and `wave-c-matrix.md` (36-row matrix at `8ab00d7e`, plus a concise
  final-patch closure appendix at `83385b0a`).
- Phase 10 product closeout (`product.closeout_done` in `status.json`)
  remains `false` and pending — that is a whole-project closeout gate, not a
  per-wave one, and is intentionally not run yet.

## Carry-forward obligations (next: Wave D, exact order E2.1 → E2.8)

1. **E2.1 — Effective environment render + Pi mixin kit generation.**
   Depends on E1.13. Render ONE stable effective `.sbxenv.yaml` per sandbox
   from authored + sidecar + Pix-owned runtime facts. This unblocks every
   other Wave D unit below.
2. **E2.2 — Creation fingerprint + drift attribution map.** Depends on
   E2.1/E1.2. **Use the authored expression plus a launcher-keyed
   HMAC/digest of the resolved value for interpolation fingerprinting —
   never persist the value or an unkeyed raw-value hash.** This carries
   forward directly from the Wave B/C interpolation-review obligation
   (never display or persist a resolved `${VAR}` value, only source variable
   + destination key path).
3. **E2.3 — Create intent + failed-create recovery state machine.** Depends
   on E2.1.
4. **E2.4 — `sandbox.PlanEnvRemove` + name-based fallback with cleanup
   report.** Depends on E2.1.
5. **E2.5 — `pix run` cutover to `sbx env create` + name-based `sbx exec`.**
   Depends on E2.1, E2.2, E2.3, E2.4, E1.9, E1.15. THE cutover unit. Build
   the **real `EffectiveMounts`/`HolderProbe`/launch-time re-gate** here:
   Wave C shipped `EffectiveMounts` as one typed set end-to-end but sandbox
   drift and live-holder probing are still `unknown (live-launch drift lands
   with a later wave)` / injectable defaulting to `NoLiveHolders` — this is
   the wave that makes them real against a live `sbx`.
6. **E2.6 — Recreate log wiring + `pix doctor --recreates`.** Depends on
   E2.5, E1.6.
7. **E2.7 — Host-service desired set = default env UNION live holders.**
   Depends on E2.5.
8. **E2.8 — Delete every selectable pack launch path.** Depends on E2.5,
   E2.7. Last unit of Wave D; after this, nothing pack-shaped remains
   user-selectable beside the environment path.

## Non-claims carried forward unchanged

- **A6 remains a non-claim:** envinfo tests Pix's merge model; Story 0 did
  not independently prove upstream multi-file composition semantics.
- **A3 remains a non-claim for host-global scope:** Wave C's E1.4 fix
  (`4ac9f494`) proved full-config sync on every env registry mutation, but
  host-global binding/MCP preservation *across environment removal*
  specifically still needs the named downstream proof in Wave D (E2.4/E2.7).
- **E3.1 owns AC-29** (`inference.json` v1 `roster` field + exact-copy model
  resolution/launch-error validation, including the `[agents].<name>` field
  spelling). Wave B/C shipped reference facts only; do not redo or
  short-circuit this in Wave D.
- **Platform risk:** non-Unix no-follow behavior is an honest Lstat/open
  fallback with residual TOCTOU, documented in code (`WB-SEC-10`). Windows
  compilation is proven; end-to-end Windows runtime behavior is not.
- **Pre-existing debt, not Wave C scope:** the pack renderer's terminal
  output is not covered by the `sys.TerminalSafe` sanitizer landed for the
  env trust bill in `8ab00d7e` — that fix was scoped to the one render
  boundary this wave owns. If a future wave routes untrusted content through
  the pack renderer's terminal output, it needs its own sanitization pass;
  do not assume `8ab00d7e` already covers it.

## External cleanup debt

Pre-fix run `run-20260824-092338-d4c384f5` leaked `pix-uatenv-fixture-image`.
The last positive host artifact listed it as running with workspace:

`/Users/mcavage/.local/state/pix/uat/sessions/10ff3baf/runs/run-20260824-092338-d4c384f5/env-matrix/environment_uses_local_candidate_image`

On the host, first verify the exact name and workspace with `sbx ls --json`.
Only if they still match, remove it through:

```bash
sbx env rm -f /Users/mcavage/.local/state/pix/uat/sessions/10ff3baf/runs/run-20260824-092338-d4c384f5/env-matrix/environment_uses_local_candidate_image/candidate-image.sbxenv.yaml
```

Probe again before calling the debt cleared. This debt is unchanged by Wave
C and remains open.

## Resume instructions

1. Read this file, `status.json`, `prd.md`, `architecture.md`, and
   `units.json`.
2. Confirm HEAD is `83385b0a3c82a78da0c995122086257696891a62` and preserve
   the seven-file user cleanup listed above exactly as-is, unstaged.
3. Do not repeat Wave A/B/C planning or UAT.
4. Start Wave D only when explicitly asked. Begin **E2.1** and follow the
   dependency order above (E2.1 → E2.2/E2.3/E2.4 → E2.5 → E2.6/E2.7 → E2.8);
   consult `units.json` for the authoritative file-conflict and
   parallel-safety data per unit.
5. Do not run full project Phase 10 closeout yet; the PM/DX/copy results
   above are the Wave C checkpoint only.
