#!/usr/bin/env python3
"""Profile local -> Cloudflare -> VPS latency for CLIProxyAPI."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import List

from profile_common import Target, print_summary, read_api_key, run_target, write_csv


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Compare local proxy, direct Cloudflare, and direct VPS origin latency."
    )
    parser.add_argument("--host", default="cliproxy.x2r.store", help="Public hostname.")
    parser.add_argument(
        "--path",
        default="/management.html",
        help="Path to profile. Default: /management.html.",
    )
    parser.add_argument(
        "--origin-ip",
        help="VPS origin IP for direct origin profiling via curl --resolve.",
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
        "--proxy",
        help="Explicit proxy URL for the proxy target. If omitted, curl uses proxy env.",
    )
    parser.add_argument(
        "--api-key-env",
        help="Environment variable containing API key for protected paths.",
    )
    parser.add_argument("--csv", type=Path, help="Write raw samples to a CSV file.")
    parser.add_argument("--curl-bin", default="curl", help="curl binary path.")
    return parser.parse_args()


def normalized_path(path: str) -> str:
    return path if path.startswith("/") else f"/{path}"


def build_targets(host: str, path: str, origin_ip: str | None, proxy: str | None) -> List[Target]:
    url = f"https://{host}{normalized_path(path)}"
    proxy_args = ["--proxy", proxy] if proxy else []
    targets = [
        Target("cf-default", url, proxy_args),
        Target("cf-direct", url, ["--noproxy", "*"]),
    ]
    if origin_ip:
        targets.append(
            Target(
                "origin-direct",
                url,
                ["--noproxy", "*", "--resolve", f"{host}:443:{origin_ip}"],
            )
        )
    return targets


def print_deltas(summary):
    print("derived")
    if "cf-default" in summary and "cf-direct" in summary:
        proxy_delta = summary["cf-default"]["total_p50_ms"] - summary["cf-direct"]["total_p50_ms"]
        print(f"proxy_delta_ms,{proxy_delta:.3f}")
    if "cf-direct" in summary and "origin-direct" in summary:
        cf_delta = summary["cf-direct"]["total_p50_ms"] - summary["origin-direct"]["total_p50_ms"]
        print(f"cloudflare_delta_ms,{cf_delta:.3f}")


def main() -> int:
    args = parse_args()
    if args.runs <= 0:
        raise SystemExit("error: runs must be greater than zero")

    api_key = read_api_key(args.api_key_env)
    samples = []
    for target in build_targets(args.host, args.path, args.origin_ip, args.proxy):
        samples.extend(run_target(args.curl_bin, target, args.runs, args.timeout, api_key))

    summary = print_summary("local latency profile", samples)
    print_deltas(summary)
    if args.csv:
        write_csv(args.csv, samples)
        print(f"csv,{args.csv}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
