#!/usr/bin/env python3
"""Generate OpenAI-compatible API keys for CLIProxyAPI."""

from __future__ import annotations

import argparse
import secrets
import sys
from typing import List


DEFAULT_BYTES = 32


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Generate sk- prefixed API keys suitable for CLIProxyAPI api-keys."
    )
    parser.add_argument(
        "-n",
        "--count",
        type=int,
        default=1,
        help="Number of API keys to generate. Default: 1.",
    )
    parser.add_argument(
        "--yaml",
        action="store_true",
        help="Output a config.yaml api-keys snippet instead of raw keys.",
    )
    return parser.parse_args()


def generate_key() -> str:
    return f"sk-{secrets.token_urlsafe(DEFAULT_BYTES)}"


def generate_keys(count: int) -> List[str]:
    if count <= 0:
        raise ValueError("count must be greater than zero")
    return [generate_key() for _ in range(count)]


def print_raw(keys: List[str]) -> None:
    for key in keys:
        print(key)


def print_yaml(keys: List[str]) -> None:
    print("api-keys:")
    for key in keys:
        print(f'  - "{key}"')


def main() -> int:
    args = parse_args()
    try:
        keys = generate_keys(args.count)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    if args.yaml:
        print_yaml(keys)
    else:
        print_raw(keys)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
