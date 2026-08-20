#!/usr/bin/env python3
"""Read one-line dotenv assignments without shell evaluation or interpolation."""

from __future__ import annotations

import argparse
import pathlib
import re
import sys


KEY_PATTERN = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")


class DotenvError(ValueError):
    pass


def parse_quoted_value(raw_value: str, quote: str, line_number: int) -> str:
    value = []
    index = 1
    escape_map = {"n": "\n", "r": "\r", "t": "\t", "\\": "\\", '"': '"'}

    while index < len(raw_value):
        char = raw_value[index]
        if char == quote:
            remainder = raw_value[index + 1 :].strip()
            if remainder and not remainder.startswith("#"):
                raise DotenvError(
                    f"line {line_number}: unexpected text after quoted value"
                )
            parsed = "".join(value)
            if "\n" in parsed or "\r" in parsed or "\0" in parsed:
                raise DotenvError(f"line {line_number}: multiline values are not supported")
            return parsed

        if char == "\\" and index + 1 < len(raw_value):
            escaped = raw_value[index + 1]
            if quote == "'":
                if escaped in {"'", "\\"}:
                    value.append(escaped)
                    index += 2
                    continue
            elif escaped in escape_map:
                value.append(escape_map[escaped])
                index += 2
                continue

        value.append(char)
        index += 1

    raise DotenvError(f"line {line_number}: unterminated quoted value")


def parse_value(raw_value: str, line_number: int) -> str:
    raw_value = raw_value.strip()
    if raw_value.startswith(("'", '"')):
        quote = raw_value[0]
        value = parse_quoted_value(raw_value, quote, line_number)
        if quote == '"' and "$" in value:
            raise DotenvError(
                f"line {line_number}: interpolation is not supported; "
                "single-quote a literal $"
            )
        return value

    comment_index = None
    for index, char in enumerate(raw_value):
        if char == "#" and (index == 0 or raw_value[index - 1].isspace()):
            comment_index = index
            break
    if comment_index is not None:
        raw_value = raw_value[:comment_index]
    value = raw_value.rstrip()
    if "$" in value:
        raise DotenvError(
            f"line {line_number}: interpolation is not supported; "
            "single-quote a literal $"
        )
    if "\0" in value:
        raise DotenvError(f"line {line_number}: NUL bytes are not supported")
    return value


def parse_dotenv(path: pathlib.Path) -> dict[str, str]:
    values: dict[str, str] = {}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as error:
        raise DotenvError(f"cannot read {path}: {error}") from error

    for line_number, line in enumerate(lines, start=1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if "=" not in line:
            raise DotenvError(f"line {line_number}: expected KEY=VALUE")
        raw_key, raw_value = line.split("=", 1)
        key = raw_key.strip()
        if not KEY_PATTERN.fullmatch(key):
            raise DotenvError(f"line {line_number}: invalid key {key!r}")
        if key in values:
            raise DotenvError(f"line {line_number}: duplicate key {key}")
        values[key] = parse_value(raw_value, line_number)

    return values


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--allow-missing", action="store_true")
    parser.add_argument("--validate", action="store_true")
    parser.add_argument("env_file", type=pathlib.Path)
    parser.add_argument("keys", nargs="*")
    args = parser.parse_args()

    if args.validate and args.keys:
        parser.error("--validate does not accept keys")
    if not args.validate and not args.keys:
        parser.error("provide at least one key or use --validate")

    try:
        values = parse_dotenv(args.env_file)
        if not args.validate:
            for key in args.keys:
                if not KEY_PATTERN.fullmatch(key):
                    raise DotenvError(f"invalid requested key {key!r}")
                if key not in values and not args.allow_missing:
                    raise DotenvError(f"missing key {key} in {args.env_file}")
                print(values.get(key, ""))
    except DotenvError as error:
        print(f"dotenv error: {error}", file=sys.stderr)
        return 2

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
