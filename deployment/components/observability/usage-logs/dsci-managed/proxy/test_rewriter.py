#!/usr/bin/env python3
"""Tests for rewriting the personal usage-dashboard LogQL queries."""
import unittest
from pathlib import Path

from dashboard_logql import (
    PERSONAL_DASHBOARD,
    assert_no_top_level_raw_strings,
    queries_from_dashboard,
)
from rewriter import QueryError, inject_user_filter


class InjectUserFilterTest(unittest.TestCase):
    def test_usage_logs_dashboard_queries(self):
        queries = queries_from_dashboard(PERSONAL_DASHBOARD)
        self.assertGreaterEqual(len(queries), 10, queries)
        for query in queries:
            with self.subTest(query=query[:80]):
                rewritten = inject_user_filter(query, "alice")
                self.assertIn('| user_id="alice"', rewritten)
                self.assertEqual(
                    rewritten.count("| user_id="), query.count("{service_name=")
                )
                assert_no_top_level_raw_strings(self, query)

    def test_api_key_identity_is_consistent_across_personal_queries(self):
        queries = queries_from_dashboard(PERSONAL_DASHBOARD)
        identity = 'label_format api_key="{{.key_name}} [{{.key_id}}]"'
        selector = next(q for q in queries if "count by (api_key)" in q)
        self.assertIn(identity, selector)
        self.assertIn('key_id!=""', selector)
        self.assertIn("keep api_key", selector)
        for query in queries:
            if "${api_key:regexliteral}" in query:
                with self.subTest(query=query[:80]):
                    self.assertIn(identity, query)
                    self.assertIn(".api_key)", query)
                    self.assertIn('label_format __ak_sel="${api_key:regexliteral}"', query)
                    self.assertNotIn('key_id!=""', query)  # All includes older logs.
                    self.assertIn('| __ak_keep="true"', query)

        dashboard = Path(PERSONAL_DASHBOARD).read_text()
        self.assertIn('customAllValue: ".*"', dashboard)
        self.assertIn('sum by (model, subscription, api_key)', dashboard)
        self.assertIn('- value: "api_key"\n                label: By API key', dashboard)

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

    def test_backtick_inside_double_quoted_label_is_not_raw_string(self):
        query = (
            'count_over_time({service_name="models-as-a-service"}'
            ' | label_format __ak_sel="foo`bar"'
            ' | __ak_keep="true" [1h])'
        )
        rewritten = inject_user_filter(query, "alice")
        self.assertIn('| user_id="alice"', rewritten)
        self.assertEqual(rewritten.count("| user_id="), 1)


if __name__ == "__main__":
    unittest.main()
