---
name: pix-dev
description: Develop and test Pix itself from a Pix session launched with --dev, using host pix, sbx, Docker, Git, and build commands through Pix MCP tools.
---
# Pix development

When Pi exposes the `mcp` adapter, first search with
`mcp({ search: "pix_host_info" })`. Use the exact returned tool name and put its
parameters inside `args`, for example
`mcp({ tool: "mcp_gateway_pix_host_info", args: {} })`. Apply the same pattern
to every host tool; do not flatten its arguments into the adapter call.

Call `pix_host_info`. Continue host development only when `development` is true
and `pix_host_exec` is available. Otherwise explain that the user must launch
`pix run --dev` from the host. No tool argument can enable this authority.

Development mode lets commands run with the host user's permissions. Use it for
the requested work; read the checkout's AGENTS.md and build instructions first.
Use ordinary workspace tools for source edits. Use the environments skill and
its narrower tools when the task is only to author or test an environment.

`pix_host_exec` takes an `argv` array, an optional host `cwd`, and a bounded
`timeout_seconds`. There is no implicit shell. For example, use
`["make", "load"]` in the Pix checkout, then poll its job. If shell syntax is
necessary, pass an explicit shell command. Poll `pix_job_status` until finished;
inspect both output and exit code. Cancel obsolete work with `pix_job_cancel`.
The session owns these jobs; leaving the session cancels them.

Use an explicitly separate `PIX_HOME` for host UAT and record its stack id.
Pass it with `env`, for example `["env", "PIX_HOME=/absolute/test/home", "pix", ...]`.
Use real Pix entry points for setup, launch, attach, and cleanup. Do not modify
or reset the user's current home, borrow its receipts, or remove foreign
sandboxes, containers, or Gateway registrations. Keep credentials out of command
arguments, output, source files, and model context; use existing reference writers.

`make load` produces the matching launcher, runtime, images, and manifest and
loads the image into sbx's separate store. Recreate an idle test sandbox after an
image or kit change. `/reload` is sufficient only for live skills. A source edit
or successful build alone does not prove the running sandbox contains the change.

Verify the production caller and relevant gate; test real model responses and
integration requests when affected. Inspect CI after pushing. Report what was
built, what was actually exercised on the host, and any remaining failures.
