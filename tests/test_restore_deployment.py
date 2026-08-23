import hashlib
import io
import pathlib
import subprocess
import tarfile
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "restore-deployment.sh"
DOTENV_READER = ROOT / "scripts" / "read-dotenv.py"
MANIFEST_TOOL = ROOT / "scripts" / "deployment-backup-manifest.py"


class RestoreDeploymentTests(unittest.TestCase):
    def write_script_copy(self, root):
        scripts = root / "scripts"
        scripts.mkdir()
        restore_script = scripts / "restore-deployment.sh"
        restore_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        restore_script.chmod(0o755)
        (scripts / "read-dotenv.py").write_text(
            DOTENV_READER.read_text(encoding="utf-8"), encoding="utf-8"
        )
        (scripts / "deployment-backup-manifest.py").write_text(
            MANIFEST_TOOL.read_text(encoding="utf-8"), encoding="utf-8"
        )
        return restore_script

    def create_backup_package(
        self,
        tmp_root,
        runtime_env_lines=None,
        lark_state="absent",
        controller_has_main=True,
    ):
        backup_src = tmp_root / "backup-src"
        backup_src.mkdir()
        runtime_src = tmp_root / "runtime-src"
        runtime_src.mkdir()
        if runtime_env_lines is None:
            runtime_env_lines = [
                "ACME_EMAIL=admin@example.test",
                "SUB2API_HOST=legacy-sub2api.example.test",
                "SUB2API_PROXY_SUBNET=172.31.20.0/24",
                "SUB2API_BACKEND_SUBNET=172.31.21.0/24",
                "SUB2API_ADMIN_HOST=admin.example.test",
                "POSTGRES_USER=sub2api",
                "POSTGRES_DB=sub2api",
                "POSTGRES_PASSWORD=sub2api-pw",
                "REDIS_PASSWORD=sub2api-redis",
                "JWT_SECRET=jwt-secret",
                "TOTP_ENCRYPTION_KEY=totp-secret",
                "ADMIN_PASSWORD=admin-secret",
                "NEW_API_HOST=ai.example.test",
                "NEW_API_IMAGE_TAG=v1.2.3",
                "NEWAPI_POSTGRES_USER=newapi",
                "NEWAPI_POSTGRES_DB=newapi",
                "NEWAPI_POSTGRES_PASSWORD=newapi-pw",
                "NEWAPI_REDIS_PASSWORD=new-api-redis",
                "NEW_API_SESSION_SECRET=session-secret",
                "NEW_API_CRYPTO_SECRET=crypto-secret",
            ]
            if lark_state == "enabled":
                runtime_env_lines.extend(
                    [
                        "NEW_API_INTEGRATION_LISTEN_ADDR=0.0.0.0:3001",
                        "LARK_CONTROLLER_MODE=active",
                        "LARK_OAUTH_PUBLIC_ENABLED=true",
                    ]
                )
        (runtime_src / ".env").write_text(
            "\n".join(runtime_env_lines) + "\n",
            encoding="utf-8",
        )
        (runtime_src / "sub2api-data").mkdir()
        (runtime_src / "sub2api-data" / "config.yaml").write_text(
            "restored: true\n", encoding="utf-8"
        )
        (runtime_src / "letsencrypt").mkdir()
        (runtime_src / "letsencrypt" / "acme.json").write_text("{}\n", encoding="utf-8")
        if lark_state == "enabled":
            policies = runtime_src / "lark-runtime" / "policies"
            policies.mkdir(parents=True)
            (policies / "approval-bindings.json").write_text(
                '{"version":1}\n', encoding="utf-8"
            )
            (policies / "2026-08.policy.json").write_text(
                '{"policy_version":"2026-08"}\n', encoding="utf-8"
            )
        subprocess.run(
            ["tar", "-czf", str(backup_src / "deployment-runtime.tgz"), "."],
            cwd=runtime_src,
            check=True,
        )
        (backup_src / "sub2api-postgres.dump").write_text(
            "sub2api dump\n", encoding="utf-8"
        )
        (backup_src / "new-api-postgres.dump").write_text(
            "newapi dump\n", encoding="utf-8"
        )
        for directory, content in [
            ("sub2api-redis-data", "sub2api redis\n"),
            ("new-api-redis-data", "newapi redis\n"),
        ]:
            (backup_src / directory).mkdir()
            (backup_src / directory / "dump.rdb").write_text(content, encoding="utf-8")

        if lark_state == "enabled":
            controller_src = tmp_root / "controller-src"
            controller_src.mkdir()
            if controller_has_main:
                (controller_src / "controller.sqlite").write_text(
                    "controller sqlite\n", encoding="utf-8"
                )
            (controller_src / "controller.sqlite-wal").write_text(
                "controller wal\n", encoding="utf-8"
            )
            subprocess.run(
                [
                    "tar",
                    "-czf",
                    str(backup_src / "lark-controller-data.tgz"),
                    ".",
                ],
                cwd=controller_src,
                check=True,
            )
        else:
            (backup_src / "lark-controller-data.absent").write_text(
                "new-api-lark-controller-data absent\n", encoding="utf-8"
            )

        subprocess.run(
            [
                "python3",
                str(MANIFEST_TOOL),
                "create",
                "--root",
                str(backup_src),
                "--created-at",
                "2026-08-23T00:00:00Z",
                "--lark-state",
                lark_state,
            ],
            check=True,
        )

        checksum_lines = []
        for path in sorted(path for path in backup_src.rglob("*") if path.is_file()):
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            checksum_lines.append(f"{digest}  ./{path.relative_to(backup_src)}")
        (backup_src / "SHA256SUMS").write_text(
            "\n".join(checksum_lines) + "\n", encoding="utf-8"
        )
        package = tmp_root / "backup.tgz"
        subprocess.run(["tar", "-czf", str(package), "."], cwd=backup_src, check=True)
        return package

    def test_restore_consumes_every_state_domain_in_the_backup_contract(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("set -euo pipefail", text)
        self.assertIn("deployment-runtime.tgz", text)
        self.assertIn("sub2api-postgres.dump", text)
        self.assertIn("new-api-postgres.dump", text)
        self.assertIn("sub2api-redis-data", text)
        self.assertIn("new-api-redis-data", text)
        self.assertIn("SHA256SUMS", text)
        self.assertIn("backup-manifest.json", text)
        self.assertNotIn("docker-compose.newapi.yml", text)
        self.assertIn("pg_restore", text)
        self.assertIn("Restore completed", text)
        self.assertIn("maintenance_lock", text)
        self.assertIn("lark-controller-data.tgz", text)
        self.assertIn("new-api-lark-controller-data", text)
        self.assertNotIn("cliproxy", text.lower())

    def test_restore_rejects_lark_enabled_backup_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text("CURRENT_ENV=true\n", encoding="utf-8")
            package = self.create_backup_package(
                tmp_root,
                runtime_env_lines=[
                    "NEW_API_INTEGRATION_LISTEN_ADDR=0.0.0.0:3001",
                ],
            )
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Lark absent backup cannot enable", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_refuses_an_existing_deployment_maintenance_owner(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text(
                "NEW_API_INTEGRATION_LISTEN_ADDR=\n", encoding="utf-8"
            )
            package = self.create_backup_package(tmp_root)
            lock = root / "lark-runtime" / "ops" / "maintenance.lock"
            lock.mkdir(parents=True)
            (lock / "mode").write_text("backup\n", encoding="utf-8")
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == *" config --quiet" ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Another deployment maintenance session owns", result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn("config --quiet", calls)
            self.assertNotIn(" down", calls)
            self.assertEqual((lock / "mode").read_text(encoding="utf-8"), "backup\n")

    def test_boundary_marker_permission_failure_releases_session_and_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text(
                "NEW_API_INTEGRATION_LISTEN_ADDR=\n", encoding="utf-8"
            )
            package = self.create_backup_package(tmp_root)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
if [[ "$*" == *" config --quiet" ]]; then
  exit 0
fi
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)
            chmod = bin_dir / "chmod"
            chmod.write_text(
                """#!/usr/bin/env bash
if [[ "$1" == "600" && "$2" == */maintenance.lock/mode ]]; then
  exit 42
fi
exec /bin/chmod "$@"
""",
                encoding="utf-8",
            )
            chmod.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 42, result.stderr)
            self.assertFalse((root / "lark-runtime/ops/maintenance.lock").exists())
            self.assertFalse((root / "lark-runtime/ops/maintenance.session").exists())

    def test_boundary_cleanup_failure_retains_session_with_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text(
                "NEW_API_INTEGRATION_LISTEN_ADDR=\n", encoding="utf-8"
            )
            package = self.create_backup_package(tmp_root)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
if [[ "$*" == *" config --quiet" ]]; then
  exit 0
fi
if [[ "$*" == "compose ps --services --filter status=running" ]]; then
  touch {root / 'lark-runtime/ops/maintenance.lock/obstruction'}
  exit 42
fi
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Could not release deployment maintenance lock", result.stderr)
            self.assertTrue((root / "lark-runtime/ops/maintenance.lock").is_dir())
            self.assertTrue((root / "lark-runtime/ops/maintenance.session").is_dir())

    def test_restore_rejects_enabled_manifest_without_controller_archive(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text(
                "NEW_API_INTEGRATION_LISTEN_ADDR=\n", encoding="utf-8"
            )
            package = self.create_backup_package(tmp_root, lark_state="enabled")
            tampered = tmp_root / "tampered"
            tampered.mkdir()
            subprocess.run(
                ["tar", "-xzf", str(package), "-C", str(tampered)], check=True
            )
            (tampered / "lark-controller-data.tgz").unlink()
            checksum_lines = []
            for path in sorted(path for path in tampered.rglob("*") if path.is_file()):
                if path.name == "SHA256SUMS":
                    continue
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
                checksum_lines.append(f"{digest}  ./{path.relative_to(tampered)}")
            (tampered / "SHA256SUMS").write_text(
                "\n".join(checksum_lines) + "\n", encoding="utf-8"
            )
            subprocess.run(
                ["tar", "-czf", str(package), "."], cwd=tampered, check=True
            )
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("manifest file inventory", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_replaces_runtime_and_both_database_domains(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text("OLD_ENV=true\n", encoding="utf-8")
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()
                (root / directory / "stale").write_text("stale\n", encoding="utf-8")
            for directory in ["policies", "secrets", "ops"]:
                path = root / "lark-runtime" / directory
                path.mkdir(parents=True)
                (path / "keep").write_text("keep\n", encoding="utf-8")
            package = self.create_backup_package(tmp_root)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            volume_list_count = root / "absent-volume-list-count"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$1" == "compose" && "$*" == *" ps -aq "* ]]; then
  echo "${{@: -1}}-container"
  exit 0
fi
if [[ "$1" == "inspect" ]]; then
  case "${{@: -1}}" in
    new-api-postgres-container) echo "new-api-postgres-data-volume" ;;
    new-api-redis-container) echo "new-api-redis-data-volume" ;;
  esac
  exit 0
fi
if [[ "$*" == "volume ls --quiet --filter name=new-api-lark-controller-data" ]]; then
  count=0
  if [[ -f {volume_list_count} ]]; then
    count="$(cat {volume_list_count})"
  fi
  count=$((count + 1))
  printf '%s\\n' "$count" > {volume_list_count}
  if [[ "$count" -eq 1 ]]; then
    printf '%s\\n' new-api-lark-controller-data
  fi
  exit 0
fi
if [[ "$*" == "volume rm -f new-api-lark-controller-data" ]]; then
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" pg_isready "* ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" pg_restore "* ]]; then
  cat >/dev/null
  exit 0
fi
if [[ "$1" == "compose" ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                    "EDGE_SUBNET": "172.31.30.0/24",
                    "SUB2API_DATA_SUBNET": "172.31.31.0/24",
                    "NEW_API_DATA_SUBNET": "172.31.32.0/24",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Restore completed", result.stdout)
            self.assertIn(
                "restored: true",
                (root / "sub2api-data" / "config.yaml").read_text(encoding="utf-8"),
            )
            self.assertFalse((root / "sub2api-postgres-data" / "stale").exists())
            self.assertFalse((root / "sub2api-redis-data" / "stale").exists())
            self.assertEqual(
                (root / "sub2api-redis-data" / "dump.rdb").read_text(
                    encoding="utf-8"
                ),
                "sub2api redis\n",
            )
            restored_env = (root / ".env").read_text(encoding="utf-8")
            self.assertEqual((root / ".env").stat().st_mode & 0o777, 0o600)
            self.assertIn("EDGE_SUBNET=172.31.30.0/24", restored_env)
            self.assertIn("SUB2API_DATA_SUBNET=172.31.31.0/24", restored_env)
            self.assertIn("NEW_API_DATA_SUBNET=172.31.32.0/24", restored_env)
            self.assertIn("SUB2API_POSTGRES_USER=sub2api", restored_env)
            self.assertIn("SUB2API_POSTGRES_DB=sub2api", restored_env)
            self.assertIn("SUB2API_POSTGRES_PASSWORD=sub2api-pw", restored_env)
            self.assertIn("SUB2API_REDIS_PASSWORD=sub2api-redis", restored_env)
            self.assertIn("NEW_API_POSTGRES_USER=newapi", restored_env)
            self.assertIn("NEW_API_POSTGRES_DB=newapi", restored_env)
            self.assertIn("NEW_API_POSTGRES_PASSWORD=newapi-pw", restored_env)
            self.assertIn("NEW_API_REDIS_PASSWORD=new-api-redis", restored_env)
            self.assertNotIn("SUB2API_PROXY_SUBNET", restored_env)
            self.assertNotIn("SUB2API_BACKEND_SUBNET", restored_env)
            self.assertNotIn("SUB2API_HOST", restored_env)
            self.assertNotIn("NEWAPI_", restored_env)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn(" down", calls)
            self.assertIn("exec -T sub2api-postgres pg_restore", calls)
            self.assertIn("exec -T new-api-postgres pg_restore", calls)
            self.assertIn("new-api-postgres-data-volume:/target", calls)
            self.assertIn("new-api-redis-data-volume:/target", calls)
            self.assertIn("volume rm -f new-api-lark-controller-data", calls)
            self.assertNotIn("volume create new-api-lark-controller-data", calls)
            self.assertFalse((root / "lark-runtime/policies").exists())
            self.assertTrue((root / "lark-runtime/secrets/keep").is_file())
            self.assertTrue((root / "lark-runtime/ops/keep").is_file())

    def test_restore_replaces_controller_volume_from_enabled_package(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text(
                "NEW_API_INTEGRATION_LISTEN_ADDR=\n", encoding="utf-8"
            )
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()
            package = self.create_backup_package(tmp_root, lark_state="enabled")

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            volume_list_count = root / "volume-list-count"
            restore_list_count = root / "restore-list-count"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  "compose --env-file "*" config --quiet"|\
  "compose ps --services --filter status=running")
    ;;
  "compose --env-file "*" down"|\
  "compose create new-api-postgres new-api-redis"|\
  "compose up -d sub2api-postgres new-api-postgres"|\
  "compose --profile lark up -d --wait --wait-timeout 120")
    ;;
  "compose ps -aq "*)
    printf '%s-container\\n' "${{@: -1}}"
    ;;
  "inspect "*)
    case "${{@: -1}}" in
      new-api-postgres-container) printf 'new-api-postgres-data-volume\\n' ;;
      new-api-redis-container) printf 'new-api-redis-data-volume\\n' ;;
    esac
    ;;
  "volume rm -f new-api-lark-controller-data"|\
  "volume create new-api-lark-controller-data")
    ;;
  "volume ls --quiet --filter name=new-api-lark-controller-data")
    count=0
    if [[ -f {volume_list_count} ]]; then
      count="$(cat {volume_list_count})"
    fi
    count=$((count + 1))
    printf '%s\\n' "$count" > {volume_list_count}
    if [[ "$count" -eq 1 ]]; then
      printf '%s\\n' new-api-lark-controller-data
    fi
    ;;
  "run "*)
    ;;
  "container ls -aq --filter name=^/new-api-lark-restore-controller$")
    count=0
    if [[ -f {restore_list_count} ]]; then
      count="$(cat {restore_list_count})"
    fi
    count=$((count + 1))
    printf '%s\\n' "$count" > {restore_list_count}
    if [[ "$count" -gt 1 ]]; then
      exit 99
    fi
    ;;
  "container rm -f new-api-lark-restore-controller")
    ;;
  "compose exec -T sub2api-postgres pg_isready "*|\
  "compose exec -T new-api-postgres pg_isready "*)
    ;;
  "compose exec -T sub2api-postgres pg_restore "*|\
  "compose exec -T new-api-postgres pg_restore "*)
    test "$(cat {root / 'lark-runtime/ops/maintenance.lock/mode'})" = restore
    cat >/dev/null
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn("volume rm -f new-api-lark-controller-data", calls)
            self.assertIn("volume create new-api-lark-controller-data", calls)
            self.assertIn("new-api-lark-controller-data:/target", calls)
            self.assertIn("lark-controller-data.tgz:/backup/controller.tgz:ro", calls)
            self.assertIn(
                "compose --profile lark up -d --wait --wait-timeout 120", calls
            )
            restored_env = (root / ".env").read_text(encoding="utf-8")
            self.assertIn("LARK_CONTROLLER_MODE=shadow", restored_env)
            self.assertIn("LARK_OAUTH_PUBLIC_ENABLED=false", restored_env)
            self.assertTrue(
                (root / "lark-runtime/policies/approval-bindings.json").is_file()
            )
            self.assertFalse((root / "lark-runtime/ops/maintenance.lock").exists())
            self.assertFalse(
                (root / "lark-runtime/ops/maintenance.session").exists()
            )

    def test_restore_failure_after_down_retains_lock_and_does_not_start_apps(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT=true\n", encoding="utf-8")
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()
            package = self.create_backup_package(tmp_root)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == *" config --quiet" || "$*" == "compose ps --services --filter status=running" ]]; then
  exit 0
fi
if [[ "$*" == *" down" || "$*" == "compose create new-api-postgres new-api-redis" ]]; then
  exit 0
fi
if [[ "$*" == "volume ls --quiet --filter name=new-api-lark-controller-data" ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" ps -aq "* ]]; then
  printf '%s-container\\n' "${{@: -1}}"
  exit 0
fi
if [[ "$1" == "inspect" ]]; then
  case "${{@: -1}}" in
    new-api-postgres-container) printf 'new-api-postgres-data-volume\\n' ;;
    new-api-redis-container) printf 'new-api-redis-data-volume\\n' ;;
  esac
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$*" == "compose up -d sub2api-postgres new-api-postgres" || "$*" == *" pg_isready "* ]]; then
  exit 0
fi
if [[ "$*" == *"exec -T sub2api-postgres pg_restore "* ]]; then
  cat >/dev/null
  echo 'restore failed' >&2
  exit 42
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 42)
            self.assertIn("restore failed", result.stderr)
            lock = root / "lark-runtime" / "ops" / "maintenance.lock"
            self.assertEqual((lock / "mode").read_text(encoding="utf-8"), "restore\n")
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            self.assertFalse(any(call == "compose up -d" for call in calls))
            self.assertFalse(any("--profile lark up -d" in call for call in calls))

    def test_restore_start_failure_reestablishes_lock_and_stops_partial_services(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT=true\n", encoding="utf-8")
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()
            package = self.create_backup_package(tmp_root)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  "compose --env-file "*" config --quiet"|\
  "compose ps --services --filter status=running"|\
  "compose --env-file "*" down"|\
  "compose down"|\
  "compose create new-api-postgres new-api-redis"|\
  "compose up -d sub2api-postgres new-api-postgres")
    ;;
  "compose up -d --wait --wait-timeout 120")
    test -d {root / 'lark-runtime/ops/maintenance.session'}
    echo 'readiness failed' >&2
    exit 42
    ;;
  "compose ps -aq "*)
    printf '%s-container\\n' "${{@: -1}}"
    ;;
  "inspect "*)
    case "${{@: -1}}" in
      new-api-postgres-container) printf 'new-api-postgres-data-volume\\n' ;;
      new-api-redis-container) printf 'new-api-redis-data-volume\\n' ;;
    esac
    ;;
  "volume ls --quiet --filter name=new-api-lark-controller-data"|\
  "run "*)
    ;;
  "compose exec -T sub2api-postgres pg_isready "*|\
  "compose exec -T new-api-postgres pg_isready "*)
    ;;
  "compose exec -T sub2api-postgres pg_restore "*|\
  "compose exec -T new-api-postgres pg_restore "*)
    cat >/dev/null
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 42, result.stderr)
            self.assertIn("readiness failed", result.stderr)
            lock = root / "lark-runtime" / "ops" / "maintenance.lock"
            self.assertEqual((lock / "mode").read_text(encoding="utf-8"), "restore\n")
            self.assertTrue(
                (root / "lark-runtime/ops/maintenance.session").is_dir()
            )
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            self.assertEqual(sum(call.endswith(" down") for call in calls), 2)
            self.assertEqual(
                calls.count("compose ps --services --filter status=running"), 2
            )

    def test_restore_rejects_controller_archive_without_main_database_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT=true\n", encoding="utf-8")
            package = self.create_backup_package(
                tmp_root,
                lark_state="enabled",
                controller_has_main=False,
            )
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("controller.sqlite", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_session_release_failure_stops_services_and_restores_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT=true\n", encoding="utf-8")
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()
            package = self.create_backup_package(tmp_root)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  "compose --env-file "*" config --quiet"|\
  "compose ps --services --filter status=running"|\
  "compose --env-file "*" down"|\
  "compose down"|\
  "compose create new-api-postgres new-api-redis"|\
  "compose up -d sub2api-postgres new-api-postgres")
    ;;
  "compose up -d --wait --wait-timeout 120")
    touch {root / 'lark-runtime/ops/maintenance.session/obstruction'}
    ;;
  "compose ps -aq "*)
    printf '%s-container\\n' "${{@: -1}}"
    ;;
  "inspect "*)
    case "${{@: -1}}" in
      new-api-postgres-container) printf 'new-api-postgres-data-volume\\n' ;;
      new-api-redis-container) printf 'new-api-redis-data-volume\\n' ;;
    esac
    ;;
  "volume ls --quiet --filter name=new-api-lark-controller-data"|\
  "run "*)
    ;;
  "compose exec -T sub2api-postgres pg_isready "*|\
  "compose exec -T new-api-postgres pg_isready "*)
    ;;
  "compose exec -T sub2api-postgres pg_restore "*|\
  "compose exec -T new-api-postgres pg_restore "*)
    cat >/dev/null
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
            chmod_count = root / "maintenance-mode-chmod-count"
            chmod = bin_dir / "chmod"
            chmod.write_text(
                f"""#!/usr/bin/env bash
if [[ "$1" == "600" && "$2" == */maintenance.lock/mode ]]; then
  count=0
  if [[ -f {chmod_count} ]]; then
    count="$(cat {chmod_count})"
  fi
  count=$((count + 1))
  printf '%s\\n' "$count" > {chmod_count}
  if [[ "$count" -eq 2 ]]; then
    exit 42
  fi
fi
exec /bin/chmod "$@"
""",
                encoding="utf-8",
            )
            chmod.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Could not release deployment maintenance session", result.stderr)
            self.assertIn(
                "Could not secure re-established deployment maintenance lock",
                result.stderr,
            )
            lock = root / "lark-runtime" / "ops" / "maintenance.lock"
            self.assertEqual((lock / "mode").read_text(encoding="utf-8"), "restore\n")
            self.assertTrue(
                (root / "lark-runtime/ops/maintenance.session").is_dir()
            )
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            self.assertEqual(sum(call.endswith(" down") for call in calls), 2)

    def test_restore_rejects_unsafe_archive_paths_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / ".env").write_text("placeholder=true\n", encoding="utf-8")
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            package = root / "unsafe.tgz"
            with tarfile.open(package, "w:gz") as archive:
                member = tarfile.TarInfo("../escape")
                payload = b"unsafe\n"
                member.size = len(payload)
                archive.addfile(member, io.BytesIO(payload))
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Unsafe path in backup package", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_rejects_unsafe_checksum_paths_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / ".env").write_text("placeholder=true\n", encoding="utf-8")
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            package = self.create_backup_package(tmp_root)
            tampered = tmp_root / "unsafe-checksum"
            tampered.mkdir()
            subprocess.run(
                ["tar", "-xzf", str(package), "-C", str(tampered)], check=True
            )
            with (tampered / "SHA256SUMS").open("a", encoding="utf-8") as receipt:
                receipt.write(f"{'0' * 64}  ./../../outside\n")
            subprocess.run(
                ["tar", "-czf", str(package), "."], cwd=tampered, check=True
            )
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("unsafe path in SHA256SUMS", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_render_failure_preserves_current_deployment(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT_ENV=true\n", encoding="utf-8")
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()
                (root / directory / "current").write_text(
                    "current\n", encoding="utf-8"
                )
            package = self.create_backup_package(tmp_root)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            mutation_marker = root / "docker-mutated"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == *" config --quiet" ]]; then
  echo "invalid restored compose" >&2
  exit 42
fi
touch {mutation_marker}
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 42)
            self.assertIn("invalid restored compose", result.stderr)
            self.assertIn("config --quiet", calls_file.read_text(encoding="utf-8"))
            self.assertFalse(mutation_marker.exists())
            self.assertEqual(
                (root / ".env").read_text(encoding="utf-8"), "CURRENT_ENV=true\n"
            )
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                self.assertTrue((root / directory / "current").exists(), directory)

    def test_restore_rejects_runtime_env_interpolation_without_execution(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT_ENV=true\n", encoding="utf-8")
            for directory in [
                "sub2api-data",
                "letsencrypt",
                "sub2api-postgres-data",
                "sub2api-redis-data",
            ]:
                (root / directory).mkdir()

            executed_marker = root / "env-executed"
            package = self.create_backup_package(
                tmp_root,
                runtime_env_lines=[f"MALICIOUS=$(touch {executed_marker})"],
            )
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
                [str(restore_script), str(package), str(root)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("interpolation is not supported", result.stderr)
            self.assertFalse(executed_marker.exists())
            self.assertFalse(docker_marker.exists())
            self.assertEqual(
                (root / ".env").read_text(encoding="utf-8"), "CURRENT_ENV=true\n"
            )


if __name__ == "__main__":
    unittest.main()
