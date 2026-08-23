import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "verify-lark-secret-permissions.sh"


class LarkSecretPermissionTests(unittest.TestCase):
    files = [
        "shared/lark_integration_secret",
        "controller/lark_app_secret",
        "controller/lark_verification_token",
        "controller/lark_encrypt_key",
        "controller/lark_grant_payload_keyring",
        "controller/new_api_bridge_client_secret",
        "new-api/lark_correction_secret",
    ]

    def prepare_secrets(self, root):
        for directory in ["shared", "controller", "new-api"]:
            path = root / directory
            path.mkdir()
            path.chmod(0o700)
        for relative_path in self.files:
            path = root / relative_path
            secret = relative_path.replace("/", "-")
            path.write_text(f"test-only-{secret}\n", encoding="utf-8")
            path.chmod(0o600)

    def run_check(
        self, root, owner=None, include_correction=False, include_next=False
    ):
        expected_owner = owner or f"{os.getuid()}:{os.getgid()}"
        command = [str(SCRIPT)]
        if include_correction:
            command.append("--include-correction")
        if include_next:
            command.append("--include-next")
        command.extend([str(root), expected_owner])
        return subprocess.run(
            command,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_accepts_exact_owner_and_modes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)

            result = self.run_check(root)

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("ownership and modes verified", result.stdout)
            self.assertNotIn("test-only-secret", result.stdout + result.stderr)

    def test_rejects_group_readable_secret(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            (root / self.files[0]).chmod(0o640)

            result = self.run_check(root)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("expected 600, got 640", result.stderr)

    def test_rejects_owner_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)

            result = self.run_check(root, f"{os.getuid() + 1}:{os.getgid()}")

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Unreadable owner", result.stderr)

    def test_correction_secret_is_optional_for_the_long_running_profile(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            (root / "new-api" / "lark_correction_secret").unlink()

            base_result = self.run_check(root)
            ops_result = self.run_check(root, include_correction=True)

            self.assertEqual(base_result.returncode, 0, base_result.stderr)
            self.assertNotEqual(ops_result.returncode, 0)
            self.assertIn("lark_correction_secret", ops_result.stderr)

    def test_rejects_reused_integration_secret_for_correction(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            integration = root / "shared" / "lark_integration_secret"
            correction = root / "new-api" / "lark_correction_secret"
            correction.write_bytes(integration.read_bytes())
            correction.chmod(0o600)

            result = self.run_check(root, include_correction=True)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("must be independent", result.stderr)

    def test_rejects_same_effective_secret_with_different_line_endings(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            integration = root / "shared" / "lark_integration_secret"
            correction = root / "new-api" / "lark_correction_secret"
            integration.write_bytes(b"same-effective-secret-value-123456\n")
            correction.write_bytes(b"same-effective-secret-value-123456\r\n")
            integration.chmod(0o600)
            correction.chmod(0o600)

            result = self.run_check(root, include_correction=True)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("must be independent", result.stderr)

    def test_rejects_secret_that_runtime_would_not_accept(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            integration = root / "shared" / "lark_integration_secret"
            integration.write_bytes(b"printable-secret-value-that-is-long-enough\n\n")
            integration.chmod(0o600)

            result = self.run_check(root)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("one printable ASCII token", result.stderr)
            self.assertNotIn("printable-secret-value", result.stderr)

    def test_rejects_short_correction_secret(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            correction = root / "new-api" / "lark_correction_secret"
            correction.write_bytes(b"too-short\n")
            correction.chmod(0o600)

            result = self.run_check(root, include_correction=True)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Correction secret must be", result.stderr)
            self.assertNotIn("too-short", result.stderr)

    def test_next_secret_is_optional_but_required_during_rotation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)

            base_result = self.run_check(root)
            rotation_result = self.run_check(root, include_next=True)

            self.assertEqual(base_result.returncode, 0, base_result.stderr)
            self.assertNotEqual(rotation_result.returncode, 0)
            self.assertIn("lark_integration_secret_next", rotation_result.stderr)

    def test_correction_must_differ_from_next_secret(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / "secrets"
            root.mkdir()
            self.prepare_secrets(root)
            correction = root / "new-api" / "lark_correction_secret"
            next_secret = root / "shared" / "lark_integration_secret_next"
            next_secret.write_bytes(correction.read_bytes())
            next_secret.chmod(0o600)

            result = self.run_check(root, include_correction=True)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Correction and next integration", result.stderr)


if __name__ == "__main__":
    unittest.main()
