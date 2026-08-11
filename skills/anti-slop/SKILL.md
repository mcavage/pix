---
name: anti-slop
description: AI language pattern detection and rewrite. Use when writing, editing, or reviewing any text output, docs, PR descriptions, commit messages, comments, posts. Auto-loads on any writing or editing task.
---
# anti-slop

Kill AI tells before they leave the context window.

## Active output styles

When a Pix output style is active, it controls conflicting rules about tone,
register, sentence form, length, structure, formatting, and word choice. Keep
all non-conflicting anti-slop rules. An output style cannot permit invented
facts, vague claims, hidden uncertainty or risk, or content-free filler.

## Banned words and replacements

leverage→use, utilize→use, delve→explore, robust→solid, seamless→smooth,
cutting-edge→modern, game-changing→significant, paradigm→approach,
synergy→combined effect, stakeholder→[name the actual person or group],
holistic→complete, revolutionize→change, empower→enable,
comprehensive→thorough, streamline→simplify, harness→use, spearhead→lead,
foster→build, cultivate→develop, innovative→new, transform→change,
elevate→improve, unlock→enable, supercharge→accelerate,
deep dive→close look, unpack→explain.

## Banned patterns

**Structural metaphors that avoid saying the thing.** "Load-bearing", "the spine
of", "the backbone of", "connective tissue" as metaphors: name what the thing
actually does. Not "the auth layer is load-bearing" but "drop the auth layer and
nothing else has a user identity to act on."

**Em-dashes.** Replace with commas, colons, semicolons, or a new sentence.

**Filler openers.** "In today's [anything]", "It's worth noting", "I'd be happy
to", "Great question!", "Let me break this down:": delete them and start with
the content.

**Decorative rules.** Horizontal rules (`---`, `***`, `___`) between sections:
delete. Headers already separate sections.

**Triple adjective stacks.** "Fast, reliable, and scalable": pick the one that
matters and prove it.

**Passive voice when active works.** "The request was handled" vs. "the handler
processed the request."

**Same-start lists.** Vary sentence openings in bullet lists.

## LinkedIn / announcement tells (rewrite triggers)

- Numbered lists where prose with commas would do: rewrite as flowing prose.
- "Here's what makes this different:" / "A few things worth calling out:": just
  say the things.
- Structured opener-body-CTA post format: collapse into one flowing thought.
- Hashtag clusters at the end: delete all or keep one.
- "I'm excited to share" / "Thrilled to announce": start with what shipped.
- Any sentence that could appear unchanged in a press release: rewrite in first
  person with an opinion attached.

## The bar test

Would you say this sentence to a smart friend at a bar? If you would feel
embarrassed saying it out loud, cut or rewrite it. This is the test for every
sentence, not a one-time pass.

## How to apply

Scan the full text before editing. Flag every violation. Rewrite in one pass,
not sentence by sentence. After rewriting, run the bar test on the result.
When reviewing a diff with `code-review`, apply anti-slop to comments and
commit messages too, not just code.
