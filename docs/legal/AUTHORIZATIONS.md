# Authorizations of record

Durable record of the human authorizations pix's release posture depends on.
This file exists so that no build, script, or agent has to re-derive (or
worse, infer) an approval at release time: the approval is recorded here
once, with who gave it and in what capacity, and everything else points at
this file.

Scope discipline: each entry records **exactly** what was approved and
nothing more. An approval recorded here is not evidence for any adjacent
claim (see "Explicitly not covered" under each entry). Where an item still
needs counsel or an account holder, it stays open in
`docs/legal/FINDINGS.md`.

---

## A-1 — DHI base redistribution for the published pix image

- **Approved:** pix may build `FROM` a Docker Hardened Image (DHI) base and
  publish the resulting multi-arch image to the project's Docker Hub
  namespace (`docker.io/<namespace>/pix`, default `mcavage/pix`), as the
  `publish` workflow does today.
- **Given by:** Mark Cavage, President, Docker, Inc. — the repository owner,
  acting in that capacity and stating the approval explicitly for the record.
- **Form:** explicit statement by the authorizing officer, recorded here as
  the durable basis. This supersedes the previous posture, where each
  operator's DHI entitlement was treated as an unrecorded, per-operator
  matter.
- **Explicitly not covered:**
  - No trademark or brand license beyond descriptive use (`NOTICE.md`).
  - No grant of DHI entitlement to third parties. A contributor building the
    image themselves still needs their own DHI-entitled Docker account, or
    the public fallback base documented in the `Dockerfile`.
  - No statement about any other DHI-derived artifact, or about DHI terms as
    they apply to anyone other than this project.

## A-2 — Employer IP / copyright holder

- **Approved:** the pix source is employer IP. Copyright is held by Docker,
  Inc. (together with third-party contributors, for their own
  contributions), and `LICENSE` names Docker, Inc. accordingly. The outbound
  license stays MIT.
- **Given by:** Mark Cavage, President, Docker, Inc., acting in that capacity.
- **Consequence recorded elsewhere:** `LICENSE` copyright line;
  `NOTICE.md`'s ownership paragraph; the inbound-license sentence in
  `CONTRIBUTING.md` and `NOTICE.md` (inbound = outbound = MIT).
- **Explicitly not covered:**
  - No assignment of, or claim over, any third-party contributor's
    copyright. Third-party contributions are licensed in under MIT, not
    assigned.
  - No opinion on the employment agreements of any other contributor.

---

## Recording conventions

- **Date/provenance:** an entry's date is the commit that introduced or last
  amended it (`git log --follow docs/legal/AUTHORIZATIONS.md`). No date is
  hand-written here, so none can drift from the record.
- **Correcting an entry:** amend this file in a normal PR, stating what
  changed and who authorized the change. Never silently rewrite an entry —
  the point of the file is that a downstream claim (`LICENSE`, `NOTICE.md`,
  the publish workflow) can be traced back to a specific human decision.
- **Scope:** if a needed permission is not written here, it is not
  authorized. Absence is not implied approval.

## What this file is not

It is not legal advice, and it is not counsel sign-off. It is a record of
who authorized what, so the claim in `NOTICE.md`, `LICENSE`, and the release
workflow has a named, durable basis instead of an inference. Items that need
counsel remain listed in `docs/legal/FINDINGS.md`.
