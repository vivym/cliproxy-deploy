import io
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from unittest import mock
import unittest


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "scripts" / "validate-api-site-compose.py"
SPEC = importlib.util.spec_from_file_location("validate_api_site_compose", MODULE_PATH)
validate_api_site_compose = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules["validate_api_site_compose"] = validate_api_site_compose
SPEC.loader.exec_module(validate_api_site_compose)


EXPECTED_HOST = "ai.x2r.store"


def valid_compose():
    return {
        "services": {
            "traefik": {"ports": ["80:80", "443:443"], "networks": ["proxy"]},
            "new-api": {
                "image": "calciumion/new-api:v0.13.2",
                "networks": ["proxy", "backend"],
                "labels": {
                    "traefik.enable": "true",
                    "traefik.http.routers.new-api.rule": "Host(`ai.x2r.store`)",
                    "traefik.http.routers.new-api.entrypoints": "websecure",
                    "traefik.http.routers.new-api.tls": "true",
                    "traefik.http.routers.new-api.tls.certresolver": "le",
                    "traefik.http.routers.new-api.service": "new-api",
                    "traefik.http.services.new-api.loadbalancer.server.port": "3000",
                },
                "environment": {
                    "SQL_DSN": "postgresql://newapi:pw@postgres:5432/newapi",
                    "REDIS_CONN_STRING": "redis://default:pw@redis:6379",
                    "SESSION_SECRET": "session",
                    "CRYPTO_SECRET": "crypto",
                },
                "volumes": ["new-api-data:/data"],
            },
            "cliproxyapi": {
                "image": "eceasy/cli-proxy-api:v1.2.3",
                "networks": ["backend"],
                "expose": ["8317"],
                "labels": {"traefik.enable": "false"},
            },
            "postgres": {
                "image": "postgres:16-alpine",
                "networks": ["backend"],
                "volumes": ["postgres-data:/var/lib/postgresql/data"],
            },
            "redis": {
                "image": "redis:7-alpine",
                "networks": ["backend"],
                "volumes": ["redis-data:/data"],
            },
            "cpa-usage-keeper": {
                "image": "ghcr.io/willxup/cpa-usage-keeper:v1.3.3",
                "networks": ["backend"],
                "volumes": ["cpa-usage-keeper-data:/data"],
            },
        },
        "networks": {"proxy": {}, "backend": {}},
        "volumes": {
            "postgres-data": {},
            "redis-data": {},
            "cpa-usage-keeper-data": {},
        },
    }


class ValidateApiSiteComposeTests(unittest.TestCase):
    def assert_has_error(self, compose, expected):
        errors = validate_api_site_compose.validate(compose, EXPECTED_HOST)

        self.assertTrue(
            any(expected in error for error in errors),
            "Expected error containing {!r}; got {!r}".format(expected, errors),
        )

    def test_valid_compose_has_no_errors(self):
        self.assertEqual(validate_api_site_compose.validate(valid_compose(), EXPECTED_HOST), [])

    def test_rejects_cliproxyapi_public_traefik_router(self):
        compose = valid_compose()
        compose["services"]["cliproxyapi"]["labels"] = {
            "traefik.enable": "true",
            "traefik.http.routers.cliproxyapi.rule": "Host(`api.x2r.store`)",
        }

        self.assert_has_error(compose, "cliproxyapi must not enable Traefik")

    def test_rejects_cliproxyapi_traefik_router_label_without_enable(self):
        compose = valid_compose()
        compose["services"]["cliproxyapi"]["labels"] = {
            "traefik.http.routers.cliproxyapi.rule": "Host(`cliproxy.x2r.store`)",
        }

        self.assert_has_error(compose, "cliproxyapi must not define Traefik labels")

    def test_rejects_untagged_backend_service_image(self):
        compose = valid_compose()
        compose["services"]["cliproxyapi"]["image"] = "eceasy/cli-proxy-api"

        self.assert_has_error(compose, "cliproxyapi image must be pinned")

    def test_rejects_backend_service_traefik_router_label(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = {
            "traefik.http.routers.redis.rule": "Host(`redis.x2r.store`)",
        }

        self.assert_has_error(compose, "redis must not define Traefik labels")

    def test_rejects_backend_service_traefik_service_label(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = {
            "traefik.http.services.redis.loadbalancer.server.port": "6379",
        }

        self.assert_has_error(compose, "redis must not define Traefik labels")

    def test_rejects_backend_service_traefik_docker_network_label(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = {
            "traefik.docker.network": "proxy",
        }

        self.assert_has_error(compose, "redis must not define Traefik labels")

    def test_rejects_backend_service_traefik_tcp_label(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = {
            "traefik.tcp.routers.redis.rule": "HostSNI(`redis.x2r.store`)",
        }

        self.assert_has_error(compose, "redis must not define Traefik labels")

    def test_rejects_backend_service_key_only_traefik_label(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = ["traefik.http.routers.redis.rule"]

        self.assert_has_error(compose, "redis must not define Traefik labels")

    def test_allows_backend_service_list_form_traefik_enable_false(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = ["traefik.enable=false"]

        self.assertEqual(validate_api_site_compose.validate(compose, EXPECTED_HOST), [])

    def test_rejects_backend_service_non_exact_traefik_enable_false(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = ["traefik.enable=False"]

        self.assert_has_error(compose, "redis must not enable Traefik")

    def test_rejects_backend_service_duplicate_list_traefik_enable(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = [
            "traefik.enable=true",
            "traefik.enable=false",
        ]

        self.assert_has_error(compose, "redis must not enable Traefik")

    def test_rejects_backend_service_boolean_traefik_enable_false(self):
        compose = valid_compose()
        compose["services"]["redis"]["labels"] = {"traefik.enable": False}

        self.assert_has_error(compose, "redis must not enable Traefik")

    def test_rejects_unexpected_service_host_ports(self):
        compose = valid_compose()
        compose["services"]["debug"] = {
            "image": "busybox:1",
            "networks": ["backend"],
            "ports": ["9000:9000"],
        }

        self.assert_has_error(compose, "debug must not publish host ports")

    def test_rejects_unexpected_service_traefik_labels(self):
        compose = valid_compose()
        compose["services"]["debug"] = {
            "image": "busybox:1",
            "networks": ["backend"],
            "labels": {"traefik.http.routers.debug.rule": "Host(`debug.x2r.store`)"},
        }

        self.assert_has_error(compose, "debug must not define Traefik labels")

    def test_rejects_unexpected_service_joining_proxy(self):
        compose = valid_compose()
        compose["services"]["debug"] = {
            "image": "busybox:1",
            "networks": ["proxy", "backend"],
            "labels": {"traefik.enable": "false"},
        }

        self.assert_has_error(compose, "debug must not join proxy")

    def test_rejects_traefik_joining_backend(self):
        compose = valid_compose()
        compose["services"]["traefik"]["networks"] = ["proxy", "backend"]

        self.assert_has_error(compose, "traefik must not join backend")

    def test_rejects_backend_network_internal_true(self):
        compose = valid_compose()
        compose["networks"]["backend"] = {"internal": True}

        self.assert_has_error(compose, "backend network must not set internal: true")

    def test_rejects_unpinned_new_api_latest_image(self):
        compose = valid_compose()
        compose["services"]["new-api"]["image"] = "calciumion/new-api:latest"

        self.assert_has_error(compose, "new-api image must be pinned")

    def test_rejects_new_api_latest_image_with_digest(self):
        compose = valid_compose()
        compose["services"]["new-api"]["image"] = "calciumion/new-api:latest@sha256:abc"

        self.assert_has_error(compose, "new-api image must be pinned")

    def test_accepts_non_latest_pinned_image_with_digest(self):
        self.assertTrue(
            validate_api_site_compose.image_is_pinned("calciumion/new-api:v0.13.2@sha256:abc")
        )

    def test_accepts_digest_only_pinned_image(self):
        self.assertTrue(
            validate_api_site_compose.image_is_pinned(
                "postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
            )
        )

    def test_accepts_single_character_non_latest_tag(self):
        self.assertTrue(validate_api_site_compose.image_is_pinned("repo:1"))

    def test_rejects_untagged_image_without_digest(self):
        self.assertFalse(validate_api_site_compose.image_is_pinned("postgres"))

    def test_rejects_latest_tag_images(self):
        self.assertFalse(validate_api_site_compose.image_is_pinned("postgres:latest"))
        self.assertFalse(validate_api_site_compose.image_is_pinned("postgres:latest@sha256:abc"))

    def test_reports_new_api_invalid_image_once(self):
        compose = valid_compose()
        compose["services"]["new-api"]["image"] = "calciumion/new-api:latest"

        errors = validate_api_site_compose.validate(compose, EXPECTED_HOST)

        image_errors = [
            error for error in errors
            if error.startswith("new-api image must be pinned")
        ]
        self.assertEqual(image_errors, ["new-api image must be pinned to a non-latest tag"])

    def test_rejects_missing_new_api_sql_dsn(self):
        compose = valid_compose()
        del compose["services"]["new-api"]["environment"]["SQL_DSN"]

        self.assert_has_error(compose, "new-api missing required environment SQL_DSN")

    def test_rejects_missing_or_wrong_new_api_traefik_labels(self):
        required_labels = {
            "traefik.http.routers.new-api.entrypoints": ("api", "websecure"),
            "traefik.http.routers.new-api.tls": ("false", "true"),
            "traefik.http.routers.new-api.tls.certresolver": ("default", "le"),
            "traefik.http.routers.new-api.service": ("wrong-service", "new-api"),
            "traefik.http.services.new-api.loadbalancer.server.port": ("8080", "3000"),
        }

        for label, (wrong_value, expected_value) in required_labels.items():
            with self.subTest(label=label, mode="missing"):
                compose = valid_compose()
                del compose["services"]["new-api"]["labels"][label]

                self.assert_has_error(
                    compose,
                    "new-api Traefik label {} must be {}".format(label, expected_value),
                )

            with self.subTest(label=label, mode="wrong"):
                compose = valid_compose()
                compose["services"]["new-api"]["labels"][label] = wrong_value

                self.assert_has_error(
                    compose,
                    "new-api Traefik label {} must be {}".format(label, expected_value),
                )

    def test_rejects_new_api_host_ports(self):
        compose = valid_compose()
        compose["services"]["new-api"]["ports"] = ["3000:3000"]

        self.assert_has_error(compose, "new-api must not publish host ports")

    def test_rejects_missing_postgres_data_volume(self):
        compose = valid_compose()
        del compose["volumes"]["postgres-data"]

        self.assert_has_error(compose, "missing required volume postgres-data")

    def test_accepts_compose_list_forms(self):
        compose = valid_compose()
        compose["services"]["new-api"]["labels"] = [
            "traefik.enable=true",
            "traefik.http.routers.new-api.rule=Host(`ai.x2r.store`)",
            "traefik.http.routers.new-api.entrypoints=websecure",
            "traefik.http.routers.new-api.tls=true",
            "traefik.http.routers.new-api.tls.certresolver=le",
            "traefik.http.routers.new-api.service=new-api",
            "traefik.http.services.new-api.loadbalancer.server.port=3000",
        ]
        compose["services"]["new-api"]["environment"] = [
            "SQL_DSN=postgresql://newapi:pw@postgres:5432/newapi",
            "REDIS_CONN_STRING=redis://default:pw@redis:6379",
            "SESSION_SECRET=session",
            "CRYPTO_SECRET=crypto",
        ]
        compose["services"]["new-api"]["networks"] = {
            "proxy": {},
            "backend": {},
        }
        compose["services"]["postgres"]["volumes"] = [
            {
                "type": "volume",
                "source": "postgres-data",
                "target": "/var/lib/postgresql/data",
            }
        ]

        self.assertEqual(validate_api_site_compose.validate(compose, EXPECTED_HOST), [])

    def test_environment_list_entries_without_value_are_included(self):
        environment = validate_api_site_compose.environment_for({"environment": ["SQL_DSN"]})

        self.assertIn("SQL_DSN", environment)
        self.assertEqual(environment["SQL_DSN"], "")

    def test_main_prints_success_for_valid_compose(self):
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json") as compose_file:
            json.dump(valid_compose(), compose_file)
            compose_file.flush()
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(
                validate_api_site_compose.sys,
                "argv",
                ["validate-api-site-compose.py", compose_file.name],
            ), mock.patch.object(validate_api_site_compose.sys, "stdout", stdout), \
                    mock.patch.object(validate_api_site_compose.sys, "stderr", stderr):
                result = validate_api_site_compose.main()

        self.assertEqual(result, 0)
        self.assertEqual(stdout.getvalue(), "api-site compose validation passed\n")
        self.assertEqual(stderr.getvalue(), "")

    def test_main_prints_errors_to_stderr_for_invalid_compose(self):
        compose = valid_compose()
        compose["networks"]["backend"] = {"internal": True}

        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json") as compose_file:
            json.dump(compose, compose_file)
            compose_file.flush()
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(
                validate_api_site_compose.sys,
                "argv",
                ["validate-api-site-compose.py", compose_file.name],
            ), mock.patch.object(validate_api_site_compose.sys, "stdout", stdout), \
                    mock.patch.object(validate_api_site_compose.sys, "stderr", stderr):
                result = validate_api_site_compose.main()

        self.assertEqual(result, 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertIn("ERROR: backend network must not set internal: true", stderr.getvalue())

    def test_main_accepts_host_override_option(self):
        compose = valid_compose()
        compose["services"]["new-api"]["labels"]["traefik.http.routers.new-api.rule"] = (
            "Host(`api.example.com`)"
        )

        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json") as compose_file:
            json.dump(compose, compose_file)
            compose_file.flush()
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(
                validate_api_site_compose.sys,
                "argv",
                ["validate-api-site-compose.py", compose_file.name, "--host", "api.example.com"],
            ), mock.patch.object(validate_api_site_compose.sys, "stdout", stdout), \
                    mock.patch.object(validate_api_site_compose.sys, "stderr", stderr):
                result = validate_api_site_compose.main()

        self.assertEqual(result, 0)
        self.assertEqual(stdout.getvalue(), "api-site compose validation passed\n")
        self.assertEqual(stderr.getvalue(), "")

    def test_main_reports_malformed_json_without_traceback(self):
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json") as compose_file:
            compose_file.write("{not-json")
            compose_file.flush()
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(
                validate_api_site_compose.sys,
                "argv",
                ["validate-api-site-compose.py", compose_file.name],
            ), mock.patch.object(validate_api_site_compose.sys, "stdout", stdout), \
                    mock.patch.object(validate_api_site_compose.sys, "stderr", stderr):
                result = validate_api_site_compose.main()

        self.assertEqual(result, 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertIn("ERROR: ", stderr.getvalue())
        self.assertNotIn("Traceback", stderr.getvalue())

    def test_main_reports_non_object_json_without_traceback(self):
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json") as compose_file:
            json.dump([], compose_file)
            compose_file.flush()
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(
                validate_api_site_compose.sys,
                "argv",
                ["validate-api-site-compose.py", compose_file.name],
            ), mock.patch.object(validate_api_site_compose.sys, "stdout", stdout), \
                    mock.patch.object(validate_api_site_compose.sys, "stderr", stderr):
                result = validate_api_site_compose.main()

        self.assertEqual(result, 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertIn("ERROR: compose JSON must be an object", stderr.getvalue())
        self.assertNotIn("Traceback", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
