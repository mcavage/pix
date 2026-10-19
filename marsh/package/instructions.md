# Pix: your Pi stack on marsh

Use upstream Pi tools directly. This package has no `subagent` tool, chain/task
API, scheduler, launcher, sandbox controller, model router or memory service.
The private configuration directory is ~/.pix-agent/agent; public content is
under /opt/pix/package. Load relevant skills, not the whole library.

## Orchestration

The marsh-orchestration skill is authoritative for execution. References in
older generic skills to `subagent`, `{tasks: ...}`, `chain`, Pix worktrees or
environment bindings mean work-order intent, not installed APIs. Translate them
to registered marsh commands, `split`/`join` and `fanout`/`collect`; never invent
those old tools or run old `pix setup`, `pix run`, `pix rm`, or `pix task` commands.
`pix` now takes Pi CLI arguments. The public roles under /opt/pix/package/roles
are prompt references, not registered agents or enforced tool/turn-limit policy.

marsh owns lifecycle/admission/cancellation and does not replay lost jobs. A
split join returns patches and does not apply them. Dependent review starts only
after the intended implementation is present; sibling forks cannot review each
other's unjoined changes. Use explicit model flags when a role needs a particular
model/vendor; inspect actual response metadata before claiming its identity.
Read an optional ~/.pix-agent/agent/roster.json before role dispatch. Its
`models.ROLE` values are requested role bindings, `defaultModel` is a fallback,
and an explicit user/task model choice wins. Pass the chosen value with
`--model`; reading this file does not prove which model answered. Do not import
authentication or assume a custom role-dispatch tool is installed.

## Authority and continuity

No business sources are configured by default. Consult the optional personal
capabilities.json in the Pix agent directory and actual MCP schemas. Discovery
is not proof of authentication; missing providers are gaps, not invented data.
Never seek host credentials or change connections from a job without approval.
Use existing MCP gateway tools, not a second MCP client/service configuration.

There is no semantic memory/automatic recall here. marsh receipts record jobs,
not remembered facts or a shared TODO service. Use provided context and report
missing history honestly. Output styles affect prose only, never facts, scope,
tool permissions or project rules. Ask before durable private-context writes.
