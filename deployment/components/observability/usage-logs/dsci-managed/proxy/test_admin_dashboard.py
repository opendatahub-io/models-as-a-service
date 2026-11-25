#!/usr/bin/env python3
"""Lint LogQL in the admin usage dashboard (no tenancy rewrite)."""
import unittest
from pathlib import Path

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

    def test_api_key_identity_is_consistent_across_admin_queries(self):
        queries = queries_from_dashboard(ADMIN_DASHBOARD)
        identity = 'label_format api_key="{{.key_name}} ({{.user_id}}) [{{.key_id}}]"'
        selector = next(q for q in queries if "count by (api_key)" in q)
        self.assertIn(identity, selector)
        self.assertIn('key_id!=""', selector)
        self.assertIn("keep api_key", selector)
        for query in queries:
            if "${api_key:regexliteral}" in query:
                with self.subTest(query=query[:80]):
                    self.assertIn(identity, query)
                    self.assertIn(".api_key)", query)
                    self.assertNotIn('key_id!=""', query)  # All includes older logs.
                    self.assertIn('| __ak_keep="true"', query)

        dashboard = Path(ADMIN_DASHBOARD).read_text()
        self.assertIn('customAllValue: ".*"', dashboard)
        self.assertIn('sum by (${view_by:raw})', dashboard)
        self.assertIn(' ({{.key_name}} [{{.key_id}}]){{end}}', dashboard)

    def test_gateway_exports_authenticated_key_id(self):
        envoy = (Path(ADMIN_DASHBOARD).parent / "envoy-otel-access-log.yaml").read_text()
        # Both Gateway auth variants record client-header absence before WASM,
        # then capture the authenticated header after WASM when it continues.
        provenance = envoy.split("# ── API key header provenance:", 1)[1].split(
            "# ── Identity header capture:", 1
        )[0]
        self.assertEqual(provenance.count("- header: X-MaaS-Key-Id"), 2)
        self.assertEqual(
            provenance.count("name: envoy.filters.http.header_to_metadata.key_id_provenance"), 2
        )
        self.assertEqual(provenance.count("operation: INSERT_BEFORE"), 2)
        self.assertEqual(provenance.count("on_header_missing:"), 2)
        self.assertEqual(provenance.count("metadata_namespace: maas.usage_logs"), 2)
        self.assertEqual(provenance.count("key: key_id_header_absent"), 2)
        self.assertEqual(provenance.count('value: "true"'), 2)
        self.assertLess(
            envoy.index("name: envoy.filters.http.header_to_metadata.key_id_provenance"),
            envoy.index("name: envoy.filters.http.header_to_metadata.identity"),
        )
        post_auth = envoy.split("# ── Identity header capture:", 1)[1].split(
            "# ── Composite filter:", 1
        )[0]
        self.assertEqual(post_auth.count("- header: X-MaaS-Key-Id"), 2)
        self.assertEqual(post_auth.count("operation: INSERT_AFTER"), 2)
        key_id_log = envoy.split("                - key: key_id\n", 1)[1].split(
            "                - key:", 1
        )[0]
        self.assertIn('"key_id" in metadata.filter_metadata', key_id_log)
        self.assertIn('metadata.filter_metadata["envoy.filters.http.header_to_metadata"]["key_id"]', key_id_log)
        self.assertIn('response.code == 429', key_id_log)
        self.assertIn('"maas.usage_logs" in metadata.filter_metadata', key_id_log)
        self.assertIn('metadata.filter_metadata["maas.usage_logs"]["key_id_header_absent"] == "true"', key_id_log)
        self.assertIn('"x-maas-key-id" in request.headers ? request.headers["x-maas-key-id"] : ""', key_id_log)


if __name__ == "__main__":
    unittest.main()
