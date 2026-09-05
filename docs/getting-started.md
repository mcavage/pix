# Your first Pix session

Follow [the installation steps](../README.md#get-started) first. This guide starts
with Pix installed and Docker Desktop running.

## Set up the environment you want

For the generated default environment:

```bash
pix setup
```

Setup offers supported installed Ollama models and cloud provider choices when
no model is selected. Choose one; setup asks for its missing connection details.
Direct provider keys use 1Password references. An environment with its own
model gateway does not need personal provider keys.

If you have an environment supplied by someone else, set it up directly:

```bash
pix env add ~/path/to/environment team
pix setup --env team
```

A Git URL works as the source too. Choose any name in place of `team`. Setup
checks completed steps and can be rerun after an interruption. If it asks to make
the environment your default, yes means future bare `pix` sessions will use it;
it does not launch a session.

## Start working

```bash
cd ~/path/to/project
pix
```

If you added `team` without making it the default, use `pix run --env team`.
Describe the outcome you want; the agent
can work with the files in that folder. Inside the session, `/getting-started`
shows the main features. Ask it to review a code change, summarize documents,
research a question, or draft a plan with supporting evidence.

The workspace is writable. Edits remain on your computer when the session ends.
Conversations are stored under `.pi-sessions` in that workspace. The temporary
sandbox is normally removed after its final session exits; `pix run --keep`
preserves its installed tools and other sandbox-only files as well.

## Keep useful context

These are commands **inside Pi**, not terminal commands:

```text
/remember Prefer short summaries followed by the evidence.
/recall preferences
/forget <memory-id>
```

Memory survives sandbox removal. It is explicit by default; automatic capture is
opt-in. Recall uses semantic search when its embedding backend is healthy and
keyword search otherwise. See [memory](memory.md).

## Choose models or work in parallel

```bash
pix run --env team --model openai/gpt-6-astra
```

The model must be available in the selected environment. You can also select
models inside Pi. To work on a Git project in a separate checkout:

```bash
pix task new feature-name
```

That creates and launches an isolated task checkout. Use `pix task ls` to list
tasks and `pix task path feature-name` to find one. See
[the command reference](reference.md) for resume, task removal, and model options.

## Check or reconnect

```bash
pix doctor
pix setup --env team
```

Doctor checks host readiness without repairing it. Setup reconnects accounts
that need attention. Add `--verbose` for diagnostic detail. Inside the session,
ask for the harness healthcheck to exercise connected tools; use the separate
repository healthcheck when you want a code-quality review.

`pix ls` shows your sandboxes. `pix rm NAME` removes an idle sandbox in the
current Pix home; it does not delete the mounted project. You usually do not need
to remove ordinary sandboxes yourself.

Upgrade with `brew upgrade mcavage/tap/pix`. Pix reconciles its own runtime on
the next launch; rerun environment setup when its connections change.

## More

- [Command reference](reference.md)
- [Security and account access](../SECURITY.md)
- [All documentation](README.md)
- [Development instructions](../AGENTS.md)
