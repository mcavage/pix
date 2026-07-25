---
name: enrich
description: Write knowledge BACK into the OKF bundle as a gated, PR-based flow. Use for "enrich the knowledge base", "document this in OKF", or promoting a learning that is shared domain truth, not personal.
---
# enrich

`ingest` reads documents into personal memory. `promote` graduates recurring
personal learnings into skills. **This one is the write side of shared
knowledge**: it adds or updates a concept in the OKF bundle: durable, cited,
domain truth that the whole team (and every future agent) reads. It never writes
to the bundle's main branch directly. Every change lands as a **branch + PR**
for human review.

**The watcher and auto-capture NEVER write OKF.** Automatic memory capture only
touches personal memory. OKF is written by exactly one thing: this skill, run
explicitly, by a human's request. That is the whole point of the gate.

## Personal vs shared: which store?

Before writing anything, decide where the fact belongs. The test:

- **Personal / situational → memory** (`ingest`, `/remember`). One person's
  preferences, this week's todo, "the user prefers X", account-specific context, a
  correction about how *you* work. It changes often and is scoped to a user.
- **Shared / domain truth → OKF** (this skill). A concept the team would want
  documented independent of any one person: how a system works, a definition, an
  architecture decision with rationale, a stable process, a "what is X and why".
  It is worth a permalink and a citation.

If it fails the shared test, stop and use `ingest`/`/remember` instead. Do not
inflate a personal note into an OKF concept.

## OKF format recap

An OKF bundle is a **git repo of markdown files**. Each concept file:

- Has YAML frontmatter. **`type` is REQUIRED**; `title`, `description`,
  `resource`, `tags`, `timestamp` are optional but you should fill the ones that
  apply.
- The **concept ID is its path minus `.md`** (e.g. `systems/auth-flow.md` →
  `systems/auth-flow`).
- Cross-links are **bundle-relative absolute paths** written as markdown links to
  `/path.md` (leading slash = bundle root, not the filesystem root).
- Ends with a `# Citations` section listing sources.
- Two reserved files per directory: **`index.md`** (a listing of the concepts in
  that directory) and **`log.md`** (an append-only history of Creation/Update
  lines).

A concept looks like:

```markdown
---
type: concept
title: Auth token refresh flow
description: How access tokens are refreshed without a full re-login.
tags: [auth, tokens, security]
timestamp: 2026-07-12T00:00:00Z
---
# Auth token refresh flow

## Summary
One or two sentences a reader gets the gist from.

## Detail
The real explanation, with structural headings. Link related concepts with
bundle-relative paths, e.g. see [the session model](/systems/sessions.md).

## Rationale
Why it works this way, if there was a decision to record.

# Citations
- [Design doc: token rotation](https://…), retrieved 2026-07-12
- Slack thread #eng-platform, 2026-06 (source of the 15-min TTL)
```

Pick a `type` that fits (`concept`, `definition`, `decision`, `process`,
`reference`, …); be consistent with what the bundle already uses. Read a few
neighbors first.

## Steps

1. **Resolve the bundle.** Get the git-mounted bundle path via the `knowledge`
   capability (see `capability-routing`) or its configured location. If
   `knowledge` resolves to `none`, say so plainly and stop, since there is nowhere
   to write. Confirm it is a git repo and note its default branch and `origin`.

2. **Read before you write.** `ls` the target directory and read its `index.md`
   plus 2-3 neighboring concepts. Match their `type` vocabulary, heading style,
   tag conventions, and link style. If the concept already exists, you are doing
   an **Update**, not a **Creation**; read the current file in full first.

3. **Draft the concept as valid OKF.** Frontmatter with a required `type`, plus a
   concrete `title`, one-line `description`, useful `tags`, and a `timestamp` (ISO
   8601, UTC). Body with structural headings. **Every non-obvious claim is
   cited** in a `# Citations` section. If you cannot cite it, do not write it.
   Never invent facts, dates, owners, or numbers to fill a gap; leave a `TODO` or
   ask. Add bundle-relative `/path.md` links to related concepts.

4. **Update the directory's `index.md`**: add or amend the one-line entry for
   this concept (path + description), keeping the file's existing ordering.

5. **Append a `log.md` line** in the concept's directory: a dated `Creation` or
   `Update` line naming the concept ID and a short reason. Append only; never
   rewrite history.

6. **Gate it behind a PR.** In the bundle repo:

   ```bash
   cd <bundle-path>
   git checkout -b enrich/<concept-id-slug>
   git add -A
   git commit -m "enrich: <Creation|Update> <concept-id>"
   git push -u origin enrich/<concept-id-slug>
   gh pr create --fill --title "enrich: <concept-id>" \
     --body "<what/why + the citations + personal-vs-shared note>"
   ```

   **Never push to the bundle's main branch.** Never `git push origin main`,
   never merge your own PR. Hand the PR URL back and let a human review and merge.
   If `gh` is unavailable, push the branch and report the compare URL so the user
   can open the PR themselves.

## The /learnings → OKF bridge

When running `promote` over recurring learnings, apply the personal-vs-shared
test to each candidate. If a recurring learning is **shared domain knowledge**
(not a personal preference or a skill-behavior fix), it does not belong in a
skill edit or a memory entry; draft it as an **OKF concept via this skill**
instead. So `promote` routes personal/behavioral learnings to skill/convention
edits, and shared/domain learnings here to a gated OKF PR. Note in the PR body
that it originated from a promoted learning.

## Voice

Write the concept in the repo owner's documented voice. Retrieve writing
preferences from memory (tag: `writing-voice`) if available; otherwise default to:
direct and concrete, no em-dashes, no AI slop, no hedging filler ("it's worth
noting", "in today's fast-paced…"). State
what is true, cite it, link it, stop. A concept is a reference, not an essay;
keep it tight and skimmable. Re-read the `anti-slop` and `writing-voice`
skills before drafting prose.
