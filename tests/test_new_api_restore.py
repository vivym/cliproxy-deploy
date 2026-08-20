import hashlib
import io
import pathlib
import subprocess
import tarfile
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
COMPOSE = ROOT / "docker-compose.yml"
RESTORE_SCRIPT = ROOT / "scripts" / "restore-new-api.sh"
DOTENV_READER = ROOT / "scripts" / "read-dotenv.py"
ENV_EXAMPLE = ROOT / ".env.example"


class NewApiRestoreTests(unittest.TestCase):
    def write_script_copy(self, root):
        scripts = root / "scripts"
        scripts.mkdir()
        restore_script = scripts / "restore-new-api.sh"
        restore_script.write_text(RESTORE_SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        restore_script.chmod(0o755)
        (scripts / "read-dotenv.py").write_text(
            DOTENV_READER.read_text(encoding="utf-8"), encoding="utf-8"
        )
        return restore_script

    def write_backup_package(self, backup_src, backup_package):
        checksum_lines = []
        for path in sorted(path for path in backup_src.rglob("*") if path.is_file()):
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            checksum_lines.append(f"{digest}  ./{path.relative_to(backup_src)}")
        (backup_src / "SHA256SUMS").write_text(
            "\n".join(checksum_lines) + "\n", encoding="utf-8"
        )
        subprocess.run(
            ["tar", "-czf", str(backup_package), "."], cwd=backup_src, check=True
        )

    def test_main_compose_routes_new_api_to_sub2api_without_cliproxy(self):
        self.assertTrue(COMPOSE.exists())
        text = COMPOSE.read_text(encoding="utf-8")

        for service in ["new-api-postgres:", "new-api-redis:", "new-api:"]:
            self.assertIn(service, text)
        self.assertIn("name: new-api-edge", text)
        self.assertIn("name: new-api-data", text)
        self.assertIn("traefik.docker.network=new-api-edge", text)
        self.assertIn("traefik.http.routers.new-api.rule=Host(`${NEW_API_HOST:?set NEW_API_HOST}`)", text)
        self.assertIn("traefik.http.services.new-api.loadbalancer.server.port=3000", text)
        for name in [
            "NEW_API_POSTGRES_USER",
            "NEW_API_POSTGRES_PASSWORD",
            "NEW_API_POSTGRES_DB",
            "NEW_API_REDIS_PASSWORD",
            "NEW_API_SESSION_SECRET",
            "NEW_API_CRYPTO_SECRET",
        ]:
            self.assertIn(name, text)

        self.assertNotIn("cliproxyapi", text)
        self.assertNotIn("cpa-usage-keeper", text)
        self.assertIn('- "80:80"', text)
        self.assertIn('- "443:443"', text)
        self.assertIn('expose:\n      - "3000"', text)
        self.assertIn('expose:\n      - "8080"', text)
        self.assertNotIn("container_name: traefik", text)

    def test_env_example_documents_new_api_runtime_values(self):
        text = ENV_EXAMPLE.read_text(encoding="utf-8")
        for line in [
            "NEW_API_HOST=ai.example.com",
            "NEW_API_IMAGE_TAG=v0.13.2",
            "NEW_API_POSTGRES_USER=new_api",
            "NEW_API_POSTGRES_DB=new_api",
            "NEW_API_POSTGRES_PASSWORD=",
            "NEW_API_REDIS_PASSWORD=",
            "NEW_API_SESSION_SECRET=",
            "NEW_API_CRYPTO_SECRET=",
        ]:
            self.assertIn(line, text)

    def test_restore_new_api_only_script_scope(self):
        self.assertTrue(RESTORE_SCRIPT.exists())
        text = RESTORE_SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -euo pipefail", text)
        self.assertIn("Usage: scripts/restore-new-api.sh BACKUP_PACKAGE [DEPLOYMENT_DIR]", text)
        self.assertNotIn("docker-compose.newapi.yml", text)
        self.assertIn("new-api-postgres", text)
        self.assertIn("new-api-redis", text)
        self.assertIn("new-api", text)
        self.assertIn("new-api-postgres.dump", text)
        self.assertIn("new-api-redis-data", text)
        self.assertIn("SHA256SUMS", text)
        self.assertNotIn("cliproxyapi", text)
        self.assertNotIn("cpa-usage-keeper", text)
        self.assertNotIn("auths", text)
        self.assertNotIn("letsencrypt", text)

    def test_restore_new_api_only_does_not_touch_sub2api_runtime(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            deployment_dir = root / "sub2api"
            deployment_dir.mkdir()
            (deployment_dir / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (deployment_dir / ".env").write_text(
                "\n".join(
                    [
                        "POSTGRES_USER=sub2api",
                        "POSTGRES_DB=sub2api",
                        "POSTGRES_PASSWORD=sub2api-pw",
                        "REDIS_PASSWORD=sub2api-redis",
                        "NEW_API_POSTGRES_USER=newapi",
                        "NEW_API_POSTGRES_DB=newapi",
                        "NEW_API_POSTGRES_PASSWORD=newapi-pw",
                        "NEW_API_REDIS_PASSWORD=new-api-redis",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            (deployment_dir / "sub2api-data").mkdir()
            (deployment_dir / "sub2api-data" / "config.yaml").write_text(
                "sub2api config\n", encoding="utf-8"
            )

            backup_src = tmp_root / "backup-src"
            backup_src.mkdir()
            runtime_src = tmp_root / "runtime-src"
            runtime_src.mkdir()
            (runtime_src / ".env").write_text(
                "\n".join(
                    [
                        "AI_HOST=ai.backup.example.com",
                        "NEW_API_IMAGE_TAG=v0.99.0",
                        "POSTGRES_USER=backup_newapi",
                        "POSTGRES_DB=backup_newapi_db",
                        "POSTGRES_PASSWORD=backup-postgres-pw",
                        "REDIS_PASSWORD=backup-redis-pw",
                        "NEW_API_SESSION_SECRET=backup-session-secret",
                        "NEW_API_CRYPTO_SECRET=backup-crypto-secret",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            subprocess.run(
                ["tar", "-czf", str(backup_src / "cliproxy-runtime.tgz"), "."],
                cwd=runtime_src,
                check=True,
            )
            (backup_src / "newapi-postgres.dump").write_text("postgres dump\n", encoding="utf-8")
            (backup_src / "redis-data").mkdir()
            (backup_src / "redis-data" / "dump.rdb").write_text("redis data\n", encoding="utf-8")
            backup_package = tmp_root / "backup.tgz"
            self.write_backup_package(backup_src, backup_package)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
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
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_isready "* ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_restore "* ]]; then
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
                [str(restore_script), str(backup_package), str(deployment_dir)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                (deployment_dir / "sub2api-data" / "config.yaml").read_text(
                    encoding="utf-8"
                ),
                "sub2api config\n",
            )
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn("stop new-api", calls)
            self.assertIn("create new-api-postgres", calls)
            self.assertIn("create new-api-redis", calls)
            self.assertIn("up -d new-api", calls)
            self.assertIn("pg_restore", calls)
            self.assertNotIn(" down", calls)
            self.assertNotIn("cliproxyapi", calls)
            self.assertNotIn("cpa-usage-keeper", calls)

    def test_restore_requires_checksums_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text("placeholder=true\n", encoding="utf-8")
            backup_src = root / "backup-src"
            backup_src.mkdir()
            (backup_src / "new-api-postgres.dump").write_text(
                "postgres dump\n", encoding="utf-8"
            )
            (backup_src / "new-api-redis-data").mkdir()
            package = root / "backup.tgz"
            subprocess.run(
                ["tar", "-czf", str(package), "."], cwd=backup_src, check=True
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
            self.assertIn("Missing required restore source", result.stderr)
            self.assertIn("SHA256SUMS", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_allows_unverified_legacy_package_only_by_explicit_opt_in(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text("placeholder=true\n", encoding="utf-8")
            backup_src = root / "backup-src"
            backup_src.mkdir()
            (backup_src / "newapi-postgres.dump").write_text(
                "postgres dump\n", encoding="utf-8"
            )
            (backup_src / "redis-data").mkdir()
            package = root / "backup.tgz"
            subprocess.run(
                ["tar", "-czf", str(package), "."], cwd=backup_src, check=True
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
                env={
                    "ALLOW_UNVERIFIED_LEGACY_BACKUP": "true",
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("WARNING: restoring an unverified historical package", result.stderr)
            self.assertIn("set NEW_API_HOST", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_restore_stop_failure_does_not_clear_volumes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text(
                "\n".join(
                    [
                        "NEW_API_HOST=ai.example.test",
                        "NEW_API_IMAGE_TAG=v1.2.3",
                        "NEW_API_POSTGRES_USER=new_api",
                        "NEW_API_POSTGRES_DB=new_api",
                        "NEW_API_POSTGRES_PASSWORD=postgres-secret",
                        "NEW_API_REDIS_PASSWORD=redis-secret",
                        "NEW_API_SESSION_SECRET=session-secret",
                        "NEW_API_CRYPTO_SECRET=crypto-secret",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            backup_src = root / "backup-src"
            backup_src.mkdir()
            (backup_src / "new-api-postgres.dump").write_text(
                "postgres dump\n", encoding="utf-8"
            )
            (backup_src / "new-api-redis-data").mkdir()
            package = root / "backup.tgz"
            self.write_backup_package(backup_src, package)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == "compose stop new-api" ]]; then
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
            self.assertIn("Failed to stop service before restore: new-api", result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertNotIn(" run ", calls)
            self.assertNotIn(" create ", calls)

    def test_restore_refuses_to_clear_volumes_when_service_remains_running(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text(
                "\n".join(
                    [
                        "NEW_API_HOST=ai.example.test",
                        "NEW_API_IMAGE_TAG=v1.2.3",
                        "NEW_API_POSTGRES_USER=new_api",
                        "NEW_API_POSTGRES_DB=new_api",
                        "NEW_API_POSTGRES_PASSWORD=postgres-secret",
                        "NEW_API_REDIS_PASSWORD=redis-secret",
                        "NEW_API_SESSION_SECRET=session-secret",
                        "NEW_API_CRYPTO_SECRET=crypto-secret",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            backup_src = root / "backup-src"
            backup_src.mkdir()
            (backup_src / "new-api-postgres.dump").write_text(
                "postgres dump\n", encoding="utf-8"
            )
            (backup_src / "new-api-redis-data").mkdir()
            package = root / "backup.tgz"
            self.write_backup_package(backup_src, package)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == "compose stop "* ]]; then
  exit 0
fi
if [[ "$*" == "compose ps --services --filter status=running" ]]; then
  printf '%s\\n' new-api
  exit 0
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
            self.assertIn("Service is still running", result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertNotIn(" run ", calls)
            self.assertNotIn(" create ", calls)

    def test_restore_new_api_only_requires_new_api_runtime_env_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            deployment_dir = root / "sub2api"
            deployment_dir.mkdir()
            (deployment_dir / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (deployment_dir / ".env").write_text(
                "\n".join(
                    [
                        "POSTGRES_USER=sub2api",
                        "POSTGRES_DB=sub2api",
                        "POSTGRES_PASSWORD=sub2api-pw",
                        "REDIS_PASSWORD=sub2api-redis",
                        "NEW_API_HOST=ai.example.com",
                        "NEW_API_IMAGE_TAG=v0.99.0",
                        "NEW_API_POSTGRES_USER=newapi",
                        "NEW_API_POSTGRES_DB=newapi",
                        "NEW_API_POSTGRES_PASSWORD=newapi-pw",
                        "NEW_API_REDIS_PASSWORD=new-api-redis",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )

            backup_src = tmp_root / "backup-src"
            backup_src.mkdir()
            (backup_src / "new-api-postgres.dump").write_text("postgres dump\n", encoding="utf-8")
            (backup_src / "redis-data").mkdir()
            (backup_src / "redis-data" / "dump.rdb").write_text("redis data\n", encoding="utf-8")
            backup_package = tmp_root / "backup.tgz"
            self.write_backup_package(backup_src, backup_package)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(restore_script), str(backup_package), str(deployment_dir)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("set NEW_API_SESSION_SECRET", result.stderr)
            self.assertNotIn("unexpected docker call", result.stderr)

    def test_restore_rejects_unsafe_archive_paths_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text("placeholder=true\n", encoding="utf-8")
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

    def test_restore_rejects_runtime_env_interpolation_without_execution(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            deployment_dir = root / "deployment"
            deployment_dir.mkdir()
            (deployment_dir / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (deployment_dir / ".env").write_text(
                "NEW_API_HOST=\nNEW_API_IMAGE_TAG=\n", encoding="utf-8"
            )

            executed_marker = root / "env-executed"
            backup_src = tmp_root / "backup-src"
            backup_src.mkdir()
            runtime_src = tmp_root / "runtime-src"
            runtime_src.mkdir()
            (runtime_src / ".env").write_text(
                f"AI_HOST=$(touch {executed_marker})\n", encoding="utf-8"
            )
            subprocess.run(
                ["tar", "-czf", str(backup_src / "cliproxy-runtime.tgz"), "."],
                cwd=runtime_src,
                check=True,
            )
            (backup_src / "new-api-postgres.dump").write_text(
                "postgres dump\n", encoding="utf-8"
            )
            (backup_src / "redis-data").mkdir()
            package = tmp_root / "backup.tgz"
            self.write_backup_package(backup_src, package)

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
                [str(restore_script), str(package), str(deployment_dir)],
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

    def test_restore_new_api_only_seeds_new_api_env_from_backup_runtime_env(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            deployment_dir = root / "sub2api"
            deployment_dir.mkdir()
            (deployment_dir / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (deployment_dir / ".env").write_text(
                "\n".join(
                    [
                        "POSTGRES_USER=sub2api",
                        "POSTGRES_DB=sub2api",
                        "POSTGRES_PASSWORD=sub2api-pw",
                        "REDIS_PASSWORD=sub2api-redis",
                        "NEW_API_HOST=",
                        "NEW_API_IMAGE_TAG=",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )

            backup_src = tmp_root / "backup-src"
            backup_src.mkdir()
            runtime_src = tmp_root / "runtime-src"
            runtime_src.mkdir()
            (runtime_src / ".env").write_text(
                "\n".join(
                    [
                        "AI_HOST=ai.backup.example.com",
                        "NEW_API_IMAGE_TAG=v0.99.0",
                        "POSTGRES_USER=backup_newapi",
                        "POSTGRES_DB=backup_newapi_db",
                        "POSTGRES_PASSWORD=backup-postgres-pw",
                        "REDIS_PASSWORD=backup-redis-pw",
                        "NEW_API_SESSION_SECRET=backup-session-secret",
                        "NEW_API_CRYPTO_SECRET=backup-crypto-secret",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            subprocess.run(
                ["tar", "-czf", str(backup_src / "cliproxy-runtime.tgz"), "."],
                cwd=runtime_src,
                check=True,
            )
            (backup_src / "new-api-postgres.dump").write_text("postgres dump\n", encoding="utf-8")
            (backup_src / "redis-data").mkdir()
            (backup_src / "redis-data" / "dump.rdb").write_text("redis data\n", encoding="utf-8")
            backup_package = tmp_root / "backup.tgz"
            self.write_backup_package(backup_src, backup_package)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "compose" && "$*" == *" ps -aq "* ]]; then
  echo "${@: -1}-container"
  exit 0
fi
if [[ "$1" == "inspect" ]]; then
  case "${@: -1}" in
    new-api-postgres-container) echo "new-api-postgres-data-volume" ;;
    new-api-redis-container) echo "new-api-redis-data-volume" ;;
  esac
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_isready "* ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_restore "* ]]; then
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
                [str(restore_script), str(backup_package), str(deployment_dir)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            env_text = (deployment_dir / ".env").read_text(encoding="utf-8")
            for expected in [
                "POSTGRES_USER=sub2api",
                "POSTGRES_DB=sub2api",
                "NEW_API_HOST=ai.backup.example.com",
                "NEW_API_IMAGE_TAG=v0.99.0",
                "NEW_API_POSTGRES_USER=backup_newapi",
                "NEW_API_POSTGRES_DB=backup_newapi_db",
                "NEW_API_POSTGRES_PASSWORD=backup-postgres-pw",
                "NEW_API_REDIS_PASSWORD=backup-redis-pw",
                "NEW_API_SESSION_SECRET=backup-session-secret",
                "NEW_API_CRYPTO_SECRET=backup-crypto-secret",
            ]:
                self.assertIn(expected, env_text)

    def test_restore_seeds_new_api_env_from_current_deployment_backup(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            deployment_dir = root / "deployment"
            deployment_dir.mkdir()
            (deployment_dir / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (deployment_dir / ".env").write_text(
                "\n".join(
                    [
                        "POSTGRES_USER=sub2api",
                        "POSTGRES_DB=sub2api",
                        "POSTGRES_PASSWORD=sub2api-pw",
                        "REDIS_PASSWORD=sub2api-redis",
                        "NEW_API_HOST=",
                        "NEW_API_IMAGE_TAG=",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )

            backup_src = tmp_root / "backup-src"
            backup_src.mkdir()
            runtime_src = tmp_root / "runtime-src"
            runtime_src.mkdir()
            (runtime_src / ".env").write_text(
                "\n".join(
                    [
                        "NEW_API_HOST=ai.current.example.com",
                        "NEW_API_IMAGE_TAG=v1.2.3",
                        "NEW_API_POSTGRES_USER=current_newapi",
                        "NEW_API_POSTGRES_DB=current_newapi_db",
                        "NEW_API_POSTGRES_PASSWORD=current-postgres-pw",
                        "NEW_API_REDIS_PASSWORD=current-redis-pw",
                        "NEW_API_SESSION_SECRET=current-session-secret",
                        "NEW_API_CRYPTO_SECRET=current-crypto-secret",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            subprocess.run(
                ["tar", "-czf", str(backup_src / "deployment-runtime.tgz"), "."],
                cwd=runtime_src,
                check=True,
            )
            (backup_src / "new-api-postgres.dump").write_text(
                "postgres dump\n", encoding="utf-8"
            )
            (backup_src / "redis-data").mkdir()
            (backup_src / "redis-data" / "dump.rdb").write_text(
                "redis data\n", encoding="utf-8"
            )
            backup_package = tmp_root / "backup.tgz"
            self.write_backup_package(backup_src, backup_package)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "compose" && "$*" == *" ps -aq "* ]]; then
  echo "${@: -1}-container"
  exit 0
fi
if [[ "$1" == "inspect" ]]; then
  case "${@: -1}" in
    new-api-postgres-container) echo "new-api-postgres-data-volume" ;;
    new-api-redis-container) echo "new-api-redis-data-volume" ;;
  esac
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_isready "* ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_restore "* ]]; then
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
                [str(restore_script), str(backup_package), str(deployment_dir)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            env_text = (deployment_dir / ".env").read_text(encoding="utf-8")
            for expected in [
                "POSTGRES_USER=sub2api",
                "POSTGRES_DB=sub2api",
                "NEW_API_HOST=ai.current.example.com",
                "NEW_API_IMAGE_TAG=v1.2.3",
                "NEW_API_POSTGRES_USER=current_newapi",
                "NEW_API_POSTGRES_DB=current_newapi_db",
                "NEW_API_POSTGRES_PASSWORD=current-postgres-pw",
                "NEW_API_REDIS_PASSWORD=current-redis-pw",
                "NEW_API_SESSION_SECRET=current-session-secret",
                "NEW_API_CRYPTO_SECRET=current-crypto-secret",
            ]:
                self.assertIn(expected, env_text)

    def test_restore_new_api_only_quotes_seeded_env_values(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            deployment_dir = root / "sub2api"
            deployment_dir.mkdir()
            (deployment_dir / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (deployment_dir / ".env").write_text(
                "\n".join(
                    [
                        "POSTGRES_USER=sub2api",
                        "POSTGRES_DB=sub2api",
                        "POSTGRES_PASSWORD=sub2api-pw",
                        "REDIS_PASSWORD=sub2api-redis",
                        "NEW_API_HOST=",
                        "NEW_API_IMAGE_TAG=",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )

            backup_src = tmp_root / "backup-src"
            backup_src.mkdir()
            runtime_src = tmp_root / "runtime-src"
            runtime_src.mkdir()
            (runtime_src / ".env").write_text(
                "\n".join(
                    [
                        "AI_HOST=ai.backup.example.com",
                        "NEW_API_IMAGE_TAG=v0.99.0",
                        "POSTGRES_USER=backup_newapi",
                        "POSTGRES_DB=backup_newapi_db",
                        "POSTGRES_PASSWORD='backup postgres $pw'",
                        "REDIS_PASSWORD='backup redis $pw'",
                        "NEW_API_SESSION_SECRET='session secret $pw'",
                        "NEW_API_CRYPTO_SECRET='crypto secret \\'quoted\\''",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            subprocess.run(
                ["tar", "-czf", str(backup_src / "cliproxy-runtime.tgz"), "."],
                cwd=runtime_src,
                check=True,
            )
            (backup_src / "new-api-postgres.dump").write_text("postgres dump\n", encoding="utf-8")
            (backup_src / "redis-data").mkdir()
            (backup_src / "redis-data" / "dump.rdb").write_text("redis data\n", encoding="utf-8")
            backup_package = tmp_root / "backup.tgz"
            self.write_backup_package(backup_src, backup_package)

            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "compose" && "$*" == *" ps -aq "* ]]; then
  echo "${@: -1}-container"
  exit 0
fi
if [[ "$1" == "inspect" ]]; then
  case "${@: -1}" in
    new-api-postgres-container) echo "new-api-postgres-data-volume" ;;
    new-api-redis-container) echo "new-api-redis-data-volume" ;;
  esac
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_isready "* ]]; then
  exit 0
fi
if [[ "$1" == "compose" && "$*" == *" exec -T new-api-postgres pg_restore "* ]]; then
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
                [str(restore_script), str(backup_package), str(deployment_dir)],
                cwd=root,
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            env_file = deployment_dir / ".env"
            env_text = env_file.read_text(encoding="utf-8")
            self.assertIn("NEW_API_POSTGRES_PASSWORD='backup postgres $pw'", env_text)
            self.assertIn("NEW_API_REDIS_PASSWORD='backup redis $pw'", env_text)
            self.assertIn("NEW_API_CRYPTO_SECRET='crypto secret \\'quoted\\''", env_text)

            read_result = subprocess.run(
                [
                    "python3",
                    str(root / "scripts" / "read-dotenv.py"),
                    str(env_file),
                    "NEW_API_CRYPTO_SECRET",
                ],
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(read_result.returncode, 0, read_result.stderr)
            self.assertEqual(read_result.stdout, "crypto secret 'quoted'\n")


if __name__ == "__main__":
    unittest.main()
