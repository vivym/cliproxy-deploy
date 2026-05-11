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
            "R2_ACCOUNT_ID=",
            "R2_BUCKET=",
            "R2_ACCESS_KEY_ID=",
            "R2_SECRET_ACCESS_KEY=",
            "CLIPROXY_LOG_ARCHIVE_R2_PREFIX=",
            "CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL=1",
            "CLIPROXY_LOG_ARCHIVE_NICE=19",
            "CLIPROXY_LOG_ARCHIVE_IONICE_IDLE=true",
            "CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT=",
        ]:
            self.assertIn(name, text)

    def test_log_archive_script_is_documented(self):
        readme = self.read("README.md")
        runbook = self.read("docs/api-site-runbook.md")
        archive_runbook = self.read("docs/cliproxy-log-archive-r2-runbook.md")
        for text in [readme, runbook, archive_runbook]:
            self.assertIn("scripts/archive-cliproxy-logs.sh", text)
            self.assertIn("Cloudflare R2", text)
            self.assertIn("gzip -1", text)
            self.assertIn("CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT", text)
            self.assertIn("CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1", text)
        self.assertIn("docs/cliproxy-log-archive-r2-runbook.md", readme)
        self.assertIn("docs/cliproxy-log-archive-r2-runbook.md", runbook)

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
        self.assertIn("127.0.0.1:8317:8317", text)
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
