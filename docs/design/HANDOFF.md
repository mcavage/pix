# Handoff: the `cmd/pix` drain

Self-contained. If you have no other context, read this and
`docs/design/architecture.md`, in that order, and you can continue.

## What this work is

`services/host/cmd/pix` was 40,905 production lines in one Go package — 91
files, 2,855 functions, all mutually visible, no import graph, so no boundary
anyone could violate. It is being drained into a five-layer architecture whose
rule is enforced by a test, not by prose.

Branch: `refactor/foundations`. Sixteen commits. Read them in order if you want
the reasoning; each message states what it found, not just what it changed.

## Current state

```
cmd/pix production LOC    40,905 -> 33,621
packages                       6 -> 14
nil-seam guards              125 -> 11   (all on domain probes; see hostenv)
```

Layers (see `architecture.md` for the rule, `arch_test.go` for the enforcement):

```
L0  sys sys/systest config routing rpc cli hostenv launcher     done
L1  inference monitor monitor/tui okf plugin slackoauth
    workspace service                                           partial
L2  readiness                                                   done
L3  —                              (workflows still inside cmd/pix)
L4  cmd/pix                                                     draining
```

## How to work

```sh
export PATH="/opt/homebrew/bin:$PATH"      # node 26, for the gate
cd /Users/mcavage/dev/pix
bash scripts/gate.sh                       # build, vet, test, node, tsc, guards
```

The gate's **12-second timing line has failed on `main` since before this work
started**. Ignore it. Every other segment must be green before you commit.

Rules, in priority order:

1. **Gate green at every commit.** Commit early — an uncommitted refactor that
   goes wrong costs the whole session (it did, once).
2. **>20 sites means write a script.** Commit the script. The compiler and the
   1,500 tests are the proof, not your eyes.
3. **Moves and behaviour changes are separate commits.** A refactor commit that
   also fixes a bug hides the bug.
4. **Net-negative on lines**, or say why in the message.

## The tools

| tool | what it is for |
|---|---|
| `scripts/extract-pkg` | Size an extraction. go/parser, NOT regex — a regex was off by 12× (said 25 external symbols for the monitor TUI; the truth was 2). |
| `scripts/migrate-shellenv/` | AST literal rewriter. Pattern to copy for composite-literal surgery. |
| `scripts/drop-nil-guards/` | AST dead-guard removal, skipping nested edits. |
| `scripts/migrate-shellenv.py` | Line-level renames. Every false positive is a compile error, which is the safety net. |

Build the sizer once: `cd scripts/extract-pkg && go build -o /tmp/extract-pkg .`
Then from the repo root: `/tmp/extract-pkg <file-prefix> [more...]`

## The next extraction

Order is **inbound count ascending** — inbound is the real blocker, because an
extracted package cannot import `package main`.

**Re-measure first.** Every extraction drops the next one's count: `rpc` alone
took `memory` from 11 to 5 without touching `memory.go`. The numbers in
`architecture.md` predate `workspace`, `service` and `readiness`.

Remaining: knowledge, secret, pack, mcp, task, run+sandbox. `doctor` stays —
110 inbound is what a composition root looks like, and "fixing" it would be
wrong.

### The recipe

1. `/tmp/extract-pkg <prefix>` — read INBOUND, not LOC.
2. If inbound is stubborn, the cause is almost always a capability calling a
   sibling. **Invert it into the workflow** rather than moving both.
3. Consider whether a *smaller thing inside* the folder is the real package.
   `readiness` measured 17 as a directory and 2 as types+snapshot+render.
4. `git mv` the files, rewrite the package clause, export the API.
5. Drive the compiler in a loop — see "the fix loop" below.
6. Add the package to `pkgLayer` in `arch_test.go`. It fails until you do.
7. Gate, commit.

### The fix loop

Renaming to an exported API produces a long cascade of field/method-case errors.
Do not fix them by hand. This loop reads the compiler's own suggestion:

```python
import re, subprocess
for _ in range(400):
    out = subprocess.run(['go','vet','./cmd/pix/','./<newpkg>/'],
                         capture_output=True, text=True).stderr
    m = re.search(r'(cmd/pix|<newpkg>)/([\w.]+\.go):(\d+):\d+: (\S+)\.([A-Za-z]+) '
                  r'undefined \(type [^)]*?, but does have (?:field |method )?([A-Za-z]+)\)', out)
    if m:
        d,f,ln,recv,bad,want = m.groups()
        p=d+'/'+f; s=open(p).read()
        ns=s.replace(recv+'.'+bad, recv+'.'+want)
        if ns==s: print("stuck:", m.group(0)[:120]); break
        open(p,'w').write(ns); continue
    m = re.search(r'(cmd/pix|<newpkg>)/([\w.]+\.go):(\d+):\d+: unknown field ([A-Za-z]+) '
                  r'in struct literal of type [^,]+, but does have ([A-Za-z]+)', out)
    if m:
        d,f,ln,bad,want = m.groups()
        p=d+'/'+f; L=open(p).read().split('\n'); i=int(ln)-1
        L[i]=re.sub(r'\b'+bad+r':', want+':', L[i]); open(p,'w').write('\n'.join(L)); continue
    print([l for l in out.split('\n') if l.startswith('vet:')][:1] or "VET CLEAN"); break
```

## Traps, all of which bit

- **Renames leak into string literals.** `"google-workspace"` became
  `"google-ws"`; `-test.run` became `-test.Run`; the doctor headline's
  "outstanding" became "Outstanding". After any bulk rename:
  `git diff HEAD -- services/host | grep -E '^\+' | grep -oE '"[^"]{4,}"' | sort -u`
  and read it.
- **Renames leak into `package` clauses and comments.** One pass renamed
  `package workspace` to `package Workspace`; the error it produced
  (`undefined: workspace` at a correct-looking import) is deeply unhelpful.
- **Non-raw Python replacement strings.** `':\2'` emits the *character* U+0002,
  not a group reference. Use `r'...\2'`.
- **Nested AST edits.** Applying an outer and inner edit from the same pass
  splices stale offsets. Keep outermost only and re-run to a fixpoint.
- **`-X main.version` is silently ignored** on a variable with a non-constant
  initializer. `main` owns the stamp and pushes it into `launcher.Version` in
  `init()`. Do not "simplify" that.
- **A local named after a package shadows it.** Rename the local; the package
  has the better claim on the name.
- **Tests follow their SUBJECT, not their filename.** When a test in a moved
  file reaches back into `cmd/pix`, ask what it is really testing. Usually it
  belongs where it came from, and moving it back is cheaper and more honest than
  copying helpers into a capability that has no business owning them.

## Phase 2 (the CLI) is interleaved, not blocked

`services/host/cli` is the command contract: a command returns an error (never
`os.Exit`), writes to `d.Out` (never `os.Stdout`), and declares flags as struct
tags so usage is generated. Library is kong — chosen over cobra because cobra's
`RunE` has no parameter for dependencies, so they arrive via untyped
`cmd.Context()` and flag targets become package-level vars.

Migrated: `models`, `agent`, `secret`. 31 verbs remain. Copy
`services/host/cmd/pix/models_cmd.go` or `agent_cmd.go`; the five-step recipe is
in `architecture.md`. One quirk to know: `cli.Run` recovers a panic to intercept
kong's `os.Exit` on `--help`. Kong offers no other hook. It is confined to one
function and commented.

## Known cosmetic debt

Commit `fe5aaf1`'s message has stray backslash escaping. Interactive rebase is
unavailable in this environment, so it was left alone.
