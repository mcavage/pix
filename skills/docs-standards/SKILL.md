---
name: docs-standards
description: Documentation writing standards for tutorials, API references, and READMEs. Use for "write docs", "add a tutorial", "document this API", or prose aimed at readers, not the compiler.
---
# docs-standards

Write for the reader who is stuck, not for the reader who already knows. Every
doc page must earn its place by saving someone time.

## Universal page rules

Every page gets:
- **Title**: a task, not a topic. "Add authentication" not "Authentication".
- **One-sentence summary**: what the reader will have done by the end.
- **Prerequisites**: tools, versions, and assumed knowledge, explicit.
- **Numbered steps**: one action per step, one expected result per step.
- **Expected output**: what success looks like (a command result, a screenshot, a response body).
- **Next steps**: 2-3 links onward.

## Tutorial shape

1. What you will build (a screenshot or a concrete terminal snippet).
2. Prerequisites (versions matter; be exact).
3. Steps: each step = action + result. If the reader can do the wrong thing, say
   what wrong looks like and how to fix it.
4. Verify: a command the reader runs to confirm it works. Prefer the project's
   test command if one exists.
5. Next steps.

Do not narrate what you are about to do. Do it, then explain.

## API reference shape

For each endpoint or method: method + path, auth required, request schema,
response schema (success and every error code), rate limits, and at least two
language examples. If a field is optional, say the default. If a field is
deprecated, say what replaces it and when it goes away.

## Banned words

Never use these in docs: simply, just, easily, obviously, of course, etc.,
various, several, powerful, flexible, robust, seamless, straightforward,
intuitive, note that, please note, it is worth noting.

If you reach for one, ask: is there a concrete fact hiding behind this filler?
Write that fact instead.

## Before you write

If the feature is not yet specified, run `build` first. If you are documenting
code that has not been reviewed yet, run `code-review` first. Do not document
behavior that the code does not yet have.

After writing, run `verify` against a real execution of the steps to confirm
they produce the stated output.
