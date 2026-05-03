import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]


class ApiSiteTemplateTests(unittest.TestCase):
    def read(self, path):
        return (ROOT / path).read_text(encoding="utf-8")

    def test_env_example_uses_ai_host_and_required_secrets(self):
        text = self.read(".env.example")
        self.assertIn("AI_HOST=ai.x2r.store", text)
        self.assertNotIn("API_HOST=cliproxy.x2r.store", text)
        for name in [
            "NEW_API_IMAGE_TAG=",
            "POSTGRES_PASSWORD=",
            "REDIS_PASSWORD=",
            "NEW_API_SESSION_SECRET=",
            "NEW_API_CRYPTO_SECRET=",
            "CLIPROXY_INTERNAL_API_KEY=",
            "CPA_USAGE_KEEPER_IMAGE=",
            "CPA_USAGE_KEEPER_IMAGE_TAG=",
            "BACKUP_DIR=",
        ]:
            self.assertIn(name, text)

    def test_cliproxy_config_template_is_production_safe(self):
        text = self.read("config.yaml.template")
        self.assertIn("disable-control-panel: true", text)
        self.assertIn("request-log: false", text)
        self.assertIn("redis-usage-queue-retention-seconds: 3600", text)
        self.assertIn("replace-with-internal-new-api-channel-key", text)

    def test_compose_declares_api_site_services(self):
        text = self.read("docker-compose.yml")
        for service in ["new-api:", "postgres:", "redis:", "cpa-usage-keeper:", "cliproxyapi:"]:
            self.assertIn(service, text)
        self.assertIn("traefik.http.routers.new-api.rule=Host(`${AI_HOST:?set AI_HOST}`)", text)
        self.assertIn("traefik.enable=false", text)
        self.assertIn("backend:", text)


if __name__ == "__main__":
    unittest.main()
