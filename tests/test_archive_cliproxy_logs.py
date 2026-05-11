import os
import pathlib
import subprocess
import tempfile
import time
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "archive-cliproxy-logs.sh"


class ArchiveCliproxyLogsTests(unittest.TestCase):
    def write_script_copy(self, root):
        scripts = root / "scripts"
        scripts.mkdir()
        archive_script = scripts / "archive-cliproxy-logs.sh"
        archive_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
        archive_script.chmod(0o755)
        return archive_script

    def base_env(self, root, bin_dir):
        return {
            "PATH": f"{bin_dir}:/usr/bin:/bin",
            "R2_ACCOUNT_ID": "account",
            "R2_BUCKET": "bucket",
            "R2_ACCESS_KEY_ID": "access",
            "R2_SECRET_ACCESS_KEY": "secret",
            "CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES": "0",
            "CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS": "1",
            "CLIPROXY_LOG_ARCHIVE_DIR": str(root / "logs"),
        }

    def write_fake_aws(self, bin_dir, calls_file, fail=False):
        aws = bin_dir / "aws"
        aws.write_text(
            f"""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> {calls_file}
if [[ "{'yes' if fail else 'no'}" == "yes" ]]; then
  exit 17
fi
exit 0
""",
            encoding="utf-8",
        )
        aws.chmod(0o755)

    def write_fake_compression_tools(self, bin_dir, calls_file):
        gzip = bin_dir / "gzip"
        gzip.write_text(
            f"""#!/usr/bin/env bash
set -euo pipefail
printf 'gzip %s\\n' "$*" >> {calls_file}
file="${{@: -1}}"
cp -- "$file" "${{file}}.gz"
rm -f -- "$file"
""",
            encoding="utf-8",
        )
        gzip.chmod(0o755)
        nice = bin_dir / "nice"
        nice.write_text(
            f"""#!/usr/bin/env bash
set -euo pipefail
printf 'nice %s\\n' "$*" >> {calls_file}
if [[ "${{1:-}}" == "-n" ]]; then
  shift 2
fi
exec "$@"
""",
            encoding="utf-8",
        )
        nice.chmod(0o755)
        cpulimit = bin_dir / "cpulimit"
        cpulimit.write_text(
            f"""#!/usr/bin/env bash
set -euo pipefail
printf 'cpulimit %s\\n' "$*" >> {calls_file}
while [[ $# -gt 0 && "$1" != "--" ]]; do
  shift
done
if [[ "${{1:-}}" == "--" ]]; then
  shift
fi
exec "$@"
""",
            encoding="utf-8",
        )
        cpulimit.chmod(0o755)

    def make_old_enough(self, path):
        old = time.time() - 2 * 60
        os.utime(path, (old, old))

    def test_script_exists_and_documents_r2_configuration(self):
        self.assertTrue(SCRIPT.exists())
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -euo pipefail", text)
        self.assertIn("R2_ACCOUNT_ID", text)
        self.assertIn("R2_BUCKET", text)
        self.assertIn("aws s3 cp", text)
        self.assertIn('CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL:-1', text)
        self.assertIn('CLIPROXY_LOG_ARCHIVE_NICE:-19', text)

    def test_compresses_old_request_logs_and_uploads_to_r2(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            script = self.write_script_copy(root)
            logs = root / "logs"
            logs.mkdir()
            request_log = logs / "request-1.log"
            request_log.write_text("full request body\n", encoding="utf-8")
            self.make_old_enough(request_log)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "aws-calls"
            self.write_fake_aws(bin_dir, calls_file)

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=self.base_env(root, bin_dir),
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            gz = logs / "request-1.log.gz"
            self.assertTrue(gz.exists())
            self.assertFalse(request_log.exists())
            self.assertTrue((logs / "request-1.log.gz.uploaded").exists())
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn("s3 cp", calls)
            self.assertIn(str(gz), calls)
            self.assertIn("s3://bucket/cliproxy-logs/request-1.log.gz", calls)
            self.assertIn("--endpoint-url https://account.r2.cloudflarestorage.com", calls)

    def test_compression_defaults_to_low_cpu_wrappers(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            script = self.write_script_copy(root)
            logs = root / "logs"
            logs.mkdir()
            request_log = logs / "request-low-cpu.log"
            request_log.write_text("full request body\n", encoding="utf-8")
            self.make_old_enough(request_log)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "tool-calls"
            self.write_fake_aws(bin_dir, calls_file)
            self.write_fake_compression_tools(bin_dir, calls_file)
            env = self.base_env(root, bin_dir)
            env["CLIPROXY_LOG_ARCHIVE_IONICE_IDLE"] = "false"

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn(f"nice -n 19 gzip -1n -- {request_log}", calls)
            self.assertIn(f"gzip -1n -- {request_log}", calls)

    def test_optional_cpulimit_wraps_compression(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            script = self.write_script_copy(root)
            logs = root / "logs"
            logs.mkdir()
            request_log = logs / "request-cpulimit.log"
            request_log.write_text("full request body\n", encoding="utf-8")
            self.make_old_enough(request_log)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "tool-calls"
            self.write_fake_aws(bin_dir, calls_file)
            self.write_fake_compression_tools(bin_dir, calls_file)
            env = self.base_env(root, bin_dir)
            env["CLIPROXY_LOG_ARCHIVE_IONICE_IDLE"] = "false"
            env["CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT"] = "25"

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            calls = calls_file.read_text(encoding="utf-8")
            self.assertIn(
                f"cpulimit -l 25 -- nice -n 19 gzip -1n -- {request_log}",
                calls,
            )

    def test_skips_recent_uncompressed_logs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            script = self.write_script_copy(root)
            logs = root / "logs"
            logs.mkdir()
            request_log = logs / "request-new.log"
            request_log.write_text("still fresh\n", encoding="utf-8")
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "aws-calls"
            self.write_fake_aws(bin_dir, calls_file)
            env = self.base_env(root, bin_dir)
            env["CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES"] = "30"

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(request_log.exists())
            self.assertFalse((logs / "request-new.log.gz").exists())
            self.assertFalse(calls_file.exists())

    def test_upload_failure_keeps_gz_without_uploaded_marker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            script = self.write_script_copy(root)
            logs = root / "logs"
            logs.mkdir()
            gz = logs / "request-2.log.gz"
            gz.write_bytes(b"compressed")
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "aws-calls"
            self.write_fake_aws(bin_dir, calls_file, fail=True)

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=self.base_env(root, bin_dir),
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(gz.exists())
            self.assertFalse((logs / "request-2.log.gz.uploaded").exists())

    def test_deletes_uploaded_gz_after_local_retention(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            script = self.write_script_copy(root)
            logs = root / "logs"
            logs.mkdir()
            gz = logs / "request-old.log.gz"
            marker = logs / "request-old.log.gz.uploaded"
            gz.write_bytes(b"compressed")
            marker.write_text("s3://bucket/cliproxy-logs/request-old.log.gz\n", encoding="utf-8")
            old = time.time() - 2 * 24 * 60 * 60
            os.utime(marker, (old, old))
            bin_dir = root / "bin"
            bin_dir.mkdir()
            calls_file = root / "aws-calls"
            self.write_fake_aws(bin_dir, calls_file)

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=self.base_env(root, bin_dir),
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse(gz.exists())
            self.assertFalse(marker.exists())
            self.assertFalse(calls_file.exists())


if __name__ == "__main__":
    unittest.main()
