import importlib.util
from pathlib import Path
import sys
from unittest import mock
import unittest


module_path = Path(__file__).resolve().parents[1] / "scripts" / "select-new-api-version.py"
spec = importlib.util.spec_from_file_location("select_new_api_version", module_path)
select_new_api_version = importlib.util.module_from_spec(spec)
assert spec.loader is not None
sys.modules["select_new_api_version"] = select_new_api_version
spec.loader.exec_module(select_new_api_version)
select_latest_stable = select_new_api_version.select_latest_stable


class SelectNewApiVersionTests(unittest.TestCase):
    def test_selects_highest_non_prerelease_semver(self):
        tags = [
            "v0.12.14",
            "v0.12.15",
            "v0.13.0",
            "v0.13.2",
            "v1.0.0-rc.1",
            "v1.0.0-rc.2",
        ]

        self.assertEqual(select_latest_stable(tags), "v0.13.2")

    def test_rejects_alpha_beta_rc_and_non_semver_tags(self):
        tags = [
            "v0.14.0-alpha.1",
            "v0.14.0-beta.1",
            "v0.14.0-rc.1",
            "latest",
            "nightly",
            "v0.13.2",
        ]

        self.assertEqual(select_latest_stable(tags), "v0.13.2")

    def test_accepts_git_ls_remote_lines(self):
        tags = [
            "abc123\trefs/tags/v0.13.1",
            "def456\trefs/tags/v0.13.2",
            "ghi789\trefs/tags/v1.0.0-rc.2",
        ]

        self.assertEqual(select_latest_stable(tags), "v0.13.2")

    def test_raises_when_no_stable_tags_exist(self):
        with self.assertRaisesRegex(ValueError, "No stable New API tags"):
            select_latest_stable(["v1.0.0-rc.1", "nightly"])

    def test_main_fetches_tags_when_stdin_is_tty(self):
        with mock.patch.object(select_new_api_version.sys.stdin, "isatty", return_value=True), \
             mock.patch.object(select_new_api_version, "fetch_tags", return_value=["v0.13.2"]), \
             mock.patch("builtins.print") as print_mock:
            self.assertEqual(select_new_api_version.main(), 0)

        print_mock.assert_called_once_with("v0.13.2")


if __name__ == "__main__":
    unittest.main()
