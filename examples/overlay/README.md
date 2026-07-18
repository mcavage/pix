# Example private overlay

A minimal, copyable scaffold for a pi-stack private overlay. See
[`docs/OVERLAY.md`](../../docs/OVERLAY.md) for the full explanation.

An overlay is its own **peer repo** (a sibling of pi-stack), kept private. To make
yours:

```bash
cp -r examples/overlay ../my-overlay      # a sibling dir, NOT inside pi-stack
cd ../my-overlay && git init              # your own private repo
# edit kit/spec.yaml, add skills under kit/files/.../skills/, fill in
# kit/files/.../capabilities.json, and put host plugins in host/overlay_*.go
```

pi-stack looks for the overlay at `../pi-stack-work` by default. If yours lives
elsewhere, pass `OVERLAY` to make (or export it in your shell):

```bash
make run OVERLAY=../my-overlay
```

`make run` stacks `kit/` automatically; `make serve` symlinks `host/overlay_*.go`
into pi-stack's `services/host/`.

Layout:

```
my-overlay/
  kit/                                              # sandbox half — mixin kit
    spec.yaml                                       # kind: mixin
    files/home/.pi/agent/
      capabilities.json                             # full routing (overwrites public)
      skills/example-data-skill/SKILL.md            # a private skill
  host/
    overlay_example.go                              # a host plugin (package main)
  overlay.mk                                        # private make targets
```
