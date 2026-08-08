---
name: docs-sync
description: Post-ship documentation sync, compare the release diff to the doc set, draft required updates, present as one batch for approval before writing. Use for "update the docs" or after `ship`.
---
# docs-sync

Goal: code shipped, docs describe reality. Never write anything until the user
approves the full batch. Never invent features: only document what the diff proves.

## Hard rules

- Only document what the diff proves.
- `CHANGELOG.md` entries must be user-facing. No helper refactors, variable renames, or internal reshuffles.
- If a doc file does not exist, suggest it. Do not create it here.
- Present all proposed changes as one batch for approval before editing anything.

## Step 1: Read the diff

Determine the comparison range (last tag, release branch, or explicit range the
user provides). Use:

```
git diff <last-tag>...HEAD
git log --oneline <last-tag>..HEAD
```

Summarize in user-facing terms: new behavior, changed behavior, removed behavior,
contributor or operational workflow changes.

## Step 2: Read the doc set

Read every file that exists: `README.md`, `CHANGELOG.md`, `ARCHITECTURE.md`,
`CONTRIBUTING.md`, `AGENTS.md`, and everything under `docs/`. Do not skip a file
because the change looks small. Small changes often invalidate setup steps,
examples, flags, defaults, and architecture notes.

## Step 3: Check each doc against the diff

For every doc file, answer:
1. Does the diff invalidate any statement here?
2. Does a command, example, config key, API field, flag, or default need updating?
3. Is this file still correct and complete enough to leave untouched?

Do not wave this away with "docs look fine." Work through every file.

What to check per file:
- **README.md**: install steps, quickstart examples, feature lists, command flags, env requirements.
- **ARCHITECTURE.md**: component diagrams, request flows, service boundaries, data model descriptions.
- **CONTRIBUTING.md**: setup steps, test and lint commands, release process, branch conventions.
- **CHANGELOG.md**: missing release entry, duplicate content, implementation detail dressed as user news.
- **AGENTS.md**: changed workflow instructions, changed tooling rules, changed release discipline.
- **docs/**: tutorials that now fail, examples using old APIs, stale filenames or caveats.

## Step 4: Draft all updates

For each affected doc: the stale section, why it is stale, and the exact or
near-exact replacement text. Keep internal mechanism changes out of docs unless
the user-facing behavior also changed.

Draft a `CHANGELOG.md` entry using only the sections that actually changed:

```
## <version or date>

### Added
### Changed
### Fixed
### Removed
```

## Step 5: Present and get approval

Before writing anything, present:
- Diff summary (user-facing)
- Doc files checked and cleared
- Doc files needing updates
- Draft changelog entry
- Proposed edits per file

The user approves the batch or requests changes. Then apply the edits.

## Missing docs

If the diff reveals a gap a new doc would fill, say so plainly:

> "No `ARCHITECTURE.md` exists. The diff changes system boundaries. Suggest adding one."

Do not create it here. Creating net-new docs is a separate task.
