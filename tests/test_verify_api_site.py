import pathlib
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
        self.assertNotIn("Skipping internal CLIProxyAPI reachability check", text)

    def test_script_has_optional_codex_responses_check(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("/v1/responses", text)
        self.assertIn("CODEX_TEST_API_KEY", text)
        self.assertIn("store", text)


if __name__ == "__main__":
    unittest.main()
