import re
import subprocess
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "generate-api-key.py"
KEY_RE = re.compile(r"^sk-[A-Za-z0-9_-]{43}$")


class GenerateApiKeyTest(unittest.TestCase):
    def run_script(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args],
            check=True,
            capture_output=True,
            text=True,
        )

    def test_generates_one_openai_style_key_by_default(self):
        result = self.run_script()
        lines = result.stdout.strip().splitlines()

        self.assertEqual(len(lines), 1)
        self.assertRegex(lines[0], KEY_RE)

    def test_generates_requested_number_of_unique_keys(self):
        result = self.run_script("-n", "5")
        lines = result.stdout.strip().splitlines()

        self.assertEqual(len(lines), 5)
        self.assertEqual(len(set(lines)), 5)
        for line in lines:
            self.assertRegex(line, KEY_RE)

    def test_generates_yaml_api_keys_snippet(self):
        result = self.run_script("-n", "2", "--yaml")
        lines = result.stdout.strip().splitlines()

        self.assertEqual(lines[0], "api-keys:")
        self.assertEqual(len(lines), 3)
        for line in lines[1:]:
            self.assertTrue(line.startswith('  - "'))
            self.assertTrue(line.endswith('"'))
            self.assertRegex(line[5:-1], KEY_RE)

    def test_rejects_non_positive_count(self):
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "-n", "0"],
            capture_output=True,
            text=True,
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("count must be greater than zero", result.stderr)


if __name__ == "__main__":
    unittest.main()
