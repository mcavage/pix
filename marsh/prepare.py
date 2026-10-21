#!/usr/bin/env python3
"""Export an explicit public native-Kit snapshot; never read personal homes."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parent.parent
HERE = ROOT / "marsh"
RUNTIME_NOTE = """> **Runtime:** this Kit uses marsh, not the legacy Pix launcher or subagent
> API. Load `marsh-orchestration` for executable delegation/composition forms.
> Historical task/chain/repair-packet outputs describe work orders only; do not
> invoke old Pix lifecycle commands or assume memory/business MCP is installed.

"""
KIT_FILES = ["pix.yaml", "Dockerfile", "entrypoint.sh"]
PACKAGE_FILES = [
    "package.json", "instructions.md", "defaults/settings.json",
    "extensions/ui.mjs", "extensions/output-style.ts", "lib/output-style.ts",
    "skills/marsh-orchestration/SKILL.md", "skills/delegation-guide/SKILL.md",
    "skills/capability-routing/SKILL.md",
]

def tracked(relative):
    raw = subprocess.check_output(["git", "ls-files", "-z", "--", relative], cwd=ROOT)
    return [ROOT / path.decode() for path in raw.split(b"\0") if path]

def copy_file(source, target):
    if source.is_symlink() or not source.is_file():
        raise ValueError(f"Public source must be a regular file: {source.relative_to(ROOT)}")
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)
    target.chmod(0o755 if source.name == "entrypoint.sh" else 0o644)

def regular_public_source(source):
    if not source.is_file() or not source.resolve().is_relative_to(ROOT):
        return False
    for part in [source, *source.parents]:
        if part == ROOT: return True
        if part.is_symlink(): return False
    return False

def export(destination):
    destination = destination.expanduser().resolve()
    if destination == ROOT or destination.is_relative_to(ROOT) or ROOT.is_relative_to(destination):
        raise ValueError("Snapshot must be outside the checkout and must not contain it")
    if destination.exists() and any(destination.iterdir()):
        raise ValueError("Destination is not empty; refusing to overwrite existing work")
    selected = json.loads((HERE / "skills.json").read_text())
    if len(selected) != len(set(selected)) or any(not name.replace("-", "").isalnum() for name in selected):
        raise ValueError("Invalid public skill selection")
    sources = []
    for name in selected:
        files = tracked(f"skills/{name}")
        if ROOT / f"skills/{name}/SKILL.md" not in files:
            raise ValueError(f"Untracked or missing public skill: {name}")
        sources.extend((source, Path("package") / source.relative_to(ROOT)) for source in files)
    for source in tracked("agents"):
        if source.suffix == ".md":
            sources.append((source, Path("package/roles") / source.name))
    for source in tracked("themes"):
        if source.suffix == ".json" and source.name != "host.json":
            sources.append((source, Path("package/themes") / source.name))
    for name in KIT_FILES:
        sources.append((HERE / "pix" / name, Path(name)))
    sources.append((HERE / "verify-public.py", Path("verify-public.py")))
    sources.append((ROOT / "LICENSE", Path("LICENSE")))
    # Exact new-package inputs, not a recursive copy. Even a local secrets.json
    # or MCP declaration in this source directory cannot enter the build context.
    overrides = [(HERE / "package" / name, Path("package") / name) for name in PACKAGE_FILES]
    # Preflight before making output, including symlinks and unreviewed files.
    for source, _ in [*sources, *overrides]:
        if not regular_public_source(source): raise ValueError("Non-regular or symlinked public input")
    destination.mkdir(parents=True, exist_ok=True)
    for source, relative in sources:
        target = destination / relative
        copy_file(source, target)
        if relative.name == "SKILL.md":
            text = target.read_text()
            end = text.find("\n---", 3)
            if not text.startswith("---\n") or end < 0: raise ValueError("Malformed skill frontmatter")
            offset = text.find("\n", end + 1) + 1
            target.write_text(text[:offset] + "\n" + RUNTIME_NOTE + text[offset:].lstrip("\n"))
    for source, relative in overrides: copy_file(source, destination / relative)
    # BuildKit preserves source directory modes. The nonroot workload must be
    # able to traverse the public package even if preparation used umask 077.
    for directory in destination.rglob("*"):
        if directory.is_dir(): directory.chmod(0o755)
    inventory = {str(file.relative_to(destination)): hashlib.sha256(file.read_bytes()).hexdigest()
                 for file in sorted(destination.rglob("*")) if file.is_file()}
    (destination / "PUBLIC-SOURCES.json").write_text(json.dumps({
        "source_commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "files_sha256": inventory,
        "note": "Public input inventory, not an image signature or authority receipt",
    }, indent=2) + "\n")
    return destination

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    try: result = export(args.destination)
    except (ValueError, OSError, subprocess.CalledProcessError) as error: parser.exit(2, f"prepare: {error}\n")
    print(f"Prepared public native Kit source: {result}")

if __name__ == "__main__": main()
