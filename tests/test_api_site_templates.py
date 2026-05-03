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
        self.assertIn("Use URL-safe generated values because these passwords are embedded in DSN URLs.", text)
        self.assertIn("openssl rand -hex 24", text)
        self.assertIn("must match config.yaml remote-management.secret-key exactly", text)
        self.assertIn("must match config.yaml api-keys entry exactly", text)
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
        self.assertIn("Must match MANAGEMENT_SECRET in .env exactly", text)
        self.assertIn("Must match CLIPROXY_INTERNAL_API_KEY in .env exactly", text)

    def test_compose_declares_api_site_services(self):
        text = self.read("docker-compose.yml")
        for service in ["new-api:", "postgres:", "redis:", "cpa-usage-keeper:", "cliproxyapi:"]:
            self.assertIn(service, text)
        self.assertIn("traefik.http.routers.new-api.rule=Host(`${AI_HOST:?set AI_HOST}`)", text)
        self.assertIn("traefik.enable=false", text)
        self.assertIn("backend:", text)
        self.assertIn("http://localhost:3000/api/status", text)
        self.assertIn("http://localhost:8317", text)
        self.assertIn("cliproxyapi:\n        condition: service_healthy", text)

    def test_cliproxy_public_override_is_template_only(self):
        text = self.read("docker-compose.cliproxy-public.override.yml.template")
        self.assertIn("TEMPORARY MAINTENANCE ONLY", text)
        self.assertIn("Decommission by: YYYY-MM-DD HH:MM TZ", text)
        self.assertIn("cliproxyapi:", text)
        self.assertIn("traefik.enable=true", text)
        self.assertNotIn("traefik.http.routers.cliproxyapi.rule", text)


if __name__ == "__main__":
    unittest.main()
