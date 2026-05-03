import importlib.util
from pathlib import Path
import sys
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

        self.assert_has_error(compose, "cliproxyapi must not define Traefik router labels")

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

    def test_rejects_missing_new_api_sql_dsn(self):
        compose = valid_compose()
        del compose["services"]["new-api"]["environment"]["SQL_DSN"]

        self.assert_has_error(compose, "new-api missing required environment SQL_DSN")

    def test_rejects_missing_postgres_data_volume(self):
        compose = valid_compose()
        del compose["volumes"]["postgres-data"]

        self.assert_has_error(compose, "missing required volume postgres-data")

    def test_accepts_compose_list_forms(self):
        compose = valid_compose()
        compose["services"]["new-api"]["labels"] = [
            "traefik.enable=true",
            "traefik.http.routers.new-api.rule=Host(`ai.x2r.store`)",
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


if __name__ == "__main__":
    unittest.main()
