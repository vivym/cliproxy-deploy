import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "convert-codex-switcher-accounts.py"


class ConvertCodexSwitcherAccountsTest(unittest.TestCase):
    def test_converts_accounts_without_printing_tokens(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            source = tmp_path / "accounts.json"
            output_dir = tmp_path / "auths"

            source.write_text(
                json.dumps(
                    {
                        "version": 1,
                        "accounts": [
                            {
                                "id": "switcher-id",
                                "name": "Account 1",
                                "email": "User+One@Example.com",
                                "plan_type": "plus",
                                "auth_mode": "chat_g_p_t",
                                "auth_data": {
                                    "type": "chat_g_p_t",
                                    "access_token": "access-secret",
                                    "account_id": "abcdef12-3456-7890-abcd-ef1234567890",
                                    "id_token": "id-secret",
                                    "refresh_token": "refresh-secret",
                                },
                            }
                        ],
                    }
                ),
                encoding="utf-8",
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    str(source),
                    str(output_dir),
                    "--now",
                    "2026-05-03T14:54:18+08:00",
                ],
                check=True,
                capture_output=True,
                text=True,
            )

            generated = output_dir / "codex-user+one@example.com-plus.json"
            self.assertTrue(generated.exists())

            converted = json.loads(generated.read_text(encoding="utf-8"))
            self.assertEqual(
                converted,
                {
                    "access_token": "access-secret",
                    "account_id": "abcdef12-3456-7890-abcd-ef1234567890",
                    "email": "User+One@Example.com",
                    "expired": "2026-05-13T14:54:18+08:00",
                    "id_token": "id-secret",
                    "last_refresh": "2026-05-03T14:54:18+08:00",
                    "refresh_token": "refresh-secret",
                    "type": "codex",
                },
            )
            self.assertEqual(stat.S_IMODE(generated.stat().st_mode), 0o600)
            self.assertIn(str(generated), result.stdout)
            self.assertNotIn("access-secret", result.stdout)
            self.assertNotIn("id-secret", result.stdout)
            self.assertNotIn("refresh-secret", result.stdout)

    def test_disambiguates_duplicate_output_names(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            source = tmp_path / "accounts.json"
            output_dir = tmp_path / "auths"

            source.write_text(
                json.dumps(
                    {
                        "accounts": [
                            {
                                "email": "same@example.com",
                                "plan_type": "plus",
                                "auth_data": {
                                    "access_token": "access-1",
                                    "account_id": "11111111-1111-1111-1111-111111111111",
                                    "id_token": "id-1",
                                    "refresh_token": "refresh-1",
                                },
                            },
                            {
                                "email": "same@example.com",
                                "plan_type": "plus",
                                "auth_data": {
                                    "access_token": "access-2",
                                    "account_id": "22222222-2222-2222-2222-222222222222",
                                    "id_token": "id-2",
                                    "refresh_token": "refresh-2",
                                },
                            },
                        ]
                    }
                ),
                encoding="utf-8",
            )

            subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    str(source),
                    str(output_dir),
                    "--now",
                    "2026-05-03T14:54:18+08:00",
                ],
                check=True,
                capture_output=True,
                text=True,
            )

            self.assertTrue((output_dir / "codex-same@example.com-plus.json").exists())
            self.assertTrue(
                (output_dir / "codex-same@example.com-plus-22222222.json").exists()
            )


if __name__ == "__main__":
    unittest.main()
