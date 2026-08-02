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

2. LITERALS  `shellEnv{run: f, quiet: true}` ->
   `shellEnv{System: &systest.Fake{RunFn: f}, quiet: true}`
   Brace/paren/string-aware, because the 205 fixtures contain multi-line
   closures that a comma-splitting regex would cut in half.
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


def split_top_level(body: str):
    """Split a composite-literal body on commas at depth 0, respecting
    (), {}, [], "" , `` and ''."""
    parts, depth, i, start = [], 0, 0, 0
    while i < len(body):
        c = body[i]
        if c in '"`\'':
            q, i = c, i + 1
            while i < len(body):
                if body[i] == "\\" and q != "`":
                    i += 2
                    continue
                if body[i] == q:
                    break
                i += 1
        elif c in "({[":
            depth += 1
        elif c in ")}]":
            depth -= 1
        elif c == "," and depth == 0:
            parts.append(body[start:i])
            start = i + 1
        i += 1
    parts.append(body[start:])
    return [p for p in parts if p.strip()]


def match_brace(text: str, open_idx: int) -> int:
    """Index of the '}' matching the '{' at open_idx."""
    depth, i = 0, open_idx
    while i < len(text):
        c = text[i]
        if c in '"`\'':
            q, i = c, i + 1
            while i < len(text):
                if text[i] == "\\" and q != "`":
                    i += 2
                    continue
                if text[i] == q:
                    break
                i += 1
        elif c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    raise ValueError("unbalanced brace")


def rewrite_literals(text: str) -> tuple[str, int]:
    n, out, pos = 0, [], 0
    for m in re.finditer(r"\bshellEnv\{", text):
        if m.start() < pos:
            continue
        open_idx = m.end() - 1
        close = match_brace(text, open_idx)
        body = text[open_idx + 1:close]
        if not body.strip():
            out.append(text[pos:m.start()])
            out.append("shellEnv{System: &systest.Fake{}}")
            pos = close + 1
            n += 1
            continue
        sysparts, keepparts = [], []
        ok = True
        for part in split_top_level(body):
            km = re.match(r"\s*([A-Za-z_][A-Za-z0-9_]*)\s*:(.*)", part, re.S)
            if not km:
                ok = False  # positional literal: leave it for a human
                break
            key, val = km.group(1), km.group(2)
            lead = part[:len(part) - len(part.lstrip())]
            if key in SYS_FIELDS:
                sysparts.append(f"{lead}{SYS_FIELDS[key][1]}:{val}")
            elif key in KEEP:
                keepparts.append(part)
            else:
                ok = False
                break
        if not ok:
            continue
        out.append(text[pos:m.start()])
        inner = ",".join(sysparts)
        if sysparts:
            inner += ","
        rendered = "shellEnv{System: &systest.Fake{" + inner + "}"
        if keepparts:
            rendered += "," + ",".join(keepparts)
        rendered += "}"
        out.append(rendered)
        pos = close + 1
        n += 1
    out.append(text[pos:])
    return "".join(out), n


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
        src, a = rewrite_literals(src)
        src, b = rewrite_uses(src)
        if src != original:
            p.write_text(src)
            files += 1
        lits += a
        uses += b
    print(f"{files} files: {lits} literals, {uses} uses")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
