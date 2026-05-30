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
        self.assertIn("docker compose exec -T redis redis-cli", text)
        self.assertIn("docker compose cp redis:/data", text)
        self.assertIn("auths", text)
        self.assertIn(".env", text)
        self.assertIn("config.yaml", text)
        self.assertIn("letsencrypt", text)
        self.assertNotIn(" logs", text)
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
        self.assertIn(
            'if [[ -e "$partial_dest" || -e "$dest" || -e "$partial_package" || -e "$package" || -e "$checksum_tmp" ]]; then',
            text,
        )
        self.assertIn('partial_package="${package}.partial"', text)
        self.assertIn('mv "$partial_package" "$package"', text)

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

    def test_script_rejects_symlink_backup_dir_inside_repo_before_docker_calls(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            in_repo_backup_target = root / "backups-target"
            in_repo_backup_target.mkdir()
            backup_link = tmp_root / "backup-link"
            backup_link.symlink_to(in_repo_backup_target, target_is_directory=True)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker_marker = root / "docker-called"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
touch {docker_marker}
echo "docker should not be called" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={
                    "BACKUP_DIR": str(backup_link),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Refusing to write backups inside repository", result.stderr)
            self.assertFalse(docker_marker.exists())

    def test_script_fails_when_docker_compose_ps_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "letsencrypt").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "compose exec -T" ]]; then
  if [[ "$4" == "redis" ]]; then
    exit 0
  fi
  echo "postgres dump"
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp redis:/data" ]]; then
  mkdir -p "$4"
  echo "redis data" > "$4/appendonly.aof"
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

    def test_script_writes_relative_checksum_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "letsencrypt").mkdir()
            (root / "logs").mkdir()
            (root / "logs" / "request-secret.log").write_text("secret\n", encoding="utf-8")
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "compose exec -T" ]]; then
  if [[ "$4" == "redis" ]]; then
    exit 0
  fi
  echo "postgres dump"
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp redis:/data" ]]; then
  mkdir -p "$4"
  echo "redis data" > "$4/dump.rdb"
  exit 0
fi
if [[ "$1 $2" == "compose ps" ]]; then
  exit 0
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

            self.assertEqual(result.returncode, 0, result.stderr)
            prefix = "Backup package written to "
            self.assertIn(prefix, result.stdout)
            backup_package = pathlib.Path(result.stdout.strip().removeprefix(prefix))
            self.assertEqual(backup_package.suffix, ".tgz")

            extract_dir = tmp_root / "extracted-backup"
            extract_dir.mkdir()
            subprocess.run(
                ["tar", "-xzf", str(backup_package), "-C", str(extract_dir)],
                text=True,
                capture_output=True,
                check=True,
            )

            sums = (extract_dir / "SHA256SUMS").read_text(encoding="utf-8")
            self.assertIn("./newapi-postgres.dump", sums)
            self.assertIn("./cliproxy-runtime.tgz", sums)
            self.assertIn("./redis-data/dump.rdb", sums)
            self.assertNotIn(".partial", sums)
            self.assertNotIn(str(backup_package), sums)
            archive_listing = subprocess.run(
                ["tar", "-tzf", str(extract_dir / "cliproxy-runtime.tgz")],
                text=True,
                capture_output=True,
                check=True,
            ).stdout
            self.assertIn(".env", archive_listing)
            self.assertIn("config.yaml", archive_listing)
            self.assertIn("auths", archive_listing)
            self.assertIn("letsencrypt", archive_listing)
            self.assertNotIn("logs", archive_listing)

    def test_script_does_not_require_unrelated_compose_label_environment(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "letsencrypt").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "compose" && -z "${CLIPROXY_HOST:-}" ]]; then
  echo "error while interpolating services.cliproxyapi.labels.[]: required variable CLIPROXY_HOST is missing a value: set CLIPROXY_HOST" >&2
  exit 1
fi
if [[ "$1 $2" == "compose ps" ]]; then
  exit 0
fi
if [[ "$1 $2 $3" == "compose exec -T" ]]; then
  if [[ "$4" == "redis" ]]; then
    exit 0
  fi
  echo "postgres dump"
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp redis:/data" ]]; then
  mkdir -p "$4"
  echo "redis data" > "$4/dump.rdb"
  exit 0
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

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Backup package written to ", result.stdout)

    def test_script_stops_keeper_while_copying_data(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "letsencrypt").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$1 $2 $3" == "compose exec -T" ]]; then
  if [[ "$4" == "redis" ]]; then
    exit 0
  fi
  echo "postgres dump"
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp redis:/data" ]]; then
  mkdir -p "$4"
  echo "redis data" > "$4/dump.rdb"
  exit 0
fi
if [[ "$1 $2" == "compose ps" ]]; then
  echo "cpa-usage-keeper"
  exit 0
fi
if [[ "$1 $2 $3" == "compose stop cpa-usage-keeper" ]]; then
  exit 0
fi
if [[ "$1 $2 $3" == "compose start cpa-usage-keeper" ]]; then
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp cpa-usage-keeper:/data" ]]; then
  mkdir -p "$4"
  echo "keeper data" > "$4/state.db"
  exit 0
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

            self.assertEqual(result.returncode, 0, result.stderr)
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            stop_index = calls.index("compose stop cpa-usage-keeper")
            copy_index = calls.index(
                next(call for call in calls if call.startswith("compose cp cpa-usage-keeper:/data "))
            )
            start_index = calls.index("compose start cpa-usage-keeper")
            self.assertLess(stop_index, copy_index)
            self.assertLess(copy_index, start_index)

    def test_script_quiesces_write_services_before_snapshot(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "letsencrypt").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$1 $2" == "compose ps" ]]; then
  echo "traefik"
  echo "new-api"
  echo "cliproxyapi"
  echo "cpa-usage-keeper"
  exit 0
fi
if [[ "$1 $2" == "compose stop" ]]; then
  exit 0
fi
if [[ "$1 $2" == "compose start" ]]; then
  exit 0
fi
if [[ "$1 $2 $3" == "compose exec -T" ]]; then
  if [[ "$4" == "redis" ]]; then
    exit 0
  fi
  echo "postgres dump"
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp redis:/data" ]]; then
  mkdir -p "$4"
  echo "redis data" > "$4/dump.rdb"
  exit 0
fi
if [[ "$1 $2 $3" == "compose cp cpa-usage-keeper:/data" ]]; then
  mkdir -p "$4"
  echo "keeper data" > "$4/state.db"
  exit 0
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

            self.assertEqual(result.returncode, 0, result.stderr)
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            snapshot_index = calls.index(
                next(call for call in calls if call.startswith("compose exec -T postgres "))
            )
            stop_services = ["cpa-usage-keeper", "new-api", "cliproxyapi", "traefik"]
            for service in stop_services:
                self.assertLess(calls.index(f"compose stop {service}"), snapshot_index)

            package_index = max(
                index
                for index, call in enumerate(calls)
                if call.startswith("compose cp cpa-usage-keeper:/data ")
                or call.startswith("compose cp redis:/data ")
                or call.startswith("compose exec -T ")
            )
            for service in ["traefik", "cliproxyapi", "new-api", "cpa-usage-keeper"]:
                self.assertGreater(calls.index(f"compose start {service}"), package_index)

    def test_script_restarts_quiesced_services_when_snapshot_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_root = pathlib.Path(tmp)
            root = tmp_root / "repo"
            root.mkdir()
            backup_script = self.write_script_copy(root)
            (root / ".env").write_text(
                "POSTGRES_USER=user\nPOSTGRES_DB=db\nREDIS_PASSWORD=redis-pw\n",
                encoding="utf-8",
            )
            (root / "config.yaml").write_text("config: true\n", encoding="utf-8")
            (root / "auths").mkdir()
            (root / "letsencrypt").mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "docker-calls"
            docker = bin_dir / "docker"
            docker.write_text(
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$1 $2" == "compose ps" ]]; then
  echo "new-api"
  echo "cliproxyapi"
  exit 0
fi
if [[ "$1 $2" == "compose stop" ]]; then
  exit 0
fi
if [[ "$1 $2" == "compose start" ]]; then
  exit 0
fi
if [[ "$1 $2 $3 $4" == "compose exec -T postgres" ]]; then
  echo "pg_dump failed" >&2
  exit 42
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
                    "BACKUP_DIR": str(tmp_root / "external-backups"),
                    "PATH": f"{bin_dir}:/usr/bin:/bin",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 42)
            self.assertIn("pg_dump failed", result.stderr)
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            self.assertIn("compose stop new-api", calls)
            self.assertIn("compose stop cliproxyapi", calls)
            self.assertIn("compose start cliproxyapi", calls)
            self.assertIn("compose start new-api", calls)
            self.assertNotIn("compose start cpa-usage-keeper", calls)


if __name__ == "__main__":
    unittest.main()
