#!/usr/bin/env python3
"""Verify this image's added public payload; not a legal grant or signature."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def regular(root, relative):
    path = root / relative
    for component in [path, *path.parents]:
        if component == root: break
        if component.is_symlink(): raise ValueError(f"symlink in public payload: {relative}")
    if not path.is_file(): raise ValueError(f"missing regular public file: {relative}")
    return path

def verify(root):
    root = root.resolve()
    license_root = "usr/local/share/licenses/pix"
    inventory_file = regular(root, license_root + "/inputs.json")
    if inventory_file.stat().st_size > 4 * 1024 * 1024: raise ValueError("input inventory too large")
    inventory = json.loads(inventory_file.read_text())
    entries = inventory["files_sha256"]
    if not isinstance(entries, dict) or len(entries) > 10000: raise ValueError("invalid input inventory")
    expected = {}
    for name, value in entries.items():
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts or not re.fullmatch(r"[a-f0-9]{64}", value):
            raise ValueError("invalid public input record")
        if name.startswith("package/"): expected[name[len("package/"):]] = value
    if not expected: raise ValueError("empty public package")
    package = root / "opt/pix/package"
    if package.is_symlink() or not package.is_dir() or package.stat().st_mode & 0o555 != 0o555:
        raise ValueError("public package root is not a nonroot-traversable directory")
    actual = {}
    for path in package.rglob("*"):
        relative = path.relative_to(package).as_posix()
        if path.is_symlink(): raise ValueError("symlink in canonical public package")
        if path.is_dir():
            if path.stat().st_mode & 0o555 != 0o555: raise ValueError("nonroot cannot traverse public directory")
        elif path.is_file():
            if path.stat().st_mode & 0o444 != 0o444: raise ValueError("nonroot cannot read public resource")
            actual[relative] = digest(path)
        else: raise ValueError("special file in public package")
    if actual != expected: raise ValueError("public package differs from selected source inventory")
    installed = {
        "LICENSE": license_root + "/LICENSE",
        "verify-public.py": license_root + "/verify-public.py",
        "entrypoint.sh": "usr/local/bin/marsh-pix",
    }
    for source, target in installed.items():
        if digest(regular(root, target)) != entries[source]: raise ValueError(f"changed installed {source}")
    if not (regular(root, "usr/local/bin/marsh-pix").stat().st_mode & 0o111):
        raise ValueError("entrypoint is not executable")
    base = json.loads(regular(root, license_root + "/base-check/image-verification.json").read_text())
    if base.get("adapter_scope", {}).get("root") != "/opt/marsh/pi-gateway":
        raise ValueError("missing actual-image Pi/base notice correspondence check")
    return {
        "schema": "pix.public-payload-verification/v1",
        "source_commit": inventory["source_commit"],
        "package_files": len(actual),
        "package_sha256": hashlib.sha256(json.dumps(actual, sort_keys=True).encode()).hexdigest(),
        "base_receipt_sha256": digest(root / license_root / "base-check/image-verification.json"),
        "qualification": "Added source payload and installed notices checked; not a legal grant, registry signature, authenticated-provider or marsh execution proof",
        "image_identity_boundary": "Host publisher must bind this receipt to the exact image digest",
    }

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("/"))
    parser.add_argument("--receipt", type=Path)
    args = parser.parse_args()
    try: result = verify(args.root)
    except (ValueError, OSError, KeyError, TypeError) as error: parser.exit(1, f"public verification: {error}\n")
    if args.receipt: args.receipt.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, sort_keys=True))

if __name__ == "__main__": main()
