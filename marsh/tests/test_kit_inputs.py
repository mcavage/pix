import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("pix_kit_inputs", ROOT / "marsh/kit_inputs.py")
kit_inputs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(kit_inputs)

DIGEST = "sha256:" + "a" * 64
BASE = {"base": "docker.io/mcavage/marsh-pi@" + DIGEST, "pi": "1.3.0", "piAcp": "0.0.34", "contract": 1}


class KitInputs(unittest.TestCase):
    def snapshot(self, temp):
        target = Path(temp) / "kit"
        subprocess.run(["python3", str(ROOT / "marsh/prepare.py"), str(target)],
                       cwd=ROOT, check=True, capture_output=True)
        return target

    def test_version_is_stable_and_stamping_is_idempotent(self):
        with tempfile.TemporaryDirectory(prefix="pix-version-") as temp:
            kit = self.snapshot(temp)
            version = kit_inputs.kit_version(kit, "1.3.0")
            self.assertRegex(version, r"^1\.3\.0-pix\.[0-9a-f]{8}$")
            arguments, stamped = kit_inputs.stamp(kit, BASE)
            self.assertEqual(stamped, version)
            self.assertEqual(arguments, {"base": BASE["base"], "pi": "1.3.0", "piAcp": "0.0.34"})
            self.assertIn(f'\nversion: "{version}"\n', (kit / "pix.yaml").read_text())
            # The inventory the image verifies matches the stamped descriptor,
            # and a stamped snapshot still yields the same version.
            inventory = json.loads((kit / "PUBLIC-SOURCES.json").read_text())["files_sha256"]
            self.assertEqual(inventory["pix.yaml"], hashlib.sha256((kit / "pix.yaml").read_bytes()).hexdigest())
            self.assertEqual(kit_inputs.kit_version(kit, "1.3.0"), version)
            self.assertEqual(kit_inputs.kit_version(kit, "1.4.0"), version.replace("1.3.0", "1.4.0"))

    def test_version_follows_public_inputs(self):
        with tempfile.TemporaryDirectory(prefix="pix-version-") as temp:
            kit = self.snapshot(temp)
            before = kit_inputs.kit_version(kit, "1.3.0")
            inventory_file = kit / "PUBLIC-SOURCES.json"
            inventory = json.loads(inventory_file.read_text())
            inventory["source_commit"] = "0" * 40
            inventory_file.write_text(json.dumps(inventory))
            self.assertEqual(kit_inputs.kit_version(kit, "1.3.0"), before, "the commit is not an input")
            inventory["files_sha256"]["package/instructions.md"] = "0" * 64
            inventory_file.write_text(json.dumps(inventory))
            self.assertNotEqual(kit_inputs.kit_version(kit, "1.3.0"), before)

    def test_decide(self):
        arguments = kit_inputs.build_args(BASE)
        version = "1.3.0-pix.0123abcd"
        self.assertTrue(kit_inputs.decide(arguments, version, None)[0])
        same = {"version": version, "base": BASE["base"]}
        self.assertFalse(kit_inputs.decide(arguments, version, same)[0])
        self.assertTrue(kit_inputs.decide(arguments, version, dict(same, version="1.3.0-pix.ffffffff"))[0])
        self.assertTrue(kit_inputs.decide(arguments, version, dict(same, base="docker.io/mcavage/marsh-pi@sha256:" + "b" * 64))[0])
        self.assertTrue(kit_inputs.decide(arguments, version, {"version": None, "base": None})[0])

    def test_contract_range(self):
        source = "pub const SUPPORTED_KIT_CONTRACTS: RangeInclusive<u32> = 1..=2;\n"
        accepted = kit_inputs.supported_range(source)
        self.assertEqual(accepted, (1, 2))
        kit_inputs.check_contract(2, accepted)
        for value in (None, 0, 3):
            with self.assertRaises(ValueError):
                kit_inputs.check_contract(value, accepted)
        with self.assertRaises(ValueError):
            kit_inputs.supported_range("no range here")

    def test_resolve_base_reads_the_pinned_image(self):
        descriptor = json.dumps({"provides": ["pi@1.4.0", "pi-acp@0.0.35", "deb/git@2.47.3"]})
        index = {"manifests": [{"digest": "sha256:" + "c" * 64, "platform": {"os": "linux", "architecture": "arm64"}}]}
        def inspect(*arguments):
            if arguments[-2:] == ("--format", "{{json .Manifest}}"):
                return json.dumps({"digest": DIGEST})
            if arguments[-2:] == ("--format", "{{json .Image}}"):
                self.assertEqual(arguments[0], BASE["base"])
                return json.dumps({"config": {"Labels": {"org.marsh.kit.contract": "1"}}})
            if arguments[1].endswith(DIGEST):
                return json.dumps(index)
            return json.dumps({"annotations": {kit_inputs.DESCRIPTOR_ANNOTATION: descriptor}})
        with mock.patch.object(kit_inputs, "imagetools", side_effect=inspect):
            self.assertEqual(kit_inputs.resolve_base(),
                             {"base": BASE["base"], "pi": "1.4.0", "piAcp": "0.0.35", "contract": 1})
            with self.assertRaises(ValueError):
                kit_inputs.resolve_base("docker.io/someone/else:latest")


if __name__ == "__main__":
    unittest.main()
