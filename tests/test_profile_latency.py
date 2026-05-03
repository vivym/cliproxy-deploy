import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
LOCAL_SCRIPT = ROOT / "scripts" / "profile-latency.py"
ORIGIN_SCRIPT = ROOT / "scripts" / "profile-origin.py"


def write_executable(path: Path, content: str) -> None:
    path.write_text(textwrap.dedent(content), encoding="utf-8")
    path.chmod(0o755)


class ProfileLatencyTest(unittest.TestCase):
    def test_local_profile_compares_proxy_cloudflare_and_origin(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            calls_path = tmp_path / "calls.jsonl"
            fake_curl = tmp_path / "curl"
            write_executable(
                fake_curl,
                f"""\
                #!/usr/bin/env python3
                import json
                import sys

                args = sys.argv[1:]
                with open({str(calls_path)!r}, "a", encoding="utf-8") as handle:
                    handle.write(json.dumps(args) + "\\n")

                if "--resolve" in args:
                    total = "0.100000"
                    remote_ip = "203.0.113.10"
                elif "--noproxy" in args:
                    total = "0.200000"
                    remote_ip = "198.51.100.20"
                else:
                    total = "0.300000"
                    remote_ip = "198.51.100.21"

                print("HTTP/2 200")
                print("cf-ray: test-ray")
                print()
                print("__CLIPROXY_PROFILE__" + json.dumps({{
                    "time_namelookup": "0.010000",
                    "time_connect": "0.020000",
                    "time_appconnect": "0.040000",
                    "time_pretransfer": "0.050000",
                    "time_starttransfer": total,
                    "time_total": total,
                    "http_code": "200",
                    "remote_ip": remote_ip
                }}))
                """,
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(LOCAL_SCRIPT),
                    "--host",
                    "cliproxy.x2r.store",
                    "--origin-ip",
                    "203.0.113.10",
                    "--runs",
                    "1",
                    "--curl-bin",
                    str(fake_curl),
                ],
                check=True,
                capture_output=True,
                text=True,
            )

            calls = [
                json.loads(line)
                for line in calls_path.read_text(encoding="utf-8").splitlines()
            ]
            self.assertEqual(len(calls), 3)
            self.assertFalse(any("--noproxy" in arg for arg in calls[0]))
            self.assertIn("--noproxy", calls[1])
            self.assertIn("--resolve", calls[2])
            self.assertIn("cf-default", result.stdout)
            self.assertIn("cf-direct", result.stdout)
            self.assertIn("origin-direct", result.stdout)
            self.assertIn("proxy_delta_ms", result.stdout)
            self.assertIn("cloudflare_delta_ms", result.stdout)

    def test_origin_profile_compares_traefik_and_cliproxy_direct(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            calls_path = tmp_path / "calls.jsonl"
            fake_curl = tmp_path / "curl"
            fake_docker = tmp_path / "docker"
            write_executable(
                fake_docker,
                """\
                #!/usr/bin/env python3
                print("172.19.0.5")
                """,
            )
            write_executable(
                fake_curl,
                f"""\
                #!/usr/bin/env python3
                import json
                import sys

                args = sys.argv[1:]
                with open({str(calls_path)!r}, "a", encoding="utf-8") as handle:
                    handle.write(json.dumps(args) + "\\n")

                if any("127.0.0.1" in arg for arg in args):
                    total = "0.050000"
                    remote_ip = "127.0.0.1"
                else:
                    total = "0.020000"
                    remote_ip = "172.19.0.5"

                print("HTTP/1.1 200")
                print()
                print("__CLIPROXY_PROFILE__" + json.dumps({{
                    "time_namelookup": "0.000000",
                    "time_connect": "0.001000",
                    "time_appconnect": "0.010000",
                    "time_pretransfer": "0.011000",
                    "time_starttransfer": total,
                    "time_total": total,
                    "http_code": "200",
                    "remote_ip": remote_ip
                }}))
                """,
            )

            env = os.environ.copy()
            env["PATH"] = f"{tmp_path}:{env['PATH']}"
            result = subprocess.run(
                [
                    sys.executable,
                    str(ORIGIN_SCRIPT),
                    "--host",
                    "cliproxy.x2r.store",
                    "--runs",
                    "1",
                    "--curl-bin",
                    str(fake_curl),
                    "--docker-bin",
                    str(fake_docker),
                ],
                check=True,
                capture_output=True,
                text=True,
                env=env,
            )

            calls = [
                json.loads(line)
                for line in calls_path.read_text(encoding="utf-8").splitlines()
            ]
            self.assertEqual(len(calls), 2)
            self.assertIn("--resolve", calls[0])
            self.assertTrue(any("http://172.19.0.5:8317/" in arg for arg in calls[1]))
            self.assertIn("traefik-loopback", result.stdout)
            self.assertIn("cliproxy-direct", result.stdout)
            self.assertIn("traefik_overhead_ms", result.stdout)


if __name__ == "__main__":
    unittest.main()
