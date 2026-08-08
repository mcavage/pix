#!/usr/bin/env python3
"""Mechanically migrate `shellEnv`'s 22 nullable func fields onto sys.System.

Run once, from the repo root, then let `go build ./...` and the 1,501 tests be
the proof. This exists as a committed script rather than a hand-edit because the
change touches ~570 sites across 91 files: a human doing that by eye will make
exactly the kind of silent, local mistake the refactor is meant to eliminate.

Two transforms:

1. USES  `x.run(...)` -> `x.Run(...)` for the 16 fields that moved to sys.
   Textual, but bounded: the compiler rejects a rename applied to the wrong
   receiver, so a false positive is a build error, never a behaviour change.
   `.probe` is excluded from the bare-value form because `fakeProber.probe` is a
   genuine method value that tests pass around.

2. ASSIGNS  `env.readFile = fn` -> `env.fake().ReadFileFn = fn`
   You cannot assign to a promoted interface method, and 116 fixtures override
   one seam after building a base env. `fake()` is a test-only accessor that
   type-asserts the embedded System back to *systest.Fake; on a real env it
   panics, which is the correct outcome for test-only code reached in
   production. All 116 sites are in _test.go files (verified before writing
   this).

Composite literals are NOT handled here. A hand-rolled brace matcher was tried
and is the wrong instrument — Go source has braces inside line comments, rune
literals ('{') and raw strings, and a scanner that does not know Go grammar
mis-pairs them. That job moved to scripts/migrate-shellenv/, which uses
go/parser to locate each literal and then edits at byte offsets.
"""
import re
import sys
from pathlib import Path

# shellEnv field -> (sys.System method, systest.Fake field)
SYS_FIELDS = {
    "run": ("Run", "RunFn"),
    "lookPath": ("LookPath", "LookPathFn"),
    "probe": ("RunTimed", "RunTimedFn"),
    "runInteractive": ("RunInteractive", "RunInteractiveFn"),
    "runInteractiveQuiet": ("RunInteractiveQuiet", "RunInteractiveQuietFn"),
    "readFile": ("ReadFile", "ReadFileFn"),
    "writeFile": ("WriteFile", "WriteFileFn"),
    "statFile": ("IsFile", "IsFileFn"),
    "fileMode": ("Mode", "ModeFn"),
    "flock": ("Lock", "LockFn"),
    "getenv": ("Getenv", "GetenvFn"),
    "homeDir": ("HomeDir", "HomeDirFn"),
    "getwd": ("Getwd", "GetwdFn"),
    "stateDir": ("StateDir", "StateDirFn"),
    "executable": ("Executable", "ExecutableFn"),
    "dial": ("DialLocal", "DialLocalFn"),
}
# Fields that stay on shellEnv: domain probes and flags that are not OS seams.
KEEP = {"quiet", "hostBinary", "identityProbe", "slackAuthTest",
        "directInferenceProbe", "ollamaInferenceProbe"}


def rewrite_assigns(text: str) -> tuple[str, int]:
    """`x.field = v` -> `x.fake().FieldFn = v`, and the read half of the
    read-then-wrap idiom (`orig := x.field`) alongside it."""
    n = 0
    for field, (_, fake) in SYS_FIELDS.items():
        pat = re.compile(r"(?<![A-Za-z0-9_.])([A-Za-z_][A-Za-z0-9_]*)\." + field + r"(\s*=[^=])")
        text, k = pat.subn(lambda m: f"{m.group(1)}.fake().{fake}{m.group(2)}", text)
        n += k
        # read half: `orig := env.field` / `run := env.run`
        pat = re.compile(r"(:?=\s*)([A-Za-z_][A-Za-z0-9_]*)\." + field + r"\b(?!\s*[(:=])")
        text, k = pat.subn(lambda m: f"{m.group(1)}{m.group(2)}.fake().{fake}", text)
        n += k
    return text, n


def rewrite_uses(text: str) -> tuple[str, int]:
    n = 0
    for field, (method, _) in SYS_FIELDS.items():
        # Call form: x.field(  ->  x.Method(
        pat = re.compile(r"(?<![A-Za-z0-9_.])([A-Za-z_][A-Za-z0-9_]*)\." + field + r"\(")
        text, k = pat.subn(lambda m: f"{m.group(1)}.{method}(", text)
        n += k
        if field != "probe":
            # Bare value form: x.field  (nil checks, assignments). `.probe` is
            # excluded: fakeProber.probe is a real method value in fixtures.
            pat = re.compile(r"(?<![A-Za-z0-9_.])([A-Za-z_][A-Za-z0-9_]*)\." + field + r"\b(?!\s*:)")
            text, k = pat.subn(lambda m: f"{m.group(1)}.{method}", text)
            n += k
    return text, n


def main() -> int:
    root = Path("services/host/cmd/pix")
    if not root.is_dir():
        print("run me from the repo root", file=sys.stderr)
        return 1
    lits = uses = files = 0
    for p in sorted(root.glob("*.go")):
        src = original = p.read_text()
        a = 0
        src, c = rewrite_assigns(src)
        src, b = rewrite_uses(src)
        uses += c
        if src != original:
            p.write_text(src)
            files += 1
        lits += a
        uses += b
    print(f"{files} files: {lits} literals, {uses} uses")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
