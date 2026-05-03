import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "backup-api-site.sh"


class BackupApiSiteTests(unittest.TestCase):
    def test_script_uses_strict_shell(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -euo pipefail", text)

    def test_script_refuses_repo_backup_dir(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("Refusing to write backups inside repository", text)
        self.assertIn("BACKUP_DIR must be an absolute path", text)
        self.assertIn("realpath", text)

    def test_script_backs_up_postgres_and_cliproxy_state(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -a", text)
        self.assertIn("source .env", text)
        self.assertIn("Missing required backup source", text)
        self.assertNotIn("2>/dev/null || true", text)
        self.assertIn("docker compose exec -T postgres pg_dump", text)
        self.assertIn("auths", text)
        self.assertIn("config.yaml", text)
        self.assertIn("cpa-usage-keeper", text)

    def test_script_rejects_relative_backup_dir_before_docker_calls(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            scripts = root / "scripts"
            scripts.mkdir()
            backup_script = scripts / "backup-api-site.sh"
            backup_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
            backup_script.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={"BACKUP_DIR": "backups", "PATH": "/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("BACKUP_DIR must be an absolute path", result.stderr)


if __name__ == "__main__":
    unittest.main()
