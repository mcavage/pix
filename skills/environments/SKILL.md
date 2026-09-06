---
name: environments
description: Create, adopt, inspect, and test Pix environments from a session. Use for new environments, model mappings, and environment trials.
---
# Environments

When Pi exposes the `mcp` adapter, first search with
`mcp({ search: "pix_host_info" })`. Use the exact returned tool name and put its
parameters inside `args`, for example
`mcp({ tool: "mcp_gateway_pix_host_info", args: {} })`. Apply the same pattern
to every host tool; do not flatten its arguments into the adapter call.

Use the Pix host MCP tools through the Gateway. Start with `pix_host_info` to
find the host workspace mapping, then `pix_env_list` and `pix_env_inspect` to
understand existing choices. Tool names may have a Gateway prefix.

Author a directory in the current workspace, for example `environments/experiment/`.
It already lives on the host because Pix mounts that workspace. Use the installed
Pix documentation and an inspected environment for the native `.sbxenv.yaml`
grammar and optional `pix.toml` sidecar. Keep model IDs literal; preserve provider,
API, endpoint, and credential mode together. Do not invent a model router.

1. Write the candidate files using ordinary workspace tools.
2. Call `pix_env_add` with `source: "environments/experiment"` and a new name.
   This adopts the directory; it does not copy it or overwrite an existing name.
   Later edits to that directory are edits to the adopted environment.
3. Poll the returned job using `pix_job_status`. Inspect the exit code and output.
4. Call `pix_env_inspect` for the adopted name. `effective: true` previews composition
   with the current workspace; the actual trial uses a separate workspace.
5. Call `pix_env_test` with the name and a small prompt that exercises the model or
   integration under test. Poll until finished; cancel unnecessary jobs with
   `pix_job_cancel`. Inspect assistant response metadata for the actual provider
   and model, tool results, and usage. A listing or model self-report is no proof.

Definitions must stay outside the writable workspaces of sandboxes using them.
The test tool creates a separate workspace under the host's Pix state directory.
Trial sessions do not receive host tools. Never add a mount that exposes Pix's
private state or the environment definition itself to its trial.

A trial preserves normal Pix trust and credentials checks. If it refuses because
host access needs approval, explain the concrete access and give the user
`pix env trust NAME` to run on the host. Do not approve trust for them, execute
setup hooks yourself, request raw keys, or encode keys in authored files.

Report the adopted source path, environment name, actual trial result and model,
and the next launch command: `pix run ~/project --env NAME`. Creating environments
is an ordinary-user workflow; it does not require development mode.
