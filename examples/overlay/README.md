# RETIRED — the overlay example is gone

**The build-time "overlay" model has been removed.** It required compiling private
Go into `pi-stack-host` (`make ... OVERLAY=..`) plus a stacked mixin kit — exactly
the coupling packs were meant to kill. The old scaffold that lived here (a `kit/`
tree + `host/overlay_example.go` + `overlay.mk`) demonstrated a mechanism that no
longer exists, so it was deleted to stop misleading readers.

Private company context is now a **pack** (git-backed, mounted at runtime, no
build), and host-executing integrations ship as **containers** the sbx gateway
runs (or, when host-only, a small host daemon). Nothing private is compiled into
`pi-stack` anymore.

Start here:

- [../../docs/OVERLAY.md](../../docs/OVERLAY.md) — the migration note (where each
  old overlay piece went, and how to add a private integration now).
- [../../docs/design/packs.md](../../docs/design/packs.md) — the pack design.

To scaffold a pack: `pi-stack pack new`, then `pi-stack pack use <path>`.
