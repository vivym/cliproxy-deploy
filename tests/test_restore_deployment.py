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
        return restore_script

    def create_backup_package(self, tmp_root, runtime_env_lines=None):
        backup_src = tmp_root / "backup-src"
        backup_src.mkdir()
        runtime_src = tmp_root / "runtime-src"
        runtime_src.mkdir()
        if runtime_env_lines is None:
            runtime_env_lines = [
                "ACME_EMAIL=admin@example.test",
                "SUB2API_HOST=sub2api.example.test",
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
                "NEWAPI_REDIS_PASSWORD=newapi-redis",
                "NEW_API_SESSION_SECRET=session-secret",
                "NEW_API_CRYPTO_SECRET=crypto-secret",
            ]
        (runtime_src / ".env").write_text(
            "\n".join(runtime_env_lines) + "\n",
            encoding="utf-8",
        )
        (runtime_src / "data").mkdir()
        (runtime_src / "data" / "config.yaml").write_text(
            "restored: true\n", encoding="utf-8"
        )
        (runtime_src / "letsencrypt").mkdir()
        (runtime_src / "letsencrypt" / "acme.json").write_text("{}\n", encoding="utf-8")
        subprocess.run(
            ["tar", "-czf", str(backup_src / "deployment-runtime.tgz"), "."],
            cwd=runtime_src,
            check=True,
        )
        (backup_src / "sub2api-postgres.dump").write_text(
            "sub2api dump\n", encoding="utf-8"
        )
        (backup_src / "newapi-postgres.dump").write_text(
            "newapi dump\n", encoding="utf-8"
        )
        for directory, content in [
            ("sub2api-redis-data", "sub2api redis\n"),
            ("redis-data", "newapi redis\n"),
        ]:
            (backup_src / directory).mkdir()
            (backup_src / directory / "dump.rdb").write_text(content, encoding="utf-8")

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
        self.assertIn("newapi-postgres.dump", text)
        self.assertIn("sub2api-redis-data", text)
        self.assertIn("redis-data", text)
        self.assertIn("SHA256SUMS", text)
        self.assertIn("docker-compose.newapi.yml", text)
        self.assertIn("pg_restore", text)
        self.assertIn("Restore completed", text)
        self.assertNotIn("cliproxy", text.lower())

    def test_restore_replaces_runtime_and_both_database_domains(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / "docker-compose.newapi.yml").write_text("services: {}\n", encoding="utf-8")
            (root / ".env").write_text("OLD_ENV=true\n", encoding="utf-8")
            for directory in ["data", "letsencrypt", "postgres_data", "redis_data"]:
                (root / directory).mkdir()
                (root / directory / "stale").write_text("stale\n", encoding="utf-8")
            package = self.create_backup_package(tmp_root)

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
    newapi-postgres-container) echo "newapi-postgres-data-volume" ;;
    newapi-redis-container) echo "newapi-redis-data-volume" ;;
  esac
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
                env={"PATH": f"{bin_dir}:/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Restore completed", result.stdout)
            self.assertIn(
                "restored: true",
                (root / "data" / "config.yaml").read_text(encoding="utf-8"),
            )
            self.assertFalse((root / "postgres_data" / "stale").exists())
            self.assertFalse((root / "redis_data" / "stale").exists())
            self.assertEqual(
                (root / "redis_data" / "dump.rdb").read_text(encoding="utf-8"),
                "sub2api redis\n",
            )
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn(" down", calls)
            self.assertIn("exec -T postgres pg_restore", calls)
            self.assertIn("exec -T newapi-postgres pg_restore", calls)
            self.assertIn("newapi-postgres-data-volume:/target", calls)
            self.assertIn("newapi-redis-data-volume:/target", calls)

    def test_restore_rejects_unsafe_archive_paths_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            restore_script = self.write_script_copy(root)
            (root / ".env").write_text("placeholder=true\n", encoding="utf-8")
            (root / "docker-compose.yml").write_text("services: {}\n", encoding="utf-8")
            (root / "docker-compose.newapi.yml").write_text("services: {}\n", encoding="utf-8")
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

    def test_restore_render_failure_preserves_current_deployment(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            restore_script = self.write_script_copy(root)
            (root / "docker-compose.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / "docker-compose.newapi.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT_ENV=true\n", encoding="utf-8")
            for directory in ["data", "letsencrypt", "postgres_data", "redis_data"]:
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
            for directory in ["data", "letsencrypt", "postgres_data", "redis_data"]:
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
            (root / "docker-compose.newapi.yml").write_text(
                "services: {}\n", encoding="utf-8"
            )
            (root / ".env").write_text("CURRENT_ENV=true\n", encoding="utf-8")
            for directory in ["data", "letsencrypt", "postgres_data", "redis_data"]:
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
