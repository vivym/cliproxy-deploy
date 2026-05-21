import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "restore-api-site.sh"


class RestoreApiSiteTests(unittest.TestCase):
    def test_script_exists_and_uses_strict_shell(self):
        self.assertTrue(SCRIPT.exists())
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -euo pipefail", text)

    def test_script_restores_full_runtime_without_logs(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("Usage: scripts/restore-api-site.sh BACKUP_PACKAGE", text)
        self.assertIn("mktemp -d", text)
        self.assertIn('tar -xzf "$backup_package"', text)
        self.assertIn("cliproxy-runtime.tgz", text)
        self.assertIn("newapi-postgres.dump", text)
        self.assertIn("redis-data", text)
        self.assertIn("cpa-usage-keeper-data", text)
        self.assertIn("pg_restore", text)
        self.assertIn("docker compose create redis", text)
        self.assertIn("docker compose create cpa-usage-keeper", text)
        self.assertNotIn("logs", text)

    def test_script_verifies_checksums_when_available(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("SHA256SUMS", text)
        self.assertIn("sha256sum -c SHA256SUMS", text)
        self.assertIn("shasum -a 256 -c SHA256SUMS", text)

    def test_script_rejects_missing_backup_package(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = subprocess.run(
                [str(SCRIPT), str(pathlib.Path(tmp) / "missing")],
                cwd=tmp,
                env={"PATH": "/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Backup package does not exist", result.stderr)


if __name__ == "__main__":
    unittest.main()
