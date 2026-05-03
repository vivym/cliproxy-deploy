#!/usr/bin/env python3
"""Profile VPS-local Traefik and CLIProxyAPI container latency."""

from __future__ import annotations

import argparse
import subprocess
from pathlib import Path
from typing import List

from profile_common import Target, print_summary, read_api_key, run_target, write_csv


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Compare VPS Traefik loopback and CLIProxyAPI direct container latency."
    )
    parser.add_argument("--host", default="cliproxy.x2r.store", help="Public hostname.")
    parser.add_argument(
        "--path",
        default="/management.html",
        help="Path to profile. Default: /management.html.",
    )
    parser.add_argument(
        "--container",
        default="cliproxyapi",
        help="CLIProxyAPI container name. Default: cliproxyapi.",
    )
    parser.add_argument(
        "--cliproxy-url",
        help="Override direct CLIProxyAPI URL, e.g. http://172.19.0.5:8317.",
    )
    parser.add_argument(
        "--runs",
        type=int,
        default=5,
        help="Number of samples per target. Default: 5.",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=20,
        help="curl max-time seconds per request. Default: 20.",
    )
    parser.add_argument(
        "--api-key-env",
        help="Environment variable containing API key for protected paths.",
    )
    parser.add_argument("--csv", type=Path, help="Write raw samples to a CSV file.")
    parser.add_argument("--curl-bin", default="curl", help="curl binary path.")
    parser.add_argument("--docker-bin", default="docker", help="docker binary path.")
    return parser.parse_args()


def normalized_path(path: str) -> str:
    return path if path.startswith("/") else f"/{path}"


def inspect_container_ip(docker_bin: str, container: str) -> str:
    command = [
        docker_bin,
        "inspect",
        "-f",
        "{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}",
        container,
    ]
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    ip = next((line.strip() for line in result.stdout.splitlines() if line.strip()), "")
    if not ip:
        raise SystemExit(f"error: could not inspect IP for container {container}")
    return ip


def build_targets(args: argparse.Namespace) -> List[Target]:
    path = normalized_path(args.path)
    traefik_url = f"https://{args.host}{path}"
    cliproxy_url = args.cliproxy_url
    if not cliproxy_url:
        container_ip = inspect_container_ip(args.docker_bin, args.container)
        cliproxy_url = f"http://{container_ip}:8317{path}"

    return [
        Target(
            "traefik-loopback",
            traefik_url,
            ["--noproxy", "*", "--resolve", f"{args.host}:443:127.0.0.1"],
        ),
        Target("cliproxy-direct", cliproxy_url, ["--noproxy", "*"]),
    ]


def print_deltas(summary):
    print("derived")
    if "traefik-loopback" in summary and "cliproxy-direct" in summary:
        overhead = (
            summary["traefik-loopback"]["total_p50_ms"]
            - summary["cliproxy-direct"]["total_p50_ms"]
        )
        print(f"traefik_overhead_ms,{overhead:.3f}")


def main() -> int:
    args = parse_args()
    if args.runs <= 0:
        raise SystemExit("error: runs must be greater than zero")

    api_key = read_api_key(args.api_key_env)
    samples = []
    for target in build_targets(args):
        samples.extend(run_target(args.curl_bin, target, args.runs, args.timeout, api_key))

    summary = print_summary("origin latency profile", samples)
    print_deltas(summary)
    if args.csv:
        write_csv(args.csv, samples)
        print(f"csv,{args.csv}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
