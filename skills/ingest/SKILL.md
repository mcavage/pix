---
name: ingest
description: Ingest a document into persistent memory with provenance, extract key facts, store with source tracking and staleness classification. Use for "ingest this doc" or "read and remember this".
---
# ingest

Goal: one document in, a set of classified facts stored with provenance. The
memory service deduplicates by content hash, so re-ingesting an unchanged
document is safe and cheap.

## Steps

1. **Read the document.** Fetch the content via whatever source is appropriate
   (file read, URL fetch, API call). Get the full text. If the document lives
   behind a capability (docs, gworkspace, calls, chat), resolve it through
   `capability-routing`, which reads `capabilities.json` and either pulls from
   the wired provider(s) or tells you it is `none`. If `none`, say so and ask
   the user to paste the text.

2. **Compute a content hash.**

   ```bash
   echo -n "<full document text>" | sha256sum
   ```

   Or in Python: `hashlib.sha256(text.encode()).hexdigest()`

3. **Extract facts.** For each fact decide:
   - `content`: one self-contained statement, 1-3 sentences max.
   - `type`: `fact`, `decision`, `preference`, or `context`.
   - `staleness_type`: `historical` for immutable past events (shipped features,
     past financials, meeting outcomes); `current_state` for anything that can
     change (active roadmap, team goals, pricing, product status).
   - `tags`: relevant topic strings.
   - `confidence`: `1.0` for explicit statements, `0.7`-`0.9` for inferred.

4. **Store via `memory_ingest`.**

   ```
   memory_ingest(
     source_type="gworkspace",    # gworkspace | url | file | calls | chat | docs
     source_id="<unique id>",
     title="<document title>",
     content_hash="<sha256>",
     source_ref="<url or path>",
     facts='[
       {
         "content": "The v2 API ships read-only in Q2 2026.",
         "type": "fact",
         "staleness_type": "current_state",
         "tags": ["api", "v2", "roadmap"],
         "confidence": 1.0
       },
       {
         "content": "The team shipped the v1 billing integration in March 2026.",
         "type": "fact",
         "staleness_type": "historical",
         "tags": ["billing", "v1", "launch"],
         "confidence": 1.0
       }
     ]'
   )
   ```

5. **Check the response.**
   - `"status": "ingested"`: stored fresh; note `facts_stored`.
   - `"status": "unchanged"`: hash matched; nothing to do.
   - `"status": "reingested"`: document changed; `current_state` facts from the
     prior version are superseded, `historical` facts are preserved, version
     increments.

## What to extract

Extract: key decisions and rationale, quantitative data (dates, counts,
milestones), strategic priorities, ownership and assignments, technical
architecture choices, action items with owners.

Skip: boilerplate and formatting artifacts, speculative statements (or tag them
with confidence 0.5-0.7), raw data that belongs in the project's data store
(store conclusions, not tables), obvious duplicates of facts already in memory.

## Staleness quick rule

`historical`: the event is over and the fact cannot be superseded: "Launched
X in Q4 2025", "Signed deal with Y on Jan 15", "Board approved Z on March 1."

`current_state`: the fact describes the world as it is now and could change:
current roadmap targets, team composition, active priorities, pricing.

## Listing what is indexed

```
memory_docs()                                     # all indexed documents
memory_docs(source_type="gworkspace")             # filter by source type
memory_doc_facts(document_id="<id>")              # facts for one document
memory_doc_facts(document_id="<id>", include_superseded=True)
```

Report: document title, `document_id`, `facts_stored`, and status. If status
is `reingested`, also report how many facts were superseded vs. preserved.
