import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "restore-api-site.sh"


class RestoreApiSiteTests(unittest.TestCase):
    def write_script_copy(self, root):
        scripts = root / "scripts"
        scripts.mkdir()
        restore_script = scripts / "restore-api-site.sh"
        restore_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        restore_script.chmod(0o755)
        return restore_script

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
        self.assertIn("clear_volume_dir postgres /var/lib/postgresql/data", text)
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

    def test_script_replaces_runtime_paths_without_touching_logs(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)

            (root / ".env").write_text(
                "POSTGRES_USER=old\nPOSTGRES_DB=old\nREDIS_PASSWORD=old\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("old: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "auths" / "stale.json").write_text("stale\n", encoding="utf-8")
            (root / "letsencrypt").mkdir()
            (root / "letsencrypt" / "stale.pem").write_text("stale\n", encoding="utf-8")
            (root / "logs").mkdir()
            (root / "logs" / "request.log").write_text("keep logs\n", encoding="utf-8")

            package_src = tmp_root / "backup-src"
            package_src.mkdir()
            runtime_src = tmp_root / "runtime-src"
            runtime_src.mkdir()
            (runtime_src / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (runtime_src / "config.yaml").write_text("new: true\n", encoding="utf-8")
            (runtime_src / "auths").mkdir()
            (runtime_src / "auths" / "current.json").write_text("current\n", encoding="utf-8")
            (runtime_src / "letsencrypt").mkdir()
            (runtime_src / "letsencrypt" / "acme.json").write_text("{}", encoding="utf-8")
            subprocess.run(
                ["tar", "-czf", str(package_src / "cliproxy-runtime.tgz"), "."],
                cwd=runtime_src,
                check=True,
            )
            (package_src / "newapi-postgres.dump").write_text("dump\n", encoding="utf-8")
            (package_src / "redis-data").mkdir()
            (package_src / "redis-data" / "dump.rdb").write_text("redis\n", encoding="utf-8")
            backup_package = tmp_root / "backup.tgz"
            subprocess.run(["tar", "-czf", str(backup_package), "."], cwd=package_src, check=True)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker_template = """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2" == "compose --env-file" && "${4:-}" == "down" ]]; then
  if [[ ! -f "__STALE_AUTH__" ]]; then
    echo "runtime files were replaced before compose down" >&2
    exit 43
  fi
  exit 0
fi
if [[ "$1 $2 ${3:-}" == "compose create postgres" ]]; then
  exit 0
fi
if [[ "$1 $2 ${3:-}" == "compose create redis" ]]; then
  exit 0
fi
if [[ "$1 $2 ${3:-}" == "compose ps -aq" ]]; then
  echo "$4-container"
  exit 0
fi
if [[ "$1" == "inspect" ]]; then
  case "${@: -1}" in
    postgres-container) echo "postgres-data-volume" ;;
    redis-container) echo "redis-data-volume" ;;
    cpa-usage-keeper-container) echo "keeper-data-volume" ;;
  esac
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1 $2 ${3:-} ${4:-}" == "compose up -d postgres" ]]; then
  exit 0
fi
if [[ "$1 $2 ${3:-}" == "compose exec -T" ]]; then
  exit 0
fi
if [[ "$1 $2 ${3:-}" == "compose up -d" ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
"""
            docker.write_text(
                docker_template.replace("__STALE_AUTH__", str(root / "auths" / "stale.json")),
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(backup_package)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / ".env").read_text(encoding="utf-8").splitlines()[0], "POSTGRES_USER=user")
            self.assertEqual((root / "config.yaml").read_text(encoding="utf-8"), "new: true\n")
            self.assertTrue((root / "auths" / "current.json").exists())
            self.assertFalse((root / "auths" / "stale.json").exists())
            self.assertTrue((root / "letsencrypt" / "acme.json").exists())
            self.assertFalse((root / "letsencrypt" / "stale.pem").exists())
            self.assertEqual((root / "logs" / "request.log").read_text(encoding="utf-8"), "keep logs\n")


if __name__ == "__main__":
    unittest.main()
