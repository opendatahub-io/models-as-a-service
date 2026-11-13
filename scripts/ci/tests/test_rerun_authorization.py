"""Check the actual identity-bound permission policy, without mocking GitHub."""

import json
from pathlib import Path
import re
import subprocess
import unittest


WORKFLOW = Path(__file__).resolve().parents[3] / ".github/workflows/rerun.yml"


class RerunAuthorizationTests(unittest.TestCase):
    def authorized(self, permission, user_id=7, role_name=None):
        match = re.search(r"WRITE_PERMISSION_FILTER='(.*?)'\n", WORKFLOW.read_text(), re.S)
        self.assertIsNotNone(match, "same-repository retries need a current permission check")
        result = subprocess.run(
            ["jq", "-e", "--argjson", "user_id", "7", match.group(1)],
            input=json.dumps({"permission": permission, "user": {"id": user_id},
                              "role_name": role_name}),
            text=True, capture_output=True,
        )
        self.assertIn(result.returncode, (0, 1), result.stderr)
        return result.returncode == 0

    def test_current_writers_and_admins_are_allowed(self):
        for permission in ("write", "admin"):
            with self.subTest(permission=permission):
                self.assertTrue(self.authorized(permission))

    def test_read_only_unknown_and_recycled_identities_are_rejected(self):
        for permission in ("read", "none", None):
            with self.subTest(permission=permission):
                self.assertFalse(self.authorized(permission))
        self.assertFalse(self.authorized("write", user_id=8))
        self.assertFalse(self.authorized("admin", user_id=None))

    def test_legacy_permission_covers_maintain_and_custom_roles(self):
        self.assertTrue(self.authorized("write", role_name="maintain"))
        self.assertFalse(self.authorized("read", role_name="admin"))
