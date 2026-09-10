# Product agreement with a continuous implementation owner

## PR/FAQ: promise and decision

Pix helps a user agree on a product before an agent starts coding, then carries
that product through implementation, verification and an independent review.
The user keeps PR/FAQ → PRD → architecture specifications. One owner holds the
whole product context; specialists resolve named uncertainties and workers execute
ready units. A small feature does not require a succession of departments.

Why keep Pix rather than use a native coding agent? The intended value is explicit
product agreement, durable specifications, a product-first independent second
opinion and optional cross-model implementation. These are design choices, not a
claim of established superiority. A successful delivery needs a useful runnable
artifact, not just compliant paperwork.

Does approval happen repeatedly? No. Existing agreement and explicit autonomy
persist. Product exploration alone is not coding authorization. A materially
changed promise needs agreement; routine refinements inside it do not.

Does "thin" remove architecture or QA? No. It removes redundant authors and
repeated reviews. Architecture remains explicit; required checks exercise real
callers and failure/recovery paths. A specialist is appropriate for a concrete
uncertainty, not automatically for every surface mentioned in a checklist.

## PRD: behavior and acceptance

- P1: New products/material features establish PR/FAQ, PRD and architecture before
  implementation. `plan`, `build`, `deliver` and `brainstorm` preserve this boundary,
  supplied specs and existing authorization. Maintenance uses a bounded contract.
- P2: The owner revisits the original user journey after the first working version.
  It addresses in-scope friction/recovery while respecting explicit non-goals and
  authority boundaries. The initial checklist cannot replace product judgment.
- P3: Optional workers receive ready interfaces, owned callers/files and concrete
  acceptance checks. Independent ready units use isolated worktrees; the owner
  integrates and validates the combined result. No required worker quota.
- P4: Review is independent and product-first. Required visual coverage uses real
  images. Missing evidence and material defects block. Explicit approval with
  nonblocking concerns is valid; pending review status alone is not a defect.
- P5: Material repairs receive focused delta review. Cosmetic/status changes need
  relevant checks and recorded disposition, not an automatic paid review loop.
  Disputed blocking findings still require independent disposition.
- P6: Local execution evidence is collected deterministically: command, input and
  candidate identities before/after, timestamps, exits and raw logs. A command
  failure, candidate mutation or missing input cannot pass; exit zero never sets
  product acceptance. Preserve previous attempts instead of overwriting them.
- P7: A truncated child response remains incomplete in single/parallel/chain
  callers. Requested model names and prose do not establish reviewer identity.

Validation combines existing skill/package guards, actual command-runner negative
controls, subagent response-metadata tests, environment binding checks and the
repository gate. Text guards verify packaging, not live model adherence or product
quality. No new product comparison is claimed by these checks.

## Architecture specification

Public skills own the product loop. `build` delegates its process to `deliver`;
`plan` produces linked product artifacts; `ship` reuses valid checks/review. The
review preset reads source and product evidence, with selection controlled by
existing bindings. The work environment selects Astra ownership, Opus review and
optional Flash engineering; users can explicitly select another worker such as
Terra. No hidden router, model table, pricing service or runtime replacement.

`capture-evidence.mjs` is a local Node CLI shipped with the delivery skill. It
reuses the repair packet's source fingerprint implementation. A small spec names
command argv, contract, criteria, relevant input files and redacted environment
identity. It executes the real command in the repository, records separate logs
and a durable result under `.pi-agent/`, and refuses existing output directories.
It neither invokes models nor scans secrets nor grants approval. Existing local
secret scanning remains required before sharing evidence. External service state
and model/cost identity still come from their actual runtime callers.

The source identity covers HEAD, staging, tracked/untracked working content,
deletions, file modes and symlink targets. Explicit ignored shipping inventory
covers generated shipping inputs; `.pi-agent/` is process evidence. Output/input
path checks prevent following symlinks into unrelated personal paths. Arbitrary
commands remain subject to the invoking agent's existing permissions; this CLI
is not a command sandbox.

## What we adopted from Codex

Inspected source revision: `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`.
These are source-backed mechanisms, not claims about every current Codex session.
No upstream code or model prompt was copied into Pix.

- [Continuous turn execution](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/session/turn.rs): keep ownership through tool execution and continuation. Pix already has a Pi loop; adopt continuity in skills rather than replacing its runtime or mandating handoffs.
- [Separate review conversation](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/tasks/review.rs): explicit rubric, configurable review model, no initial history and restricted capabilities. Pix retains its fresh read-only reviewer and adds cross-vendor identity and product evidence requirements.
- [Bounded skill catalog](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/ext/skills/src/render.rs): context is finite. Pix keeps concise entrypoints and loads detailed references only when relevant; this change does not port Codex's catalog renderer.

Specifications can improve alignment; more specification authors do not guarantee
better design. Independent review can improve assurance; extra review calls do
not guarantee new findings. Evaluate the resulting product to establish either
benefit. Model training and native runtime differences are not isolated here.
