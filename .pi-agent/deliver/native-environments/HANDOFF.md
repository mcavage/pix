# Native environments delivery handoff

Continue the native-environments delivery from this durable handoff.

**Repository:** `/Users/mcavage/dev/pix`
**Branch:** `design/simplify-pack-rigs`
**Wave C closure HEAD:** `83385b0a3c82a78da0c995122086257696891a62`

## Current state

Story 0 / Wave A, Story 1 foundations / Wave B, and Story 1 verbs / Wave C
are all complete. Units E0.1-E0.7, E1.0-E1.6, and E1.7-E1.15 are done. At the
user's explicit direction this delivery is now OPEN full-send through Waves
D-H (the full migration, no partial stop). **Wave D (launch cutover) is
in_progress: E2.1 is running, E2.2-E2.8 are pending.** Wave E (literal
roster) is running concurrently per the architecture's cross-wave
parallelism: E3.1 is running, E3.2-E3.4 are pending. Waves F, G and H remain
pending and unopened until their gating waves close (E5.1 gates G; G gates
H's SECURITY.md follow-up).

Phase 0.5 product framing (`product/pm-frame.md`, `dx.md`, `copy.md` in
`status.json`) is a whole-PRD artifact, not a per-wave one; it remains valid
unchanged for Waves D-H and is not re-run. Frozen product decisions (PRD, the
six taste calls in `status.json`) are unchanged by this update.

### Architect corrections incorporated at open (findings C1-C12, `status.json`)

1. **C1/C2 — E2.2 now `depends_on` E2.1 directly** (it consumes E2.1's
   effective render), plus a new requirement: the launcher-keyed HMAC key used
   for interpolation fingerprinting is exactly **one** stored `hosttrust`
   record, and `pix reset` invalidates/rotates that one record alongside
   every acceptance record (extends F6's one-store invariant to the key).
2. **C3/C4/C5/C6 — E2.1's renderer is a pure function over a caller-supplied
   `RuntimeFacts` value.** `envinfo` must import neither `mcp` nor `sandbox`
   (new fitness function **F17**), it is the single producer of the Pi mixin
   kit (no second materialization site in `cmd/pix/env_cmd.go` or
   `workflow/launch`), and its file list now includes
   `services/host/cmd/pix/env_cmd.go`.
3. **C7 — E2.5 wires the `env forget`/`env show` seams** to the new
   live-launch facts: `forget` tears down a bound lease/instance before
   invalidating trust; `show` renders real live-holder/drift/attach state
   instead of the Wave C placeholder.
4. **C8 — E2.7's desired-set union lives solely in
   `workflow/launch/hostservices_env.go` + `serve.go`**, never in
   `services/host/pack_units.go` (E5.2 deletes that file outright; a durable
   Wave D capability cannot live inside doomed pack machinery).
5. **C11 — E2.8's private-work-environment gate is now enforced, not
   descriptive**: it refuses to land without a recorded live-conversion
   evidence artifact (commit or log path in this delivery's evidence tree).
6. **C9 — E5.1's named pack-trust test file list is a snapshot, not an
   authoritative input.** At execution time, re-derive the enumeration from
   the live tree (Waves D-F may move or add trust-adjacent tests before
   Wave G starts) rather than trusting the list frozen in `units.json`.
7. **C10 — E6.1 excludes `SECURITY.md`.** It is one of the user's seven
   pending unstaged legal-file cleanup items and stays out of scope until the
   user resolves that disposition; the rest of E6.1's doc cut (README,
   reference, getting-started, AGENTS.md, onboarding/healthcheck skills)
   proceeds unaffected.
8. **C12 — `status.json`'s acceptance bar and Wave D block were stale
   pre-open planning state**; both now explicitly record the full-migration
   commitment and the in-progress/running/pending unit statuses.

Full detail for every corrected unit is in `units.json` (look for
"ARCHITECT CORRECTION" markers) and `architecture.md` section 3 (F17) and the
new Wave D architect-corrections paragraph. C9, C10 and C11 remain `open` in
the findings ledger (the plan is corrected; the underlying condition — live
re-derivation, the user's SECURITY.md disposition, and the recorded private-host
conversion evidence — is only resolved when the gated unit actually executes).
C1-C8 and C12 are `plan-fixed` now, in this same edit.

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
   RUNNING NOW. Depends on E1.13. Render ONE stable effective `.sbxenv.yaml`
   per sandbox from authored + sidecar + Pix-owned runtime facts. This
   unblocks every other Wave D unit below. **Corrected (C3-C6):** the
   renderer is a pure function over one caller-supplied `RuntimeFacts` value;
   it imports neither `mcp` nor `sandbox` (F17); it is the single producer of
   the Pi mixin kit (no duplicate materialization in `cmd/pix/env_cmd.go` or
   `workflow/launch`); its file list includes `services/host/cmd/pix/env_cmd.go`.
2. **E2.2 — Creation fingerprint + drift attribution map.** Depends on
   E2.1/E1.2/E1.4 (**corrected, C1**: now depends_on `E2.1` directly, not
   only transitively). **Use the authored expression plus a launcher-keyed
   HMAC/digest of the resolved value for interpolation fingerprinting —
   never persist the value or an unkeyed raw-value hash.** This carries
   forward directly from the Wave B/C interpolation-review obligation
   (never display or persist a resolved `${VAR}` value, only source variable
   + destination key path). **Corrected (C2):** the HMAC key itself is
   exactly one stored `hosttrust` record, and `pix reset` invalidates/rotates
   that one record alongside every acceptance record (F6 extension).
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
   the wave that makes them real against a live `sbx`. **Corrected (C7):**
   also wires the `env forget`/`env show` seams to these facts — `forget`
   tears down a bound lease/instance before invalidating trust, and `show`
   replaces the Wave C placeholder text with real drift/attach state.
6. **E2.6 — Recreate log wiring + `pix doctor --recreates`.** Depends on
   E2.5, E1.6.
7. **E2.7 — Host-service desired set = default env UNION live holders.**
   Depends on E2.5. **Corrected (C8):** lives solely in
   `workflow/launch/hostservices_env.go` + `serve.go`, never in
   `services/host/pack_units.go` (E5.2 deletes that file outright).
8. **E2.8 — Delete every selectable pack launch path.** Depends on E2.5,
   E2.7. Last unit of Wave D; after this, nothing pack-shaped remains
   user-selectable beside the environment path. **Corrected (C11), tighter
   gate:** refuses to land without a recorded evidence artifact proving the
   private work environment was converted to a native environment and run
   live end-to-end through E2.5's cutover path (PRD §6, A8/R6) — a verbal
   confirmation does not satisfy this gate.

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
4. **Waves D-H are OPEN** (user's explicit full-send). Continue **E2.1**
   (running) and follow the dependency order above (E2.1 →
   E2.2/E2.3/E2.4 → E2.5 → E2.6/E2.7 → E2.8), with **E3.1** (Wave E,
   running) proceeding concurrently per architecture section 6; consult
   `units.json` for the authoritative file-conflict and parallel-safety data
   per unit, including every "ARCHITECT CORRECTION" marker.
5. When Wave G opens, re-derive E5.1's pack-trust test enumeration from the
   live tree before trusting the list in `units.json` (C9, still `open`).
6. When Wave H opens, do not stage, edit, or restore `SECURITY.md` in E6.1
   until the user resolves its disposition (C10, still `open`); land the
   rest of the doc cut normally.
7. E2.8 does not land without a recorded private-work-environment
   live-conversion evidence artifact (C11, still `open`).
8. Do not run full project Phase 10 closeout yet; the PM/DX/copy results
   above are the Wave C checkpoint only. Phase 0.5 covers the whole PRD and
   needs no re-run for Waves D-H.
