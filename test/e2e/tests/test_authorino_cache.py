"""E2E coverage for Authorino's maas-api metadata caches."""

import contextlib
import json
import re
import socket
import subprocess
import time
import uuid

import pytest
import requests

from test_helper import (
    GATEWAY_AUTH_POLICY_NAME,
    GATEWAY_NAMESPACE,
    MAAS_API_DEPLOYMENT_NAMESPACE,
    MODEL_NAMESPACE,
    MODEL_REF,
    SIMULATOR_SUBSCRIPTION,
    _apply_cr,
    _create_api_key_raw,
    _create_sa_token,
    _delete_cr,
    _delete_sa,
    _get_cluster_token,
    _get_cr,
    _inference,
    _ns,
    _poll_status,
    _revoke_api_key,
    _run_oc,
    _wait_for_cr_absent,
    _wait_for_maas_subscription_phase,
    _wait_for_subscription_inference_ready,
    log,
)


VALIDATE_ROUTE = "/internal/v1/api-keys/validate"
SELECT_ROUTE = "/internal/v1/subscriptions/select"
REQUEST_COUNT = 12
# Each Authorino pod misses a new cache key once, so a working cache stays far below one
# call per request; allowing half the requests keeps a mostly broken cache from passing.
MAX_CACHED_CALLS = REQUEST_COUNT // 2

pytestmark = [
    pytest.mark.xdist_group("authorino_cache"),
    pytest.mark.serial_exclusive,  # counts maas-api calls across all pods
    pytest.mark.full_suite,
]


@pytest.fixture(scope="module")
def cache_ttl():
    policy = _get_cr("authpolicy", GATEWAY_AUTH_POLICY_NAME, namespace=GATEWAY_NAMESPACE)
    assert policy is not None, "gateway AuthPolicy is missing"
    cache = (
        policy.get("spec", {})
        .get("defaults", {})
        .get("rules", {})
        .get("metadata", {})
        .get("apiKeyValidation", {})
        .get("cache")
    )
    if cache is None:
        pytest.skip("gateway metadata caching is disabled")
    return int(cache["ttl"])


@pytest.fixture
def metrics_token():
    service_account = f"maas-cache-e2e-{uuid.uuid4().hex[:8]}"
    cluster_role = f"{service_account}-metrics"
    namespace = MAAS_API_DEPLOYMENT_NAMESPACE
    _apply_cr(
        {
            "apiVersion": "v1",
            "kind": "ServiceAccount",
            "metadata": {"name": service_account, "namespace": namespace},
        }
    )
    try:
        _apply_cr(
            {
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "ClusterRole",
                "metadata": {"name": cluster_role},
                "rules": [{"nonResourceURLs": ["/metrics"], "verbs": ["get"]}],
            }
        )
        _apply_cr(
            {
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "ClusterRoleBinding",
                "metadata": {"name": cluster_role},
                "roleRef": {
                    "apiGroup": "rbac.authorization.k8s.io",
                    "kind": "ClusterRole",
                    "name": cluster_role,
                },
                "subjects": [
                    {
                        "kind": "ServiceAccount",
                        "name": service_account,
                        "namespace": namespace,
                    }
                ],
            }
        )
        result = _run_oc(["oc", "create", "token", service_account, "-n", namespace, "--duration=30m"])
        assert result.returncode == 0, result.stderr
        token = result.stdout.strip()
        assert token, "oc created an empty service-account token"
        yield token
    finally:
        for kind, name in (
            ("clusterrolebinding", cluster_role),
            ("clusterrole", cluster_role),
        ):
            delete = _run_oc(
                ["oc", "delete", kind, name, "--ignore-not-found", "--timeout=30s"]
            )
            if delete.returncode != 0:
                log.warning("Failed to delete %s/%s: %s", kind, name, delete.stderr.strip())
        _delete_cr("serviceaccount", service_account, namespace=namespace)


@pytest.fixture
def api_key():
    oc_token = _get_cluster_token()
    with _api_key(oc_token, SIMULATOR_SUBSCRIPTION) as key:
        yield key


@contextlib.contextmanager
def _api_key(oc_token, subscription):
    name = f"e2e-authorino-cache-{uuid.uuid4().hex[:8]}"
    response = _create_api_key_raw(oc_token, name=name, subscription=subscription)
    assert response.status_code in (200, 201), f"{response.status_code}: {response.text[:300]}"
    created = response.json()
    try:
        yield created["key"]
    finally:
        _revoke_api_key(oc_token, created["id"])


def _maas_api_pods():
    result = _run_oc(
        [
            "oc",
            "get",
            "pods",
            "-n",
            MAAS_API_DEPLOYMENT_NAMESPACE,
            "-l",
            "app.kubernetes.io/name=maas-api",
            "-o",
            "json",
        ]
    )
    assert result.returncode == 0, result.stderr
    pods = json.loads(result.stdout).get("items", [])
    return [
        pod["metadata"]["name"]
        for pod in pods
        if pod.get("status", {}).get("phase") == "Running"
        and not pod["metadata"].get("deletionTimestamp")
        and pod["metadata"].get("labels", {}).get("app.kubernetes.io/component") == "api"
    ]


def _parse_route_counts(metrics):
    counts = {}
    for line in metrics.splitlines():
        if not line.startswith("maas_api_http_requests_total{"):
            continue
        route_match = re.search(r'route="([^"]+)"', line)
        if not route_match:
            continue
        try:
            value = float(line.rsplit(" ", 1)[1])
        except (IndexError, ValueError):
            continue
        route = route_match.group(1)
        counts[route] = counts.get(route, 0.0) + value
    return counts


@contextlib.contextmanager
def _scrape_pod_metrics(pod, token):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    process = subprocess.Popen(
        ["oc", "port-forward", "-n", MAAS_API_DEPLOYMENT_NAMESPACE, pod, f"{port}:9090"],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    deadline = time.monotonic() + 15
    headers = {"Authorization": f"Bearer {token}"}
    try:
        while time.monotonic() < deadline:
            if process.poll() is not None:
                output = process.stdout.read() if process.stdout else ""
                raise RuntimeError(f"port-forward to {pod} exited: {output.strip()}")
            for scheme in ("https", "http"):
                try:
                    response = requests.get(
                        f"{scheme}://127.0.0.1:{port}/metrics",
                        headers=headers,
                        verify=False,
                        timeout=2,
                    )
                except requests.RequestException:
                    continue
                if response.status_code == 200:
                    yield response.text
                    return
            time.sleep(0.25)
        raise RuntimeError(f"timed out scraping metrics from {pod}")
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def _scrape_route_counts(token):
    pods = _maas_api_pods()
    assert pods, f"no running maas-api pods in {MAAS_API_DEPLOYMENT_NAMESPACE}"
    counts = {}
    for pod in pods:
        with _scrape_pod_metrics(pod, token) as metrics:
            for route, value in _parse_route_counts(metrics).items():
                counts[route] = counts.get(route, 0.0) + value
    return pods, counts


def _maas_api_calls(metrics_token, send_requests, warmed_routes=(VALIDATE_ROUTE, SELECT_ROUTE)):
    """Return the maas-api calls per route that send_requests() caused."""
    pods_before, before = _scrape_route_counts(metrics_token)
    # Every test warms up with a fresh identity, so each warmed route has been called at least
    # once. A missing counter would otherwise read as zero calls.
    for route in warmed_routes:
        assert route in before, f"no {route} calls counted; did the metric's route label change?"
    send_requests()
    pods_after, after = _scrape_route_counts(metrics_token)

    assert pods_after == pods_before, "maas-api pods changed while counters were sampled"
    calls = {}
    for route in (VALIDATE_ROUTE, SELECT_ROUTE):
        calls[route] = after.get(route, 0.0) - before.get(route, 0.0)
        assert calls[route] >= 0, f"{route} counter decreased from {before.get(route)} to {after.get(route)}"
    log.info("maas-api calls for %d requests: %s", REQUEST_COUNT, calls)
    return calls


def _send_inference(credential, headers_for_request=lambda i: None):
    def send():
        for i in range(REQUEST_COUNT):
            headers = headers_for_request(i)
            response = _inference(credential, extra_headers=headers, max_tokens=1)
            assert response.status_code == 200, f"{headers}: {response.status_code}: {response.text[:200]}"
    return send


def _assert_lookups_cached(cache_ttl, metrics_token, api_key):
    if cache_ttl < REQUEST_COUNT:
        pytest.skip(f"cache TTL {cache_ttl}s is too short for {REQUEST_COUNT} requests")

    _poll_status(api_key, 200)
    calls = _maas_api_calls(metrics_token, _send_inference(api_key))

    for route in (VALIDATE_ROUTE, SELECT_ROUTE):
        assert calls[route] <= MAX_CACHED_CALLS, (
            f"{REQUEST_COUNT} identical requests made {calls[route]:.0f} calls to {route}; "
            f"Authorino should reuse cached lookups for {cache_ttl}s"
        )


def test_repeated_requests_reuse_cached_maas_api_lookups(
    cache_ttl, metrics_token, api_key
):
    _assert_lookups_cached(cache_ttl, metrics_token, api_key)


def test_large_subscription_lookup_is_cached(cache_ttl, metrics_token):
    oc_token = _get_cluster_token()
    name = f"e2e-authorino-cache-large-{uuid.uuid4().hex[:8]}"
    namespace = _ns()
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "annotations": {
                    "openshift.io/display-name": "Authorino cache entry size check",
                    "openshift.io/description": "x" * 600,
                },
            },
            "spec": {
                "owner": {"groups": [{"name": "system:authenticated"}]},
                "modelRefs": [
                    {
                        "name": MODEL_REF,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [
                            {"limit": 1_000_000, "window": window}
                            for window in ("1m", "5m", "15m", "1h", "24h")
                        ],
                    }
                ],
                "tokenMetadata": {
                    "labels": {f"label-{index}": f"value-{index}" for index in range(8)}
                },
            },
        }
    )
    try:
        _wait_for_maas_subscription_phase(name, "Active", namespace=namespace)
        _wait_for_subscription_inference_ready(
            name,
            MODEL_REF,
            namespace=namespace,
            model_namespace=MODEL_NAMESPACE,
        )
        with _api_key(oc_token, name) as api_key:
            _assert_lookups_cached(cache_ttl, metrics_token, api_key)
    finally:
        _delete_cr("maassubscription", name, namespace=namespace)
        _wait_for_cr_absent("maassubscription", name, namespace=namespace)


@pytest.fixture
def service_account_token():
    """Token of a fresh ServiceAccount, so its token budget is not shared with other tests."""
    sa_name = f"e2e-authorino-cache-{uuid.uuid4().hex[:8]}"
    try:
        yield _create_sa_token(sa_name)
    finally:
        _delete_sa(sa_name)


@pytest.fixture
def service_account_api_key(service_account_token):
    """API key minted by a fresh ServiceAccount, so its token budget is not shared with other tests."""
    with _api_key(service_account_token, SIMULATOR_SUBSCRIPTION) as key:
        yield key


def test_api_key_subscription_cache_ignores_subscription_header(
    cache_ttl, metrics_token, service_account_api_key
):
    """An API key's subscription is bound at mint, so x-maas-subscription must not split its cache."""
    if cache_ttl < REQUEST_COUNT:
        pytest.skip(f"cache TTL {cache_ttl}s is too short for {REQUEST_COUNT} requests")

    _poll_status(service_account_api_key, 200)
    calls = _maas_api_calls(metrics_token, _send_inference(
        service_account_api_key, lambda i: {"x-maas-subscription": f"e2e-ignored-{i}"}
    ))

    assert calls[SELECT_ROUTE] <= MAX_CACHED_CALLS, (
        f"{REQUEST_COUNT} requests with one API key and different x-maas-subscription values made "
        f"{calls[SELECT_ROUTE]:.0f} calls to {SELECT_ROUTE}; "
        "the header must not be part of the API-key cache key"
    )


def test_token_requests_reuse_cached_subscription_lookup(cache_ttl, metrics_token, service_account_token):
    """Kubernetes tokens use the full subscription-info cache key; it must still repeat."""
    if cache_ttl < REQUEST_COUNT:
        pytest.skip(f"cache TTL {cache_ttl}s is too short for {REQUEST_COUNT} requests")

    headers = {"x-maas-subscription": SIMULATOR_SUBSCRIPTION}
    _poll_status(service_account_token, 200, extra_headers=headers)
    # Tokens never call validate, so its counter may not exist.
    calls = _maas_api_calls(
        metrics_token, _send_inference(service_account_token, lambda i: headers), warmed_routes=(SELECT_ROUTE,)
    )

    assert calls[VALIDATE_ROUTE] == 0, f"token requests made {calls[VALIDATE_ROUTE]:.0f} API key validations"
    assert calls[SELECT_ROUTE] <= MAX_CACHED_CALLS, (
        f"{REQUEST_COUNT} identical token requests made {calls[SELECT_ROUTE]:.0f} calls to {SELECT_ROUTE}; "
        f"Authorino should reuse the cached subscription for {cache_ttl}s"
    )
