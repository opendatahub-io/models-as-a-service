"""Check the actual jq selection policy, without mocking GitHub API calls."""

import json
from pathlib import Path
import re
import subprocess
import unittest


WORKFLOW = Path(__file__).resolve().parents[3] / ".github/workflows/rerun.yml"
SELECTOR = re.search(r"RUN_FILTER='(.*?)'\n", WORKFLOW.read_text(), re.S).group(1)


def run(number, conclusion, status="completed", workflow=10, repo=7, branch="feature"):
    return {"id": number, "run_number": number, "workflow_id": workflow,
            "head_repository": {"id": repo}, "head_branch": branch,
            "status": status, "conclusion": conclusion}


class RerunSelectionTests(unittest.TestCase):
    def select(self, *pages):
        result = subprocess.run(
            ["jq", "-r", "--argjson", "head_repo", "7", "--arg", "head_branch", "feature", SELECTOR],
            input=json.dumps([{"workflow_runs": page} for page in pages]),
            text=True, capture_output=True, check=True,
        )
        return sorted(int(line.split("\t")[0]) for line in result.stdout.splitlines())

    def test_newer_success_or_active_run_suppresses_older_failure_across_pages(self):
        for conclusion, status in (("success", "completed"), (None, "queued"), (None, "in_progress")):
            for old in ("failure", "cancelled"):
                with self.subTest(old=old, conclusion=conclusion, status=status):
                    older = run(1, old)
                    older["updated_at"] = "2099-01-01T00:00:00Z"
                    self.assertEqual(self.select([older], [run(2, conclusion, status)]), [])

    def test_latest_failed_or_cancelled_runs_remain_eligible_per_workflow(self):
        self.assertEqual(self.select([run(1, "failure"), run(2, "failure"),
                                      run(3, "cancelled", workflow=11)]), [2, 3])

    def test_other_head_repositories_and_branches_do_not_affect_selection(self):
        self.assertEqual(self.select([run(1, "failure"), run(2, "success", repo=8),
                                      run(3, "success", branch="other"),
                                      run(4, "failure", workflow=11, repo=8),
                                      run(5, "failure", workflow=11, branch="other")]), [1])
