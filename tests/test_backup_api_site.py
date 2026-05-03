import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "backup-api-site.sh"


class BackupApiSiteTests(unittest.TestCase):
    def write_script_copy(self, root):
        scripts = root / "scripts"
        scripts.mkdir()
        backup_script = scripts / "backup-api-site.sh"
        backup_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        backup_script.chmod(0o755)
        return backup_script

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
        self.assertIn('running_services="$(docker compose ps --services --filter status=running)"', text)

    def test_script_uses_portable_commands(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertNotIn("realpath -m", text)
        self.assertNotIn("--warning=no-file-changed", text)
        self.assertIn("tar -czf", text)
        self.assertIn("command -v sha256sum", text)
        self.assertIn("shasum -a 256", text)

    def test_script_uses_partial_dir_before_final_move(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn('partial_dest="${dest}.partial"', text)
        self.assertIn('if [[ -e "$partial_dest" || -e "$dest" ]]; then', text)
        self.assertIn('mv "$partial_dest" "$dest"', text)

    def test_script_rejects_relative_backup_dir_before_docker_calls(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            backup_script = self.write_script_copy(root)

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

    def test_script_fails_when_docker_compose_ps_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text("POSTGRES_USER=user\nPOSTGRES_DB=db\n", encoding="utf-8")
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "compose exec -T" ]]; then
  echo "postgres dump"
  exit 0
fi
if [[ "$1 $2" == "compose ps" ]]; then
  echo "compose ps failed" >&2
  exit 42
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)
            sha256sum = bin_dir / "sha256sum"
            sha256sum.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
while [[ $# -gt 0 ]]; do
  printf 'fakehash  %s\\n' "$1"
  shift
done
""",
                encoding="utf-8",
            )
            sha256sum.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(tmp_root / "external-backups"),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("compose ps failed", result.stderr)
            self.assertNotIn("Backup written to", result.stdout)


if __name__ == "__main__":
    unittest.main()
