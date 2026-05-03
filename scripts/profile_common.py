"""Shared helpers for CLIProxyAPI latency profiling scripts."""

from __future__ import annotations

import csv
import json
import os
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from statistics import median
from typing import Dict, Iterable, List, Optional, Sequence


MARKER = "__CLIPROXY_PROFILE__"
METRIC_KEYS = (
    "time_namelookup",
    "time_connect",
    "time_appconnect",
    "time_pretransfer",
    "time_starttransfer",
    "time_total",
    "http_code",
    "remote_ip",
)


@dataclass
class Target:
    name: str
    url: str
    extra_args: List[str]


@dataclass
class Sample:
    target: str
    run: int
    http_code: str
    remote_ip: str
    cf_ray: str
    dns_ms: float
    tcp_ms: float
    tls_ms: float
    pretransfer_ms: float
    server_wait_ms: float
    ttfb_ms: float
    total_ms: float


def percentile(values: Sequence[float], pct: float) -> float:
    if not values:
        return 0.0
    sorted_values = sorted(values)
    index = round((len(sorted_values) - 1) * pct)
    return sorted_values[index]


def ms(value: float) -> float:
    return round(value * 1000.0, 3)


def parse_cf_ray(output: str) -> str:
    rays: List[str] = []
    for line in output.splitlines():
        name, separator, value = line.partition(":")
        if separator and name.lower() == "cf-ray":
            rays.append(value.strip())
    return rays[-1] if rays else ""


def parse_curl_output(target: str, run: int, output: str) -> Sample:
    marker_line = ""
    for line in reversed(output.splitlines()):
        if line.startswith(MARKER):
            marker_line = line[len(MARKER) :]
            break
    if not marker_line:
        raise ValueError("curl output did not contain metrics marker")

    raw = json.loads(marker_line)
    metrics: Dict[str, str] = {key: str(raw.get(key, "")) for key in METRIC_KEYS}

    time_namelookup = float(metrics["time_namelookup"] or 0)
    time_connect = float(metrics["time_connect"] or 0)
    time_appconnect = float(metrics["time_appconnect"] or 0)
    time_pretransfer = float(metrics["time_pretransfer"] or 0)
    time_starttransfer = float(metrics["time_starttransfer"] or 0)
    time_total = float(metrics["time_total"] or 0)

    return Sample(
        target=target,
        run=run,
        http_code=metrics["http_code"],
        remote_ip=metrics["remote_ip"],
        cf_ray=parse_cf_ray(output),
        dns_ms=ms(time_namelookup),
        tcp_ms=ms(max(0.0, time_connect - time_namelookup)),
        tls_ms=ms(max(0.0, time_appconnect - time_connect)),
        pretransfer_ms=ms(time_pretransfer),
        server_wait_ms=ms(max(0.0, time_starttransfer - time_pretransfer)),
        ttfb_ms=ms(time_starttransfer),
        total_ms=ms(time_total),
    )


def curl_write_out() -> str:
    return (
        "\\n"
        + MARKER
        + json.dumps({key: f"%{{{key}}}" for key in METRIC_KEYS}, separators=(",", ":"))
        + "\\n"
    )


def build_curl_command(
    curl_bin: str,
    target: Target,
    timeout: int,
    api_key: Optional[str],
) -> List[str]:
    command = [
        curl_bin,
        "-sS",
        "-D",
        "-",
        "-o",
        os.devnull,
        "--max-time",
        str(timeout),
        "-w",
        curl_write_out(),
    ]
    if api_key:
        command.extend(["-H", f"Authorization: Bearer {api_key}"])
    command.extend(target.extra_args)
    command.append(target.url)
    return command


def run_target(
    curl_bin: str,
    target: Target,
    runs: int,
    timeout: int,
    api_key: Optional[str],
) -> List[Sample]:
    samples: List[Sample] = []
    for run in range(1, runs + 1):
        command = build_curl_command(curl_bin, target, timeout, api_key)
        result = subprocess.run(command, check=True, capture_output=True, text=True)
        samples.append(parse_curl_output(target.name, run, result.stdout))
    return samples


def summarize(samples: Iterable[Sample]) -> Dict[str, Dict[str, float]]:
    grouped: Dict[str, List[Sample]] = {}
    for sample in samples:
        grouped.setdefault(sample.target, []).append(sample)

    summary: Dict[str, Dict[str, float]] = {}
    for target, target_samples in grouped.items():
        totals = [sample.total_ms for sample in target_samples]
        ttfbs = [sample.ttfb_ms for sample in target_samples]
        waits = [sample.server_wait_ms for sample in target_samples]
        summary[target] = {
            "runs": float(len(target_samples)),
            "total_p50_ms": round(median(totals), 3),
            "total_p90_ms": round(percentile(totals, 0.90), 3),
            "total_max_ms": round(max(totals), 3),
            "ttfb_p50_ms": round(median(ttfbs), 3),
            "server_wait_p50_ms": round(median(waits), 3),
        }
    return summary


def print_summary(title: str, samples: List[Sample]) -> Dict[str, Dict[str, float]]:
    summary = summarize(samples)
    print(title)
    print("target,runs,total_p50_ms,total_p90_ms,total_max_ms,ttfb_p50_ms,server_wait_p50_ms")
    for target, values in summary.items():
        print(
            ",".join(
                [
                    target,
                    str(int(values["runs"])),
                    f"{values['total_p50_ms']:.3f}",
                    f"{values['total_p90_ms']:.3f}",
                    f"{values['total_max_ms']:.3f}",
                    f"{values['ttfb_p50_ms']:.3f}",
                    f"{values['server_wait_p50_ms']:.3f}",
                ]
            )
        )
    return summary


def write_csv(path: Path, samples: List[Sample]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fields = [
        "target",
        "run",
        "http_code",
        "remote_ip",
        "cf_ray",
        "dns_ms",
        "tcp_ms",
        "tls_ms",
        "pretransfer_ms",
        "server_wait_ms",
        "ttfb_ms",
        "total_ms",
    ]
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        for sample in samples:
            writer.writerow({field: getattr(sample, field) for field in fields})


def read_api_key(env_name: Optional[str]) -> Optional[str]:
    if not env_name:
        return None
    value = os.environ.get(env_name)
    if not value:
        print(f"error: environment variable {env_name} is empty", file=sys.stderr)
        raise SystemExit(1)
    return value
