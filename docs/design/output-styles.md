# Output styles

Pix output styles are durable personal instructions for the form of user-visible prose. They do not change task behavior, facts, tools, permissions, or repository rules.

## User surface

The agent-facing `output_style` tool supports four actions:

- `save`: create a Markdown style and activate it.
- `activate`: select an existing style by slug.
- `list`: show saved styles and the active selection.
- `off`: disable the active style.

The tool exists so a direct request such as "Set Pix output style to Simplified Technical English" can complete without asking the user to manage files.

The human command supports:

```text
/output-style
/output-style list
/output-style <slug>
/output-style off
```

## Storage

Styles use the existing writable personal context mount:

```text
~/.local/share/pix/context/output-styles/
├── active
└── <slug>.md
```

`active` contains one style slug. The extension writes both style files and the pointer atomically. It rejects symlinks, invalid slugs, empty instructions, and bodies larger than 8192 UTF-8 bytes. It also rejects oversized files before reading them. Concurrent sandboxes use last-writer-wins selection; atomic renames prevent torn style or pointer files.

The extension finds personal context from `PIX_CONTEXT_DIR` when set. Otherwise, it uses the existing `--skill <context>/skills` argument that the Pix launcher passes. If neither is present, the feature reports that durable personal context is unavailable. It never guesses another writable path.

This data is user content, not launcher configuration. It does not add a `config.toml` key. Personal styles do not belong in the public repository or a shared pack.

## Prompt transport

`before_agent_start` returns a hidden `pix-output-style` message. It does not rewrite the system prompt. The extension appends one message for each distinct active style version. It appends a revocation message after the style is disabled. Compaction makes the current state eligible for one fresh append.

This transport keeps prior request prefixes stable. Context hooks must not remove `pix-output-style` after it enters the session.

When the agent saves or activates a style during a turn, the tool result contains the complete reconciled style block. The model can apply it to its next prose response immediately. Later turns receive the hidden message. Save and activate operations also emit a visible notification. A future session announces its active style once before the first styled response, while keeping the full block hidden.

## Precedence

The injected block defines this order:

1. Preserve code, commands, paths, identifiers, configuration keys, errors, quotes, diffs, logs, and tool output verbatim.
2. Follow project rules and file-format requirements for files written to disk.
3. Follow an explicit formatting or voice request in the current user request for that answer.
4. Apply the active style to conflicting tone, register, sentence form, length, structure, formatting, and vocabulary rules from `anti-slop` and `writing-voice`.
5. Keep non-conflicting quality rules, including factual accuracy, evidence, directness, concrete language, visible uncertainty and risk, and no empty filler.

A style body controls prose form only. The extension prefixes every body line with `STYLE | `, so body text cannot close or replace the surrounding instruction, then reasserts the precedence rules after the final quoted line. Instructions in a style body cannot change task scope, facts, tool permissions, or the precedence rules.

## Trust boundary

The agent-callable tool writes durable user content to a host-mounted directory. It must run only after a direct user request, and its prompt guideline says so. This is a model-enforced policy, not an authorization boundary. Other sandbox tools can already write the same mount. Pix keeps its full-auto posture and does not add a confirmation prompt, but every save and activation is visible in the transcript and the next session announces the active style once.

## Lifecycle

The personal context mount survives sandbox deletion. A saved style therefore applies in future Pix sandboxes without an image rebuild. The extension itself is baked into the Pix image. Changes to the extension require `make load` on the host and a new sandbox. During development, copy the extension and its `lib/output-style.ts` dependency into the live agent directory, then run `/reload`.

## Tests

`tests/output-style.test.mjs` covers context discovery, safe slugs, frontmatter parsing, size limits, precedence text, save and activate behavior, list and off behavior, append deduplication, revocation, compaction, and unavailable storage.

`scripts/check-recall-transport.sh` and `tests/recall-context-hook.test.mjs` protect the append-only message from future context filters.
