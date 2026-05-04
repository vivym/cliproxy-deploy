import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "manage-cliproxy-auth-priority.py"


class RecordingHandler(BaseHTTPRequestHandler):
    files = []
    requests = []

    def log_message(self, format, *args):
        return

    def _record(self, body=b""):
        self.__class__.requests.append(
            {
                "method": self.command,
                "path": self.path,
                "authorization": self.headers.get("Authorization"),
                "content_type": self.headers.get("Content-Type"),
                "body": body.decode("utf-8") if body else "",
            }
        )

    def do_GET(self):
        self._record()
        if self.path != "/v0/management/auth-files":
            self.send_response(404)
            self.end_headers()
            return
        payload = json.dumps({"files": self.__class__.files}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_PATCH(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        self._record(body)
        if self.path != "/v0/management/auth-files/fields":
            self.send_response(404)
            self.end_headers()
            return
        payload = b'{"status":"ok"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


class ManageCliproxyAuthPriorityTest(unittest.TestCase):
    def setUp(self):
        RecordingHandler.files = [
            {
                "name": "codex-a@example.com-plus.json",
                "provider": "codex",
                "email": "a@example.com",
                "priority": 20,
                "note": "expires soon",
                "disabled": False,
                "unavailable": False,
                "status": "active",
            },
            {
                "name": "codex-b@example.com-plus.json",
                "provider": "codex",
                "email": "b@example.com",
                "disabled": True,
            },
        ]
        RecordingHandler.requests = []
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), RecordingHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown()
        self.thread.join(timeout=5)
        self.server.server_close()

    def run_script(self, *args, check=True):
        env = os.environ.copy()
        env["MANAGEMENT_SECRET"] = "test-management-secret"
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--base-url",
                self.base_url,
                *args,
            ],
            check=check,
            capture_output=True,
            text=True,
            env=env,
        )

    def test_list_prints_auth_priority_summary(self):
        result = self.run_script("list")

        self.assertIn("codex-a@example.com-plus.json", result.stdout)
        self.assertIn("a@example.com", result.stdout)
        self.assertIn("20", result.stdout)
        self.assertIn("expires soon", result.stdout)
        self.assertIn("codex-b@example.com-plus.json", result.stdout)
        self.assertEqual(RecordingHandler.requests[0]["method"], "GET")
        self.assertEqual(
            RecordingHandler.requests[0]["authorization"],
            "Bearer test-management-secret",
        )

    def test_set_patches_single_auth_priority_and_note(self):
        self.run_script(
            "set",
            "--name",
            "codex-a@example.com-plus.json",
            "--priority",
            "30",
            "--note",
            "batch-a",
        )

        patch = RecordingHandler.requests[-1]
        self.assertEqual(patch["method"], "PATCH")
        self.assertEqual(patch["path"], "/v0/management/auth-files/fields")
        self.assertEqual(patch["authorization"], "Bearer test-management-secret")
        self.assertEqual(patch["content_type"], "application/json")
        self.assertEqual(
            json.loads(patch["body"]),
            {
                "name": "codex-a@example.com-plus.json",
                "priority": 30,
                "note": "batch-a",
            },
        )

    def test_apply_dry_run_reads_plan_without_patching(self):
        with tempfile.TemporaryDirectory() as tmp:
            plan = Path(tmp) / "priorities.json"
            plan.write_text(
                json.dumps(
                    [
                        {
                            "name": "codex-a@example.com-plus.json",
                            "priority": 30,
                            "note": "batch-a",
                        }
                    ]
                ),
                encoding="utf-8",
            )

            result = self.run_script("apply", str(plan), "--dry-run")

        self.assertIn("DRY-RUN", result.stdout)
        self.assertIn("codex-a@example.com-plus.json", result.stdout)
        self.assertEqual(RecordingHandler.requests, [])

    def test_apply_patches_every_entry_in_json_plan(self):
        with tempfile.TemporaryDirectory() as tmp:
            plan = Path(tmp) / "priorities.json"
            plan.write_text(
                json.dumps(
                    [
                        {"name": "codex-a@example.com-plus.json", "priority": 30},
                        {
                            "name": "codex-b@example.com-plus.json",
                            "priority": 10,
                            "note": "batch-b",
                        },
                    ]
                ),
                encoding="utf-8",
            )

            self.run_script("apply", str(plan))

        patches = [req for req in RecordingHandler.requests if req["method"] == "PATCH"]
        self.assertEqual(len(patches), 2)
        self.assertEqual(
            [json.loads(req["body"]) for req in patches],
            [
                {"name": "codex-a@example.com-plus.json", "priority": 30},
                {
                    "name": "codex-b@example.com-plus.json",
                    "priority": 10,
                    "note": "batch-b",
                },
            ],
        )

    def test_requires_management_secret(self):
        env = os.environ.copy()
        env.pop("MANAGEMENT_SECRET", None)
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--base-url",
                self.base_url,
                "list",
            ],
            capture_output=True,
            text=True,
            env=env,
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("MANAGEMENT_SECRET", result.stderr)


if __name__ == "__main__":
    unittest.main()
