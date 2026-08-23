#!/usr/bin/env python3
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import stat
import sys
import tarfile
import uuid


FORMAT = "new-api-deployment-backup"
FORMAT_VERSION = 2
MANIFEST_NAME = "backup-manifest.json"
CHECKSUM_NAME = "SHA256SUMS"
ABSENT_MARKER = "lark-controller-data.absent"
CONTROLLER_ARCHIVE = "lark-controller-data.tgz"
REQUIRED_FILES = {
    "deployment-runtime.tgz",
    "new-api-postgres.dump",
    "sub2api-postgres.dump",
}
CHECKSUM_LINE = re.compile(r"^([0-9a-f]{64}) [ *](\./[^\n]+)$")


def fail(message):
    raise ValueError(message)


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def inventory(root):
    files = []
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root).as_posix()
        if any(character in relative for character in ("\n", "\r", "\\")):
            fail(f"unsupported backup entry name: {relative!r}")
        if relative in {MANIFEST_NAME, CHECKSUM_NAME}:
            continue
        mode = path.lstat().st_mode
        if stat.S_ISDIR(mode):
            continue
        if not stat.S_ISREG(mode):
            fail(f"unsupported backup entry: {relative}")
        files.append(
            {
                "path": relative,
                "sha256": sha256_file(path),
                "size": path.stat().st_size,
            }
        )
    return files


def checksum_inventory(root):
    checksum_path = root / CHECKSUM_NAME
    try:
        checksum_lines = checksum_path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError) as error:
        fail(f"cannot read checksum receipt: {error}")
    entries = []
    for line in checksum_lines:
        match = CHECKSUM_LINE.fullmatch(line)
        if match is None:
            fail("invalid SHA256SUMS receipt line")
        relative = match.group(2).removeprefix("./")
        path = pathlib.PurePosixPath(relative)
        if (
            not relative
            or path.is_absolute()
            or ".." in path.parts
            or any(character in relative for character in ("\n", "\r", "\\"))
        ):
            fail(f"unsafe path in SHA256SUMS: {relative!r}")
        entries.append(relative)
    if len(entries) != len(set(entries)):
        fail("SHA256SUMS contains duplicate paths")
    return entries


def validate_contract(document, root):
    if not isinstance(document, dict):
        fail("backup manifest must be a JSON object")
    if set(document) != {
        "barrier",
        "created_at",
        "files",
        "format",
        "format_version",
        "lark",
        "package_id",
    }:
        fail("backup manifest has unexpected or missing top-level fields")
    if document["format"] != FORMAT or document["format_version"] != FORMAT_VERSION:
        fail("unsupported backup manifest format")
    barrier = document.get("barrier")
    if not isinstance(barrier, dict):
        fail("backup barrier receipt must be a JSON object")
    if (
        not isinstance(document["package_id"], str)
        or not isinstance(document["created_at"], str)
        or not isinstance(barrier.get("id"), str)
    ):
        fail("backup manifest identity fields must be strings")
    try:
        uuid.UUID(document["package_id"])
        uuid.UUID(barrier["id"])
        created_at = datetime.datetime.fromisoformat(
            document["created_at"].replace("Z", "+00:00")
        )
    except (KeyError, TypeError, ValueError) as error:
        fail(f"invalid backup manifest identity: {error}")
    if created_at.utcoffset() != datetime.timedelta(0):
        fail("backup manifest created_at must be UTC")
    if set(barrier) != {"id", "kind", "lock_mode", "stopped_services"}:
        fail("invalid backup barrier receipt")
    if barrier["kind"] != "offline-quiesce-v1":
        fail("unsupported backup barrier kind")
    if barrier["lock_mode"] != "backup":
        fail("backup barrier lock mode is not backup")
    stopped_services = barrier["stopped_services"]
    if not isinstance(stopped_services, list) or any(
        not isinstance(service, str) or not service for service in stopped_services
    ):
        fail("invalid stopped service receipt")

    actual_files = inventory(root)
    if document["files"] != actual_files:
        fail("backup manifest file inventory or digest does not match package contents")
    actual_paths = {entry["path"] for entry in actual_files}
    missing = REQUIRED_FILES - actual_paths
    if missing:
        fail(f"backup manifest is missing required files: {', '.join(sorted(missing))}")
    if not any(path.startswith("sub2api-redis-data/") for path in actual_paths):
        fail("backup manifest is missing Sub2API Redis state")
    if not any(path.startswith("new-api-redis-data/") for path in actual_paths):
        fail("backup manifest is missing New API Redis state")

    checksum_path = root / CHECKSUM_NAME
    if checksum_path.exists():
        checksum_entries = checksum_inventory(root)
        expected_checksum_entries = sorted(actual_paths | {MANIFEST_NAME})
        if sorted(checksum_entries) != expected_checksum_entries:
            fail("SHA256SUMS does not cover the exact v2 package file set")

    lark = document["lark"]
    if not isinstance(lark, dict):
        fail("Lark state receipt must be a JSON object")
    if set(lark) != {
        "controller_archive",
        "controller_state",
        "integration_listener_configured",
        "state",
    }:
        fail("invalid Lark state receipt")
    if lark["state"] == "enabled":
        if (
            lark["integration_listener_configured"] is not True
            or lark["controller_state"] != "present"
            or lark["controller_archive"] != CONTROLLER_ARCHIVE
            or CONTROLLER_ARCHIVE not in actual_paths
            or ABSENT_MARKER in actual_paths
        ):
            fail("Lark enabled receipt is not paired with Controller state")
    elif lark["state"] == "absent":
        if (
            lark["integration_listener_configured"] is not False
            or lark["controller_state"] != "absent"
            or lark["controller_archive"] is not None
            or ABSENT_MARKER not in actual_paths
            or CONTROLLER_ARCHIVE in actual_paths
        ):
            fail("Lark absent receipt is not paired with the absent marker")
    else:
        fail("unsupported Lark state receipt")


def create_manifest(args):
    root = pathlib.Path(args.root).resolve()
    output = root / MANIFEST_NAME
    if output.exists():
        fail(f"backup manifest already exists: {output}")
    if args.lark_state == "enabled":
        lark = {
            "state": "enabled",
            "integration_listener_configured": True,
            "controller_state": "present",
            "controller_archive": CONTROLLER_ARCHIVE,
        }
    else:
        lark = {
            "state": "absent",
            "integration_listener_configured": False,
            "controller_state": "absent",
            "controller_archive": None,
        }
    document = {
        "format": FORMAT,
        "format_version": FORMAT_VERSION,
        "package_id": str(uuid.uuid4()),
        "created_at": args.created_at,
        "barrier": {
            "id": str(uuid.uuid4()),
            "kind": "offline-quiesce-v1",
            "lock_mode": "backup",
            "stopped_services": sorted(set(args.stopped_service)),
        },
        "lark": lark,
        "files": inventory(root),
    }
    validate_contract(document, root)
    output.write_text(
        json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    os.chmod(output, 0o600)


def validate_manifest(args):
    root = pathlib.Path(args.root).resolve()
    manifest = root / MANIFEST_NAME
    try:
        document = json.loads(manifest.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"cannot read backup manifest: {error}")
    validate_contract(document, root)
    if args.print_lark_state:
        print(document["lark"]["state"])


def validate_checksums(args):
    checksum_inventory(pathlib.Path(args.root).resolve())


def validate_archive_member(args):
    archive_path = pathlib.Path(args.archive).resolve()
    try:
        with tarfile.open(archive_path, mode="r:gz") as archive:
            matches = []
            for entry in archive.getmembers():
                normalized = pathlib.PurePosixPath(entry.name)
                parts = tuple(part for part in normalized.parts if part not in ("", "."))
                if parts == (args.member,):
                    matches.append(entry)
    except (OSError, tarfile.TarError) as error:
        fail(f"cannot inspect archive member: {error}")
    if len(matches) != 1 or not matches[0].isfile():
        fail(f"archive must contain exactly one regular {args.member}")


def parse_args():
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    create = subparsers.add_parser("create")
    create.add_argument("--root", required=True)
    create.add_argument("--created-at", required=True)
    create.add_argument("--lark-state", choices=("enabled", "absent"), required=True)
    create.add_argument("--stopped-service", action="append", default=[])
    create.set_defaults(handler=create_manifest)
    validate = subparsers.add_parser("validate")
    validate.add_argument("--root", required=True)
    validate.add_argument("--print-lark-state", action="store_true")
    validate.set_defaults(handler=validate_manifest)
    checksums = subparsers.add_parser("validate-checksums")
    checksums.add_argument("--root", required=True)
    checksums.set_defaults(handler=validate_checksums)
    archive_member = subparsers.add_parser("validate-archive-member")
    archive_member.add_argument("--archive", required=True)
    archive_member.add_argument("--member", required=True)
    archive_member.set_defaults(handler=validate_archive_member)
    return parser.parse_args()


def main():
    args = parse_args()
    try:
        args.handler(args)
    except ValueError as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
