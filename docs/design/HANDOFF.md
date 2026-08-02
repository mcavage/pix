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
cmd/pix production LOC    40,905 -> 27,836   (64 files, was 91)
packages                       6 -> 22
nil-seam guards              125 -> 11        (all on domain probes; see hostenv)
```

Layers (see `architecture.md` for the rule, `arch_test.go` for the enforcement):

```
L0  sys sys/systest config routing rpc cli launcher
    hostenv hostenv/hostenvtest workspace                       done
L1  inference monitor monitor/tui okf plugin slackoauth
    service memory knowledge secret mcp                         partial
L2  readiness                                                   done
L3  workflow/man workflow/backup workflow/upgrade               open
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
| `scripts/extract-pkg` | Size an extraction. go/parser, NOT regex — a regex was off by 12× (said 25 external symbols for the monitor TUI; the truth was 2). Run it from the REPO ROOT with bare basename prefixes (`extract-pkg pack kitref`), not paths. |
| `scripts/movesym` | Move declarations between files in one package, carrying doc comments. The most useful tool here: most extractions are unblocked by moving symbols home, not files. |
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

Remaining, inbound ascending: onboard (6), pack (6), reset (9), gworkspace (19),
status (20), task (25), run+sandbox (47). `doctor` (30) and `setup` (56) stay —
a high inbound count is what a composition root looks like, and "fixing" it
would be wrong.

Three counts are noise in EVERY domain: `main`, `version`, `defaultShellEnv`.
Subtract them first.

### pack is next, and it is a pack/run separation

pack is 5,443 lines and reads as 6 inbound, but three of those are noise and
the real content is one boundary:

```
runOpts                <- sbxargs.go     pack.applyPackToLaunch takes run's options
writeOllamaBridgeFile  <- run.go         pack.writePackContextFiles calls run's writer
hostBinaryResolver     <- main.go        4x localMCPClassifier(env, hostBinaryResolver)
```

`applyPackToLaunch`, `applyPackStackToLaunch` and `writePackContextFiles` are
LAUNCH functions that live in pack.go. They are run's half of the boundary, not
pack's — pack proper is resolve / validate / trust / activate. Move those three
to run's side first and re-measure; the rest is `hostBinaryResolver` becoming a
parameter of the four `localMCPClassifier` callers, which is the same inversion
`slack.RegisterFn` and `pack.RegisterFn` already did.

Both RegisterFn seams are already in place, so pack no longer calls mcp.

### The recipe

1. `/tmp/extract-pkg <prefix>` — read INBOUND, not LOC.
2. Look at WHERE each inbound symbol is declared. If it is named after your
   domain and lives in someone else's file, `movesym` it home — that is usually
   most of the count. `mcp` went 23 -> 5 this way with no file moving.
3. Then split the argv seams (`runFooCmd`, anything calling `os.Exit`) into a
   `foo_cmd.go` in cmd/pix. Those are the calls that legitimately need
   `defaultShellEnv` and the composition root, and they belong at L4.
4. If inbound is STILL stubborn, the cause is a capability calling a sibling.
   **Invert it** — define a struct of the facts it needs and let the workflow
   fill it (see `mcp.Credentials`). Do not move both packages.
5. Consider whether a *smaller thing inside* the folder is the real package.
   `readiness` measured 17 as a directory and 2 as types+snapshot+render.
   Equally: a file named `mcp_catalog_gate.go` was a SETUP gate; renaming it to
   match its subject removed it from the extraction entirely.
6. `git mv` the files, rewrite the package clause, export the API.
7. Drive the compiler in a loop — see "the fix loop" below.
8. Add the package to `pkgLayer` in `arch_test.go`. It fails until you do.
9. Gate, **diff the string literals AND the comments** (see traps), commit.

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

- **Renames leak into string literals AND comments.** This keeps happening and
  it keeps reaching users. `"google-workspace"` -> `"google-ws"`; `-test.run`
  -> `-test.Run`; "outstanding" -> "Outstanding"; `provenance` -> 
  `upgrade.Provenance` in 23 strings and 57 comments including three sentences
  doctor/status/pack print; `gog` -> `Gog` in 28 strings and 42 comments; and a
  LOCAL variable rename that turned the advice `pix config set mcp <server>`
  into `pix config set group <server>`. Diff both, every time:
  ```sh
  git diff -- services/host | grep -E '^\+' | grep -oE '"[^"]{5,}"' | sort -u > /tmp/n
  git diff -- services/host | grep -E '^-' | grep -oE '"[^"]{5,}"' | sort -u > /tmp/o
  comm -23 /tmp/n /tmp/o
  ```
  Repair inside literals and comment tails SEPARATELY — a single pass over the
  whole file re-breaks the code you just renamed.
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

Migrated: `models`, `agent`, `secret`. 31 verbs remain. `mcp` has its argv seam
split out (`cmd/pix/mcp_cmd.go`) but still hand-parses; that file is the natural
next one to convert. Copy
`services/host/cmd/pix/models_cmd.go` or `agent_cmd.go`; the five-step recipe is
in `architecture.md`. One quirk to know: `cli.Run` recovers a panic to intercept
kong's `os.Exit` on `--help`. Kong offers no other hook. It is confined to one
function and commented.

## Known cosmetic debt

Commit `fe5aaf1`'s message has stray backslash escaping. Interactive rebase is
unavailable in this environment, so it was left alone.
