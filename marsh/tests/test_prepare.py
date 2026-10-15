import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[2]

class Preparation(unittest.TestCase):
    def prepare(self, target):
        return subprocess.run(["python3", str(ROOT / "marsh/prepare.py"), str(target)],
                              cwd=ROOT, text=True, capture_output=True)

    def test_export_contains_public_resources_not_local_files(self):
        # Fake non-secret canaries, never real credentials. Probe untracked
        # files both beside a selected public skill and in the new Kit source.
        canaries = [ROOT / "skills/analyze" / ("private-" + uuid.uuid4().hex + ".md"),
                    ROOT / "marsh/pix/local-mcp.json",
                    ROOT / "marsh/package/local-secrets.json"]
        for canary in canaries:
            self.assertFalse(canary.exists())
            canary.write_text("PUBLIC_BOUNDARY_TEST_CANARY")
        try:
            with tempfile.TemporaryDirectory(prefix="pix-public-") as temp:
                target = Path(temp) / "kit"
                result = self.prepare(target)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(list((target / "package/skills").glob("*/SKILL.md"))), 37)
                self.assertEqual(len(list((target / "package/roles").glob("*.md"))), 19)
                for excluded in ["environments", "healthcheck", "ingest", "onboarding", "pix-dev", "promote"]:
                    self.assertFalse((target / "package/skills" / excluded).exists())
                self.assertFalse((target / "package/themes/host.json").exists())
                all_bytes = b"\n".join(p.read_bytes() for p in target.rglob("*") if p.is_file())
                self.assertNotIn(b"PUBLIC_BOUNDARY_TEST_CANARY", all_bytes)
                for marker in [b"/Users/", b"op://", b"host-state.json", b"secrets.env", b"gw.docker.com"]:
                    self.assertNotIn(marker, all_bytes)
                inventory = json.loads((target / "PUBLIC-SOURCES.json").read_text())
                self.assertIn("package/extensions/ui.mjs", inventory["files_sha256"])
                self.assertIn("marsh-orchestration", (target / "package/skills/deliver/SKILL.md").read_text())
        finally:
            for canary in canaries: canary.unlink()

    def test_existing_destination_is_preserved(self):
        with tempfile.TemporaryDirectory(prefix="pix-public-") as temp:
            target = Path(temp) / "kit"
            target.mkdir()
            file = target / "keep.txt"
            file.write_text("existing work")
            result = self.prepare(target)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(file.read_text(), "existing work")
            self.assertEqual(list(target.iterdir()), [file])

    def test_checkout_destination_is_refused(self):
        result = self.prepare(ROOT / "marsh/test-output")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((ROOT / "marsh/test-output").exists())

if __name__ == "__main__": unittest.main()
