#!/usr/bin/env python3
"""Resolve marsh's Pi image into the Kit's build arguments, and decide whether
the published marsh-pix is out of date.

The Kit builds FROM marsh's packaged Pi image. CI resolves its moving tag to a
digest once, builds from exactly that digest, and records it in the image's
org.opencontainers.image.base.name label. The Kit version is the Pi version
plus a hash of the exported public inputs, so the same inputs always give the
same version and a rebuild for a new base alone switches users silently.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys

BASE_REPOSITORY = "docker.io/mcavage/marsh-pi"
BASE_TAG = BASE_REPOSITORY + ":latest"
PUBLISHED_TAG = "docker.io/mcavage/marsh-pix:stable"
CONTRACT_LABEL = "org.marsh.kit.contract"
BASE_LABEL = "org.opencontainers.image.base.name"
VERSION_ANNOTATION = "org.opencontainers.image.version"
DESCRIPTOR_ANNOTATION = "vnd.docker.sandbox.kit.descriptor"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
UNSTAMPED = "0.0.0-local"
VERSION_LINE = re.compile(r'^version: .*$', re.M)
RELEASE = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
SUPPORTED = re.compile(
    r"^pub const SUPPORTED_KIT_CONTRACTS: RangeInclusive<u32> = (\d+)\.\.=(\d+);$", re.M)


def imagetools(*arguments):
    result = subprocess.run(["docker", "buildx", "imagetools", "inspect", *arguments],
                            capture_output=True, text=True, timeout=300)
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or "docker buildx imagetools inspect failed")
    return result.stdout


def repository(reference):
    if "@" in reference:
        return reference.rsplit("@", 1)[0]
    head, _, last = reference.rpartition("/")
    return f"{head}/{last.split(':', 1)[0]}" if head else last.split(":", 1)[0]


def arm64_manifest(reference):
    top = json.loads(imagetools("--raw", reference))
    if "manifests" not in top:
        return top
    for entry in top["manifests"]:
        platform = entry.get("platform", {})
        if platform.get("os") == "linux" and platform.get("architecture") == "arm64":
            return json.loads(imagetools("--raw", f"{repository(reference)}@{entry['digest']}"))
    raise ValueError(f"{reference} has no linux/arm64 manifest")


def labels(reference):
    image = json.loads(imagetools(reference, "--format", "{{json .Image}}"))
    if "config" not in image:
        image = image.get("linux/arm64", {})
    return (image.get("config") or {}).get("Labels") or {}


def provided(manifest):
    """name -> version for the descriptor's non-package `provides` entries."""
    raw = (manifest.get("annotations") or {}).get(DESCRIPTOR_ANNOTATION)
    if not raw:
        raise ValueError("the image has no Kit descriptor")
    result = {}
    for entry in json.loads(raw).get("provides", []):
        name, _, version = str(entry).rpartition("@")
        if name and "/" not in name:
            result[name] = version
    return result


def contract(values):
    value = str(values.get(CONTRACT_LABEL, "")).strip()
    return int(value) if value.isdigit() else None


def supported_range(source):
    match = SUPPORTED.search(source)
    if not match:
        raise ValueError("kit_source.rs declares no supported Kit contract range")
    return int(match.group(1)), int(match.group(2))


def check_contract(value, accepted):
    if accepted and (value is None or not accepted[0] <= value <= accepted[1]):
        raise ValueError(f"Kit contract {value} is outside what stable marsh supports "
                         f"({accepted[0]}-{accepted[1]})")


def resolve_base(reference=BASE_TAG):
    """The Pi image `reference` names now, pinned by digest."""
    if repository(reference) != BASE_REPOSITORY:
        raise ValueError(f"the Pi base must be {BASE_REPOSITORY}, not {reference}")
    digest = json.loads(imagetools(reference, "--format", "{{json .Manifest}}")).get("digest", "")
    if not DIGEST.fullmatch(digest):
        raise ValueError(f"could not resolve {reference} to a digest")
    pinned = f"{BASE_REPOSITORY}@{digest}"
    versions = provided(arm64_manifest(pinned))
    for name in ("pi", "pi-acp"):
        if not RELEASE.fullmatch(versions.get(name, "")):
            raise ValueError(f"{pinned} does not provide a released {name} version")
    return {"base": pinned, "pi": versions["pi"], "piAcp": versions["pi-acp"],
            "contract": contract(labels(pinned))}


def _version_line(text, version):
    stamped, count = VERSION_LINE.subn(f'version: "{version}"', text, count=1)
    if count != 1:
        raise ValueError("pix.yaml has no top-level version line")
    return stamped


def kit_version(snapshot, pi):
    """`<pi>-pix.<hash of the exported public inputs>`.

    The commit and an already stamped version line are not hashed, so the
    same inputs always give the same version.
    """
    snapshot = Path(snapshot)
    inventory = dict(json.loads((snapshot / "PUBLIC-SOURCES.json").read_text())["files_sha256"])
    descriptor = _version_line((snapshot / "pix.yaml").read_text(), UNSTAMPED)
    inventory["pix.yaml"] = hashlib.sha256(descriptor.encode()).hexdigest()
    encoded = json.dumps(inventory, sort_keys=True, separators=(",", ":")).encode()
    return f"{pi}-pix.{hashlib.sha256(encoded).hexdigest()[:8]}"


def build_args(base):
    """Descriptor arguments (pix.yaml `args`) for building on `base`."""
    return {"base": base["base"], "pi": base["pi"], "piAcp": base["piAcp"]}


def stamp(snapshot, base):
    """Write the Kit version into a prepared snapshot; return (args, version)."""
    snapshot = Path(snapshot)
    version = kit_version(snapshot, base["pi"])
    descriptor = snapshot / "pix.yaml"
    descriptor.write_text(_version_line(descriptor.read_text(), version))
    inventory_file = snapshot / "PUBLIC-SOURCES.json"
    inventory = json.loads(inventory_file.read_text())
    inventory["files_sha256"]["pix.yaml"] = hashlib.sha256(descriptor.read_bytes()).hexdigest()
    inventory_file.write_text(json.dumps(inventory, indent=2) + "\n")
    return build_args(base), version


def published(reference):
    """The version and base of a published Kit; None when the tag does not exist."""
    try:
        manifest = arm64_manifest(reference)
    except RuntimeError as error:
        if "not found" in str(error).lower():
            return None
        raise
    return {"version": (manifest.get("annotations") or {}).get(VERSION_ANNOTATION),
            "base": labels(reference).get(BASE_LABEL)}


def decide(arguments, version, current):
    """(publish?, reason) for a build of `version` with `arguments`."""
    if current is None:
        return True, "nothing is published yet"
    if current.get("version") != version:
        return True, f"Kit version {current.get('version')} -> {version}"
    if current.get("base") != arguments["base"]:
        return True, f"Pi image moved to {arguments['base']}"
    return False, f"{version} on {arguments['base']} is already published"


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    commands = parser.add_subparsers(dest="command", required=True)
    plan = commands.add_parser("plan", help="print build arguments and whether to publish, as JSON")
    plan.add_argument("snapshot", type=Path, help="a directory prepared by marsh/prepare.py")
    plan.add_argument("--base", default=BASE_TAG)
    plan.add_argument("--current", default=PUBLISHED_TAG)
    plan.add_argument("--supported-source", type=Path, help="stable marsh's kit_source.rs")
    flags = commands.add_parser("build-args",
                                help="stamp the version into a snapshot; print its --build-arg flags")
    flags.add_argument("snapshot", type=Path)
    flags.add_argument("--base", default=BASE_TAG)
    verify = commands.add_parser("verify", help="check a published image before moving a tag to it")
    verify.add_argument("reference")
    verify.add_argument("--version", required=True)
    verify.add_argument("--base", required=True)
    verify.add_argument("--supported-source", type=Path)
    args = parser.parse_args()
    try:
        accepted = (supported_range(args.supported_source.read_text())
                    if getattr(args, "supported_source", None) else None)
        if args.command == "build-args":
            arguments, _ = stamp(args.snapshot, resolve_base(args.base))
            for key, value in arguments.items():
                print(f"--build-arg={key}={value}")
        elif args.command == "plan":
            base = resolve_base(args.base)
            check_contract(base["contract"], accepted)
            arguments, version = build_args(base), kit_version(args.snapshot, base["pi"])
            publish, reason = decide(arguments, version, published(args.current))
            print(json.dumps({"args": arguments, "version": version, "contract": base["contract"],
                              "publish": publish, "reason": reason}, indent=2))
        else:
            if "@" not in args.reference or not DIGEST.fullmatch(args.reference.rsplit("@", 1)[1]):
                raise ValueError("verify needs an image reference by digest")
            found = labels(args.reference)
            check_contract(contract(found), accepted)
            version = (arm64_manifest(args.reference).get("annotations") or {}).get(VERSION_ANNOTATION)
            if version != args.version:
                raise ValueError(f"published version is {version}, expected {args.version}")
            if found.get(BASE_LABEL) != args.base:
                raise ValueError(f"published base is {found.get(BASE_LABEL)}, expected {args.base}")
            print(f"{args.reference}: version {version}, contract {contract(found)}, base {args.base}")
    except (RuntimeError, ValueError, OSError, KeyError, json.JSONDecodeError) as error:
        parser.exit(1, f"kit_inputs: {error}\n")


if __name__ == "__main__":
    sys.exit(main())
