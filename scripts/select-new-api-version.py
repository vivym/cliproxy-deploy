#!/usr/bin/env python3
"""Select the latest stable New API tag from git tag output."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass
from typing import Optional


TAG_RE = re.compile(r"^v(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)$")


@dataclass(frozen=True, order=True)
class Version:
    major: int
    minor: int
    patch: int
    tag: str


def parse_stable_tag(tag: str) -> Optional[Version]:
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


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--repo",
        default="https://github.com/QuantumNous/new-api.git",
        help="Git repository to query when stdin is empty.",
    )
    args = parser.parse_args([] if argv is None else argv)

    stdin = "" if sys.stdin.isatty() else sys.stdin.read().strip()
    tags = stdin.splitlines() if stdin else fetch_tags(args.repo)
    print(select_latest_stable(tags))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
