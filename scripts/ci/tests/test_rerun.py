"""Check the workflow's Bash parser only; GitHub integration is unverified here."""

import os
from pathlib import Path
import subprocess
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".github/workflows/rerun.yml"
# Stop before the first API call. No fake GitHub responses or scheduler.
PARSER = textwrap.dedent(WORKFLOW.read_text().split("        run: |\n", 1)[1]).split("\nPR_HEAD=", 1)[0]


class RerunParserTests(unittest.TestCase):
    def parse(self, comment):
        return subprocess.run(
            ["bash", "-e", "-o", "pipefail", "-c", PARSER + '\nprintf "%s\\n%s\\n" "$MODE" "$TARGET_WORKFLOW"'],
            env={"PATH": os.environ["PATH"], "COMMENT_BODY": comment},
            capture_output=True, text=True,
        )

    def test_commands(self):
        for command, expected in (
            ("/rerun-failed", ["--failed", ""]),
            ("/rerun-all", ["", ""]),
            ("/rerun-workflow MaaS Controller", ["", "MaaS Controller"]),
            ('/rerun-workflow "MaaS Controller"', ["", "MaaS Controller"]),
        ):
            with self.subTest(command=command):
                result = self.parse(command)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(result.stdout.splitlines(), expected)

    def test_commands_in_lf_and_crlf_comments(self):
        for newline in ("\n", "\r\n"):
            for command in ("/rerun-failed", "/rerun-all", '/rerun-workflow "MaaS Controller"'):
                with self.subTest(newline=newline, command=command):
                    result = self.parse(newline.join(["Please retry CI:", command, "Thanks"]))
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_rejects_mentions_prefixes_and_missing_workflow_names(self):
        for command in ("Mention /rerun-all", "> /rerun-all", "/rerun-alligator",
                        "/rerun-failed-extra", "/rerun-workflow ", '/rerun-workflow ""'):
            with self.subTest(command=command):
                self.assertNotEqual(self.parse(command).returncode, 0)


if __name__ == "__main__":
    unittest.main()
