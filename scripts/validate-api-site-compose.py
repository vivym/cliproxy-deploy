#!/usr/bin/env python3
import argparse
import json
import re
import sys
from typing import Any, Dict, List, Set, Tuple


PINNED_TAG_RE = re.compile(r":(?!latest$)[A-Za-z0-9][A-Za-z0-9_.-]*$")

REQUIRED_SERVICES = {
    "traefik",
    "new-api",
    "postgres",
    "redis",
    "cliproxyapi",
    "cpa-usage-keeper",
}
REQUIRED_IMAGE_SERVICES = {
    "new-api",
    "postgres",
    "redis",
    "cliproxyapi",
    "cpa-usage-keeper",
}
REQUIRED_NETWORKS = {"proxy", "backend"}
REQUIRED_VOLUMES = {
    "postgres-data",
    "redis-data",
    "cpa-usage-keeper-data",
}
REQUIRED_NEW_API_ENV = {
    "SQL_DSN",
    "REDIS_CONN_STRING",
    "SESSION_SECRET",
    "CRYPTO_SECRET",
}
PUBLIC_TRAEFIK_SERVICES = {
    "new-api": "3000",
    "cliproxyapi": "8317",
    "cpa-usage-keeper": "8080",
}
PRIVATE_BACKEND_ONLY_SERVICES = {
    "postgres",
    "redis",
}
REQUIRED_SERVICE_VOLUMES = {
    "postgres": "postgres-data",
    "redis": "redis-data",
    "cpa-usage-keeper": "cpa-usage-keeper-data",
}
CLIPROXY_LOOPBACK_PORT_ERROR = (
    "cliproxyapi must publish 127.0.0.1:8317:8317 for SSH tunnel access only"
)


def labels_for(service: Dict[str, Any]) -> Dict[str, str]:
    labels = service.get("labels", {})
    if isinstance(labels, dict):
        return {str(key): str(value) for key, value in labels.items()}
    if isinstance(labels, list):
        result = {}
        for label in labels:
            if not isinstance(label, str):
                continue
            if "=" in label:
                key, value = label.split("=", 1)
            else:
                key, value = label, ""
            result[key] = value
        return result
    return {}


def label_pairs_for(service: Dict[str, Any]) -> List[Tuple[str, str]]:
    labels = service.get("labels", {})
    if isinstance(labels, dict):
        return [(str(key), str(value)) for key, value in labels.items()]
    if isinstance(labels, list):
        result = []
        for label in labels:
            if not isinstance(label, str):
                continue
            if "=" in label:
                key, value = label.split("=", 1)
            else:
                key, value = label, ""
            result.append((key, value))
        return result
    return []


def networks_for(service: Dict[str, Any]) -> Set[str]:
    networks = service.get("networks", [])
    if isinstance(networks, dict):
        return set(str(network) for network in networks.keys())
    if isinstance(networks, list):
        return set(str(network) for network in networks)
    return set()


def has_host_ports(service: Dict[str, Any]) -> bool:
    return bool(service.get("ports"))


def cliproxyapi_has_loopback_port(service: Dict[str, Any]) -> bool:
    ports = service.get("ports", [])
    if not isinstance(ports, list) or len(ports) != 1:
        return False

    port = ports[0]
    if isinstance(port, str):
        return port == "127.0.0.1:8317:8317"

    if not isinstance(port, dict):
        return False

    host_ip = str(port.get("host_ip", ""))
    published = str(port.get("published", ""))
    target = str(port.get("target", ""))
    protocol = str(port.get("protocol", "tcp"))
    if (
        host_ip == "127.0.0.1"
        and published == "8317"
        and target == "8317"
        and protocol == "tcp"
    ):
        return True

    return False


def image_is_pinned(image: str) -> bool:
    image_without_digest, separator, digest = image.partition("@")
    if separator:
        return (
            bool(image_without_digest)
            and digest.startswith("sha256:")
            and len(digest) > len("sha256:")
            and not image_without_digest.endswith(":latest")
        )
    return bool(PINNED_TAG_RE.search(image_without_digest))


def environment_for(service: Dict[str, Any]) -> Dict[str, str]:
    environment = service.get("environment", {})
    if isinstance(environment, dict):
        return {str(key): "" if value is None else str(value) for key, value in environment.items()}
    if isinstance(environment, list):
        result = {}
        for item in environment:
            if not isinstance(item, str):
                continue
            if "=" in item:
                key, value = item.split("=", 1)
            else:
                key, value = item, ""
            result[key] = value
        return result
    return {}


def service_mounts_for(service: Dict[str, Any]) -> Set[str]:
    volumes = service.get("volumes", [])
    mounts = set()
    if not isinstance(volumes, list):
        return mounts

    for volume in volumes:
        if isinstance(volume, str):
            source = volume.split(":", 1)[0]
            if source:
                mounts.add(source)
        elif isinstance(volume, dict):
            source = volume.get("source")
            if source:
                mounts.add(str(source))
    return mounts


def _service(compose: Dict[str, Any], name: str) -> Dict[str, Any]:
    services = compose.get("services", {})
    if not isinstance(services, dict):
        return {}
    service = services.get(name, {})
    return service if isinstance(service, dict) else {}


def required_public_traefik_labels(
    service_name: str,
    expected_host: str,
    internal_port: str,
) -> Dict[str, str]:
    return {
        "traefik.enable": "true",
        "traefik.docker.network": "proxy",
        "traefik.http.routers.{}.rule".format(service_name): "Host(`{}`)".format(expected_host),
        "traefik.http.routers.{}.entrypoints".format(service_name): "websecure",
        "traefik.http.routers.{}.tls".format(service_name): "true",
        "traefik.http.routers.{}.tls.certresolver".format(service_name): "le",
        "traefik.http.routers.{}.service".format(service_name): service_name,
        "traefik.http.services.{}.loadbalancer.server.port".format(service_name): internal_port,
    }


def validate_public_traefik_service(
    service_name: str,
    service: Dict[str, Any],
    expected_host: str,
    internal_port: str,
    errors: List[str],
) -> None:
    labels = labels_for(service)
    required_labels = required_public_traefik_labels(
        service_name,
        expected_host,
        internal_port,
    )
    for label, expected_value in sorted(required_labels.items()):
        actual_value = labels.get(label)
        if label.endswith(".tls") or label == "traefik.enable":
            matches = actual_value is not None and actual_value.lower() == expected_value
        else:
            matches = actual_value == expected_value
        if not matches:
            errors.append(
                "{} Traefik label {} must be {}".format(
                    service_name,
                    label,
                    expected_value,
                )
            )

    router_prefix = "traefik.http.routers.{}.".format(service_name)
    service_prefix = "traefik.http.services.{}.".format(service_name)
    for label in labels:
        if label.startswith("traefik.http.routers.") and not label.startswith(router_prefix):
            errors.append("{} must not define extra Traefik router labels".format(service_name))
        if label.startswith("traefik.http.services.") and not label.startswith(service_prefix):
            errors.append("{} must not define extra Traefik service labels".format(service_name))
        if (
            label.startswith("traefik.")
            and label not in {"traefik.enable", "traefik.docker.network"}
            and not label.startswith(router_prefix)
            and not label.startswith(service_prefix)
        ):
            errors.append("{} must not define unsupported Traefik labels".format(service_name))

    service_networks = networks_for(service)
    for network_name in ("proxy", "backend"):
        if network_name not in service_networks:
            errors.append("{} must join {}".format(service_name, network_name))


def validate(
    compose: Dict[str, Any],
    expected_host: str,
    expected_cliproxy_host: str = "cliproxy.x2r.store",
    expected_keeper_host: str = "keeper.x2r.store",
) -> List[str]:
    errors = []
    services = compose.get("services", {})
    networks = compose.get("networks", {})
    volumes = compose.get("volumes", {})

    if not isinstance(services, dict):
        services = {}
    if not isinstance(networks, dict):
        networks = {}
    if not isinstance(volumes, dict):
        volumes = {}

    for service_name in sorted(REQUIRED_SERVICES):
        if service_name not in services:
            errors.append("missing required service {}".format(service_name))

    for network_name in sorted(REQUIRED_NETWORKS):
        if network_name not in networks:
            errors.append("missing required network {}".format(network_name))

    backend = networks.get("backend", {})
    if isinstance(backend, dict) and backend.get("internal") is True:
        errors.append("backend network must not set internal: true")

    for volume_name in sorted(REQUIRED_VOLUMES):
        if volume_name not in volumes:
            errors.append("missing required volume {}".format(volume_name))

    traefik = _service(compose, "traefik")
    if "backend" in networks_for(traefik):
        errors.append("traefik must not join backend")

    public_hosts = {
        "new-api": expected_host,
        "cliproxyapi": expected_cliproxy_host,
        "cpa-usage-keeper": expected_keeper_host,
    }
    for service_name, internal_port in sorted(PUBLIC_TRAEFIK_SERVICES.items()):
        validate_public_traefik_service(
            service_name,
            _service(compose, service_name),
            public_hosts[service_name],
            internal_port,
            errors,
        )

    new_api = _service(compose, "new-api")
    new_api_environment = environment_for(new_api)
    for env_name in sorted(REQUIRED_NEW_API_ENV):
        if env_name not in new_api_environment:
            errors.append("new-api missing required environment {}".format(env_name))

    keeper_environment = environment_for(_service(compose, "cpa-usage-keeper"))
    if keeper_environment.get("AUTH_ENABLED", "").lower() != "true":
        errors.append("cpa-usage-keeper must enable AUTH_ENABLED")
    if not keeper_environment.get("LOGIN_PASSWORD"):
        errors.append("cpa-usage-keeper must set LOGIN_PASSWORD")

    for service_name, service in sorted(services.items()):
        if not isinstance(service, dict):
            continue
        if service_name not in {"traefik", "cliproxyapi"} and has_host_ports(service):
            errors.append("{} must not publish host ports".format(service_name))
        if service_name == "cliproxyapi" and not cliproxyapi_has_loopback_port(service):
            errors.append(CLIPROXY_LOOPBACK_PORT_ERROR)
        if service_name in {"traefik", *PUBLIC_TRAEFIK_SERVICES}:
            continue
        labels = label_pairs_for(service)
        if any(label == "traefik.enable" and value != "false" for label, value in labels):
            errors.append("{} must not enable Traefik".format(service_name))
        if any(
            label.startswith("traefik.")
            and not (
                label == "traefik.enable"
                and value == "false"
            )
            for label, value in labels
        ):
            errors.append("{} must not define Traefik labels".format(service_name))
        if "proxy" in networks_for(service):
            errors.append("{} must not join proxy".format(service_name))

    for service_name in sorted(PRIVATE_BACKEND_ONLY_SERVICES):
        service = _service(compose, service_name)
        if networks_for(service) != {"backend"}:
            errors.append("{} must only join backend".format(service_name))

    for service_name, volume_name in sorted(REQUIRED_SERVICE_VOLUMES.items()):
        if volume_name not in service_mounts_for(_service(compose, service_name)):
            errors.append("{} must mount required volume {}".format(service_name, volume_name))

    for service_name in sorted(REQUIRED_IMAGE_SERVICES):
        service = _service(compose, service_name)
        if not service.get("image"):
            errors.append("{} image must be pinned to a non-latest tag".format(service_name))

    for service_name, service in sorted(services.items()):
        if not isinstance(service, dict):
            continue
        if "image" in service and not image_is_pinned(str(service.get("image", ""))):
            errors.append("{} image must be pinned to a non-latest tag".format(service_name))

    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate rendered api-site Compose JSON.")
    parser.add_argument("compose_json", help="Path to rendered Compose JSON")
    parser.add_argument("--host", default="ai.x2r.store", help="Expected public hostname for New API")
    parser.add_argument(
        "--cliproxy-host",
        default="cliproxy.x2r.store",
        help="Expected public hostname for CLIProxyAPI",
    )
    parser.add_argument(
        "--keeper-host",
        default="keeper.x2r.store",
        help="Expected public hostname for CPA Usage Keeper",
    )
    args = parser.parse_args()

    try:
        with open(args.compose_json, "r", encoding="utf-8") as compose_file:
            compose = json.load(compose_file)
    except (OSError, json.JSONDecodeError) as error:
        print("ERROR: {}".format(error), file=sys.stderr)
        return 1

    if not isinstance(compose, dict):
        print("ERROR: compose JSON must be an object", file=sys.stderr)
        return 1

    errors = validate(compose, args.host, args.cliproxy_host, args.keeper_host)
    if errors:
        for error in errors:
            print("ERROR: {}".format(error), file=sys.stderr)
        return 1

    print("api-site compose validation passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
