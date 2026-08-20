import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]


class DeploymentTemplateTests(unittest.TestCase):
    def test_root_compose_is_the_stable_sub2api_deployment(self):
        text = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")

        self.assertIn("name: sub2api", text)
        for service in ["traefik:", "postgres:", "redis:", "sub2api:"]:
            self.assertIn(service, text)
        self.assertIn("name: sub2api-proxy", text)
        self.assertIn("name: sub2api-backend", text)
        self.assertNotIn("cliproxyapi:", text)
        self.assertNotIn("cpa-usage-keeper:", text)

    def test_newapi_overlay_and_environment_template_live_at_root(self):
        override = (ROOT / "docker-compose.newapi.yml").read_text(encoding="utf-8")
        env_example = (ROOT / ".env.example").read_text(encoding="utf-8")

        for service in ["newapi-postgres:", "newapi-redis:", "new-api:"]:
            self.assertIn(service, override)
        self.assertIn("name: sub2api-proxy", override)
        self.assertIn("name: newapi-backend", override)
        for variable in [
            "SUB2API_HOST=",
            "SUB2API_ADMIN_HOST=",
            "NEW_API_HOST=",
            "NEWAPI_POSTGRES_PASSWORD=",
            "NEWAPI_REDIS_PASSWORD=",
            "BACKUP_DIR=",
            "SUB2API_TEST_API_KEY=",
            "NEW_API_TEST_API_KEY=",
        ]:
            self.assertIn(variable, env_example)

        self.assertFalse((ROOT / "sub2api").exists())

    def test_readme_documents_only_the_current_root_deployment(self):
        text = (ROOT / "README.md").read_text(encoding="utf-8")

        self.assertIn("docker compose -f docker-compose.yml -f docker-compose.newapi.yml", text)
        self.assertIn("scripts/backup-deployment.sh", text)
        self.assertIn("scripts/restore-deployment.sh", text)
        self.assertIn("scripts/restore-newapi.sh", text)
        self.assertIn("name: sub2api", text)
        for legacy_term in [
            "../scripts/",
            "CLIProxyAPI",
            "CPA Usage Keeper",
            "cliproxy-runtime.tgz",
        ]:
            self.assertNotIn(legacy_term, text)

    def test_lark_architecture_targets_the_root_deployment(self):
        architecture = ROOT / "docs" / "architecture" / "lark-entitlement-integration.md"
        text = architecture.read_text(encoding="utf-8")

        self.assertIn("`docker-compose.newapi.yml`", text)
        self.assertNotIn("`sub2api/docker-compose.newapi.yml`", text)
        self.assertEqual(
            list((ROOT / "docs" / "superpowers").rglob("*.md")),
            [],
        )

    def test_newapi_restore_defaults_to_the_root_deployment(self):
        script = ROOT / "scripts" / "restore-newapi.sh"
        text = script.read_text(encoding="utf-8")

        self.assertIn(
            "Usage: scripts/restore-newapi.sh BACKUP_PACKAGE [DEPLOYMENT_DIR]",
            text,
        )
        self.assertIn('deployment_dir="${2:-${repo_root}}"', text)
        self.assertNotIn('repo_root}/sub2api', text)

    def test_root_layout_migration_runbook_has_data_identity_gates(self):
        runbook = ROOT / "docs" / "runbooks" / "promote-sub2api-to-root.md"
        text = runbook.read_text(encoding="utf-8")

        for required_receipt in [
            "docker inspect",
            "docker volume ls",
            "name: sub2api",
            "scripts/backup-deployment.sh",
            "scripts/verify-deployment.sh",
            "rollback",
        ]:
            self.assertIn(required_receipt, text)

        self.assertIn('if [[ -e "$path" || ! -e "sub2api/$path" ]]', text)
        self.assertIn('mv -n "sub2api/$path" "$path"', text)

    def test_gitignore_protects_runtime_during_layout_migration(self):
        text = (ROOT / ".gitignore").read_text(encoding="utf-8")

        for path in [
            "/sub2api/data/",
            "/sub2api/postgres_data/",
            "/sub2api/redis_data/",
            "/sub2api/letsencrypt/",
        ]:
            self.assertIn(path, text)


if __name__ == "__main__":
    unittest.main()
