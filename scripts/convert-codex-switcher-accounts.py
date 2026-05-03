#!/usr/bin/env python3
"""Convert codex-switcher accounts.json into CLIProxyAPI Codex auth files."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from datetime import datetime, timedelta
from pathlib import Path
from typing import Any, Dict, Iterable, List, Set


REQUIRED_AUTH_FIELDS = ("access_token", "account_id", "id_token", "refresh_token")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Convert codex-switcher accounts.json into one "
            "codex-<email>-<plan>.json file per account."
        )
    )
    parser.add_argument("source", type=Path, help="Path to codex-switcher accounts.json")
    parser.add_argument("output_dir", type=Path, help="Directory for generated auth files")
    parser.add_argument(
        "--now",
        help=(
            "Timestamp for last_refresh, as ISO-8601. Defaults to current local time. "
            "Useful for reproducible tests."
        ),
    )
    parser.add_argument(
        "--expires-days",
        type=int,
        default=10,
        help="Number of days after last_refresh to set expired. Default: 10.",
    )
    return parser.parse_args()


def load_accounts(path: Path) -> List[Dict[str, Any]]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except FileNotFoundError:
        raise ValueError(f"source file not found: {path}") from None
    except json.JSONDecodeError as exc:
        raise ValueError(f"source file is not valid JSON: {exc}") from None

    accounts = payload.get("accounts") if isinstance(payload, dict) else None
    if not isinstance(accounts, list):
        raise ValueError("source JSON must be an object with an accounts array")
    return accounts


def parse_now(value: str | None) -> datetime:
    if value is None:
        return datetime.now().astimezone().replace(microsecond=0)
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        raise ValueError(f"--now must be an ISO-8601 timestamp: {value}") from None
    if parsed.tzinfo is None:
        parsed = parsed.astimezone()
    return parsed.replace(microsecond=0)


def sanitize_filename_part(value: Any, fallback: str) -> str:
    text = str(value or "").strip().lower()
    text = re.sub(r"[^a-z0-9@._+-]+", "-", text)
    text = text.strip("-._")
    return text or fallback


def account_label(account: Dict[str, Any], index: int) -> str:
    email = account.get("email") or f"account #{index}"
    return str(email)


def convert_account(
    account: Dict[str, Any], index: int, last_refresh: datetime, expires_days: int
) -> Dict[str, Any]:
    auth_data = account.get("auth_data")
    if not isinstance(auth_data, dict):
        raise ValueError(f"{account_label(account, index)} is missing auth_data")

    missing = [field for field in REQUIRED_AUTH_FIELDS if not auth_data.get(field)]
    if missing:
        fields = ", ".join(missing)
        raise ValueError(f"{account_label(account, index)} is missing auth_data fields: {fields}")

    email = account.get("email")
    if not email:
        raise ValueError(f"account #{index} is missing email")

    expired = last_refresh + timedelta(days=expires_days)
    return {
        "access_token": auth_data["access_token"],
        "account_id": auth_data["account_id"],
        "email": email,
        "expired": expired.isoformat(),
        "id_token": auth_data["id_token"],
        "last_refresh": last_refresh.isoformat(),
        "refresh_token": auth_data["refresh_token"],
        "type": "codex",
    }


def base_output_path(output_dir: Path, account: Dict[str, Any]) -> Path:
    email = sanitize_filename_part(account.get("email"), "unknown-email")
    plan = sanitize_filename_part(account.get("plan_type"), "unknown-plan")
    return output_dir / f"codex-{email}-{plan}.json"


def unique_output_path(
    output_dir: Path, account: Dict[str, Any], converted: Dict[str, Any], used: Set[Path]
) -> Path:
    base = base_output_path(output_dir, account)
    if base not in used:
        return base

    account_id = sanitize_filename_part(converted.get("account_id"), "duplicate")
    suffix = account_id[:8] or "duplicate"
    candidate = base.with_name(f"{base.stem}-{suffix}{base.suffix}")
    counter = 2
    while candidate in used:
        candidate = base.with_name(f"{base.stem}-{suffix}-{counter}{base.suffix}")
        counter += 1
    return candidate


def write_json_secret(path: Path, payload: Dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
    finally:
        os.chmod(path, 0o600)


def convert_all(
    accounts: Iterable[Dict[str, Any]],
    output_dir: Path,
    last_refresh: datetime,
    expires_days: int,
) -> List[Path]:
    written: List[Path] = []
    used: Set[Path] = set()
    for index, account in enumerate(accounts, start=1):
        if not isinstance(account, dict):
            raise ValueError(f"account #{index} must be an object")
        converted = convert_account(account, index, last_refresh, expires_days)
        path = unique_output_path(output_dir, account, converted, used)
        write_json_secret(path, converted)
        used.add(path)
        written.append(path)
    return written


def main() -> int:
    args = parse_args()
    try:
        last_refresh = parse_now(args.now)
        accounts = load_accounts(args.source)
        written = convert_all(accounts, args.output_dir, last_refresh, args.expires_days)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    for path in written:
        print(path)
    print(f"converted {len(written)} account(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
