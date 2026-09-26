#!/usr/bin/env python3
"""Tests for rewriting the personal usage-dashboard LogQL queries."""
import unittest
from pathlib import Path

from rewriter import QueryError, _read_string, inject_user_filter

_DASHBOARD = Path(__file__).resolve().parent.parent / "usage-logs-dashboard.yaml"


def _dashboard_logql_queries():
    """`expr:` / `query:` strings as Perses sends them (folded YAML and raw lines)."""
    queries = []
    lines = _DASHBOARD.read_text().splitlines()
    i = 0
    while i < len(lines):
        raw = lines[i]
        stripped = raw.lstrip()
        indent = len(raw) - len(stripped)
        if stripped.startswith("expr:"):
            val = stripped[len("expr:") :].strip()
            if len(val) >= 2 and val[0] == val[-1] == "'":
                queries.append(val[1:-1])
            i += 1
            continue
        if stripped.startswith("query:") and stripped[len("query:") :].strip() in (
            ">-",
            ">",
            "|-",
            "|",
        ):
            i += 1
            block = []
            while i < len(lines):
                line = lines[i]
                if not line.strip():
                    i += 1
                    continue
                if len(line) - len(line.lstrip()) <= indent:
                    break
                block.append(line.strip())
                i += 1
            queries.append(" ".join(block))
            queries.append("\n".join(block))
            continue
        i += 1
    return queries


class InjectUserFilterTest(unittest.TestCase):
    def test_usage_logs_dashboard_queries(self):
        queries = _dashboard_logql_queries()
        self.assertGreaterEqual(len(queries), 10, queries)
        for query in queries:
            with self.subTest(query=query[:80]):
                rewritten = inject_user_filter(query, "alice")
                self.assertIn('| user_id="alice"', rewritten)
                self.assertEqual(
                    rewritten.count("| user_id="), query.count("{service_name=")
                )
                # The in-cluster proxy rejects top-level LogQL raw strings.
                i, n = 0, len(query)
                while i < n:
                    if query[i] == '"':
                        _, i = _read_string(query, i)
                        continue
                    self.assertNotEqual(
                        query[i],
                        "`",
                        "dashboard query must not use top-level LogQL raw strings",
                    )
                    i += 1

    def test_comment_hiding_selector_is_rejected(self):
        query = """# {fake="x"}
# "
{service_name="models-as-a-service"}
# "
"""
        with self.assertRaises(QueryError):
            inject_user_filter(query, "alice")


    def test_quote_inside_raw_string_does_not_hide_selector(self):
        query = (
            'count_over_time({service_name="models-as-a-service"}'
            ' | label_format x=`{{if eq "a" "b"}}`'
            ' [1h])'
        )
        rewritten = inject_user_filter(query, "alice")
        self.assertIn('| user_id="alice"', rewritten)
        self.assertEqual(rewritten.count("| user_id="), 1)

    def test_backtick_matcher_value_is_rejected(self):
        query = (
            'sum by (user_id) (count_over_time({service_name="models-as-a-service"} |= `"` [1h]))'
            ' or sum by (user_id) (count_over_time({service_name=`models-as-a-service`} |= `"` [1h]))'
        )
        with self.assertRaises(QueryError):
            inject_user_filter(query, "alice")

    def test_unterminated_raw_string_is_rejected(self):
        query = '{service_name="models-as-a-service"} | label_format x=`oops'
        with self.assertRaises(QueryError):
            inject_user_filter(query, "alice")

if __name__ == "__main__":
    unittest.main()
