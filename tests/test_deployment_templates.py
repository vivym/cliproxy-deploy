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
            "lark-quota-controller:",
            "new-api-config-endpoint:",
            "lark-config:",
            "new-api-correction-endpoint:",
            "lark-correction-readonly:",
            "lark-correction:",
        ]:
            self.assertIn(service, text)
        self.assertIn("name: new-api-edge", text)
        self.assertIn("name: new-api-sub2api-data", text)
        self.assertIn("name: new-api-data", text)
        self.assertIn("name: new-api-postgres-data", text)
        self.assertIn("name: new-api-redis-data", text)
        self.assertIn("name: new-api-lark-integration", text)
        self.assertIn("name: new-api-lark-config", text)
        self.assertIn("name: new-api-lark-config-egress", text)
        self.assertIn("name: new-api-lark-controller-data", text)
        self.assertIn("traefik.http.routers.new-api.rule", text)
        self.assertIn("traefik.http.routers.sub2api-admin.rule", text)
        self.assertIn("!PathPrefix(`/v1`)", text)
        self.assertNotIn("traefik.http.routers.sub2api-api.rule", text)
        self.assertNotIn("cliproxyapi:", text)
        self.assertNotIn("cpa-usage-keeper:", text)
        self.assertNotIn("calciumion/new-api", text)
        self.assertIn('profiles: ["lark"]', text)
        self.assertIn("INTEGRATION_LISTEN_ADDR", text)
        self.assertIn("LARK_INTEGRATION_SECRET_FILE", text)
        self.assertIn("LARK_CORRECTION_SECRET_FILE", text)
        self.assertIn("LARK_INTEGRATION_SECRET_NEXT_FILE", text)
        self.assertIn("traefik.http.routers.lark-events.rule", text)
        self.assertIn("traefik.http.routers.lark-oauth.rule", text)
        self.assertIn("LARK_OAUTH_PUBLIC_ENABLED", text)
        self.assertNotIn("lark-oauth-disabled.invalid", text)
        self.assertIn("traefik.http.routers.lark-events.priority", text)
        self.assertNotIn('"3001:3001"', text)
        self.assertNotIn("traefik.http.services.new-api-integration", text)
        new_api_block = text.split("  new-api:", 1)[1].split(
            "\n  lark-quota-controller:", 1
        )[0]
        controller_block = text.split("  lark-quota-controller:", 1)[1].split(
            "\n  new-api-config-endpoint:", 1
        )[0]
        config_endpoint_block = text.split("  new-api-config-endpoint:", 1)[1].split(
            "\n  lark-config:", 1
        )[0]
        config_cli_block = text.split("  lark-config:", 1)[1].split(
            "\n  new-api-correction-endpoint:", 1
        )[0]
        endpoint_block = text.split("  new-api-correction-endpoint:", 1)[1].split(
            "\n  lark-correction-readonly:", 1
        )[0]
        readonly_block = text.split("  lark-correction-readonly:", 1)[1].split(
            "\n  lark-correction:", 1
        )[0]
        correction_block = text.split("\n  lark-correction:", 1)[1].split(
            "\nnetworks:", 1
        )[0]
        self.assertIn("lark-runtime/secrets/shared", new_api_block)
        self.assertNotIn("lark-runtime/secrets/controller", new_api_block)
        self.assertNotIn("lark-runtime/secrets/new-api", new_api_block)
        self.assertNotIn("LARK_CORRECTION_SECRET_FILE", new_api_block)
        self.assertIn("maintenance.lock", new_api_block)
        self.assertIn("lark-runtime/secrets/controller", controller_block)
        self.assertIn("lark-runtime/secrets/shared", controller_block)
        self.assertNotIn("lark-runtime/secrets/new-api", controller_block)
        self.assertNotIn("LARK_CORRECTION_SECRET_FILE", controller_block)
        self.assertIn("maintenance.lock", controller_block)
        self.assertIn('profiles: ["lark-config"]', config_endpoint_block)
        self.assertIn("LARK_CONFIG_SECRET_FILE", config_endpoint_block)
        self.assertIn("LARK_CONFIG_BRIDGE_CLIENT_SECRET_FILE", config_endpoint_block)
        self.assertNotIn("LARK_INTEGRATION_SECRET_FILE", config_endpoint_block)
        self.assertNotIn("traefik", config_endpoint_block)
        self.assertNotIn("ports:", config_endpoint_block)
        self.assertIn('profiles: ["lark-config"]', config_cli_block)
        self.assertIn("target: config", config_cli_block)
        self.assertIn("HOME: /var/lib/lark-controller", config_cli_block)
        self.assertIn("lark-config-cli-data:/var/lib/lark-controller", config_cli_block)
        self.assertIn("create_host_path: false", config_cli_block)
        self.assertIn("lark-config-egress", config_cli_block)
        self.assertNotIn("lark-runtime/secrets/controller", config_cli_block)
        self.assertNotIn("lark-runtime/secrets/shared", config_cli_block)
        self.assertNotIn("traefik", config_cli_block)
        self.assertNotIn("ports:", config_cli_block)
        self.assertIn("lark-runtime/secrets/shared", endpoint_block)
        self.assertIn("lark-runtime/secrets/new-api", endpoint_block)
        self.assertNotIn("lark-runtime/secrets/controller", endpoint_block)
        self.assertIn('profiles: ["lark-ops"]', endpoint_block)
        self.assertIn("host-side Lark correction maintenance lock", endpoint_block)
        self.assertIn('cat /run/lark-ops/maintenance.lock/mode', endpoint_block)
        self.assertIn('!= "correction"', endpoint_block)
        self.assertNotIn("http://new-api:", endpoint_block)
        self.assertNotIn("http://lark-quota-controller:", endpoint_block)
        self.assertNotIn("traefik", endpoint_block)
        self.assertIn("lark-controller-data:/var/lib/lark-controller:ro", readonly_block)
        self.assertIn("network_mode: none", readonly_block)
        self.assertNotIn("lark-runtime/secrets", readonly_block)
        self.assertNotIn("maintenance.lock", readonly_block)
        self.assertIn("lark-runtime/secrets/new-api", correction_block)
        self.assertNotIn("lark-runtime/secrets/shared", correction_block)
        self.assertNotIn("lark-runtime/secrets/controller", correction_block)
        self.assertIn('profiles: ["lark-ops"]', correction_block)
        self.assertIn("host-side Lark correction maintenance lock", correction_block)
        self.assertIn('cat /run/lark-ops/maintenance.lock/mode', correction_block)
        self.assertIn('!= "correction"', correction_block)
        self.assertNotIn(
            '"./lark-runtime/secrets:/run/secrets/lark-controller:ro"', text
        )
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
            "NEW_API_IMAGE_REPOSITORY=",
            "LARK_CONTROLLER_IMAGE_REPOSITORY=",
            "LARK_CONTROLLER_IMAGE_TAG=",
            "LARK_CORRECTION_IMAGE_REPOSITORY=",
            "LARK_CORRECTION_IMAGE_TAG=",
            "LARK_CONFIG_IMAGE_REPOSITORY=",
            "LARK_CONFIG_IMAGE_TAG=",
            "NEW_API_INTEGRATION_LISTEN_ADDR=",
            "NEW_API_LARK_INTEGRATION_SECRET_NEXT_FILE=",
            "LARK_CONTROLLER_MODE=",
            "LARK_OAUTH_PUBLIC_ENABLED=",
            "LARK_RECONCILIATION_HEALTH_OPEN_ID=",
            "LARK_CONFIG_EGRESS_SUBNET=",
        ]:
            self.assertIn(variable, env_example)

        for compiled_variable in [
            "LARK_APP_ID=",
            "LARK_APP_SECRET_FILE=",
            "LARK_TENANT_KEY=",
            "LARK_ACTIVE_POLICY_VERSION=",
            "LARK_GRANT_PAYLOAD_KEYRING_FILE=",
            "LARK_INTEGRATION_SECRET_FILE=",
            "NEW_API_BRIDGE_CLIENT_ID=",
            "NEW_API_BRIDGE_CLIENT_SECRET_FILE=",
        ]:
            self.assertNotIn(compiled_variable, env_example)

        self.assertNotIn("SUB2API_HOST=", env_example)
        self.assertNotIn("SUB2API_TEST_API_KEY=", env_example)
        self.assertNotIn("NEWAPI_", env_example)
        self.assertFalse((ROOT / "sub2api").exists())

    def test_lark_controller_image_is_reproducible_and_does_not_copy_runtime_secrets(self):
        dockerfile = (ROOT / "lark-controller" / "Dockerfile").read_text(encoding="utf-8")
        dockerignore = (ROOT / "lark-controller" / ".dockerignore").read_text(encoding="utf-8")

        self.assertIn("AS builder", dockerfile)
        self.assertIn("go build", dockerfile)
        self.assertIn("./cmd/lark-config", dockerfile)
        self.assertIn("@larksuite/cli@${LARK_CLI_VERSION}", dockerfile)
        self.assertIn("USER 10001:10001", dockerfile)
        self.assertIn("/healthz", dockerfile)
        controller_target = dockerfile.split("FROM runtime AS controller", 1)[1].split(
            "FROM runtime AS correction", 1
        )[0]
        correction_target = dockerfile.split("FROM runtime AS correction", 1)[1]
        self.assertIn("/out/lark-controller", controller_target)
        self.assertNotIn("/out/lark-correction", controller_target)
        self.assertIn("/out/lark-correction", correction_target)
        self.assertNotIn("/out/lark-controller", correction_target)
        self.assertIn("secrets", dockerignore)
        self.assertIn("*.sqlite", dockerignore)

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

    def test_lark_configuration_runbook_restarts_controller_around_apply(self):
        runbook = ROOT / "docs" / "runbooks" / "lark-tenant-configuration.md"
        text = runbook.read_text(encoding="utf-8")

        stop = "docker compose --profile lark stop lark-quota-controller"
        apply = "docker compose --profile lark-config run --rm lark-config apply"
        start = "docker compose --profile lark up -d lark-quota-controller"
        self.assertLess(text.index(stop), text.index(apply))
        self.assertLess(text.index(apply), text.index(start))
        self.assertIn("does not hot-reload", text)
        self.assertIn("new_api_bridge_client_secret", text)
        self.assertIn("secret_refs.bridge_client_secret", text)

    def test_gitignore_protects_current_runtime(self):
        text = (ROOT / ".gitignore").read_text(encoding="utf-8")

        for path in [
            "/sub2api-data/",
            "/sub2api-postgres-data/",
            "/sub2api-redis-data/",
            "/letsencrypt/",
            "/lark-runtime/secrets/*",
            "!/lark-runtime/secrets/shared/.gitkeep",
            "!/lark-runtime/secrets/controller/.gitkeep",
            "!/lark-runtime/secrets/new-api/.gitkeep",
            "!/lark-runtime/secrets/config/.gitkeep",
            "/lark-runtime/ops/*",
            "!/lark-runtime/ops/.gitkeep",
            "/lark-runtime/config/*",
            "!/lark-runtime/config/.gitkeep",
            "/lark-runtime/runtime/*",
            "!/lark-runtime/runtime/.gitkeep",
        ]:
            self.assertIn(path, text)


if __name__ == "__main__":
    unittest.main()
