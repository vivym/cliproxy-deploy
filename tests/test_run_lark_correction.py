import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "run-lark-correction.sh"


class RunLarkCorrectionTests(unittest.TestCase):
    def prepare_runtime(self, root, docker_body):
        scripts = root / "scripts"
        bin_dir = root / "bin"
        ops_dir = root / "lark-runtime" / "ops"
        scripts.mkdir(parents=True)
        bin_dir.mkdir()
        ops_dir.mkdir(parents=True)

        runner = scripts / "run-lark-correction.sh"
        runner.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        runner.chmod(0o755)
        verifier = scripts / "verify-lark-secret-permissions.sh"
        verifier.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
        verifier.chmod(0o755)

        docker = bin_dir / "docker"
        docker.write_text(docker_body, encoding="utf-8")
        docker.chmod(0o755)
        return runner, bin_dir, ops_dir

    def run_runner(self, runner, bin_dir, *args):
        env = os.environ.copy()
        env["PATH"] = f"{bin_dir}{os.pathsep}/usr/bin:/bin"
        command_args = list(args) if args else ["--apply"]
        return subprocess.run(
            [str(runner), *command_args],
            cwd=runner.parents[1],
            env=env,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_refuses_a_running_primary_even_if_it_is_unhealthy(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            marker = root / "docker-mutated"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  printf '%s\\n' new-api
  exit 0
fi
touch {marker}
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("service is running: new-api", result.stderr)
            self.assertFalse(marker.exists())
            self.assertFalse((ops_dir / "maintenance.lock").exists())

    def test_runs_inside_lock_and_removes_endpoint_before_unlock(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            calls_file = root / "docker-calls"
            ops_dir = root / "lark-runtime" / "ops"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  exit 0
fi
if [[ "$*" == *" up -d --wait --wait-timeout 60 new-api-correction-endpoint" ]]; then
  test "$(cat {ops_dir / 'maintenance.lock/mode'})" = correction
  test "$(stat -f %Lp {ops_dir / 'maintenance.lock'} 2>/dev/null || stat -c %a {ops_dir / 'maintenance.lock'})" = 755
  test "$(stat -f %Lp {ops_dir / 'maintenance.lock/mode'} 2>/dev/null || stat -c %a {ops_dir / 'maintenance.lock/mode'})" = 644
  exit 0
fi
if [[ "$*" == *" run --name new-api-lark-correction-ops --rm --no-deps lark-correction "* ]]; then
  test "$(cat {ops_dir / 'maintenance.lock/mode'})" = correction
  printf '%s\\n' '{{"mode":"pending"}}'
  exit 0
fi
if [[ "$*" == *" rm -f new-api-correction-endpoint"* || "$*" == *" rm -sf new-api-correction-endpoint"* ]]; then
  exit 0
fi
if [[ "$*" == *" ps -aq new-api-correction-endpoint"* ]]; then
  exit 0
fi
if [[ "$*" == *"container rm -f new-api-lark-correction-ops"* || "$*" == *"container ls -aq --filter name=^/new-api-lark-correction-ops$"* ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir)

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('"mode":"pending"', result.stdout)
            self.assertFalse((ops_dir / "maintenance.lock").exists())
            calls = calls_file.read_text(encoding="utf-8").splitlines()
            run_index = next(i for i, call in enumerate(calls) if " run " in call)
            cleanup_index = next(
                i for i, call in enumerate(calls) if " rm -sf " in call
            )
            self.assertLess(run_index, cleanup_index)

    def test_service_starting_during_lock_acquisition_aborts_before_endpoint(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            calls_file = root / "docker-calls"
            count_file = root / "ps-count"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  count=0
  if [[ -f {count_file} ]]; then
    count="$(cat {count_file})"
  fi
  count=$((count + 1))
  printf '%s\\n' "$count" > {count_file}
  if [[ "$count" -eq 2 ]]; then
    printf '%s\\n' new-api
  fi
  exit 0
fi
if [[ "$*" == *" rm -sf new-api-correction-endpoint"* ]]; then
  exit 0
fi
if [[ "$*" == *" ps -aq new-api-correction-endpoint"* ]]; then
  exit 0
fi
if [[ "$*" == *"container rm -f new-api-lark-correction-ops"* || "$*" == *"container ls -aq --filter name=^/new-api-lark-correction-ops$"* ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("service is running: new-api", result.stderr)
            self.assertFalse((ops_dir / "maintenance.lock").exists())
            calls = calls_file.read_text(encoding="utf-8")
            self.assertNotIn(" up ", calls)
            self.assertNotIn(" run ", calls)

    def test_cli_failure_still_removes_endpoint_and_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            calls_file = root / "docker-calls"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  *" ps --services --filter status=running"*) exit 0 ;;
  *" up -d --wait --wait-timeout 60 new-api-correction-endpoint"*) exit 0 ;;
  *" run --name new-api-lark-correction-ops --rm --no-deps lark-correction "*) exit 42 ;;
  *" rm -f new-api-correction-endpoint"*|*" rm -sf new-api-correction-endpoint"*) exit 0 ;;
  *" ps -aq new-api-correction-endpoint"*) exit 0 ;;
  *"container rm -f new-api-lark-correction-ops"*|*"container ls -aq --filter name=^/new-api-lark-correction-ops$"*) exit 0 ;;
esac
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir)

            self.assertEqual(result.returncode, 42)
            self.assertFalse((ops_dir / "maintenance.lock").exists())
            self.assertIn(
                "rm -sf new-api-correction-endpoint",
                calls_file.read_text(encoding="utf-8"),
            )

    def test_list_pending_uses_readonly_service_without_secret_or_endpoint(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            calls_file = root / "docker-calls"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  *" ps --services --filter status=running"*) exit 0 ;;
  *"volume inspect new-api-lark-controller-data"*) exit 0 ;;
  *" run --name new-api-lark-correction-readonly-ops --rm --no-deps lark-correction-readonly "*)
    test "$(cat {root / 'lark-runtime/ops/maintenance.lock/mode'})" = readonly
    printf '%s\\n' '{{"mode":"pending"}}'
    exit 0
    ;;
  *"container rm -f new-api-lark-correction-readonly-ops"*|\
  *"container ls -aq --filter name=^/new-api-lark-correction-readonly-ops$"*) exit 0 ;;
esac
echo "unexpected docker call: $*" >&2
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir, "--list-pending")

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('"mode":"pending"', result.stdout)
            self.assertFalse((ops_dir / "maintenance.lock").exists())
            self.assertFalse((ops_dir / "maintenance.session").exists())
            calls = calls_file.read_text(encoding="utf-8")
            self.assertNotIn("new-api-correction-endpoint", calls)
            self.assertNotIn("lark_correction_secret", calls)

    def test_list_pending_refuses_an_existing_maintenance_session_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            marker = root / "docker-called"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"#!/usr/bin/env bash\ntouch {marker}\nexit 99\n",
            )
            (ops_dir / "maintenance.session").mkdir()

            result = self.run_runner(runner, bin_dir, "--list-pending")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("maintenance session", result.stderr)
            self.assertFalse(marker.exists())
            self.assertTrue((ops_dir / "maintenance.session").is_dir())

    def test_boundary_marker_permission_failure_releases_session_and_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                """#!/usr/bin/env bash
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  exit 0
fi
exit 99
""",
            )
            chmod = bin_dir / "chmod"
            chmod.write_text(
                """#!/usr/bin/env bash
if [[ "$1" == "644" && "$2" == */maintenance.lock/mode ]]; then
  exit 42
fi
exec /bin/chmod "$@"
""",
                encoding="utf-8",
            )
            chmod.chmod(0o755)

            result = self.run_runner(runner, bin_dir)

            self.assertEqual(result.returncode, 42, result.stderr)
            self.assertFalse((ops_dir / "maintenance.lock").exists())
            self.assertFalse((ops_dir / "maintenance.session").exists())

    def test_readonly_boundary_marker_failure_releases_session_and_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                "#!/usr/bin/env bash\nexit 99\n",
            )
            chmod = bin_dir / "chmod"
            chmod.write_text(
                """#!/usr/bin/env bash
if [[ "$1" == "644" && "$2" == */maintenance.lock/mode ]]; then
  exit 42
fi
exec /bin/chmod "$@"
""",
                encoding="utf-8",
            )
            chmod.chmod(0o755)

            result = self.run_runner(runner, bin_dir, "--list-pending")

            self.assertEqual(result.returncode, 42, result.stderr)
            self.assertFalse((ops_dir / "maintenance.lock").exists())
            self.assertFalse((ops_dir / "maintenance.session").exists())

    def test_list_pending_retains_readonly_boundary_when_container_cleanup_is_uncertain(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                """#!/usr/bin/env bash
case "$*" in
  *" ps --services --filter status=running"*) exit 0 ;;
  *"volume inspect new-api-lark-controller-data"*) exit 0 ;;
  *" run --name new-api-lark-correction-readonly-ops --rm --no-deps lark-correction-readonly "*) exit 42 ;;
  *"container rm -f new-api-lark-correction-readonly-ops"*) exit 1 ;;
  *"container ls -aq --filter name=^/new-api-lark-correction-readonly-ops$"*)
    printf '%s\n' uncertain-readonly-container
    exit 0
    ;;
esac
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir, "--list-pending")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("readonly correction container", result.stderr.lower())
            self.assertEqual(
                (ops_dir / "maintenance.lock/mode").read_text(encoding="utf-8"),
                "readonly\n",
            )
            self.assertTrue((ops_dir / "maintenance.session").is_dir())

    def test_list_pending_refuses_a_running_controller_before_readonly_container(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            marker = root / "docker-mutated"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  printf '%s\\n' lark-quota-controller
  exit 0
fi
touch {marker}
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir, "--list-pending")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("service is running: lark-quota-controller", result.stderr)
            self.assertFalse(marker.exists())
            self.assertFalse((ops_dir / "maintenance.lock").exists())

    def test_cleanup_failure_retains_lock_and_returns_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            calls_file = root / "docker-calls"
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
case "$*" in
  *" ps --services --filter status=running"*) exit 0 ;;
  *" up -d --wait --wait-timeout 60 new-api-correction-endpoint"*) exit 0 ;;
  *" run --name new-api-lark-correction-ops --rm --no-deps lark-correction "*) exit 0 ;;
  *" rm -f new-api-correction-endpoint"*) exit 0 ;;
  *" rm -sf new-api-correction-endpoint"*) exit 1 ;;
  *"container rm -f new-api-lark-correction-ops"*|*"container ls -aq --filter name=^/new-api-lark-correction-ops$"*) exit 0 ;;
  *" ps -aq new-api-correction-endpoint"*)
    printf '%s\\n' still-running-container
    exit 0
    ;;
esac
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir)

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue((ops_dir / "maintenance.lock").exists())
            self.assertEqual(
                (ops_dir / "maintenance.lock" / "mode").read_text(encoding="utf-8"),
                "correction\n",
            )
            self.assertIn("maintenance lock retained", result.stderr)
            self.assertIn("rmdir", result.stderr)

    def test_cli_cleanup_failure_also_retains_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            runner, bin_dir, ops_dir = self.prepare_runtime(
                root,
                """#!/usr/bin/env bash
case "$*" in
  *" ps --services --filter status=running"*) exit 0 ;;
  *" up -d --wait --wait-timeout 60 new-api-correction-endpoint"*) exit 0 ;;
  *" run --name new-api-lark-correction-ops --rm --no-deps lark-correction "*) exit 42 ;;
  *" rm -f new-api-correction-endpoint"*|*" rm -sf new-api-correction-endpoint"*) exit 0 ;;
  *"container rm -f new-api-lark-correction-ops"*) exit 1 ;;
  *"container ls -aq --filter name=^/new-api-lark-correction-ops$"*)
    printf '%s\\n' uncertain-cli-container
    exit 0
    ;;
esac
exit 99
""",
            )

            result = self.run_runner(runner, bin_dir)

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue((ops_dir / "maintenance.lock").exists())
            self.assertEqual(
                (ops_dir / "maintenance.lock" / "mode").read_text(encoding="utf-8"),
                "correction\n",
            )
            self.assertIn("correction CLI removal", result.stderr)


if __name__ == "__main__":
    unittest.main()
