import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "verify-deployment.sh"
DOTENV_READER = ROOT / "scripts" / "read-dotenv.py"


class VerifyDeploymentTests(unittest.TestCase):
    def test_verify_checks_current_public_and_internal_routes(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("set -euo pipefail", text)
        self.assertNotIn("SUB2API_HOST", text)
        self.assertIn("SUB2API_ADMIN_HOST", text)
        self.assertIn("NEW_API_HOST", text)
        self.assertIn("/health", text)
        self.assertIn("/api/status", text)
        self.assertIn("http://sub2api:8080/health", text)
        self.assertIn("http://lark-quota-controller:8080/readyz", text)
        self.assertIn("http://127.0.0.1:3001/api/integrations/v1/principals", text)
        self.assertIn("expected 401", text)
        self.assertNotIn("--spider", text)
        self.assertIn("Lark integration verification checks completed", text)
        self.assertIn("LARK_OAUTH_PUBLIC_ENABLED", text)
        self.assertIn("OAuth routes are disabled", text)
        self.assertIn("expected 404", text)
        self.assertIn("NEW_API_TEST_API_KEY", text)
        self.assertNotIn("SUB2API_TEST_API_KEY", text)
        self.assertNotIn("docker-compose.newapi.yml", text)
        self.assertNotIn("curl -k", text)
        for legacy_term in ["CLIPROXY", "CPA_USAGE_KEEPER", "cliproxyapi"]:
            self.assertNotIn(legacy_term, text)

    def test_verify_succeeds_with_current_services_and_skips_optional_keys(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            scripts = root / "scripts"
            bin_dir = root / "bin"
            scripts.mkdir()
            bin_dir.mkdir()
            script = scripts / "verify-deployment.sh"
            script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
            script.chmod(0o755)
            (scripts / "read-dotenv.py").write_text(
                DOTENV_READER.read_text(encoding="utf-8"), encoding="utf-8"
            )
            (root / ".env").write_text(
                "\n".join(
                    [
                        "SUB2API_ADMIN_HOST=admin.example.test",
                        "NEW_API_HOST=ai.example.test",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  printf '%s\\n' traefik sub2api-postgres sub2api-redis sub2api new-api-postgres new-api-redis new-api
  exit 0
fi
if [[ "$*" == *" exec -T new-api wget "* ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)
            curl = bin_dir / "curl"
            curl.write_text(
                """#!/usr/bin/env bash
if [[ "$*" == *" -w %{http_code} "* ]]; then
  printf '404'
fi
exit 0
""",
                encoding="utf-8",
            )
            curl.chmod(0o755)
            env = os.environ.copy()
            env["PATH"] = f"{bin_dir}{os.pathsep}/usr/bin:/bin"

            result = subprocess.run(
                [str(script)],
                cwd=root,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Deployment verification checks completed", result.stdout)
            self.assertIn("Skipping New API credentialed check", result.stdout)
            self.assertIn("Skipping Lark integration checks", result.stdout)

    def test_verify_checks_lark_profile_when_controller_is_running(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            scripts = root / "scripts"
            bin_dir = root / "bin"
            scripts.mkdir()
            bin_dir.mkdir()
            script = scripts / "verify-deployment.sh"
            script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
            script.chmod(0o755)
            (scripts / "read-dotenv.py").write_text(
                DOTENV_READER.read_text(encoding="utf-8"), encoding="utf-8"
            )
            (root / ".env").write_text(
                "SUB2API_ADMIN_HOST=admin.example.test\nNEW_API_HOST=ai.example.test\n",
                encoding="utf-8",
            )
            docker = bin_dir / "docker"
            docker.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *" ps --services --filter status=running" ]]; then
  printf '%s\\n' traefik sub2api-postgres sub2api-redis sub2api new-api-postgres new-api-redis new-api lark-quota-controller
  exit 0
fi
if [[ "$*" == *"http://127.0.0.1:3001/api/integrations/v1/principals"* ]]; then
  printf '%s\\n' '  HTTP/1.1 401 Unauthorized' >&2
  exit 1
fi
if [[ "$*" == *" exec -T new-api wget "* ]]; then
  exit 0
fi
echo "unexpected docker call: $*" >&2
exit 99
""",
                encoding="utf-8",
            )
            docker.chmod(0o755)
            curl = bin_dir / "curl"
            curl.write_text(
                """#!/usr/bin/env bash
if [[ "$*" == *" -w %{http_code} "* ]]; then
  printf '404'
fi
exit 0
""",
                encoding="utf-8",
            )
            curl.chmod(0o755)
            env = os.environ.copy()
            env["PATH"] = f"{bin_dir}{os.pathsep}/usr/bin:/bin"

            result = subprocess.run(
                [str(script)], cwd=root, env=env, text=True, capture_output=True, check=False
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Lark integration verification checks completed", result.stdout)


if __name__ == "__main__":
    unittest.main()
