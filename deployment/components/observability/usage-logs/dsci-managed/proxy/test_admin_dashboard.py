#!/usr/bin/env python3
"""Lint LogQL in the admin usage dashboard (no tenancy rewrite)."""
import unittest

from dashboard_logql import (
    ADMIN_DASHBOARD,
    assert_no_top_level_raw_strings,
    assert_query_is_walkable,
    queries_from_dashboard,
)


class AdminDashboardLogQLTest(unittest.TestCase):
    def test_usage_logs_admin_dashboard_queries(self):
        queries = queries_from_dashboard(ADMIN_DASHBOARD)
        self.assertGreaterEqual(len(queries), 10, queries)
        for query in queries:
            with self.subTest(query=query[:80]):
                assert_no_top_level_raw_strings(self, query)
                assert_query_is_walkable(self, query)


if __name__ == "__main__":
    unittest.main()
