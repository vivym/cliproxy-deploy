import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "backup-deployment.sh"
DOTENV_READER = ROOT / "scripts" / "read-dotenv.py"


class BackupDeploymentTests(unittest.TestCase):
    def write_script_copy(self, root):
        scripts = root / "scripts"
        scripts.mkdir()
        backup_script = scripts / "backup-deployment.sh"
        backup_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        backup_script.chmod(0o755)
        (scripts / "read-dotenv.py").write_text(
            DOTENV_READER.read_text(encoding="utf-8"), encoding="utf-8"
        )
        return backup_script

    def prepare_runtime(self, root):
        (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
        (root / "docker-compose.newapi.yml").write_text("services: {}\n", encoding="utf-8")
        (root / ".env").write_text(
            "\n".join(
                [
                    "POSTGRES_USER=sub2api",
                    "POSTGRES_DB=sub2api",
                    "REDIS_PASSWORD=sub2api-redis",
                    "NEWAPI_POSTGRES_USER=newapi",
                    "NEWAPI_POSTGRES_DB=newapi",
                    "NEWAPI_REDIS_PASSWORD=newapi-redis",
                ]
            )
            + "\n",
            encoding="utf-8",
        )
        for directory in ["data", "postgres_data", "redis_data", "letsencrypt"]:
            (root / directory).mkdir()
        (root / "data" / "config.yaml").write_text("runtime: true\n", encoding="utf-8")

    def test_backup_covers_both_application_state_domains(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("set -euo pipefail", text)
        self.assertIn(
            "Usage: scripts/backup-deployment.sh [DEPLOYMENT_DIR]",
            text,
        )
        self.assertIn('deployment_dir="${1:-${script_repo_root}}"', text)
        self.assertIn("sub2api-postgres.dump", text)
        self.assertIn("newapi-postgres.dump", text)
        self.assertIn("sub2api-redis-data", text)
        self.assertIn("redis-data", text)
        self.assertIn("deployment-runtime.tgz", text)
        self.assertIn("SHA256SUMS", text)
        for service in ["traefik", "sub2api", "new-api"]:
            self.assertIn(f"stop_running_service {service}", text)
        self.assertLess(
            text.index("compose exec -T redis redis-cli SAVE"),
            text.index("stop_running_service redis"),
        )
        self.assertLess(
            text.index("stop_running_service redis"),
            text.index("compose cp redis:/data"),
        )
        self.assertLess(
            text.index("compose exec -T newapi-redis redis-cli"),
            text.index("stop_running_service newapi-redis"),
        )
        self.assertLess(
            text.index("stop_running_service newapi-redis"),
            text.index("compose cp newapi-redis:/data"),
        )
        for legacy_term in ["cliproxyapi", "cpa-usage-keeper", "config.yaml", "auths"]:
            self.assertNotIn(legacy_term, text)

    def test_backup_package_is_complete_and_write_services_are_quiesced(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            deployment = root / "sub2api"
            deployment.mkdir()
            self.prepare_runtime(deployment)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  "compose -f docker-compose.yml -f docker-compose.newapi.yml ps --services --filter status=running")
    printf '%s\\n' traefik new-api sub2api redis newapi-redis
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml stop "*|\
  "compose -f docker-compose.yml -f docker-compose.newapi.yml start "*)
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml exec -T postgres pg_dump "*)
    printf 'sub2api postgres dump\\n'
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml exec -T newapi-postgres pg_dump "*)
    printf 'newapi postgres dump\\n'
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml exec -T redis redis-cli SAVE"|\
  "compose -f docker-compose.yml -f docker-compose.newapi.yml exec -T newapi-redis redis-cli "*)
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml cp redis:/data "*)
    destination="${{@: -1}}"
    mkdir -p "$destination"
    printf 'sub2api redis\\n' > "$destination/dump.rdb"
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml cp newapi-redis:/data "*)
    destination="${{@: -1}}"
    mkdir -p "$destination"
    printf 'newapi redis\\n' > "$destination/dump.rdb"
    ;;
  *)
    echo "unexpected docker call: $*" >&2
    exit 99
    ;;
esac
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(backup_script), str(deployment)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(tmp_root / "backups"),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            prefix = "Backup package written to "
            self.assertTrue(result.stdout.startswith(prefix), result.stdout)
            package = pathlib.Path(result.stdout.strip()[len(prefix) :])
            extract_dir = tmp_root / "extracted"
            extract_dir.mkdir()
            subprocess.run(["tar", "-xzf", str(package), "-C", str(extract_dir)], check=True)

            for relative_path in [
                "sub2api-postgres.dump",
                "newapi-postgres.dump",
                "sub2api-redis-data/dump.rdb",
                "redis-data/dump.rdb",
                "deployment-runtime.tgz",
                "SHA256SUMS",
            ]:
                self.assertTrue((extract_dir / relative_path).exists(), relative_path)

            calls = calls_file.read_text(encoding="utf-8").splitlines()
            snapshot_index = next(
                index for index, call in enumerate(calls) if "postgres pg_dump" in call
            )
            for service in ["traefik", "new-api", "sub2api"]:
                stop_index = calls.index(
                    f"compose -f docker-compose.yml -f docker-compose.newapi.yml stop {service}"
                )
                self.assertLess(stop_index, snapshot_index)
                self.assertIn(
                    f"compose -f docker-compose.yml -f docker-compose.newapi.yml start {service}",
                    calls,
                )

    def test_snapshot_failure_restarts_only_previously_running_services(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            self.prepare_runtime(root)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  "compose -f docker-compose.yml -f docker-compose.newapi.yml ps --services --filter status=running")
    printf '%s\\n' new-api sub2api
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml stop "*|\
  "compose -f docker-compose.yml -f docker-compose.newapi.yml start "*)
    ;;
  "compose -f docker-compose.yml -f docker-compose.newapi.yml exec -T postgres pg_dump "*)
    echo "pg_dump failed" >&2
    exit 42
    ;;
  *)
    echo "unexpected docker call: $*" >&2
    exit 99
    ;;
esac
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)
            backup_root = tmp_root / "backups"

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(backup_root),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 42)
            self.assertIn("pg_dump failed", result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn("stop new-api", calls)
            self.assertIn("stop sub2api", calls)
            self.assertIn("start new-api", calls)
            self.assertIn("start sub2api", calls)
            self.assertNotIn("start traefik", calls)
            self.assertEqual(list(backup_root.glob("*.partial")), [])

    def test_existing_backup_lock_fails_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            self.prepare_runtime(root)
            backup_root = tmp_root / "backups"
            backup_root.mkdir()
            (backup_root / ".backup.lock").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker_marker = root / "docker-called"
            docker = bin_dir / "docker"
            docker.write_text(
                f"#!/usr/bin/env bash\ntouch {docker_marker}\nexit 99\n",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(backup_root),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Another deployment backup is already running", result.stderr)
            self.assertFalse(docker_marker.exists())


if __name__ == "__main__":
    unittest.main()
