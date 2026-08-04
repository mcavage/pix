# U04a — L0 lease package: evidence & findings

Scope: **hardened per-sandbox lifecycle/ref-lock primitives only** — no
policy, no supervisor loop, no CLI, no wiring into `services/host`
(behavior wiring is explicitly out of scope for this ticket).

## Provenance note (read first)

No `PRD`, `Story04`, or `AC-LIFE` document exists anywhere in this repo or
worktree (searched `AGENTS.md`, `docs/`, git history, and the filesystem for
`AC-LIFE` / `U04a` / `Story04` / `*lease*` — no hits beyond this task). This
package was built directly from the task's literal, detailed requirements
list, which is specific enough to serve as the spec. Nothing here should be
read as claiming a PRD line ID was consulted; if `Story04`/`AC-LIFE` land
later with different acceptance wording, reconcile against them then.

## Why a standalone module, not `services/host/lease/`

The task forbids editing `services/host/arch_test.go`, `services/host/config`,
and `services/host/go.mod`. `arch_test.go` fails closed on any package under
`services/host` absent from its `pkgLayer` map — so a new package placed there
would need that file edited regardless of intent. This package is therefore
its own Go module (`lease/go.mod`, module `pix/lease`, zero non-stdlib
dependencies — so no `go.sum`, nothing to add to any other module's
requirements). It is sized as **L0 foundation** (no domain knowledge, unix
syscalls + stdlib only, same tier as `sys`/`config`/`workspace`) for whoever
integrates it: move the directory to `services/host/lease/`, delete this
`go.mod`, add `"lease": layerFoundation` to `arch_test.go`'s `pkgLayer`. See
`doc.go` for the same note in the source.

## API surface delivered

| file | primitive |
| --- | --- |
| `paths.go` | `ValidateInstanceID`, `SandboxDir` — traversal/symlink-adjacent refusal via an **allowlist** charset, not a `..` denylist; `openNoFollow` — every leaf file this package touches is opened via a raw `syscall.Open` with `O_NOFOLLOW\|O_CLOEXEC` explicit at the syscall, not left to `os.OpenFile`'s implicit default (see finding below); `ensureSandboxDir` — 0700, symlink-refused, TOCTOU-rechecked after create |
| `record.go` | `CreateRecord`/`ReadRecord` — write-once (`O_EXCL` at the syscall, not just an app-level check) immutable `{InstanceID, CreatedAt, CreatedPID}`; a second create with the same ID is a no-op returning the original, a different ID is refused |
| `keep.go` | `SetKeep`/`ClearKeep`/`ReadKeep` — identity-bound mutable marker; refuses to bind/clear across a different identity; RMW guarded by a **dedicated** `keep.lock` (never the reference `lease.lock` — see finding below) |
| `lock.go` | `Open`/`AcquireShared`/`AcquireExclusive`/`TryExclusive`/`Unlock`/`Close` — the reference lock. `AcquireShared`/`AcquireExclusive` are deadline-bounded via `ctx` (flock has no native timeout, so it's a bounded poll over `LOCK_NB`). `TryExclusive` is the one-shot non-blocking **zero-holder proof** a reaper trusts. PIDs are never a correctness input anywhere in this file — `Record.CreatedPID` is the only PID this package stores, and it is documented advisory-only in three places (doc.go, record.go, and its own doc comment) |

## Findings worth flagging to the integrator

1. **`os.OpenFile` already sets `O_CLOEXEC`** (verified: `$GOROOT/src/os/file_unix.go:261` ORs `syscall.O_CLOEXEC` into every `open(2)` it makes) — so the "O_CLOEXEC lease fd" requirement is only a real hardening decision *because* this package needs `O_NOFOLLOW` too, which has no `os` package equivalent, forcing a raw `syscall.Open`. That bypass is exactly the place CLOEXEC could have been silently dropped; `openNoFollow` sets it explicitly and `TestOpenNoFollow_SetsCLOEXEC` / `TestCLOEXEC_ChildDoesNotInheritLeaseFd` pin it down with a real subprocess, not an assumption.
2. **`keep.json`'s RMW guard cannot reuse `lease.lock`.** flock conflicts are per *open file description*, not per path or per process: a process already holding the shared reference lock via one fd that then re-opened `lease.lock` for an exclusive guard via a second fd would deadlock against its own reference. `keep.lock` is a separate file solely so a keep-alive holder can also mutate its own keep state without that self-deadlock. Documented in `keep.go`'s `withKeepGuard`.
3. **PID reuse is real, not theoretical, in this design**, which is why nothing here branches on `CreatedPID`. The correctness primitive is exclusively the kernel flock (auto-released on any fd close, including `SIGKILL` — see `TestSIGKILL_ReleasesLock`).

## Tests: no mocks, real kernel + real processes

34 tests, `go vet` and `gofmt -l` clean. Full list: `go test -list '.*' ./...`
in this directory. The multi-process suite (`lock_process_test.go`) re-execs
the compiled test binary itself as a subprocess (the same pattern
`os/exec_test.go` uses for `helperCommand`/`TestHelperProcess`), dispatched by
an env var — nothing here fakes the kernel or another process:

- **`TestSIGKILL_ReleasesLock`** — spawns a real child holding the EXCLUSIVE
  lease, confirms via a real non-blocking flock probe (`TryExclusive` →
  `ErrHeld`) that it's genuinely held, `SIGKILL`s the child (zero chance to
  run its own cleanup), then confirms this process re-acquires the same lock
  — proving the *kernel*, not our code, released it.
- **`TestCLOEXEC_ChildDoesNotInheritLeaseFd`** — opens an "unprotected"
  comparison fd via raw `syscall.Open` *without* `O_CLOEXEC` alongside a real
  lease fd, execs a real child that scans its own `/proc/self/fd`, and
  asserts the unprotected fd DOES leak into the child (proving the test
  methodology is real, not vacuously true) while the lease fd does NOT.
- **`TestEightHolders_RaceThenZeroHolderProof`** — starts 8 real subprocesses
  concurrently racing for the SHARED lease, confirms all 8 hold it at once
  and that `TryExclusive` correctly refuses meanwhile, releases all 8, then
  confirms the zero-holder proof succeeds.
- **`TestAcquireExclusive_DeadlineExceededWhileHeld`** — proves the
  deadline-bounded lock API actually bounds: returns `context.DeadlineExceeded`
  within roughly its 100ms deadline while a real holder blocks it.
- Plus: shared/shared coexistence, exclusive excludes shared, traversal and
  symlink refusal (directories, leaf opens, and `SandboxDir`'s lexical
  check), 0700/0600 perms, immutable record write-once + relabel refusal,
  identity-bound keep set/clear/refuse, and a real goroutine-concurrency race
  over `SetKeep` (`-race` clean) proving exactly one identity ever wins.

```
$ gofmt -l .          # (no output — clean)
$ go vet ./...        # (no output — clean)
$ go test ./... -race -count=1 -v   # 34/34 PASS, ~10s wall (dominated by the
                                     # 8-subprocess race test under -race)
```

## LOC

```
   47 doc.go
   97 record.go
  131 paths.go
  149 lock.go
  164 keep.go
  588 total (implementation)

   99 record_test.go
  119 paths_test.go
  154 keep_test.go
  212 lock_test.go
  321 lock_process_test.go
  905 total (tests)

 1493 grand total
```

Test:code ratio ≈ 1.54:1 — weighted toward the multi-process lock suite,
which is where the real risk (kernel semantics, fd inheritance, concurrent
holders) lives.
