import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]


class DeploymentTemplateTests(unittest.TestCase):
    def test_root_compose_is_the_complete_new_api_deployment(self):
        text = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")

        self.assertIn("name: new-api", text)
        for service in [
            "traefik:",
            "sub2api-postgres:",
            "sub2api-redis:",
            "sub2api:",
            "new-api-postgres:",
            "new-api-redis:",
            "new-api:",
        ]:
            self.assertIn(service, text)
        self.assertIn("name: new-api-edge", text)
        self.assertIn("name: new-api-sub2api-data", text)
        self.assertIn("name: new-api-data", text)
        self.assertIn("name: new-api-postgres-data", text)
        self.assertIn("name: new-api-redis-data", text)
        self.assertIn("traefik.http.routers.new-api.rule", text)
        self.assertIn("traefik.http.routers.sub2api-admin.rule", text)
        self.assertIn("!PathPrefix(`/v1`)", text)
        self.assertNotIn("traefik.http.routers.sub2api-api.rule", text)
        self.assertNotIn("cliproxyapi:", text)
        self.assertNotIn("cpa-usage-keeper:", text)
        self.assertFalse((ROOT / "docker-compose.newapi.yml").exists())

    def test_environment_template_describes_new_api_entry_and_sub2api_upstream(self):
        env_example = (ROOT / ".env.example").read_text(encoding="utf-8")

        for variable in [
            "NEW_API_HOST=",
            "NEW_API_POSTGRES_PASSWORD=",
            "NEW_API_REDIS_PASSWORD=",
            "SUB2API_POSTGRES_PASSWORD=",
            "SUB2API_REDIS_PASSWORD=",
            "SUB2API_ADMIN_HOST=",
            "BACKUP_DIR=",
            "NEW_API_TEST_API_KEY=",
        ]:
            self.assertIn(variable, env_example)

        self.assertNotIn("SUB2API_HOST=", env_example)
        self.assertNotIn("SUB2API_TEST_API_KEY=", env_example)
        self.assertNotIn("NEWAPI_", env_example)
        self.assertFalse((ROOT / "sub2api").exists())

    def test_readme_documents_only_the_current_root_deployment(self):
        text = (ROOT / "README.md").read_text(encoding="utf-8")

        self.assertIn("# New API gateway deployment", text)
        self.assertIn("docker compose up -d", text)
        self.assertIn("scripts/backup-deployment.sh", text)
        self.assertIn("scripts/restore-deployment.sh", text)
        self.assertIn("scripts/restore-new-api.sh", text)
        self.assertIn("name: new-api", text)
        for legacy_term in [
            "../scripts/",
            "CLIProxyAPI",
            "CPA Usage Keeper",
            "cliproxy-runtime.tgz",
            "docker-compose.newapi.yml",
            "Sub2API + New API deployment",
        ]:
            self.assertNotIn(legacy_term, text)

    def test_lark_architecture_targets_the_single_root_compose(self):
        architecture = ROOT / "docs" / "architecture" / "lark-entitlement-integration.md"
        text = architecture.read_text(encoding="utf-8")

        self.assertIn("`docker-compose.yml`", text)
        self.assertNotIn("`docker-compose.newapi.yml`", text)
        self.assertEqual(
            list((ROOT / "docs" / "superpowers").rglob("*.md")),
            [],
        )

    def test_new_api_restore_defaults_to_the_root_deployment(self):
        script = ROOT / "scripts" / "restore-new-api.sh"
        text = script.read_text(encoding="utf-8")

        self.assertIn(
            "Usage: scripts/restore-new-api.sh BACKUP_PACKAGE [DEPLOYMENT_DIR]",
            text,
        )
        self.assertIn('deployment_dir="${2:-${repo_root}}"', text)
        self.assertNotIn('repo_root}/sub2api', text)
        self.assertNotIn("docker-compose.newapi.yml", text)

    def test_new_api_migration_runbook_uses_backup_restore_for_project_rename(self):
        runbook = ROOT / "docs" / "runbooks" / "migrate-to-new-api-deploy.md"
        text = runbook.read_text(encoding="utf-8")

        for required_receipt in [
            "docker inspect",
            "docker volume ls",
            "name: new-api",
            "scripts/migrations/backup-legacy-deployment.sh",
            "scripts/restore-deployment.sh",
            "scripts/verify-deployment.sh",
            "rollback",
        ]:
            self.assertIn(required_receipt, text)

        self.assertIn("/opt/new-api-deploy", text)
        self.assertIn("project and named-volume identities change", text)
        self.assertNotIn("docker-compose.newapi.yml", text)

    def test_gitignore_protects_current_runtime(self):
        text = (ROOT / ".gitignore").read_text(encoding="utf-8")

        for path in [
            "/sub2api-data/",
            "/sub2api-postgres-data/",
            "/sub2api-redis-data/",
            "/letsencrypt/",
        ]:
            self.assertIn(path, text)


if __name__ == "__main__":
    unittest.main()
