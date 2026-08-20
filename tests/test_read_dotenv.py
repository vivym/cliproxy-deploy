import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "read-dotenv.py"


class ReadDotenvTests(unittest.TestCase):
    def run_reader(self, text, *args):
        with tempfile.TemporaryDirectory() as tmp:
            env_file = pathlib.Path(tmp) / ".env"
            env_file.write_text(text, encoding="utf-8")
            return subprocess.run(
                ["python3", str(SCRIPT), *args, str(env_file), "VALUE"],
                text=True,
                capture_output=True,
                check=False,
            )

    def test_reads_plain_and_literal_quoted_values(self):
        plain = self.run_reader("VALUE=plain-value # comment\n")
        quoted = self.run_reader("VALUE='literal $value and \\'quote\\''\n")

        self.assertEqual(plain.returncode, 0, plain.stderr)
        self.assertEqual(plain.stdout, "plain-value\n")
        self.assertEqual(quoted.returncode, 0, quoted.stderr)
        self.assertEqual(quoted.stdout, "literal $value and 'quote'\n")

    def test_rejects_interpolation_without_executing_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            marker = root / "executed"
            env_file = root / ".env"
            env_file.write_text(f"VALUE=$(touch {marker})\n", encoding="utf-8")

            result = subprocess.run(
                ["python3", str(SCRIPT), str(env_file), "VALUE"],
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("interpolation is not supported", result.stderr)
            self.assertFalse(marker.exists())

    def test_rejects_duplicate_keys(self):
        result = self.run_reader("VALUE=first\nVALUE=second\n")

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("duplicate key VALUE", result.stderr)


if __name__ == "__main__":
    unittest.main()
