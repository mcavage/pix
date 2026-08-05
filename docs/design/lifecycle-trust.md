# Sandbox lifecycle and pack trust

Two related but separate mechanisms answer two different questions: "is this
sandbox still needed" (lifecycle) and "should this pack's host-executing code
run on my machine" (trust). Both fail closed, and both are designed so a human
never has to remember to clean up or to notice a change — the machinery
notices for them.

## Sandbox lifecycle

A pix sandbox is **ephemeral**: `pix run DIR` creates one if none exists for
that directory, or reattaches to it as-is (running or stopped) if one does.
There is no `pix reset` — see "no automated wipe" below.

```
no sandbox named N         -> pix run creates it
a sandbox named N exists   -> pix run ATTACHES to it as-is
last shell out             -> non-force teardown, automatically
```

**Bare TTY vs. non-interactive.** `pix DIR` (no `run`) is shorthand for `pix
run DIR`, but **only from an interactive terminal**. From a script or a pipe
(no TTY), the exact same bare form refuses on stderr with exit 2 and touches
nothing — no create, no attach, no config read — because nobody is there to
have meant it. The explicit `pix run DIR` is unaffected either way; a script
always uses the explicit verb.

**Last-shell teardown.** When the shell that created a sandbox exits, pix
attempts a **non-force** removal automatically, gated on a kernel-verified,
zero-holder proof (no other shell still references this sandbox, no
lifecycle transition in flight, the runtime's own instance id still matches
the creation record, and no keep is held on it). Any weaker answer — a held
reference, a held keep, an untrusted probe, a mismatched instance id, a name
outside `pix-*` — **keeps** the sandbox and journals why, rather than forcing
anything. This is the same proof `pix rm` uses explicitly, so "did the last
shell out really finish, or is a stale entry still around" always has one
implementation to trust.

**Explicit removal.**

```
pix rm NAME                remove one (non-force, same zero-reference proof)
pix rm NAME --force         force it — the ONLY forced seam, and only on a
                            name typed by a human
pix rm --all --keep NAME    remove every pix-* sandbox but the kept one(s)
                            (-k is the short flag; never forced)
pix rm --orphans            remove only pix-owned sandboxes with zero live
                            references and no keep (never forced)
```

`--all` and `--orphans` can never be forced, by construction — the only way
to skip the zero-reference proof is to name one sandbox explicitly with
`--force`. `pix rm` only ever touches `pix-*` names; it cannot reach a sandbox
it did not create.

**No automated wipe.** `reset`/`state reset` are retired. Recovery from a
broken host-side config or data directory is manual and evidence-first: run
`pix doctor` to name the exact gap, back up whatever `pix config path` /
`pix status --json` show is worth keeping, then `pix setup` to rebuild. No
command wipes state for you — see
`services/host/cmd/pix/corpus/retirement.jsonl` entry `reset` for the full
reasoning.

Full mechanism and edge cases: `docs/design/serve-lifecycle.md` (the host
services half) and the `launch` package's `reap.go` (the sandbox teardown
half, heavily commented at the source).

## Pack trust

A pack is a git-backed bundle of skills, knowledge, MCP integrations, and
wrappers, activated with `pix setup --pack <url>` or `pix pack use`. Most of
a pack's content (skills, prompts, `capabilities.json`) is inert text; the
trust gate exists only for the part of a pack that is **not** inert: anything
that would run HOST code — a local MCP command, a `host=true` wrapper, an
external `[[bin]]`, or a trusted `[[services]]` unit `pix-host serve`
supervises.

**Two gates in series.**

1. **Adoption gate.** The first time a pack asks to run host code, pix shows a
   Tier-1 bill-of-materials (every command, argv, and pinned SHA it would
   execute) and asks `[y/N]`. A non-interactive session (no TTY) fails
   **closed** without an explicit `--yes` — it never silently assumes consent.
2. **Fingerprint gate.** Acceptance is recorded in a **launcher-owned**
   host-state store, never in the pack's own payload (a pack cannot vouch for
   itself). A canonical fingerprint over every host-executing surface — MCP
   argv, wrapper scripts, `[[bin]]` pins, `[[services]]` fields — is
   recomputed on every load; ANY change to that surface re-gates, even if the
   pack was accepted before.

**Fails closed by default.** Whether an MCP command classifies as local
(host-executing) or remote (gateway-run container) must be knowable; an
unclassifiable entry is treated as the more dangerous case. Credentials a pack
declares are `op://` references only — never a literal secret value — resolved
through 1Password at the point of use, never stored in the pack, the
registration, or the host-state store.

**Where each fact lives.** `services/host/workflow/pack/trust.go` (the
fingerprint + gate decision), `truststore.go` (the launcher-owned acceptance
store), `service.go` (the `[[services]]` vocabulary a trusted pack may
declare, wired into `pix-host serve`'s Suture supervision tree once accepted —
see AGENTS.md's "go-plugin + Suture host architecture"). Full design:
`docs/design/packs-v2.md` and `docs/design/packs-v2-impl.md`.
