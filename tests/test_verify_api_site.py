import os
import pathlib
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "verify-api-site.sh"


class VerifyApiSiteTests(unittest.TestCase):
    def test_script_checks_public_new_api_and_blocks_cliproxy_public(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("source .env", text)
        self.assertIn("AI_HOST", text)
        self.assertIn("/v1/models", text)
        self.assertIn("docker compose exec -T new-api", text)
        self.assertIn("http://cliproxyapi:8317/v1/models", text)
        self.assertIn("CLIPROXY_PUBLIC_HOST", text)
        self.assertIn("CLIPROXY_PUBLIC_HOST:?set CLIPROXY_PUBLIC_HOST", text)
        self.assertIn("must not be publicly reachable", text)
        self.assertNotIn("curl -k", text)
        self.assertNotIn("Skipping internal CLIProxyAPI reachability check", text)

    def test_script_has_optional_codex_responses_check(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("/v1/responses", text)
        self.assertIn("CODEX_TEST_API_KEY", text)
        self.assertIn("store", text)

    def run_script_with_public_mode(self, public_mode):
        with tempfile.TemporaryDirectory() as tmp:
            repo = pathlib.Path(tmp) / "repo"
            scripts = repo / "scripts"
            bin_dir = repo / "bin"
            scripts.mkdir(parents=True)
            bin_dir.mkdir()
            shutil.copy2(SCRIPT, scripts / "verify-api-site.sh")
            (repo / ".env").write_text(
                "\n".join(
                    [
                        "AI_HOST=ai.example.test",
                        "CLIPROXY_PUBLIC_HOST=legacy.example.test",
                        "CLIPROXY_INTERNAL_API_KEY=internal-secret",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            self.write_executable(
                bin_dir / "curl",
                r"""#!/usr/bin/env bash
set -euo pipefail

url=""
for arg in "$@"; do
  case "$arg" in
    http://*|https://*) url="$arg" ;;
  esac
done

if [[ "$url" == "https://legacy.example.test/v1/models" ]]; then
  case "${FAKE_PUBLIC_MODE:?set FAKE_PUBLIC_MODE}" in
    http)
      printf '200'
      exit 0
      ;;
    refused)
      printf '000'
      exit 7
      ;;
    timeout)
      printf '000'
      exit 28
      ;;
    dns)
      printf '000'
      exit 6
      ;;
    tls)
      printf '000'
      exit 60
      ;;
  esac
fi

exit 0
""",
            )
            self.write_executable(
                bin_dir / "docker",
                """#!/usr/bin/env bash
exit 0
""",
            )

            env = os.environ.copy()
            env["PATH"] = f"{bin_dir}{os.pathsep}{env['PATH']}"
            env["FAKE_PUBLIC_MODE"] = public_mode
            return subprocess.run(
                [str(scripts / "verify-api-site.sh")],
                cwd=repo,
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )

    def write_executable(self, path, text):
        path.write_text(textwrap.dedent(text), encoding="utf-8")
        path.chmod(path.stat().st_mode | stat.S_IXUSR)

    def test_public_cliproxy_http_response_fails_verification(self):
        result = self.run_script_with_public_mode("http")

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must not be publicly reachable", result.stderr)
        self.assertIn("HTTP 200", result.stderr)

    def test_public_cliproxy_connection_refused_passes_negative_check(self):
        result = self.run_script_with_public_mode("refused")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("API-site verification checks completed", result.stdout)

    def test_public_cliproxy_timeout_passes_negative_check(self):
        result = self.run_script_with_public_mode("timeout")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("API-site verification checks completed", result.stdout)

    def test_public_cliproxy_dns_failure_fails_verification(self):
        result = self.run_script_with_public_mode("dns")

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("curl exit code 6", result.stderr)
        self.assertIn("HTTP 000", result.stderr)

    def test_public_cliproxy_tls_failure_fails_verification(self):
        result = self.run_script_with_public_mode("tls")

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("curl exit code 60", result.stderr)
        self.assertIn("HTTP 000", result.stderr)

    def test_optional_credentialed_checks_are_skipped_when_keys_unset(self):
        result = self.run_script_with_public_mode("refused")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Skipping /v1/models credentialed check", result.stdout)
        self.assertIn("Skipping /v1/responses check", result.stdout)
        self.assertNotIn("internal-secret", result.stdout)
        self.assertNotIn("internal-secret", result.stderr)


if __name__ == "__main__":
    unittest.main()
