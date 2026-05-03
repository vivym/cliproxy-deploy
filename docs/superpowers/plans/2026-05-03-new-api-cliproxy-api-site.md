# New API + CLIProxyAPI API Site Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Convert this repository from a public CLIProxyAPI-only deployment into a single-domain `ai.x2r.store` API-site deployment with New API as the public gateway and CLIProxyAPI as an internal upstream.

**Architecture:** Traefik remains the only public edge on ports `80/443`, but public routing moves from `cliproxyapi` to `new-api`. New API joins both `proxy` and `backend` networks; Postgres, Redis, CLIProxyAPI, and CPA Usage Keeper stay on `backend` only. The backend network is private by convention and has no published service ports, but it is not Docker `internal: true` because CLIProxyAPI and New API need outbound internet access. Validation scripts enforce routing, network, image, logging, and launch-gate invariants before any production rollout.

**Tech Stack:** Docker Compose, Traefik v3, New API, PostgreSQL, Redis, CLIProxyAPI, CPA Usage Keeper, Python `unittest`, shell scripts, `docker compose config --format json`.

---

## Source Documents

- Spec: `docs/superpowers/specs/2026-05-03-new-api-cliproxy-api-site-design.md`
- Existing deployment plan: `docs/superpowers/plans/2026-05-03-cliproxy-deploy-config.md`
- Existing deployment files: `docker-compose.yml`, `config.yaml.template`, `.env.example`, `README.md`
- New API docs to check during implementation:
  - `https://github.com/QuantumNous/new-api/releases`
  - `https://docs.newapi.pro/en/docs/installation`
  - `https://doc.newapi.pro/installation/environment-variables/`
- CLIProxyAPI docs to check during implementation:
  - `https://help.router-for.me/management/redis-usage-queue.html`
- CPA Usage Keeper docs to check during implementation:
  - `https://github.com/Willxup/cpa-usage-keeper`

## Scope Check

This plan implements one deployable subsystem: the Docker Compose deployment and local operational tooling for the API-site architecture. It does not create a custom billing system, build a custom frontend, connect online payment, operate real upstream accounts, or perform live server rollout. New API admin configuration that cannot be represented safely in static files is documented as a runbook and verified through launch gates.

## File Structure

### Modify

- `docker-compose.yml`
  - Add `new-api`, `postgres`, `redis`, `cpa-usage-keeper`, `backup-runner`-compatible volumes, and `backend` network.
  - Move the only public Traefik route to `new-api` on `${AI_HOST}`.
  - Remove public Traefik routing from `cliproxyapi`.
  - Keep `cliproxyapi` reachable only on `backend`.

- `config.yaml.template`
  - Set production-safe CLIProxyAPI defaults for the API-site shape.
  - Keep management enabled for internal operations, but disable the public control panel and production request body logging.
  - Set usage queue retention to `3600`.

- `.env.example`
  - Replace `API_HOST=cliproxy.x2r.store` with `AI_HOST=ai.x2r.store`.
  - Add New API, Postgres, Redis, CLIProxyAPI internal key, CPA Usage Keeper, and backup variables.

- `README.md`
  - Update setup, DNS, routing, CLIProxyAPI operations, New API launch gates, backup, and verification instructions.

### Create

- `scripts/select-new-api-version.py`
  - Select the highest stable New API semver tag from `git ls-remote` output or stdin.
  - Explicitly reject `alpha`, `beta`, `rc`, nightly, and non-semver tags.

- `scripts/validate-api-site-compose.py`
  - Validate rendered Compose JSON for public routing, private backend services, pinned images, networks, volumes, and environment wiring.

- `scripts/backup-api-site.sh`
  - Run a Postgres dump and archive required local runtime directories.
  - Write backups outside the repo under a configurable directory.

- `scripts/verify-api-site.sh`
  - Run operator-facing HTTP checks for New API, CLIProxyAPI internal reachability, and public-exposure negative checks.
  - Leave credentialed API checks behind explicit environment variables so secrets are not printed.

- `docs/api-site-runbook.md`
  - Document New API admin hardening, invite-code setup, redeem-code setup, model/channel/rate configuration, Codex validation, fallback validation, backups, restore drill, and rollback.

- `docker-compose.cliproxy-public.override.yml.template`
  - Optional temporary maintenance override for CLIProxyAPI public access.
  - Must not be included automatically by production Compose.

### Tests

- `tests/test_select_new_api_version.py`
  - Unit tests for stable tag selection.

- `tests/test_validate_api_site_compose.py`
  - Unit tests for Compose JSON validation logic using small fixtures.

- `tests/test_api_site_templates.py`
  - Static tests for `docker-compose.yml`, `.env.example`, and `config.yaml.template`.

- `tests/test_backup_api_site.py`
  - Static and dry-run tests for backup script safety behavior.

- `tests/test_verify_api_site.py`
  - Static and mocked command tests for verification script behavior where practical.

---

### Task 1: New API Stable Version Selector

**Files:**
- Create: `scripts/select-new-api-version.py`
- Create: `tests/test_select_new_api_version.py`

- [ ] **Step 1: Write failing tests for stable semver selection**

Create `tests/test_select_new_api_version.py`:

```python
import importlib.util
from pathlib import Path
import sys
from unittest import mock
import unittest


module_path = Path(__file__).resolve().parents[1] / "scripts" / "select-new-api-version.py"
spec = importlib.util.spec_from_file_location("select_new_api_version", module_path)
select_new_api_version = importlib.util.module_from_spec(spec)
assert spec.loader is not None
sys.modules["select_new_api_version"] = select_new_api_version
spec.loader.exec_module(select_new_api_version)
select_latest_stable = select_new_api_version.select_latest_stable


class SelectNewApiVersionTests(unittest.TestCase):
    def test_selects_highest_non_prerelease_semver(self):
        tags = [
            "v0.12.14",
            "v0.12.15",
            "v0.13.0",
            "v0.13.2",
            "v1.0.0-rc.1",
            "v1.0.0-rc.2",
        ]

        self.assertEqual(select_latest_stable(tags), "v0.13.2")

    def test_rejects_alpha_beta_rc_and_non_semver_tags(self):
        tags = [
            "v0.14.0-alpha.1",
            "v0.14.0-beta.1",
            "v0.14.0-rc.1",
            "latest",
            "nightly",
            "v0.13.2",
        ]

        self.assertEqual(select_latest_stable(tags), "v0.13.2")

    def test_accepts_git_ls_remote_lines(self):
        tags = [
            "abc123\trefs/tags/v0.13.1",
            "def456\trefs/tags/v0.13.2",
            "ghi789\trefs/tags/v1.0.0-rc.2",
        ]

        self.assertEqual(select_latest_stable(tags), "v0.13.2")

    def test_raises_when_no_stable_tags_exist(self):
        with self.assertRaisesRegex(ValueError, "No stable New API tags"):
            select_latest_stable(["v1.0.0-rc.1", "nightly"])

    def test_main_fetches_tags_when_stdin_is_tty(self):
        with mock.patch.object(select_new_api_version.sys.stdin, "isatty", return_value=True), \
             mock.patch.object(select_new_api_version, "fetch_tags", return_value=["v0.13.2"]), \
             mock.patch("builtins.print") as print_mock:
            self.assertEqual(select_new_api_version.main(), 0)

        print_mock.assert_called_once_with("v0.13.2")


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
python -m unittest tests.test_select_new_api_version -v
```

Expected: FAIL with `FileNotFoundError` for `scripts/select-new-api-version.py`.

- [ ] **Step 3: Implement the selector**

Create `scripts/select-new-api-version.py`:

```python
#!/usr/bin/env python3
"""Select the latest stable New API tag from git tag output."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass


TAG_RE = re.compile(r"^v(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)$")


@dataclass(frozen=True, order=True)
class Version:
    major: int
    minor: int
    patch: int
    tag: str


def parse_stable_tag(tag: str) -> Version | None:
    tag = tag.strip().rsplit("/", 1)[-1]
    match = TAG_RE.match(tag)
    if not match:
        return None
    return Version(
        major=int(match.group("major")),
        minor=int(match.group("minor")),
        patch=int(match.group("patch")),
        tag=tag.strip(),
    )


def select_latest_stable(tags: list[str]) -> str:
    versions = [version for tag in tags if (version := parse_stable_tag(tag))]
    if not versions:
        raise ValueError("No stable New API tags found")
    return max(versions).tag


def fetch_tags(repo: str) -> list[str]:
    result = subprocess.run(
        ["git", "ls-remote", "--tags", "--refs", repo, "refs/tags/v*"],
        check=True,
        text=True,
        capture_output=True,
    )
    tags = []
    for line in result.stdout.splitlines():
        ref = line.split()[-1]
        tags.append(ref.rsplit("/", 1)[-1])
    return tags


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--repo",
        default="https://github.com/QuantumNous/new-api.git",
        help="Git repository to query when stdin is empty.",
    )
    args = parser.parse_args()

    stdin = "" if sys.stdin.isatty() else sys.stdin.read().strip()
    tags = stdin.splitlines() if stdin else fetch_tags(args.repo)
    print(select_latest_stable(tags))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Run selector tests**

Run:

```bash
python -m unittest tests.test_select_new_api_version -v
```

Expected: PASS.

- [ ] **Step 5: Run selector against a fixture**

Run:

```bash
printf '%s\n' v0.13.2 v1.0.0-rc.2 nightly | python scripts/select-new-api-version.py
```

Expected output:

```text
v0.13.2
```

- [ ] **Step 6: Make the script executable**

Run:

```bash
chmod +x scripts/select-new-api-version.py
```

- [ ] **Step 7: Commit**

```bash
git add scripts/select-new-api-version.py tests/test_select_new_api_version.py
git commit -m "feat: select stable new api version"
```

---

### Task 2: Compose Validation Utility

**Files:**
- Create: `scripts/validate-api-site-compose.py`
- Create: `tests/test_validate_api_site_compose.py`

- [ ] **Step 1: Write failing tests for Compose invariants**

Create `tests/test_validate_api_site_compose.py`:

```python
import importlib.util
from pathlib import Path
import sys
import unittest


module_path = Path(__file__).resolve().parents[1] / "scripts" / "validate-api-site-compose.py"
spec = importlib.util.spec_from_file_location("validate_api_site_compose", module_path)
validate_api_site_compose = importlib.util.module_from_spec(spec)
assert spec.loader is not None
sys.modules["validate_api_site_compose"] = validate_api_site_compose
spec.loader.exec_module(validate_api_site_compose)


class ValidateApiSiteComposeTests(unittest.TestCase):
    def valid_compose(self):
        return {
            "services": {
                "traefik": {
                    "ports": ["80:80", "443:443"],
                    "networks": ["proxy"],
                },
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

    def test_valid_compose_has_no_errors(self):
        self.assertEqual(validate_api_site_compose.validate(self.valid_compose(), "ai.x2r.store"), [])

    def test_cliproxy_public_router_is_rejected(self):
        compose = self.valid_compose()
        compose["services"]["cliproxyapi"]["labels"] = {"traefik.enable": "true"}

        errors = validate_api_site_compose.validate(compose, "ai.x2r.store")

        self.assertTrue(any("cliproxyapi must not enable Traefik" in error for error in errors))

    def test_traefik_on_backend_is_rejected(self):
        compose = self.valid_compose()
        compose["services"]["traefik"]["networks"] = ["proxy", "backend"]

        errors = validate_api_site_compose.validate(compose, "ai.x2r.store")

        self.assertTrue(any("traefik must not join backend" in error for error in errors))

    def test_backend_network_must_not_disable_egress(self):
        compose = self.valid_compose()
        compose["networks"]["backend"] = {"internal": True}

        errors = validate_api_site_compose.validate(compose, "ai.x2r.store")

        self.assertTrue(any("backend network must not set internal: true" in error for error in errors))

    def test_unpinned_new_api_image_is_rejected(self):
        compose = self.valid_compose()
        compose["services"]["new-api"]["image"] = "calciumion/new-api:latest"

        errors = validate_api_site_compose.validate(compose, "ai.x2r.store")

        self.assertTrue(any("new-api image must be pinned" in error for error in errors))

    def test_missing_new_api_env_is_rejected(self):
        compose = self.valid_compose()
        del compose["services"]["new-api"]["environment"]["SQL_DSN"]

        errors = validate_api_site_compose.validate(compose, "ai.x2r.store")

        self.assertTrue(any("new-api missing required environment: SQL_DSN" in error for error in errors))

    def test_missing_required_volume_is_rejected(self):
        compose = self.valid_compose()
        del compose["volumes"]["postgres-data"]

        errors = validate_api_site_compose.validate(compose, "ai.x2r.store")

        self.assertTrue(any("missing required volume: postgres-data" in error for error in errors))


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
python -m unittest tests.test_validate_api_site_compose -v
```

Expected: FAIL because `scripts/validate-api-site-compose.py` does not exist.

- [ ] **Step 3: Implement validation logic**

Create `scripts/validate-api-site-compose.py`:

```python
#!/usr/bin/env python3
"""Validate rendered Docker Compose JSON for the API-site deployment."""

from __future__ import annotations

import argparse
import json
import re
import sys
from typing import Any


PINNED_TAG_RE = re.compile(r":(?!latest$)[A-Za-z0-9][A-Za-z0-9_.-]+$")


def labels_for(service: dict[str, Any]) -> dict[str, str]:
    labels = service.get("labels") or {}
    if isinstance(labels, dict):
        return {str(key): str(value) for key, value in labels.items()}
    result: dict[str, str] = {}
    for item in labels:
        key, _, value = str(item).partition("=")
        result[key] = value
    return result


def networks_for(service: dict[str, Any]) -> set[str]:
    networks = service.get("networks") or []
    if isinstance(networks, dict):
        return set(networks)
    return {str(network) for network in networks}


def has_host_ports(service: dict[str, Any]) -> bool:
    return bool(service.get("ports"))


def image_is_pinned(image: str) -> bool:
    return bool(PINNED_TAG_RE.search(image)) and not image.endswith(":latest")


def environment_for(service: dict[str, Any]) -> dict[str, str]:
    environment = service.get("environment") or {}
    if isinstance(environment, dict):
        return {str(key): str(value) for key, value in environment.items()}
    result: dict[str, str] = {}
    for item in environment:
        key, _, value = str(item).partition("=")
        result[key] = value
    return result


def service_mounts_for(service: dict[str, Any]) -> list[str]:
    mounts = []
    for item in service.get("volumes") or []:
        if isinstance(item, str):
            mounts.append(item)
        elif isinstance(item, dict) and item.get("source"):
            mounts.append(f"{item.get('source')}:{item.get('target', '')}")
    return mounts


def validate(compose: dict[str, Any], expected_host: str) -> list[str]:
    errors: list[str] = []
    services = compose.get("services") or {}
    networks = compose.get("networks") or {}
    volumes = compose.get("volumes") or {}

    for name in ["traefik", "new-api", "postgres", "redis", "cliproxyapi", "cpa-usage-keeper"]:
        if name not in services:
            errors.append(f"missing required service: {name}")

    if "backend" not in networks:
        errors.append("missing backend network")
    if "proxy" not in networks:
        errors.append("missing proxy network")
    if isinstance(networks.get("backend"), dict) and networks.get("backend", {}).get("internal") is True:
        errors.append("backend network must not set internal: true because services need outbound access")

    for volume_name in ["postgres-data", "redis-data", "cpa-usage-keeper-data"]:
        if volume_name not in volumes:
            errors.append(f"missing required volume: {volume_name}")

    traefik = services.get("traefik", {})
    if "backend" in networks_for(traefik):
        errors.append("traefik must not join backend network")

    new_api = services.get("new-api", {})
    new_api_labels = labels_for(new_api)
    if new_api_labels.get("traefik.enable") != "true":
        errors.append("new-api must enable Traefik")
    expected_rule = f"Host(`{expected_host}`)"
    if expected_rule not in new_api_labels.values():
        errors.append(f"new-api must route expected host: {expected_rule}")
    if not {"proxy", "backend"}.issubset(networks_for(new_api)):
        errors.append("new-api must join proxy and backend networks")
    if not image_is_pinned(str(new_api.get("image", ""))):
        errors.append("new-api image must be pinned and must not use latest")
    new_api_env = environment_for(new_api)
    for env_name in ["SQL_DSN", "REDIS_CONN_STRING", "SESSION_SECRET", "CRYPTO_SECRET"]:
        if env_name not in new_api_env:
            errors.append(f"new-api missing required environment: {env_name}")

    cliproxyapi = services.get("cliproxyapi", {})
    cliproxy_labels = labels_for(cliproxyapi)
    if cliproxy_labels.get("traefik.enable", "false") != "false":
        errors.append("cliproxyapi must not enable Traefik")
    if has_host_ports(cliproxyapi):
        errors.append("cliproxyapi must not publish host ports")
    if networks_for(cliproxyapi) != {"backend"}:
        errors.append("cliproxyapi must only join backend network")

    for private_service_name in ["postgres", "redis", "cpa-usage-keeper"]:
        service = services.get(private_service_name, {})
        if has_host_ports(service):
            errors.append(f"{private_service_name} must not publish host ports")
        if networks_for(service) != {"backend"}:
            errors.append(f"{private_service_name} must only join backend network")

    volume_requirements = {
        "postgres": "postgres-data",
        "redis": "redis-data",
        "cpa-usage-keeper": "cpa-usage-keeper-data",
    }
    for service_name, required_volume in volume_requirements.items():
        service_volumes = service_mounts_for(services.get(service_name, {}))
        if not any(mount.startswith(f"{required_volume}:") for mount in service_volumes):
            errors.append(f"{service_name} must mount required volume: {required_volume}")

    for service_name, service in services.items():
        image = str(service.get("image", ""))
        if image.endswith(":latest"):
            errors.append(f"{service_name} image must not use latest")

    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("compose_json", help="Path to `docker compose config --format json` output.")
    parser.add_argument("--host", default="ai.x2r.store", help="Expected public host.")
    args = parser.parse_args()

    with open(args.compose_json, encoding="utf-8") as handle:
        compose = json.load(handle)

    errors = validate(compose, args.host)
    if errors:
        for error in errors:
            print(f"ERROR: {error}", file=sys.stderr)
        return 1
    print("api-site compose validation passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Make the script executable**

Run:

```bash
chmod +x scripts/validate-api-site-compose.py
```

- [ ] **Step 5: Run validation utility tests**

Run:

```bash
python -m unittest tests.test_validate_api_site_compose -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add scripts/validate-api-site-compose.py tests/test_validate_api_site_compose.py
git commit -m "feat: validate api site compose"
```

---

### Task 3: API Site Templates And Compose Topology

**Files:**
- Create: `tests/test_api_site_templates.py`
- Modify: `.env.example`
- Modify: `config.yaml.template`
- Modify: `docker-compose.yml`

- [ ] **Step 1: Write failing static tests for target templates**

Create `tests/test_api_site_templates.py`:

```python
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
python -m unittest tests.test_api_site_templates -v
```

Expected: FAIL because templates still describe the old CLIProxyAPI-public deployment.

- [ ] **Step 3: Update `.env.example`**

Replace `.env.example` with:

```dotenv
# Let's Encrypt account email.
ACME_EMAIL=admin@example.com

# Single public API-site hostname. Users, SDKs, and New API admin all use this host.
AI_HOST=ai.x2r.store

# Pin images after validation. Do not use GitHub's "Latest" marker blindly for New API.
NEW_API_IMAGE_TAG=v0.13.2
CLIPROXYAPI_IMAGE_TAG=replace-with-pinned-version
CPA_USAGE_KEEPER_IMAGE_TAG=v1.3.3
CPA_USAGE_KEEPER_IMAGE=ghcr.io/willxup/cpa-usage-keeper

# New API security. Generate unique values before launch:
# openssl rand -hex 32
NEW_API_SESSION_SECRET=replace-with-session-secret
NEW_API_CRYPTO_SECRET=replace-with-crypto-secret

# Postgres.
POSTGRES_DB=newapi
POSTGRES_USER=newapi
POSTGRES_PASSWORD=replace-with-postgres-password

# Redis.
REDIS_PASSWORD=replace-with-redis-password

# Internal-only API key used by New API when calling CLIProxyAPI.
# Generate with: scripts/generate-api-key.py
CLIPROXY_INTERNAL_API_KEY=replace-with-internal-new-api-channel-key

# CLIProxyAPI management key. Also used by CPA Usage Keeper to read usage queue.
MANAGEMENT_SECRET=replace-with-management-secret

# CPA Usage Keeper dashboard stays private by default. Set only if protected exposure is added later.
CPA_USAGE_KEEPER_AUTH_ENABLED=true
CPA_USAGE_KEEPER_AUTH_PASSWORD=replace-with-keeper-password

# Backups are written outside the repository by scripts/backup-api-site.sh.
BACKUP_DIR=/var/backups/cliproxy-api-site

# Required negative check for launch validation. Keep this set while the legacy
# CLIProxyAPI hostname exists; verification fails if any HTTP response is seen.
CLIPROXY_PUBLIC_HOST=cliproxy.x2r.store

# Passed through for compatibility with the upstream CLIProxyAPI compose file.
# Leave empty for normal local/server filesystem deployment.
DEPLOY=
```

- [ ] **Step 4: Run template tests**

Run:

```bash
python -m unittest tests.test_api_site_templates -v
```

Expected: still FAIL because `docker-compose.yml` and `config.yaml.template` are not updated yet. Do not commit while tests are red.

- [ ] **Step 5: Update CLIProxyAPI template values**

Make these exact value changes in `config.yaml.template`:

```yaml
remote-management:
  allow-remote: true
  secret-key: "replace-with-management-secret"
  disable-control-panel: true
```

```yaml
api-keys:
  - "replace-with-internal-new-api-channel-key"
```

```yaml
request-log: false
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 3600
```

Keep `remote-management.allow-remote: true` because CPA Usage Keeper needs the management key to authenticate to the Redis-compatible usage queue, but the service is internal-only and must not have a public router.

- [ ] **Step 6: Update comments in `config.yaml.template`**

Add comments explaining:

```yaml
# CLIProxyAPI is internal-only in the API-site deployment.
# Management stays enabled for internal container operations and usage queue AUTH.
# Do not expose /management.html or /v0/management/* publicly in production.
```

Add comments explaining:

```yaml
# Production default is false. Enable request-log only during short troubleshooting windows.
```

- [ ] **Step 7: Run template tests**

Run:

```bash
python -m unittest tests.test_api_site_templates -v
```

Expected: still FAIL until `docker-compose.yml` is updated. Do not commit while tests are red.

- [ ] **Step 8: Update `docker-compose.yml` networks**

Set networks at the bottom:

```yaml
networks:
  proxy:
    name: proxy
  backend:
    name: cliproxy_backend
```

Do not add `internal: true`; CLIProxyAPI and New API need outbound internet access.

- [ ] **Step 9: Keep Traefik only on proxy**

Ensure `traefik.networks` is:

```yaml
networks:
  - proxy
```

- [ ] **Step 10: Add `postgres` service**

Add:

```yaml
  postgres:
    image: postgres:16-alpine
    container_name: newapi-postgres
    restart: unless-stopped
    environment:
      POSTGRES_DB: ${POSTGRES_DB:?set POSTGRES_DB}
      POSTGRES_USER: ${POSTGRES_USER:?set POSTGRES_USER}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD}
    volumes:
      - "postgres-data:/var/lib/postgresql/data"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:?set POSTGRES_USER} -d ${POSTGRES_DB:?set POSTGRES_DB}"]
      interval: 10s
      timeout: 5s
      retries: 5
    networks:
      - backend
```

- [ ] **Step 11: Add `redis` service**

Add:

```yaml
  redis:
    image: redis:7-alpine
    container_name: newapi-redis
    restart: unless-stopped
    command: ["redis-server", "--appendonly", "yes", "--requirepass", "${REDIS_PASSWORD:?set REDIS_PASSWORD}"]
    volumes:
      - "redis-data:/data"
    healthcheck:
      test: ["CMD", "redis-cli", "-a", "${REDIS_PASSWORD:?set REDIS_PASSWORD}", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
    networks:
      - backend
```

- [ ] **Step 12: Add `new-api` service**

Add:

```yaml
  new-api:
    image: calciumion/new-api:${NEW_API_IMAGE_TAG:?set NEW_API_IMAGE_TAG}
    container_name: new-api
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    environment:
      TZ: Asia/Shanghai
      SQL_DSN: postgresql://${POSTGRES_USER:?set POSTGRES_USER}:${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB:?set POSTGRES_DB}
      REDIS_CONN_STRING: redis://default:${REDIS_PASSWORD:?set REDIS_PASSWORD}@redis:6379
      SESSION_SECRET: ${NEW_API_SESSION_SECRET:?set NEW_API_SESSION_SECRET}
      CRYPTO_SECRET: ${NEW_API_CRYPTO_SECRET:?set NEW_API_CRYPTO_SECRET}
      MEMORY_CACHE_ENABLED: "true"
      STREAMING_TIMEOUT: "300"
      CHANNEL_UPDATE_FREQUENCY: "1440"
    expose:
      - "3000"
    networks:
      - proxy
      - backend
    labels:
      - "traefik.enable=true"
      - "traefik.docker.network=proxy"
      - "traefik.http.services.new-api.loadbalancer.server.port=3000"
      - "traefik.http.routers.new-api.rule=Host(`${AI_HOST:?set AI_HOST}`)"
      - "traefik.http.routers.new-api.entrypoints=websecure"
      - "traefik.http.routers.new-api.tls=true"
      - "traefik.http.routers.new-api.tls.certresolver=le"
      - "traefik.http.routers.new-api.service=new-api"
```

If the validated New API image uses a different listening port, update the service port and document the reason in `README.md`.

- [ ] **Step 13: Make `cliproxyapi` private**

Modify `cliproxyapi`:

```yaml
    image: eceasy/cli-proxy-api:${CLIPROXYAPI_IMAGE_TAG:?set CLIPROXYAPI_IMAGE_TAG}
    networks:
      - backend
    labels:
      - "traefik.enable=false"
```

Remove all existing `traefik.http.routers.cliproxyapi.*` labels from the base Compose file.

- [ ] **Step 14: Confirm CPA Usage Keeper image and environment**

Use the upstream README values verified during planning:

```text
Image: ghcr.io/willxup/cpa-usage-keeper:v1.3.3
CPA_BASE_URL: http://cliproxyapi:8317
CPA_MANAGEMENT_KEY: CLIProxyAPI management key
REDIS_QUEUE_ADDR: cliproxyapi:8317
AUTH_ENABLED: true
LOGIN_PASSWORD: dashboard password
WORK_DIR: /data
```

Before editing Compose, re-check the README for changes. If these names changed or the image/tag is unavailable, stop and ask for approval before substituting a custom collector.

- [ ] **Step 15: Add `cpa-usage-keeper` service**

Use the confirmed CPA Usage Keeper image name and environment variables:

```yaml
  cpa-usage-keeper:
    image: ${CPA_USAGE_KEEPER_IMAGE:?set CPA_USAGE_KEEPER_IMAGE}:${CPA_USAGE_KEEPER_IMAGE_TAG:?set CPA_USAGE_KEEPER_IMAGE_TAG}
    container_name: cpa-usage-keeper
    restart: unless-stopped
    depends_on:
      - cliproxyapi
    environment:
      TZ: Asia/Shanghai
      CPA_BASE_URL: http://cliproxyapi:8317
      CPA_MANAGEMENT_KEY: ${MANAGEMENT_SECRET:?set MANAGEMENT_SECRET}
      REDIS_QUEUE_ADDR: cliproxyapi:8317
      AUTH_ENABLED: ${CPA_USAGE_KEEPER_AUTH_ENABLED:-true}
      LOGIN_PASSWORD: ${CPA_USAGE_KEEPER_AUTH_PASSWORD:?set CPA_USAGE_KEEPER_AUTH_PASSWORD}
      USAGE_SYNC_MODE: redis
      REDIS_QUEUE_BATCH_SIZE: "1000"
      REDIS_QUEUE_IDLE_INTERVAL: 1s
      WORK_DIR: /data
    volumes:
      - "cpa-usage-keeper-data:/data"
    networks:
      - backend
```

If CPA Usage Keeper uses different variable names in the current README, use its documented names and update `.env.example`, tests, and `README.md` in the same task. Do not attach Traefik labels.

- [ ] **Step 16: Add volumes**

Add:

```yaml
volumes:
  postgres-data:
  redis-data:
  cpa-usage-keeper-data:
```

- [ ] **Step 17: Run template tests**

Run:

```bash
python -m unittest tests.test_api_site_templates -v
```

Expected: PASS.

- [ ] **Step 18: Render Compose config**

Run:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
cp docker-compose.yml "$tmpdir"/
cp .env.example "$tmpdir/.env"
cp config.yaml.template "$tmpdir/config.yaml"
mkdir -p "$tmpdir/auths" "$tmpdir/logs" "$tmpdir/letsencrypt"
touch "$tmpdir/letsencrypt/acme.json"
chmod 600 "$tmpdir/letsencrypt/acme.json"
(cd "$tmpdir" && docker compose config --format json) > /tmp/api-site-compose.json
python scripts/validate-api-site-compose.py /tmp/api-site-compose.json --host ai.x2r.store
```

Expected: `api-site compose validation passed`.

- [ ] **Step 19: Commit**

```bash
git add docker-compose.yml .env.example config.yaml.template tests/test_api_site_templates.py
git commit -m "feat: add new api site compose"
```

---

### Task 4: Temporary CLIProxyAPI Public Override Template

**Files:**
- Create: `docker-compose.cliproxy-public.override.yml.template`
- Test: `tests/test_api_site_templates.py`

- [ ] **Step 1: Add a static test that override is clearly non-production**

Append to `tests/test_api_site_templates.py`:

```python
    def test_cliproxy_public_override_is_template_only(self):
        text = self.read("docker-compose.cliproxy-public.override.yml.template")
        self.assertIn("TEMPORARY MAINTENANCE ONLY", text)
        self.assertIn("cliproxyapi:", text)
        self.assertIn("traefik.enable=true", text)
        self.assertNotIn("traefik.http.routers.cliproxyapi.rule", text)
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
python -m unittest tests.test_api_site_templates.ApiSiteTemplateTests.test_cliproxy_public_override_is_template_only -v
```

Expected: FAIL because the template does not exist.

- [ ] **Step 3: Create override template**

Create `docker-compose.cliproxy-public.override.yml.template`:

```yaml
# TEMPORARY MAINTENANCE ONLY.
# Do not copy this file to docker-compose.override.yml.
# Use only with an explicit command and a dated decommission window:
# docker compose -f docker-compose.yml -f docker-compose.cliproxy-public.override.yml up -d cliproxyapi

services:
  cliproxyapi:
    networks:
      - proxy
      - backend
    labels:
      - "traefik.enable=true"
      - "traefik.docker.network=proxy"
      - "traefik.http.services.cliproxyapi.loadbalancer.server.port=8317"
      - "traefik.http.routers.cliproxyapi-temp.rule=Host(`${TEMP_CLIPROXY_HOST:?set TEMP_CLIPROXY_HOST}`)"
      - "traefik.http.routers.cliproxyapi-temp.entrypoints=websecure"
      - "traefik.http.routers.cliproxyapi-temp.tls=true"
      - "traefik.http.routers.cliproxyapi-temp.tls.certresolver=le"
      - "traefik.http.routers.cliproxyapi-temp.service=cliproxyapi"
```

- [ ] **Step 4: Run the test**

Run:

```bash
python -m unittest tests.test_api_site_templates.ApiSiteTemplateTests.test_cliproxy_public_override_is_template_only -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add docker-compose.cliproxy-public.override.yml.template tests/test_api_site_templates.py
git commit -m "docs: add temporary cliproxy access override"
```

---

### Task 5: Backup Script

**Files:**
- Create: `scripts/backup-api-site.sh`
- Create: `tests/test_backup_api_site.py`

- [ ] **Step 1: Write failing backup script tests**

Create `tests/test_backup_api_site.py`:

```python
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "backup-api-site.sh"


class BackupApiSiteTests(unittest.TestCase):
    def test_script_uses_strict_shell(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -euo pipefail", text)

    def test_script_refuses_repo_backup_dir(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("Refusing to write backups inside repository", text)
        self.assertIn("BACKUP_DIR must be an absolute path", text)
        self.assertIn("realpath", text)

    def test_script_backs_up_postgres_and_cliproxy_state(self):
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("set -a", text)
        self.assertIn("source .env", text)
        self.assertIn("Missing required backup source", text)
        self.assertNotIn("2>/dev/null || true", text)
        self.assertIn("docker compose exec -T postgres pg_dump", text)
        self.assertIn("auths", text)
        self.assertIn("config.yaml", text)
        self.assertIn("cpa-usage-keeper", text)

    def test_script_rejects_relative_backup_dir_before_docker_calls(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            scripts = root / "scripts"
            scripts.mkdir()
            backup_script = scripts / "backup-api-site.sh"
            backup_script.write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
            backup_script.chmod(0o755)

            result = subprocess.run(
                [str(backup_script)],
                cwd=root,
                env={"BACKUP_DIR": "backups", "PATH": "/usr/bin:/bin"},
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("BACKUP_DIR must be an absolute path", result.stderr)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
python -m unittest tests.test_backup_api_site -v
```

Expected: FAIL because `scripts/backup-api-site.sh` does not exist.

- [ ] **Step 3: Implement backup script**

Create `scripts/backup-api-site.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

backup_root="${BACKUP_DIR:-/var/backups/cliproxy-api-site}"
if [[ "$backup_root" != /* ]]; then
  echo "BACKUP_DIR must be an absolute path outside the repository: $backup_root" >&2
  exit 1
fi
backup_root="$(realpath -m "$backup_root")"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${backup_root}/${timestamp}"

case "$backup_root" in
  "$repo_root"|"$repo_root"/*)
    echo "Refusing to write backups inside repository: $backup_root" >&2
    exit 1
    ;;
esac

mkdir -p "$dest"
chmod 700 "$dest"

for required_path in config.yaml auths; do
  if [[ ! -e "$required_path" ]]; then
    echo "Missing required backup source: $required_path" >&2
    exit 1
  fi
done

docker compose exec -T postgres pg_dump \
  -U "${POSTGRES_USER:?set POSTGRES_USER}" \
  -d "${POSTGRES_DB:?set POSTGRES_DB}" \
  --format=custom \
  > "${dest}/newapi-postgres.dump"

tar --warning=no-file-changed -czf "${dest}/cliproxy-runtime.tgz" \
  config.yaml \
  auths

if docker compose ps --services --filter status=running | grep -qx "cpa-usage-keeper"; then
  docker compose cp cpa-usage-keeper:/data "${dest}/cpa-usage-keeper-data"
fi

find "$dest" -type f ! -name SHA256SUMS -print0 \
  | sort -z \
  | xargs -0 sha256sum \
  > "${dest}/SHA256SUMS"

echo "Backup written to ${dest}"
```

- [ ] **Step 4: Make executable**

Run:

```bash
chmod +x scripts/backup-api-site.sh
```

- [ ] **Step 5: Run tests**

Run:

```bash
python -m unittest tests.test_backup_api_site -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add scripts/backup-api-site.sh tests/test_backup_api_site.py
git commit -m "feat: add api site backup script"
```

---

### Task 6: Launch Verification Script

**Files:**
- Create: `scripts/verify-api-site.sh`
- Create: `tests/test_verify_api_site.py`

- [ ] **Step 1: Write failing static tests for verification script**

Create `tests/test_verify_api_site.py`:

```python
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
python -m unittest tests.test_verify_api_site -v
```

Expected: FAIL because `scripts/verify-api-site.sh` does not exist.

- [ ] **Step 3: Implement verification script**

Create `scripts/verify-api-site.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

ai_host="${AI_HOST:-ai.x2r.store}"
base_url="https://${ai_host}"

echo "Checking New API public endpoint: ${base_url}"
curl -fsS -I "${base_url}" >/dev/null

if [[ -n "${NEW_API_TEST_API_KEY:-}" ]]; then
  echo "Checking /v1/models with NEW_API_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${NEW_API_TEST_API_KEY}" \
    "${base_url}/v1/models" >/dev/null
else
  echo "Skipping /v1/models credentialed check; set NEW_API_TEST_API_KEY to enable"
fi

if [[ -n "${CODEX_TEST_API_KEY:-}" ]]; then
  echo "Checking /v1/responses with CODEX_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${CODEX_TEST_API_KEY}" \
    -H "Content-Type: application/json" \
    "${base_url}/v1/responses" \
    -d '{"model":"codex-cli","input":"Reply with ok.","store":false}' >/dev/null
else
  echo "Skipping /v1/responses check; set CODEX_TEST_API_KEY to enable"
fi

echo "Checking New API container can reach internal CLIProxyAPI"
docker compose exec -T new-api sh -lc \
  "wget -qO- --header='Authorization: Bearer ${CLIPROXY_INTERNAL_API_KEY:?set CLIPROXY_INTERNAL_API_KEY}' http://cliproxyapi:8317/v1/models >/dev/null"

echo "Checking CLIProxyAPI public host is blocked: ${CLIPROXY_PUBLIC_HOST:?set CLIPROXY_PUBLIC_HOST}"
http_code="$(curl -k -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 "https://${CLIPROXY_PUBLIC_HOST}/v1/models" || true)"
if [[ "$http_code" != "000" ]]; then
  echo "CLIProxyAPI must not be publicly reachable: ${CLIPROXY_PUBLIC_HOST} returned HTTP ${http_code}" >&2
  exit 1
fi

echo "API-site verification checks completed"
```

- [ ] **Step 4: Make executable and run tests**

Run:

```bash
chmod +x scripts/verify-api-site.sh
python -m unittest tests.test_verify_api_site -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add scripts/verify-api-site.sh tests/test_verify_api_site.py
git commit -m "feat: add api site verification script"
```

---

### Task 7: API Site Runbook

**Files:**
- Create: `docs/api-site-runbook.md`
- Modify: `README.md`

- [ ] **Step 1: Draft runbook**

Create `docs/api-site-runbook.md` with these sections:

```markdown
# API Site Runbook

## Release Version Selection

Use:

```bash
scripts/select-new-api-version.py
```

Do not use GitHub's Latest marker blindly. Select the highest non-prerelease semver tag.

## Initial Setup

1. Copy `.env.example` to `.env`.
2. Generate secrets with `openssl rand -hex 32`.
3. Generate `CLIPROXY_INTERNAL_API_KEY` with `scripts/generate-api-key.py`.
4. Copy `config.yaml.template` to `config.yaml`.
5. Replace `config.yaml` `remote-management.secret-key` with the exact `MANAGEMENT_SECRET` value from `.env`.
6. Replace `config.yaml` `api-keys` with the exact `CLIPROXY_INTERNAL_API_KEY` value from `.env`.
7. Create `auths`, `logs`, and `letsencrypt`.

`MANAGEMENT_SECRET` must match in `.env` and `config.yaml`; CPA Usage Keeper reads it from `.env`, while CLIProxyAPI authenticates against `config.yaml`. `CLIPROXY_INTERNAL_API_KEY` must also match; New API uses the `.env` value for the internal channel and CLIProxyAPI accepts the `config.yaml` value.

## New API Admin Hardening

- Rotate default admin credentials before public launch.
- Enable login rate limiting and captcha/2FA where supported.
- Disable unused authentication methods.
- Disable online payment providers.
- Disable registration bonus/free-credit settings.
- Confirm the only public top-up route is redeem-code redemption.

## New API Business Configuration

- Enable invitation-code registration.
- Set new user initial balance to `$0`.
- Configure redeem-code denominations: `$10`, `$50`, `$100`, `$500`.
- Preserve `$1 = 500,000 quota units`.
- Create groups: `unactivated`, `standard`, `trusted`, `admin-test`.
- Keep `standard` users on CLIProxyAPI-backed channels only.

## Channel Configuration

- Add CLIProxyAPI as an internal New API channel using `http://cliproxyapi:8317`.
- Use `CLIPROXY_INTERNAL_API_KEY` as the channel key.
- Configure `codex-cli` only after `/v1/responses` validation.
- Configure official OpenAI fallback only for `admin-test`.

## Codex Validation

Record:

- Codex CLI version.
- New API version.
- CLIProxyAPI version.
- Model alias.
- Request id correlation.
- Balance before and after.

Run a real Codex CLI agent session in a disposable test repository.

## Billing Acceptance

Before any model is visible to `standard` users, record:

- New API model ratio.
- Completion/output ratio if supported.
- Group ratio.
- Cache-token handling if supported.
- Reasoning-token handling if reported.
- Failed-attempt charging behavior.
- Retry charging behavior.
- Effective total-token billing rule if separate input/output/cache/reasoning rates are unavailable.

For each case, record user balance before, user balance after, observed quota delta, expected quota delta, request id or correlation id, New API channel, CLIProxyAPI usage event if applicable, and official provider bill if applicable:

| Case | Endpoint | Required validation |
| --- | --- | --- |
| Responses non-stream | `/v1/responses` | Input/output usage and quota delta match configured model/group formula within documented tolerance. |
| Responses stream | `/v1/responses` streaming | Final usage appears and deduction matches equivalent non-stream usage within tolerance. |
| Tool/function call | `/v1/responses` | Tool-call tokens are included in deduction or explicitly documented if provider usage excludes them. |
| Long context | `/v1/responses` | Request succeeds with correct deduction or is rejected before upstream call. |
| Upstream retry | CLIProxyAPI-backed model | User is charged only for final billable provider usage, or retry charging is documented and priced. |
| Upstream failure | CLIProxyAPI-backed model | No balance deduction unless New API/provider reports billable usage. |
| Official fallback test | `admin-test` only | New API deduction and official provider bill reconcile before any trusted-user exposure. |

## Usage Keeper

- Keep CPA Usage Keeper private on the backend network.
- Persist its data.
- Use the CLIProxyAPI management key from `.env`.
- Do not expose the dashboard unless protected.

## Backup And Restore

- Run `scripts/backup-api-site.sh`.
- Store encrypted backups off-host.
- Restore Postgres into a disposable environment before meaningful paid usage.

## Rollback

- Keep previous pinned image tags.
- Back up Postgres before upgrades.
- Roll back Compose image tags and run `docker compose up -d`.

## Launch Gates

Run:

```bash
test -f .env
test -f config.yaml
! rg -n 'replace-with-|CHANGEME|change-me' .env config.yaml
docker compose config --format json > /tmp/api-site-compose.json
scripts/validate-api-site-compose.py /tmp/api-site-compose.json --host ai.x2r.store
scripts/verify-api-site.sh
```
```

- [ ] **Step 2: Update `README.md`**

Add a short top-level section near the top:

```markdown
## API Site Mode

This repository now targets `ai.x2r.store` as a New API-fronted public API site. New API is the only public user and SDK entry point. CLIProxyAPI is internal-only and is used as a New API upstream channel.

For implementation and operations, see:

- `docs/superpowers/specs/2026-05-03-new-api-cliproxy-api-site-design.md`
- `docs/superpowers/plans/2026-05-03-new-api-cliproxy-api-site.md`
- `docs/api-site-runbook.md`
```

Also update old sections that say `cliproxy.x2r.store` is the main public hostname so they either point to `ai.x2r.store` or clearly describe historical CLIProxyAPI-only mode.

- [ ] **Step 3: Run docs sanity checks**

Run:

```bash
rg -n "cliproxy\\.x2r\\.store|API_HOST|request-log: true|management.html" README.md docs/api-site-runbook.md
```

Expected: any remaining matches are explicitly marked as legacy, temporary, or internal-ops references.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/api-site-runbook.md
git commit -m "docs: add api site runbook"
```

---

### Task 8: Compose Render And Validation Documentation

**Files:**
- Modify: `README.md`
- Modify: `docs/api-site-runbook.md`

- [ ] **Step 1: Add exact local validation command to README**

Add:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
cp docker-compose.yml "$tmpdir"/
cp .env.example "$tmpdir/.env"
cp config.yaml.template "$tmpdir/config.yaml"
mkdir -p "$tmpdir/auths" "$tmpdir/logs" "$tmpdir/letsencrypt"
touch "$tmpdir/letsencrypt/acme.json"
chmod 600 "$tmpdir/letsencrypt/acme.json"
(cd "$tmpdir" && docker compose config --format json) > /tmp/api-site-compose.json
scripts/validate-api-site-compose.py /tmp/api-site-compose.json --host ai.x2r.store
```

- [ ] **Step 2: Add production launch command sequence to runbook**

Add:

```bash
docker compose config
scripts/backup-api-site.sh
docker compose pull
docker compose up -d
docker compose ps
scripts/verify-api-site.sh
```

- [ ] **Step 3: Run grep checks**

Run:

```bash
rg -n "validate-api-site-compose|backup-api-site|verify-api-site" README.md docs/api-site-runbook.md
```

Expected: all three scripts are referenced.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/api-site-runbook.md
git commit -m "docs: document api site validation"
```

---

### Task 9: Full Local Test Suite

**Files:**
- No new files expected.
- Verify all changed files.

- [ ] **Step 1: Run unit tests**

Run:

```bash
python -m unittest discover -s tests -v
```

Expected: PASS.

- [ ] **Step 2: Run Compose validation using `.env.example`**

Run:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
cp docker-compose.yml "$tmpdir"/
cp .env.example "$tmpdir/.env"
cp config.yaml.template "$tmpdir/config.yaml"
mkdir -p "$tmpdir/auths" "$tmpdir/logs" "$tmpdir/letsencrypt"
touch "$tmpdir/letsencrypt/acme.json"
chmod 600 "$tmpdir/letsencrypt/acme.json"
(cd "$tmpdir" && docker compose config --format json) > /tmp/api-site-compose.json
scripts/validate-api-site-compose.py /tmp/api-site-compose.json --host ai.x2r.store
```

Expected: `api-site compose validation passed`.

- [ ] **Step 3: Run static grep checks for risky legacy defaults**

Run:

```bash
! rg -n 'traefik.http.routers.cliproxyapi|API_HOST=cliproxy|request-log: true|disable-control-panel: false' docker-compose.yml .env.example config.yaml.template
test -f .env
test -f config.yaml
! rg -n 'replace-with-|CHANGEME|change-me' .env config.yaml
```

Expected: command exits `0`, meaning none of those risky defaults remain in production templates.

- [ ] **Step 4: Confirm git status**

Run:

```bash
git status --short
```

Expected: clean.

- [ ] **Step 5: Commit any final fixes**

If any fixes were required:

```bash
git add <fixed-files>
git commit -m "fix: complete api site validation"
```

If no fixes were required, do not create an empty commit.

---

## Implementation Notes

- Do not use `latest` for New API or CPA Usage Keeper once validation has chosen a tag.
- Pin `CLIPROXYAPI_IMAGE_TAG` before production launch so the Compose validator and rollback plan stay meaningful.
- Keep secrets out of git. `.env` and `config.yaml` remain local-only runtime files.
- Do not expose CPA Usage Keeper through Traefik in the base Compose file.
- Do not expose CLIProxyAPI through Traefik in the base Compose file.
- Do not treat CPA Usage Keeper as billing source of truth. New API remains the billing ledger.
- Real Codex CLI validation requires a live environment and should not be simulated only with `curl`.
