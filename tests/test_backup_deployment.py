import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "backup-deployment.sh"
LEGACY_SCRIPT = ROOT / "scripts" / "migrations" / "backup-legacy-deployment.sh"
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

    def write_legacy_script_copy(self, root):
        backup_script = self.write_script_copy(root)
        migrations = root / "scripts" / "migrations"
        migrations.mkdir()
        legacy_script = migrations / "backup-legacy-deployment.sh"
        legacy_script.write_text(
            LEGACY_SCRIPT.read_text(encoding="utf-8"), encoding="utf-8"
        )
        legacy_script.chmod(0o755)
        return backup_script, legacy_script

    def prepare_runtime(self, root):
        (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
        (root / ".env").write_text(
            "\n".join(
                [
                    "SUB2API_POSTGRES_USER=sub2api",
                    "SUB2API_POSTGRES_DB=sub2api",
                    "SUB2API_REDIS_PASSWORD=sub2api-redis",
                    "NEW_API_POSTGRES_USER=newapi",
                    "NEW_API_POSTGRES_DB=newapi",
                    "NEW_API_REDIS_PASSWORD=new-api-redis",
                ]
            )
            + "\n",
            encoding="utf-8",
        )
        for directory in [
            "sub2api-data",
            "sub2api-postgres-data",
            "sub2api-redis-data",
            "letsencrypt",
        ]:
            (root / directory).mkdir()
        (root / "sub2api-data" / "config.yaml").write_text(
            "runtime: true\n", encoding="utf-8"
        )

    def test_backup_covers_both_application_state_domains(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("set -euo pipefail", text)
        self.assertIn(
            "Usage: scripts/backup-deployment.sh [DEPLOYMENT_DIR]",
            text,
        )
        self.assertIn('deployment_dir="${1:-${script_repo_root}}"', text)
        self.assertIn("sub2api-postgres.dump", text)
        self.assertIn("new-api-postgres.dump", text)
        self.assertIn("sub2api-redis-data", text)
        self.assertIn("new-api-redis-data", text)
        self.assertIn("deployment-runtime.tgz", text)
        self.assertIn("SHA256SUMS", text)
        self.assertIn("Lark quiesce backup barrier is not implemented", text)
        self.assertIn("running New API integration listener", text)
        self.assertIn("new-api-lark-controller-data", text)
        for service in ["traefik", "sub2api", "new-api"]:
            self.assertIn(f"stop_running_service {service}", text)
        self.assertLess(
            text.index('compose exec -T "$sub2api_redis_service" redis-cli SAVE'),
            text.index('stop_running_service "$sub2api_redis_service"'),
        )
        self.assertLess(
            text.index('stop_running_service "$sub2api_redis_service"'),
            text.index('compose cp "${sub2api_redis_service}:/data"'),
        )
        self.assertLess(
            text.index('compose exec -T "$new_api_redis_service" redis-cli'),
            text.index('stop_running_service "$new_api_redis_service"'),
        )
        self.assertLess(
            text.index('stop_running_service "$new_api_redis_service"'),
            text.index('compose cp "${new_api_redis_service}:/data"'),
        )
        for legacy_term in [
            "cliproxyapi",
            "cpa-usage-keeper",
            "config.yaml",
            "auths",
            "docker-compose.newapi.yml",
            "NEWAPI_",
            "dotenv_value POSTGRES_USER)",
            "dotenv_value REDIS_PASSWORD)",
        ]:
            self.assertNotIn(legacy_term, text)

    def test_backup_rejects_enabled_lark_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            self.prepare_runtime(root)
            with (root / ".env").open("a", encoding="utf-8") as env_file:
                env_file.write("NEW_API_INTEGRATION_LISTEN_ADDR=0.0.0.0:3001\n")

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
                    "BACKUP_DIR": str(tmp_root / "backups"),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Lark quiesce backup barrier is not implemented", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_backup_rejects_stale_env_when_running_new_api_listener_is_enabled(self):
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
if [[ "$*" == "compose ps --services --filter status=running" ]]; then
  printf '%s\\n' new-api
  exit 0
fi
if [[ "$*" == *"exec -T new-api sh -c"* ]]; then
  printf '0.0.0.0:3001'
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(tmp_root / "backups"),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("running New API integration listener", result.stderr)
            self.assertNotIn("compose stop", calls_file.read_text(encoding="utf-8"))

    def test_backup_rejects_stopped_controller_state_volume(self):
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
if [[ "$*" == "compose ps --services --filter status=running" ]]; then
  exit 0
fi
if [[ "$*" == "volume ls --quiet --filter name=new-api-lark-controller-data" ]]; then
  printf '%s\\n' new-api-lark-controller-data
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(tmp_root / "backups"),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Controller SQLite state exists", result.stderr)
            self.assertNotIn("compose stop", calls_file.read_text(encoding="utf-8"))

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
  "compose ps --services --filter status=running")
    printf '%s\\n' traefik new-api sub2api sub2api-redis new-api-redis
    ;;
  "compose exec -T new-api sh -c "*)
    ;;
  "volume ls --quiet --filter name=new-api-lark-controller-data")
    ;;
  "compose stop "*|\
  "compose start "*)
    ;;
  "compose exec -T sub2api-postgres pg_dump "*)
    printf 'sub2api postgres dump\\n'
    ;;
  "compose exec -T new-api-postgres pg_dump "*)
    printf 'newapi postgres dump\\n'
    ;;
  "compose exec -T sub2api-redis redis-cli SAVE"|\
  "compose exec -T new-api-redis redis-cli "*)
    ;;
  "compose cp sub2api-redis:/data "*)
    destination="${{@: -1}}"
    mkdir -p "$destination"
    printf 'sub2api redis\\n' > "$destination/dump.rdb"
    ;;
  "compose cp new-api-redis:/data "*)
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
                "new-api-postgres.dump",
                "sub2api-redis-data/dump.rdb",
                "new-api-redis-data/dump.rdb",
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
                    f"compose stop {service}"
                )
                self.assertLess(stop_index, snapshot_index)
                self.assertIn(
                    f"compose start {service}",
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
  "compose ps --services --filter status=running")
    printf '%s\\n' new-api sub2api
    ;;
  "compose exec -T new-api sh -c "*|\
  "volume ls --quiet --filter name=new-api-lark-controller-data")
    ;;
  "compose stop "*|\
  "compose start "*)
    ;;
  "compose exec -T sub2api-postgres pg_dump "*)
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

    def test_legacy_mode_adapts_the_source_compose_and_service_names(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            _, legacy_script = self.write_legacy_script_copy(root)
            deployment = root / "legacy"
            deployment.mkdir()
            self.prepare_runtime(deployment)
            for current_name, legacy_name in [
                ("sub2api-data", "data"),
                ("sub2api-postgres-data", "postgres_data"),
                ("sub2api-redis-data", "redis_data"),
            ]:
                (deployment / current_name).rename(deployment / legacy_name)
            (deployment / ".env").write_text(
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
            (deployment / "docker-compose.newapi.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  printf '%s\\n' traefik new-api sub2api redis newapi-redis
  exit 0
fi
if [[ "$*" == *" exec -T new-api sh -c "* ]]; then
  exit 0
fi
if [[ "$*" == "volume ls --quiet --filter name=new-api-lark-controller-data" ]]; then
  exit 0
fi
if [[ "$*" == *" stop "* || "$*" == *" start "* ]]; then
  exit 0
fi
if [[ "$*" == *" exec -T postgres pg_dump "* ]]; then
  printf 'legacy sub2api postgres dump\\n'
  exit 0
fi
if [[ "$*" == *" exec -T newapi-postgres pg_dump "* ]]; then
  printf 'legacy newapi postgres dump\\n'
  exit 0
fi
if [[ "$*" == *" redis-cli "* ]]; then
  exit 0
fi
if [[ "$*" == *" cp redis:/data "* || "$*" == *" cp newapi-redis:/data "* ]]; then
  destination="${{@: -1}}"
  mkdir -p "$destination"
  printf 'redis snapshot\\n' > "$destination/dump.rdb"
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(legacy_script), str(deployment)],
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
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn(
                "compose -f docker-compose.yml -f docker-compose.newapi.yml", calls
            )
            self.assertIn("exec -T postgres pg_dump", calls)
            self.assertIn("exec -T redis redis-cli SAVE", calls)
            prefix = "Backup package written to "
            self.assertTrue(result.stdout.startswith(prefix), result.stdout)
            package = result.stdout.strip()[len(prefix) :]
            archive = subprocess.run(
                ["tar", "-tzf", package],
                text=True,
                capture_output=True,
                check=True,
            ).stdout.splitlines()
            self.assertIn("./new-api-postgres.dump", archive)
            self.assertIn("./new-api-redis-data/dump.rdb", archive)

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
