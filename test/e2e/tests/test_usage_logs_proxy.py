"""E2E coverage for the maas-api usage-logs Loki proxy.

These tests cover the deployed integration path: the retired Python proxy is
gone, maas-api has scoped Loki access, the proxy is not exposed publicly, and
TokenReview remains the only accepted caller identity transport.
"""

import base64
import logging
import os
import re
import time
from urllib.parse import urlencode, urlparse

import pytest
import requests

from multitenancy_helpers import get_json_or_none, list_json
from test_helper import (
    DEPLOYMENT_NAMESPACE,
    GATEWAY_NAMESPACE,
    MAAS_API_DEPLOYMENT_NAMESPACE,
    MODEL_REF,
    _get_cluster_token,
    _maas_api_url,
    _request_with_gateway_retry,
    _wait_for_gateway_auth_enforced,
    kubectl_curl,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.smoke,
    pytest.mark.xdist_group("usage_logs_proxy"),
]

DEFAULT_LOKI_SERVICE = "data-science-usage-logs-lokistack-gateway-http"
LEGACY_LOKI_SERVICE = "usage-gateway-http"
DEFAULT_LOKI_INSTANCE = "data-science-usage-logs-lokistack"
LEGACY_LOKI_INSTANCE = "usage"
MONITORING_NETWORK_POLICY = "maas-api-allow-monitoring"
EGRESS_NETWORK_POLICY = "maas-api-egress-restrict"
ENVOY_FILTER_NAME = "payload-processing"
ROUTE_NAME_RE = re.compile(r"\.maas-api-route\.(\d+)$")

LEGACY_PROXY_RESOURCES = (
    ("serviceaccount", "usage-tenancy-proxy"),
    ("deployment", "usage-logs-tenancy-proxy"),
    ("configmap", "usage-tenancy-proxy-source"),
    ("configmap", "usage-logs-tenancy-proxy-ca-bundle"),
    ("service", "usage-logs-tenancy-proxy"),
    ("networkpolicy", "usage-tenancy-proxy-allow-perses"),
    ("networkpolicy", "usage-tenancy-proxy-egress-restrict"),
    ("rolebinding", "usage-tenancy-proxy-scc-nonroot-v2"),
)

LEGACY_PROXY_CLUSTER_RESOURCES = (
    ("clusterrole", "usage-tenancy-proxy"),
    ("clusterrolebinding", "usage-tenancy-proxy"),
)

PUBLIC_MAAS_API_PATHS = (
    "/v1/models",
    "/v1/subscriptions",
    "/v1/api-keys/config",
    f"/v1/model/{MODEL_REF}/subscriptions",
    "/v1/tenants",
    "/v1/me",
)


def _monitoring_namespace():
    override = os.environ.get("MONITORING_NAMESPACE")
    if override:
        return override
    configmap = get_json_or_none("configmap", "maas-parameters", DEPLOYMENT_NAMESPACE)
    if configmap:
        namespace = (configmap.get("data") or {}).get("monitoring-namespace")
        if namespace:
            return namespace
    return "opendatahub"


def _maas_api_container():
    deployment = get_json_or_none(
        "deployment", "maas-api", MAAS_API_DEPLOYMENT_NAMESPACE
    )
    assert deployment is not None, (
        f"Deployment maas-api must exist in {MAAS_API_DEPLOYMENT_NAMESPACE}"
    )
    containers = (
        deployment.get("spec", {})
        .get("template", {})
        .get("spec", {})
        .get("containers", [])
    )
    container = next(
        (item for item in containers if item.get("name") == "maas-api"), None
    )
    assert container is not None, (
        "Deployment maas-api must contain a maas-api container"
    )
    return container


def _container_env(container, name):
    return next(
        (
            item.get("value")
            for item in container.get("env", [])
            if item.get("name") == name
        ),
        None,
    )


def _loki_endpoint_override(namespace):
    secret = get_json_or_none("secret", "usage-logs-loki", namespace)
    if not secret:
        return None
    encoded = (secret.get("data") or {}).get("LOKI_READ_ENDPOINT")
    if not encoded:
        return None
    endpoint = base64.b64decode(encoded).decode("utf-8").strip()
    try:
        parsed = urlparse(endpoint)
        if parsed.scheme not in ("http", "https") or not parsed.hostname:
            return None
        parsed.port
    except ValueError:
        return None
    return endpoint


def _loki_egress_rules(network_policy):
    rules = []
    for rule in network_policy.get("spec", {}).get("egress", []):
        ports = rule.get("ports", [])
        if any(
            port.get("protocol") == "TCP" and port.get("port") == 8080 for port in ports
        ):
            rules.append(rule)
    return rules


def _maas_config():
    return get_json_or_none("configs.maas.opendatahub.io", "default")


def _usage_logging_enabled():
    config = _maas_config()
    return bool(config and config.get("spec", {}).get("usageLogging"))


def _has_allow_all_egress(network_policy):
    return any(not rule for rule in network_policy.get("spec", {}).get("egress", []))


def _selected_loki_service():
    endpoint = _container_env(_maas_api_container(), "LOKI_READ_ENDPOINT")
    if not endpoint:
        return None
    hostname = urlparse(endpoint).hostname or ""
    parts = hostname.split(".")
    if len(parts) < 2 or "svc" not in parts:
        return None
    return parts[0], parts[1]


def _expected_loki_service():
    return LEGACY_LOKI_SERVICE if _usage_logging_enabled() else DEFAULT_LOKI_SERVICE


def _expected_loki_instance():
    return LEGACY_LOKI_INSTANCE if _usage_logging_enabled() else DEFAULT_LOKI_INSTANCE


def _expected_loki_endpoint():
    service = _expected_loki_service()
    return (
        f"https://{service}.{_monitoring_namespace()}.svc:8080/api/logs/v1/application"
    )


def _loki_service_has_ready_endpoint(service_name, service_namespace):
    endpoint_slices = list_json(
        "endpointslice",
        service_namespace,
        labels=f"kubernetes.io/service-name={service_name}",
    )
    for endpoint_slice in endpoint_slices:
        for endpoint in endpoint_slice.get("endpoints", []):
            if (
                endpoint.get("addresses")
                and endpoint.get("conditions", {}).get("ready") is not False
            ):
                return True
    return False


def _perses_datasource(name, namespace):
    return get_json_or_none("persesdatasource", name, namespace)


def test_maas_api_loki_proxy_is_configured():
    container = _maas_api_container()
    endpoint = _container_env(container, "LOKI_READ_ENDPOINT")
    override = _loki_endpoint_override(MAAS_API_DEPLOYMENT_NAMESPACE)

    assert endpoint, "maas-api must set LOKI_READ_ENDPOINT"
    if override:
        assert endpoint == override, (
            "maas-api must use the usage-logs-loki Secret endpoint override"
        )
    else:
        assert endpoint == _expected_loki_endpoint(), (
            "maas-api must use the deployment-mode-specific default Loki endpoint"
        )

    assert _container_env(container, "LOKI_READ_TOKEN_FILE"), (
        "maas-api must use a file-based Loki credential"
    )
    assert _container_env(container, "LOKI_READ_CA_FILE"), (
        "maas-api must use a file-based Loki CA bundle"
    )


def test_maas_api_loki_egress_matches_deployment_mode():
    endpoint = _container_env(_maas_api_container(), "LOKI_READ_ENDPOINT")
    if endpoint != _expected_loki_endpoint():
        pytest.skip(
            "Loki endpoint is overridden; the default LokiStack rule is not rendered"
        )

    config = _maas_config()
    if (
        config
        and config.get("spec", {}).get("maasApiEgressNetworkPolicy") == "Disabled"
    ):
        pytest.skip("maas-api egress NetworkPolicy is customer-managed")
    if config and config.get("spec", {}).get("networkPolicyEgressRules"):
        pytest.skip("maas-api uses custom egress rules")

    network_policy = get_json_or_none(
        "networkpolicy", EGRESS_NETWORK_POLICY, MAAS_API_DEPLOYMENT_NAMESPACE
    )
    assert network_policy is not None, (
        f"NetworkPolicy {EGRESS_NETWORK_POLICY} must exist in "
        f"{MAAS_API_DEPLOYMENT_NAMESPACE}"
    )

    expect_restrictive = (
        os.environ.get("E2E_EXPECT_MAAS_API_RESTRICTED_EGRESS", "").lower() == "true"
    )
    if _has_allow_all_egress(network_policy):
        assert not expect_restrictive, (
            "maas-api egress unexpectedly uses an allow-all rule on a deployment "
            "configured for restrictive egress"
        )
        return

    expected_labels = {
        "namespaceSelector": {
            "matchLabels": {
                "kubernetes.io/metadata.name": _monitoring_namespace(),
            }
        },
        "podSelector": {
            "matchLabels": {
                "app.kubernetes.io/name": "lokistack",
                "app.kubernetes.io/component": "lokistack-gateway",
                "app.kubernetes.io/instance": _expected_loki_instance(),
            }
        },
    }
    matching_rules = [
        rule
        for rule in _loki_egress_rules(network_policy)
        if rule.get("to") == [expected_labels]
    ]
    assert len(matching_rules) == 1, (
        "Restrictive maas-api egress must contain exactly one explicit LokiStack "
        f"peer; found {len(matching_rules)}"
    )


def test_monitoring_ingress_is_restricted_to_perses_operator():
    network_policy = get_json_or_none(
        "networkpolicy", MONITORING_NETWORK_POLICY, MAAS_API_DEPLOYMENT_NAMESPACE
    )
    assert network_policy is not None, (
        f"NetworkPolicy {MONITORING_NETWORK_POLICY} must exist in "
        f"{MAAS_API_DEPLOYMENT_NAMESPACE}"
    )

    https_rules = [
        rule
        for rule in network_policy.get("spec", {}).get("ingress", [])
        if any(
            port.get("protocol") == "TCP" and int(port.get("port", 0)) == 8443
            for port in rule.get("ports", [])
        )
    ]
    assert https_rules, "maas-api must allow Perses to reach the HTTPS Loki proxy"
    expected_namespace = _monitoring_namespace()
    for rule in https_rules:
        peers = rule.get("from", [])
        assert peers, "The HTTPS monitoring rule must not be namespace-wide"
        for peer in peers:
            namespace_labels = peer.get("namespaceSelector", {}).get("matchLabels", {})
            pod_labels = peer.get("podSelector", {}).get("matchLabels", {})
            assert namespace_labels.get("kubernetes.io/metadata.name") == (
                expected_namespace
            )
            assert pod_labels.get("app.kubernetes.io/managed-by") == "perses-operator"


def test_retired_python_usage_proxy_is_removed():
    namespace = _monitoring_namespace()
    for kind, name in LEGACY_PROXY_RESOURCES:
        resource = get_json_or_none(kind, name, namespace)
        assert resource is None, (
            f"Retired Python proxy resource {kind}/{name} still exists"
        )
    for kind, name in LEGACY_PROXY_CLUSTER_RESOURCES:
        resource = get_json_or_none(kind, name)
        assert resource is None, (
            f"Retired Python proxy resource {kind}/{name} still exists"
        )


@pytest.mark.parametrize("path", PUBLIC_MAAS_API_PATHS)
def test_public_maas_api_paths_preserve_backend_suffixes(path):
    _wait_for_gateway_auth_enforced()
    token = _get_cluster_token()
    response = _request_with_gateway_retry(
        requests.get,
        f"{_maas_api_url()}{path}",
        headers={"Authorization": f"Bearer {token}"},
    )
    assert response.status_code == 200, (
        f"Public maas-api path {path} should return 200, got "
        f"{response.status_code}: {response.text[:500]}"
    )


def test_loki_proxy_is_not_exposed_through_gateway():
    response = _request_with_gateway_retry(
        requests.get,
        f"{_maas_api_url()}/v1/logs/loki/api/v1/query",
        auth_denial_is_transient=False,
    )
    assert response.status_code == 404, (
        "The Loki proxy must not be routed through the public gateway; "
        f"got {response.status_code}: {response.text[:500]}"
    )


def test_internal_loki_proxy_requires_tokenreview(
    maas_api_internal_url: str, token: str
):
    query = urlencode(
        {
            "only_user": "true",
            "query": '{service_name="models-as-a-service"}',
        }
    )
    url = f"{maas_api_internal_url}/v1/logs/loki/api/v1/query?{query}"

    status, _ = kubectl_curl(url)
    assert status == 401, f"Unauthenticated Loki query should return 401, got {status}"

    status, _ = kubectl_curl(
        url,
        headers={"Authorization": "Bearer invalid-token-12345"},
    )
    assert status == 401, f"Invalid Loki bearer token should return 401, got {status}"

    status, _ = kubectl_curl(
        url,
        headers={
            "Authorization": f"Bearer {token}",
            "X-MaaS-Username": "spoofed-user",
        },
    )
    assert status == 401, (
        "A valid bearer token with a spoofed trusted identity header must return 401, "
        f"got {status}"
    )


def test_internal_loki_proxy_query_when_lokistack_exists(
    maas_api_internal_url: str,
    token: str,
):
    selected = _selected_loki_service()
    if selected is None:
        pytest.skip("Loki endpoint is external or has no in-cluster Service")
    service_name, service_namespace = selected
    if get_json_or_none("service", service_name, service_namespace) is None:
        pytest.skip("Selected usage-logs LokiStack is not installed")
    readiness_deadline = time.monotonic() + 60
    while (
        not _loki_service_has_ready_endpoint(service_name, service_namespace)
        and time.monotonic() < readiness_deadline
    ):
        time.sleep(5)
    if not _loki_service_has_ready_endpoint(service_name, service_namespace):
        pytest.fail("Selected usage-logs LokiStack has no ready gateway endpoints")

    query = urlencode(
        {
            "only_user": "true",
            "query": 'sum(count_over_time({service_name="models-as-a-service"}[5m]))',
        }
    )
    url = f"{maas_api_internal_url}/v1/logs/loki/api/v1/query?{query}"
    status, body = 0, ""
    query_deadline = time.monotonic() + 120
    while time.monotonic() < query_deadline:
        status, body = kubectl_curl(
            url,
            headers={"Authorization": f"Bearer {token}"},
            attempts=1,
            max_duration=max(1, int(query_deadline - time.monotonic())),
        )
        if status == 200 or status not in (0, 502, 503):
            break
        remaining = query_deadline - time.monotonic()
        if remaining <= 0:
            break
        time.sleep(min(5, remaining))
    assert status == 200, (
        "TokenReview-authenticated self-scoped Loki query should return 200, "
        f"got {status}: {body[:500]}"
    )


def test_payload_processing_envoyfilter_covers_all_public_routes():
    envoy_filter = get_json_or_none("envoyfilter", ENVOY_FILTER_NAME, GATEWAY_NAMESPACE)
    assert envoy_filter is not None, (
        f"EnvoyFilter {ENVOY_FILTER_NAME} must exist in {GATEWAY_NAMESPACE}"
    )

    route_indices = set()
    for patch in envoy_filter.get("spec", {}).get("configPatches", []):
        route = (
            patch.get("match", {})
            .get("routeConfiguration", {})
            .get("vhost", {})
            .get("route", {})
            .get("name", "")
        )
        match = ROUTE_NAME_RE.search(route)
        if match:
            route_indices.add(int(match.group(1)))

    assert route_indices == set(range(11)), (
        "Payload-processing must disable ext_proc for maas-api route rules 0–10; "
        f"found {sorted(route_indices)}"
    )


def test_perses_datasources_use_maas_api_when_installed():
    namespace = _monitoring_namespace()
    crd = get_json_or_none("customresourcedefinition", "persesdatasources.perses.dev")
    if crd is None:
        pytest.skip("PersesDatasource CRD is not installed")

    maas_api_url = (
        f"https://maas-api.{MAAS_API_DEPLOYMENT_NAMESPACE}.svc.cluster.local:8443"
        "/v1/logs"
    )
    expected = {
        "usage-logs-multi-tenancy": f"{maas_api_url}?only_user=true",
        "usage-logs-all": (
            f"https://{LEGACY_LOKI_SERVICE}.{namespace}.svc.cluster.local:8080"
            "/api/logs/v1/application"
            if _usage_logging_enabled()
            else maas_api_url
        ),
    }
    for name, url in expected.items():
        datasource = _perses_datasource(name, namespace)
        assert datasource is not None, (
            f"Perses datasource {name} must exist in {namespace}"
        )
        actual = (
            datasource.get("spec", {})
            .get("config", {})
            .get("plugin", {})
            .get("spec", {})
            .get("proxy", {})
            .get("spec", {})
            .get("url")
        )
        assert actual == url, (
            f"Perses datasource {name} must use maas-api, got {actual}"
        )
