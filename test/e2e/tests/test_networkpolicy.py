"""
E2E tests for MaaS data-path NetworkPolicies.

Validates that the NetworkPolicy allows ext_proc gRPC traffic from all
Istio-managed gateway pods (not just data-science-gateway), explicitly permits
RHOAI Authorino callbacks to maas-api, and that MaaS API endpoints are reachable
through maas-default-gateway.
"""

import json

import pytest
import requests

from conftest import TLS_VERIFY
from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    INFRA_NAMESPACE,
    _oc_run,
    get_json_or_none,
    list_json,
)

NETWORKPOLICY_NAME = "payload-processing"
AUTHORINO_NETWORKPOLICY_NAME = "maas-authorino-allow"
RHCL_AUTHORINO_NAMESPACE = "rh-connectivity-link"
EXPECTED_MANAGED_LABEL = "gateway.istio.io/managed"
EXPECTED_MANAGED_VALUE = "istio.io-gateway-controller"
EXT_PROC_PORT = 9004


class TestPayloadProcessingNetworkPolicyExists:
    """Verify the default-tenant payload-processing NetworkPolicy exists with correct selectors."""

    def test_networkpolicy_exists(self):
        np = get_json_or_none("networkpolicy", NETWORKPOLICY_NAME, GATEWAY_NAMESPACE)
        assert np is not None, (
            f"NetworkPolicy {NETWORKPOLICY_NAME} must exist in {GATEWAY_NAMESPACE}"
        )

    def test_pod_selector_covers_both_processors(self):
        np = get_json_or_none("networkpolicy", NETWORKPOLICY_NAME, GATEWAY_NAMESPACE)
        assert np is not None
        pod_selector = np["spec"]["podSelector"]
        match_exprs = pod_selector.get("matchExpressions") or []
        app_expr = next(
            (e for e in match_exprs if e.get("key") == "app" and e.get("operator") == "In"),
            None,
        )
        if app_expr:
            values = set(app_expr.get("values") or [])
            assert "payload-processing" in values, "podSelector must include payload-processing"
            assert "payload-pre-processing" in values, "podSelector must include payload-pre-processing"
        else:
            match_labels = pod_selector.get("matchLabels") or {}
            assert "app" in match_labels, (
                f"podSelector must select payload-processing pods, got {pod_selector!r}"
            )

    def test_ingress_uses_istio_managed_label(self):
        """Ingress must use gateway.istio.io/managed to cover all gateways."""
        np = get_json_or_none("networkpolicy", NETWORKPOLICY_NAME, GATEWAY_NAMESPACE)
        assert np is not None
        ingress_rules = np["spec"].get("ingress") or []
        assert len(ingress_rules) > 0, "NetworkPolicy must have ingress rules"

        ext_proc_rule = None
        for rule in ingress_rules:
            ports = rule.get("ports") or []
            if any(p.get("port") == EXT_PROC_PORT for p in ports):
                ext_proc_rule = rule
                break

        assert ext_proc_rule is not None, (
            f"Must have an ingress rule for ext_proc port {EXT_PROC_PORT}"
        )

        from_selectors = ext_proc_rule.get("from") or []
        assert len(from_selectors) > 0, "ext_proc ingress rule must have 'from' selectors"

        pod_selector = from_selectors[0].get("podSelector", {})
        match_labels = pod_selector.get("matchLabels") or {}
        assert match_labels.get(EXPECTED_MANAGED_LABEL) == EXPECTED_MANAGED_VALUE, (
            f"ext_proc ingress must use {EXPECTED_MANAGED_LABEL}: {EXPECTED_MANAGED_VALUE} "
            f"to cover all Istio-managed gateways, got matchLabels: {match_labels!r}"
        )

    def test_ingress_does_not_hardcode_single_gateway(self):
        """Default-tenant ingress must NOT pin ext_proc to one gateway via gateway-name label."""
        np = get_json_or_none("networkpolicy", NETWORKPOLICY_NAME, GATEWAY_NAMESPACE)
        assert np is not None
        ingress_rules = np["spec"].get("ingress") or []

        for rule in ingress_rules:
            ports = rule.get("ports") or []
            if not any(p.get("port") == EXT_PROC_PORT for p in ports):
                continue
            for from_selector in rule.get("from") or []:
                pod_labels = (from_selector.get("podSelector") or {}).get("matchLabels") or {}
                assert "gateway.networking.k8s.io/gateway-name" not in pod_labels, (
                    "default-tenant ext_proc ingress must NOT use "
                    "gateway.networking.k8s.io/gateway-name as pod selector — "
                    "this restricts traffic to a single gateway. "
                    f"Found: {pod_labels!r}"
                )


class TestMaaSAuthorinoNetworkPolicy:
    """Verify the shipped policy permits RHOAI RHCL Authorino callbacks."""

    def test_rhcl_authorino_namespace_is_allowed(self):
        np = get_json_or_none(
            "networkpolicy",
            AUTHORINO_NETWORKPOLICY_NAME,
            INFRA_NAMESPACE,
        )
        assert np is not None, (
            f"NetworkPolicy {AUTHORINO_NETWORKPOLICY_NAME} must exist in "
            f"{INFRA_NAMESPACE}"
        )

        ingress_rules = np.get("spec", {}).get("ingress") or []
        assert len(ingress_rules) == 1, "Expected exactly one ingress rule"
        peers = ingress_rules[0].get("from") or []
        assert len(peers) == 1, "Expected exactly one ingress peer"

        peer = peers[0]
        pod_labels = (peer.get("podSelector") or {}).get("matchLabels") or {}
        assert pod_labels == {"authorino-resource": "authorino"}, (
            "Authorino ingress must be restricted to "
            f"authorino-resource=authorino, got {pod_labels!r}"
        )

        match_expressions = (
            (peer.get("namespaceSelector") or {}).get("matchExpressions") or []
        )
        assert len(match_expressions) == 1, (
            "Expected exactly one namespace match expression"
        )
        expression = match_expressions[0]
        assert expression.get("key") == "kubernetes.io/metadata.name"
        assert expression.get("operator") == "In"

        allowed_namespaces = set(expression.get("values") or [])
        expected_namespaces = {
            "kuadrant-system",
            "openshift-operators",
            RHCL_AUTHORINO_NAMESPACE,
        }
        assert allowed_namespaces == expected_namespaces, (
            f"{AUTHORINO_NETWORKPOLICY_NAME} namespace allowlist mismatch: "
            f"expected {sorted(expected_namespaces)!r}, "
            f"got {sorted(allowed_namespaces)!r}"
        )


class TestPayloadProcessingConnectivity:
    """Verify that MaaS gateway pods can reach payload-processing."""

    def test_maas_gateway_pod_exists(self):
        """maas-default-gateway pod must exist in the gateway namespace."""
        pods = list_json(
            "pod",
            GATEWAY_NAMESPACE,
            labels=f"gateway.networking.k8s.io/gateway-name={DEFAULT_GATEWAY_NAME}",
        )
        assert len(pods) > 0, (
            f"No pods found for gateway {DEFAULT_GATEWAY_NAME} in {GATEWAY_NAMESPACE}"
        )
        ready_pods = [
            p for p in pods
            if any(
                cs.get("ready")
                for cs in (p.get("status") or {}).get("containerStatuses") or []
            )
        ]
        assert len(ready_pods) > 0, (
            f"Gateway {DEFAULT_GATEWAY_NAME} has pods but none are ready"
        )

    def test_maas_gateway_has_istio_managed_label(self):
        """maas-default-gateway pods must carry the Istio managed label."""
        pods = list_json(
            "pod",
            GATEWAY_NAMESPACE,
            labels=f"gateway.networking.k8s.io/gateway-name={DEFAULT_GATEWAY_NAME}",
        )
        assert len(pods) > 0
        pod = pods[0]
        labels = pod.get("metadata", {}).get("labels") or {}
        assert labels.get(EXPECTED_MANAGED_LABEL) == EXPECTED_MANAGED_VALUE, (
            f"Gateway pod must have label {EXPECTED_MANAGED_LABEL}={EXPECTED_MANAGED_VALUE}, "
            f"got labels: {json.dumps({k: v for k, v in labels.items() if 'gateway' in k.lower() or 'istio' in k.lower() or 'managed' in k.lower()})}"
        )

    def test_payload_processing_pods_running(self):
        """payload-processing pods must be running in the gateway namespace."""
        pods = list_json("pod", GATEWAY_NAMESPACE, labels="app=payload-processing")
        assert len(pods) > 0, (
            f"No payload-processing pods found in {GATEWAY_NAMESPACE}"
        )
        running = [
            p for p in pods
            if (p.get("status") or {}).get("phase") == "Running"
        ]
        assert len(running) > 0, "payload-processing pods exist but none are Running"


class TestMaaSGatewayFunctional:
    """Verify MaaS API is reachable through the gateway (ext_proc not blocked)."""

    def test_maas_api_health(self, maas_api_base_url: str):
        """MaaS API health endpoint must respond (not hang on ext_proc timeout)."""
        for path in ("/health", "/healthz"):
            try:
                r = requests.get(
                    f"{maas_api_base_url}{path}",
                    timeout=15,
                    verify=TLS_VERIFY,
                )
                assert r.status_code in (200, 401, 404), (
                    f"Health endpoint returned {r.status_code}, expected 200/401/404. "
                    f"A 500 or timeout indicates ext_proc connectivity failure "
                    f"(NetworkPolicy may be blocking gateway -> payload-processing)."
                )
                return
            except requests.exceptions.Timeout:
                pytest.fail(
                    f"Health endpoint {path} timed out after 15s. "
                    "This typically indicates the NetworkPolicy is blocking "
                    "ext_proc traffic from the gateway to payload-processing pods."
                )
            except requests.exceptions.ConnectionError:
                continue

        pytest.fail("Neither /health nor /healthz responded")

    def test_maas_api_models_reachable(self, maas_api_base_url: str, headers: dict):
        """GET /v1/models must respond without ext_proc timeout."""
        try:
            r = requests.get(
                f"{maas_api_base_url}/v1/models",
                headers=headers,
                timeout=15,
                verify=TLS_VERIFY,
            )
        except requests.exceptions.Timeout:
            pytest.fail(
                "/v1/models timed out after 15s — ext_proc connectivity failure. "
                "Check that the payload-processing NetworkPolicy allows ingress "
                "from maas-default-gateway pods."
            )
        assert r.status_code != 500, (
            f"/v1/models returned 500: {r.text[:300]}. "
            "If body contains 'ext_proc error', the NetworkPolicy is likely "
            "blocking gateway -> payload-processing traffic."
        )
        assert r.status_code in (200, 401, 403), (
            f"/v1/models returned unexpected {r.status_code}: {r.text[:300]}"
        )
