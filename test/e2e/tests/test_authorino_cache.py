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
    SIMULATOR_SUBSCRIPTION,
    _apply_cr,
    _create_api_key_raw,
    _delete_cr,
    _get_cluster_token,
    _get_cr,
    _inference,
    _poll_status,
    _revoke_api_key,
    _run_oc,
    log,
)


VALIDATE_ROUTE = "/internal/v1/api-keys/validate"
SELECT_ROUTE = "/internal/v1/subscriptions/select"
REQUEST_COUNT = 12

pytestmark = [pytest.mark.serial, pytest.mark.full_suite]


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
        result = _run_oc(
            ["oc", "create", "token", service_account, "-n", namespace, "--duration=30m"]
        )
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
    name = f"e2e-authorino-cache-{uuid.uuid4().hex[:8]}"
    response = _create_api_key_raw(oc_token, name=name, subscription=SIMULATOR_SUBSCRIPTION)
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


def test_repeated_requests_reuse_cached_maas_api_lookups(cache_ttl, metrics_token, api_key):
    if cache_ttl < REQUEST_COUNT:
        pytest.skip(f"cache TTL {cache_ttl}s is too short for {REQUEST_COUNT} requests")

    _poll_status(api_key, 200)
    pods_before, before = _scrape_route_counts(metrics_token)
    for _ in range(REQUEST_COUNT):
        response = _inference(api_key)
        assert response.status_code == 200, f"{response.status_code}: {response.text[:200]}"
    pods_after, after = _scrape_route_counts(metrics_token)

    assert pods_after == pods_before, "maas-api pods changed while counters were sampled"
    for route in (VALIDATE_ROUTE, SELECT_ROUTE):
        calls = after.get(route, 0.0) - before.get(route, 0.0)
        assert calls >= 0, f"{route} counter decreased from {before.get(route)} to {after.get(route)}"
        assert calls < REQUEST_COUNT, (
            f"{REQUEST_COUNT} identical requests made {calls:.0f} calls to {route}; "
            f"Authorino should reuse cached lookups for {cache_ttl}s"
        )
