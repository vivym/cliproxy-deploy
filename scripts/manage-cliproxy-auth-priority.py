#!/usr/bin/env python3
"""Manage CLIProxyAPI auth priorities through the management API."""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict, Iterable, List


DEFAULT_BASE_URL = "http://127.0.0.1:8317"
AUTH_FILES_PATH = "/v0/management/auth-files"
AUTH_FIELDS_PATH = "/v0/management/auth-files/fields"


class APIError(RuntimeError):
    pass


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="List and update CLIProxyAPI auth file priorities."
    )
    parser.add_argument(
        "--base-url",
        default=os.environ.get("CLIPROXY_MANAGEMENT_URL", DEFAULT_BASE_URL),
        help=(
            "CLIProxyAPI management base URL. Defaults to "
            f"{DEFAULT_BASE_URL}, or CLIPROXY_MANAGEMENT_URL when set."
        ),
    )
    parser.add_argument(
        "--management-key",
        default=os.environ.get("MANAGEMENT_SECRET"),
        help="Management key. Defaults to MANAGEMENT_SECRET.",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=10.0,
        help="HTTP request timeout in seconds. Default: 10.",
    )

    subparsers = parser.add_subparsers(dest="command", required=True)

    list_parser = subparsers.add_parser("list", help="List auth files and priorities.")
    list_parser.add_argument("--json", action="store_true", help="Print raw JSON output.")

    set_parser = subparsers.add_parser("set", help="Set one auth file priority.")
    set_parser.add_argument("--name", required=True, help="Auth file name or auth ID.")
    set_parser.add_argument("--priority", required=True, type=int, help="Priority value.")
    set_parser.add_argument("--note", help="Optional note to persist with the auth file.")

    apply_parser = subparsers.add_parser("apply", help="Apply a JSON priority plan.")
    apply_parser.add_argument("plan", type=Path, help="Path to JSON priority plan.")
    apply_parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Print planned updates without calling the management API.",
    )

    return parser.parse_args()


def require_management_key(value: str | None) -> str:
    if not value:
        raise ValueError("set MANAGEMENT_SECRET or pass --management-key")
    return value


def join_url(base_url: str, path: str) -> str:
    return base_url.rstrip("/") + path


def request_json(
    base_url: str,
    management_key: str,
    method: str,
    path: str,
    payload: Dict[str, Any] | None = None,
    timeout: float = 10.0,
) -> Dict[str, Any]:
    data = None
    headers = {
        "Authorization": f"Bearer {management_key}",
        "Accept": "application/json",
    }
    if payload is not None:
        data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        headers["Content-Type"] = "application/json"

    req = urllib.request.Request(
        join_url(base_url, path),
        data=data,
        headers=headers,
        method=method,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        raise APIError(f"{method} {path} failed with HTTP {exc.code}: {body}") from None
    except urllib.error.URLError as exc:
        raise APIError(f"{method} {path} failed: {exc.reason}") from None

    if not body:
        return {}
    try:
        decoded = json.loads(body.decode("utf-8"))
    except json.JSONDecodeError as exc:
        raise APIError(f"{method} {path} returned invalid JSON: {exc}") from None
    if not isinstance(decoded, dict):
        raise APIError(f"{method} {path} returned non-object JSON")
    return decoded


def list_auth_files(base_url: str, management_key: str, timeout: float) -> List[Dict[str, Any]]:
    payload = request_json(base_url, management_key, "GET", AUTH_FILES_PATH, timeout=timeout)
    files = payload.get("files")
    if not isinstance(files, list):
        raise APIError("management API response is missing files array")
    normalized = []
    for item in files:
        if isinstance(item, dict):
            normalized.append(item)
    return normalized


def patch_auth_file(
    base_url: str,
    management_key: str,
    name: str,
    priority: int,
    note: str | None,
    timeout: float,
) -> None:
    payload: Dict[str, Any] = {"name": name, "priority": priority}
    if note is not None:
        payload["note"] = note
    request_json(
        base_url,
        management_key,
        "PATCH",
        AUTH_FIELDS_PATH,
        payload=payload,
        timeout=timeout,
    )


def as_text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bool):
        return "yes" if value else "no"
    return str(value)


def format_table(rows: List[Dict[str, Any]]) -> str:
    columns = [
        ("name", "name"),
        ("provider", "provider"),
        ("email", "email"),
        ("priority", "priority"),
        ("status", "status"),
        ("disabled", "disabled"),
        ("unavailable", "unavailable"),
        ("note", "note"),
    ]
    table = []
    headers = [label for _, label in columns]
    table.append(headers)
    for row in rows:
        table.append([as_text(row.get(key)) for key, _ in columns])

    widths = [0] * len(columns)
    for row in table:
        for index, cell in enumerate(row):
            widths[index] = max(widths[index], len(cell))

    lines = []
    for row_index, row in enumerate(table):
        line = "  ".join(cell.ljust(widths[index]) for index, cell in enumerate(row))
        lines.append(line.rstrip())
        if row_index == 0:
            lines.append("  ".join("-" * width for width in widths).rstrip())
    return "\n".join(lines)


def load_plan(path: Path) -> List[Dict[str, Any]]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise ValueError(f"plan file not found: {path}") from None
    except json.JSONDecodeError as exc:
        raise ValueError(f"plan file is not valid JSON: {exc}") from None

    entries = payload.get("accounts") if isinstance(payload, dict) else payload
    if not isinstance(entries, list):
        raise ValueError("plan must be a JSON array or an object with an accounts array")

    plan: List[Dict[str, Any]] = []
    for index, entry in enumerate(entries, start=1):
        if not isinstance(entry, dict):
            raise ValueError(f"plan entry #{index} must be an object")
        name = str(entry.get("name") or "").strip()
        if not name:
            raise ValueError(f"plan entry #{index} is missing name")
        priority = entry.get("priority")
        if not isinstance(priority, int):
            raise ValueError(f"plan entry #{index} priority must be an integer")
        item: Dict[str, Any] = {"name": name, "priority": priority}
        if "note" in entry:
            item["note"] = "" if entry["note"] is None else str(entry["note"])
        plan.append(item)
    return plan


def print_plan(plan: Iterable[Dict[str, Any]], prefix: str = "") -> None:
    for entry in plan:
        suffix = ""
        if "note" in entry:
            suffix = f" note={entry['note']!r}"
        print(f"{prefix}{entry['name']} priority={entry['priority']}{suffix}")


def main() -> int:
    args = parse_args()
    try:
        management_key = require_management_key(args.management_key)

        if args.command == "list":
            files = list_auth_files(args.base_url, management_key, args.timeout)
            if args.json:
                print(json.dumps({"files": files}, ensure_ascii=False, indent=2))
            else:
                print(format_table(files))
            return 0

        if args.command == "set":
            patch_auth_file(
                args.base_url,
                management_key,
                args.name,
                args.priority,
                args.note,
                args.timeout,
            )
            print(f"updated {args.name} priority={args.priority}")
            return 0

        if args.command == "apply":
            plan = load_plan(args.plan)
            if args.dry_run:
                print("DRY-RUN planned updates:")
                print_plan(plan, prefix="  ")
                return 0
            for entry in plan:
                patch_auth_file(
                    args.base_url,
                    management_key,
                    entry["name"],
                    entry["priority"],
                    entry.get("note"),
                    args.timeout,
                )
                print(f"updated {entry['name']} priority={entry['priority']}")
            return 0

        raise ValueError(f"unknown command: {args.command}")
    except (APIError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
