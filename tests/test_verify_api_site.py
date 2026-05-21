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
    def test_script_checks_public_new_api_cliproxy_and_keeper(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("source .env", text)
        self.assertIn("AI_HOST", text)
        self.assertIn("CLIPROXY_HOST", text)
        self.assertIn("CPA_USAGE_KEEPER_HOST", text)
        self.assertIn("/v1/models", text)
        self.assertIn("/management.html", text)
        self.assertIn("docker compose exec -T new-api", text)
        self.assertIn("http://cliproxyapi:8317/v1/models", text)
        self.assertIn("/api/v1/usage/overview", text)
        self.assertIn("must require authentication", text)
        self.assertNotIn("CLIPROXY_PUBLIC_HOST", text)
        self.assertNotIn("curl -k", text)
        self.assertNotIn("Skipping internal CLIProxyAPI reachability check", text)

    def test_script_has_optional_codex_responses_check(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("/v1/responses", text)
        self.assertIn("CODEX_TEST_API_KEY", text)
        self.assertIn("store", text)

    def run_script_with_keeper_status(self, keeper_status):
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
                        "CLIPROXY_HOST=cliproxy.example.test",
                        "CPA_USAGE_KEEPER_HOST=keeper.example.test",
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
  exit 99
fi

if [[ "$url" == "https://keeper.example.test/api/v1/usage/overview" ]]; then
  case "${FAKE_KEEPER_STATUS:?set FAKE_KEEPER_STATUS}" in
    401)
      printf '401'
      exit 0
      ;;
    200)
      printf '200'
      exit 0
      ;;
    000)
      printf '000'
      exit 7
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
            env["FAKE_KEEPER_STATUS"] = keeper_status
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

    def test_keeper_unauthenticated_401_passes_public_check(self):
        result = self.run_script_with_keeper_status("401")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("API-site verification checks completed", result.stdout)

    def test_keeper_unauthenticated_success_fails_public_check(self):
        result = self.run_script_with_keeper_status("200")

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("CPA Usage Keeper must require authentication", result.stderr)
        self.assertIn("HTTP 200", result.stderr)

    def test_optional_credentialed_checks_are_skipped_when_keys_unset(self):
        result = self.run_script_with_keeper_status("401")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Skipping /v1/models credentialed check", result.stdout)
        self.assertIn("Skipping /v1/responses check", result.stdout)
        self.assertNotIn("internal-secret", result.stdout)
        self.assertNotIn("internal-secret", result.stderr)


if __name__ == "__main__":
    unittest.main()
