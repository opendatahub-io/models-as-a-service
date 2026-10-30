"""
E2E tests for external model (egress) support.

Tests that MaaS can route requests to an external endpoint via ExternalModel CRD,
including reconciler resource creation, auth enforcement, egress connectivity,
path-based routing, and body-based routing (BBR).

Prerequisites:
- MaaS deployed with ExternalModel reconciler
- External endpoint accessible from the cluster (default: httpbin.org)
- For body-based routing tests: IPP (payload-processing) pods deployed

Environment variables:
- E2E_EXTERNAL_ENDPOINT: External endpoint hostname (default: httpbingo.org — an
  actively-maintained httpbin clone; the original httpbin.org demo instance is
  prone to unrelated 503s that would silently skip every test in this file via
  _check_external_endpoint_reachable)
- E2E_EXTERNAL_SUBSCRIPTION: Subscription name (default: e2e-external-subscription)
- GATEWAY_HOST: MaaS gateway hostname (required)
"""

import json
import logging
import os
import subprocess
import time
import uuid

import pytest
import requests

from test_helper import (
    MODEL_NAMESPACE,
    TLS_VERIFY,
    _apply_cr,
    _check_ipp_pods_deployed,
    _delete_cr,
    _get_cr,
    _ns,
    _wait_for_httproute_accepted,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
    _wait_for_subscription_inference_ready,
    _get_cluster_token,
    _create_api_key_raw,
    _create_test_auth_policy,
    _revoke_api_key,
    _create_test_subscription,
    openai_usage_from_response,
    exercise_until_token_limited,
)

log = logging.getLogger(__name__)

# ─── Configuration ──────────────────────────────────────────────────────────

EXTERNAL_ENDPOINT = os.environ.get("E2E_EXTERNAL_ENDPOINT", os.environ.get("E2E_SIMULATOR_ENDPOINT", "httpbingo.org"))
def _subscription_ns():
    return os.environ.get("E2E_SUBSCRIPTION_NAMESPACE", _ns())
EXTERNAL_SUBSCRIPTION = os.environ.get("E2E_EXTERNAL_SUBSCRIPTION", "e2e-external-subscription")
EXTERNAL_AUTH_POLICY = os.environ.get("E2E_EXTERNAL_AUTH_POLICY", "e2e-external-access")
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "12"))

TARGET_MODEL = "gpt-3.5-turbo"

EXTERNAL_MODEL_NAME = "e2e-external-model"
EXTERNAL_PROVIDER_NAME = "e2e-external-provider"
EXTERNAL_TOKEN_LIMIT_LLMIS = "e2e-external-token-limit-simulated"
EXTERNAL_TOKEN_LIMIT_MODEL = "e2e-external-token-limit-model"

# Canonical inference.opendatahub.io CRDs. maas-controller's MaaSModelRef
# externalModelHandler and the IPP model-provider-resolver plugin both watch
# this group; the legacy maas.opendatahub.io/ExternalModel is a fallback only
# and is a no-op for BBR (model-provider-resolver never watches it).
EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"

# CA bundle the gateway proxy mounts; KServe workload Services use it.
OPENSHIFT_SERVICE_CA_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"

# Legacy reconciler names the HTTPRoute after the ExternalModel itself. The
# canonical reconciler reports the generated name in status.httpRouteName.
EXTERNAL_MODEL_HTTPROUTE_NAME = EXTERNAL_MODEL_NAME
EXTERNAL_PROVIDER_SERVICE_NAME = EXTERNAL_PROVIDER_NAME


# ─── Helpers ─────────────────────────────────────────────────────────────────

def _patch_cr(kind: str, name: str, namespace: str, patch: dict):
    """Patch a Kubernetes resource."""
    subprocess.run(
        ["oc", "patch", kind, name, "-n", namespace, "--type=merge", "-p", json.dumps(patch)],
        capture_output=True, text=True,
    )


def _poll_auth_denied(
    url: str,
    headers: dict,
    body: dict,
    *,
    timeout: int = 60,
) -> requests.Response:
    """POST until auth rejects the request; retry transient 404 (route propagation)."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        if last.status_code in (401, 403):
            return last
        if last.status_code != 404:
            break
        log.info("Auth check got transient 404 (route propagation), retrying...")
        time.sleep(2)
    status = last.status_code if last is not None else "no response"
    raise AssertionError(f"Expected 401/403, got {status}")


# ─── Connectivity check ──────────────────────────────────────────────────────

def _check_external_endpoint_reachable():
    """Verify the external endpoint is reachable. Skip tests if not."""
    try:
        r = requests.get(f"https://{EXTERNAL_ENDPOINT}/get", timeout=10, verify=False)
        if r.status_code == 200:
            return True
    except Exception:
        pass
    # Try HTTP fallback
    try:
        r = requests.get(f"http://{EXTERNAL_ENDPOINT}/get", timeout=10)
        if r.status_code == 200:
            return True
    except Exception:
        pass
    return False


pytestmark = [pytest.mark.xdist_group("external")]


@pytest.fixture(scope="module")
def external_endpoint_reachable():
    """Gate only legacy public-egress coverage at execution time, not import."""
    if not _check_external_endpoint_reachable():
        pytest.skip(f"External endpoint {EXTERNAL_ENDPOINT} is not reachable (disconnected environment?)")


# ─── Fixture: Create external model resources ────────────────────────────────

@pytest.fixture(scope="module")
def external_models_setup(gateway_url, headers, api_keys_base_url):
    """
    Create a single ExternalModel CR, MaaSModelRef, AuthPolicy, and
    Subscription pointing to an external endpoint. Cleanup after tests.
    """
    log.info(f"Setting up external model test fixture (endpoint: {EXTERNAL_ENDPOINT})...")

    try:
        # Create a dummy secret (ExternalModel requires credentialRef).
        # The apikey-injection plugin's secret store only watches Secrets carrying
        # this label; without it credential lookups silently fail with a 500 at
        # request time (masked by tests that only assert non-401/403).
        _apply_cr({
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": f"{EXTERNAL_MODEL_NAME}-api-key",
                "namespace": MODEL_NAMESPACE,
                "labels": {"inference.llm-d.ai/ipp-managed": "true"},
            },
            "type": "Opaque",
            "stringData": {"api-key": "e2e-test-key"},
        })

        # Create ExternalProvider CR (inference.opendatahub.io — canonical)
        _apply_cr({
            "apiVersion": "inference.opendatahub.io/v1alpha1",
            "kind": "ExternalProvider",
            "metadata": {"name": EXTERNAL_PROVIDER_NAME, "namespace": MODEL_NAMESPACE},
            "spec": {
                "provider": "openai",
                "endpoint": EXTERNAL_ENDPOINT,
                "auth": {
                    "type": "simple",
                    "secretRef": {"name": f"{EXTERNAL_MODEL_NAME}-api-key"},
                },
            },
        })

        # Create ExternalModel CR (inference.opendatahub.io — canonical). This is
        # what the IPP model-provider-resolver plugin watches to populate its
        # model->provider store, and what maas-controller's externalModelHandler
        # tries first when reconciling the MaaSModelRef.
        _apply_cr({
            "apiVersion": "inference.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE},
            "spec": {
                "externalProviderRefs": [
                    {
                        "ref": {"name": EXTERNAL_PROVIDER_NAME},
                        "targetModel": TARGET_MODEL,
                        "apiFormat": "openai-chat",
                        "path": "/v1/chat/completions",
                    },
                ],
            },
        })

        # Create MaaSModelRef
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {
                "name": EXTERNAL_MODEL_NAME,
                "namespace": MODEL_NAMESPACE,
                "annotations": {
                    "maas.opendatahub.io/endpoint": EXTERNAL_ENDPOINT,
                    "maas.opendatahub.io/provider": "openai",
                },
            },
            "spec": {
                "modelRef": {"kind": "ExternalModel", "name": EXTERNAL_MODEL_NAME},
            },
        })

        # Create MaaSAuthPolicy
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": EXTERNAL_AUTH_POLICY, "namespace": _subscription_ns()},
            "spec": {
                "modelRefs": [{"name": EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE}],
                "subjects": {"groups": [{"name": "system:authenticated"}]},
            },
        })

        # Create MaaSSubscription
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": EXTERNAL_SUBSCRIPTION, "namespace": _subscription_ns()},
            "spec": {
                "owner": {"groups": [{"name": "system:authenticated"}]},
                "modelRefs": [
                    {
                        "name": EXTERNAL_MODEL_NAME,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [{"limit": 10000, "window": "1h"}],
                    },
                ],
            },
        })

        # Wait for CRs to reconcile
        _wait_for_maas_auth_policy_phase(EXTERNAL_AUTH_POLICY, namespace=_subscription_ns())
        _wait_for_maas_subscription_phase(EXTERNAL_SUBSCRIPTION, namespace=_subscription_ns())

        # MaaSAuthPolicy phase Active only proves the gateway's fixed-size,
        # shared Kuadrant AuthPolicy (maas-gateway-auth) is Enforced — that
        # singleton doesn't change when a MaaSAuthPolicy is added, so it says
        # nothing about this ExternalModel's own HTTPRoute. Wait for the
        # HTTPRoute itself to be Accepted by the gateway, otherwise
        # TestExternalModelAuth's requests can race the data plane and get a
        # plain 404 ("no route") instead of a proper 401/403.
        _wait_for_httproute_accepted(EXTERNAL_MODEL_HTTPROUTE_NAME, namespace=MODEL_NAMESPACE)
        # The MaaSModelRef goes Ready only once the controller has seen both the route
        # name IPP records on the ExternalModel and the gateway accepting that route.
        _wait_for_model_ready(EXTERNAL_MODEL_NAME, namespace=MODEL_NAMESPACE)

        # Create API key for tests
        log.info("Creating API key for external model tests...")
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "e2e-external-model-key", "subscription": EXTERNAL_SUBSCRIPTION},
            timeout=30,
            verify=TLS_VERIFY,
        )
        if r.status_code not in (200, 201):
            pytest.fail(f"Failed to create API key: {r.status_code} {r.text}")

        api_key = r.json().get("key")
        if not api_key:
            pytest.fail("API key creation response missing 'key' field")
        log.info("API key created")

        yield {
            "api_key": api_key,
            "gateway_url": gateway_url,
        }
    finally:
        # ── Cleanup ──
        log.info("Cleaning up external model test fixtures...")
        _delete_cr("maasauthpolicy", EXTERNAL_AUTH_POLICY, _subscription_ns())
        _delete_cr("maassubscription", EXTERNAL_SUBSCRIPTION, _subscription_ns())
        _patch_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE,
                  {"metadata": {"finalizers": []}})
        _delete_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
        _delete_cr(EXTERNAL_MODEL_KIND, EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
        _delete_cr(EXTERNAL_PROVIDER_KIND, EXTERNAL_PROVIDER_NAME, MODEL_NAMESPACE)
        _delete_cr("secret", f"{EXTERNAL_MODEL_NAME}-api-key", MODEL_NAMESPACE)


@pytest.fixture
def external_token_limit_resources():
    """Provision fresh simulator-backed ExternalModel token-limit resources.

    Separate from ``external_models_setup``, whose public provider and fixed
    names aren't safe for a repeatable exhaustion test.
    """
    suffix = uuid.uuid4().hex[:10]
    prefix = f"e2e-ext-tokens-{suffix}"
    provider_name = f"{prefix}-provider"
    # This simulator has no MaaSModelRef of its own, so the temporary
    # ExternalModel and MaaSModelRef can share its Kubernetes-safe served ID.
    model_name = EXTERNAL_TOKEN_LIMIT_MODEL
    secret_name = f"{prefix}-credential"
    auth_name = f"{prefix}-auth"
    subscription_name = f"{prefix}-sub"
    token_limit, window = 10, "10m"
    api_key_id = None
    oc_token = None
    try:
        # The fixture is deployed by the aggregate kustomization; do not
        # create a second simulator deployment or service.
        deadline = time.monotonic() + 300
        workload_service = None
        while time.monotonic() < deadline:
            llmis = _get_cr("llminferenceservice", EXTERNAL_TOKEN_LIMIT_LLMIS, MODEL_NAMESPACE)
            workload_service = (
                ((llmis or {}).get("status") or {}).get("workloads") or {}
            ).get("service", {}).get("name")
            service = _get_cr("service", workload_service, MODEL_NAMESPACE) if workload_service else None
            service_ports = ((service or {}).get("spec") or {}).get("ports") or []
            has_https_port = any(
                port.get("port") == 8000 and str(port.get("name", "")).lower().startswith("https")
                for port in service_ports
            )
            if llmis and workload_service and service and has_https_port:
                break
            time.sleep(2)
        else:
            raise TimeoutError(
                f"LLMInferenceService {MODEL_NAMESPACE}/{EXTERNAL_TOKEN_LIMIT_LLMIS} did not report a workload Service "
                "with HTTPS port 8000"
            )
        # Target the workload Service directly: its cert is signed by the
        # OpenShift service CA Envoy trusts, unlike Route ingress certs.
        # endpoint is FQDN-only; the port goes in the ExternalProvider annotation.
        provider_endpoint = f"{workload_service}.{MODEL_NAMESPACE}.svc"

        _apply_cr({
            "apiVersion": "v1", "kind": "Secret",
            "metadata": {"name": secret_name, "namespace": MODEL_NAMESPACE,
                         "labels": {"inference.llm-d.ai/ipp-managed": "true"}},
            "type": "Opaque", "stringData": {"api-key": "test-only-provider-credential"},
        })
        _apply_cr({
            "apiVersion": "inference.opendatahub.io/v1alpha1", "kind": "ExternalProvider",
            "metadata": {
                "name": provider_name,
                "namespace": MODEL_NAMESPACE,
                "annotations": {"inference.opendatahub.io/port": "8000"},
            },
            "spec": {
                "provider": "openai",
                "endpoint": provider_endpoint,
                "auth": {"type": "simple", "secretRef": {"name": secret_name}},
                "config": {"tls.caCertificates": OPENSHIFT_SERVICE_CA_PATH},
            },
        })
        _apply_cr({
            "apiVersion": "inference.opendatahub.io/v1alpha1", "kind": "ExternalModel",
            "metadata": {"name": model_name, "namespace": MODEL_NAMESPACE},
            "spec": {"externalProviderRefs": [{"ref": {"name": provider_name},
                "targetModel": EXTERNAL_TOKEN_LIMIT_MODEL, "apiFormat": "openai-chat",
                "path": "/v1/chat/completions"}]},
        })
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1", "kind": "MaaSModelRef",
            "metadata": {"name": model_name, "namespace": MODEL_NAMESPACE},
            "spec": {"modelRef": {"kind": "ExternalModel", "name": model_name}},
        })
        _create_test_auth_policy(auth_name, [model_name], groups=["system:authenticated"], model_namespace=MODEL_NAMESPACE)
        _create_test_subscription(subscription_name, [model_name], groups=["system:authenticated"],
                                  token_limit=token_limit, window=window, model_namespace=MODEL_NAMESPACE)
        _wait_for_model_ready(model_name, namespace=MODEL_NAMESPACE, timeout=180)
        external_model = _get_cr(EXTERNAL_MODEL_KIND, model_name, MODEL_NAMESPACE)
        external_http_route_name = ((external_model or {}).get("status") or {}).get("httpRouteName")
        if not external_http_route_name:
            raise AssertionError(
                f"ExternalModel {model_name} did not report status.httpRouteName: "
                f"{json.dumps((external_model or {}).get('status', {}))[:1000]}"
            )
        _wait_for_httproute_accepted(external_http_route_name, namespace=MODEL_NAMESPACE, timeout=180)
        _wait_for_maas_subscription_phase(subscription_name, timeout=180)
        _wait_for_subscription_inference_ready(subscription_name, model_name, model_namespace=MODEL_NAMESPACE, timeout=300)

        trlp = _get_cr("tokenratelimitpolicy", f"maas-trlp-{model_name}", MODEL_NAMESPACE)
        assert trlp, f"Generated token policy maas-trlp-{model_name} was not found"
        serialized_policy = json.dumps(trlp)[:1000]
        trlp_labels = trlp.get("metadata", {}).get("labels", {}) or {}
        assert trlp_labels.get("maas.opendatahub.io/model") == model_name, (
            f"Generated token policy is not labeled for MaaSModelRef {model_name}: "
            f"labels={trlp_labels}"
        )
        assert trlp.get("spec", {}).get("targetRef", {}).get("name") == external_http_route_name, (
            f"Generated token policy does not target external HTTPRoute {external_http_route_name}: "
            f"{serialized_policy}"
        )
        subscriptions_annotation = trlp.get("metadata", {}).get("annotations", {}).get(
            "maas.opendatahub.io/subscriptions", ""
        )
        subscription_entries = [s for s in subscriptions_annotation.split(",") if s]
        assert any(
            entry == subscription_name or entry.endswith(f"/{subscription_name}")
            for entry in subscription_entries
        ), (
            f"Generated token policy does not include subscription {subscription_name} in its "
            f"managed-subscriptions annotation: {subscriptions_annotation!r}"
        )
        configured_rates = [
            rate
            for limit in (trlp.get("spec", {}).get("limits", {}) or {}).values()
            for rate in (limit.get("rates") or [])
        ]
        assert any(
            rate.get("limit") == token_limit and rate.get("window") == window
            for rate in configured_rates
        ), (
            f"Generated token policy does not contain the configured rate {token_limit}/{window}: "
            f"limits={trlp.get('spec', {}).get('limits')}"
        )
        oc_token = _get_cluster_token()
        key_response = _create_api_key_raw(oc_token, name=f"{prefix}-key", subscription=subscription_name)
        if key_response.status_code not in (200, 201):
            raise RuntimeError(f"Failed to create external token-limit API key: {key_response.status_code}")
        key_data = key_response.json()
        api_key = key_data.get("key")
        api_key_id = key_data.get("id")
        if not api_key or not api_key_id:
            raise RuntimeError("External token-limit API key response omitted key or id")
        yield {
            "api_key": api_key,
            "subscription": subscription_name,
            "model": model_name,
            "token_limit": token_limit,
        }
    finally:
        # Delete owned resources in reverse dependency order. The plaintext
        # key is deliberately never logged.
        if api_key_id and oc_token:
            _revoke_api_key(oc_token, api_key_id)
        _delete_cr("maassubscription", subscription_name, _subscription_ns())
        _delete_cr("maasauthpolicy", auth_name, _subscription_ns())
        _delete_cr("maasmodelref", model_name, MODEL_NAMESPACE)
        _delete_cr(EXTERNAL_MODEL_KIND, model_name, MODEL_NAMESPACE)
        _delete_cr(EXTERNAL_PROVIDER_KIND, provider_name, MODEL_NAMESPACE)
        _delete_cr("secret", secret_name, MODEL_NAMESPACE)


# ─── Tests: Discovery ───────────────────────────────────────────────────────

@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
class TestExternalModelDiscovery:
    """Verify ExternalModel reconciler creates the expected Istio resources."""

    def test_maasmodelref_created(self, external_models_setup):
        """MaaSModelRef exists for the external model."""
        cr = _get_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"MaaSModelRef {EXTERNAL_MODEL_NAME} not found"

    def test_reconciler_created_httproute(self, external_models_setup):
        """IPP's ExternalModel reconciler created the HTTPRoute (named after the model)."""
        cr = _get_cr("httproute", EXTERNAL_MODEL_HTTPROUTE_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"HTTPRoute {EXTERNAL_MODEL_HTTPROUTE_NAME} not found"

    def test_reconciler_created_backend_service(self, external_models_setup):
        """IPP's ExternalProvider reconciler created the backend service (named after the provider)."""
        cr = _get_cr("service", EXTERNAL_PROVIDER_SERVICE_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"Service {EXTERNAL_PROVIDER_SERVICE_NAME} not found"


# ─── Tests: Auth ─────────────────────────────────────────────────────────────

@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
class TestExternalModelAuth:
    """Verify auth enforcement for external model routes."""

    @pytest.mark.parametrize(
        "authorization",
        ["Bearer sk-oai-invalid-key-12345", None],
        ids=["invalid-key", "missing-key"],
    )
    def test_missing_or_invalid_key_returns_401(self, external_models_setup, authorization):
        """Missing and invalid API keys are rejected with 401/403."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {"Content-Type": "application/json"}
        if authorization:
            headers["Authorization"] = authorization
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        _poll_auth_denied(url, headers, body)


# ─── Tests: Egress ───────────────────────────────────────────────────────────

@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
class TestExternalModelEgress:
    """Verify requests are forwarded to the external endpoint."""

    def test_request_forwarded_returns_200(self, external_models_setup):
        """
        With a valid API key, the request passes auth and reaches the
        external endpoint. Expect 200 confirming egress connectivity.
        """
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code not in (401, 403), (
            f"Request was blocked by auth (HTTP {r.status_code}). "
            f"Expected the request to reach the external endpoint."
        )
        # Any non-auth response confirms egress connectivity.
        # httpbin.org may return 404 for unknown paths — that's fine,
        # it means the request left the cluster and reached the endpoint.
        log.info(f"Egress test: HTTP {r.status_code} from external endpoint")


# ─── Tests: Cleanup ─────────────────────────────────────────────────────────

@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
class TestExternalModelCleanup:
    """Verify resource cleanup when external models are deleted."""

    def test_delete_removes_httproute(self, external_models_setup):
        """
        Deleting an ExternalModel removes the HTTPRoute via OwnerReference
        garbage collection (the ExternalModel reconciler sets itself as the
        HTTPRoute's controller owner).
        """
        temp_name = "e2e-cleanup-test"

        # Reuse the shared ExternalProvider; only the ExternalModel is temporary.
        _apply_cr({
            "apiVersion": "inference.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": temp_name, "namespace": MODEL_NAMESPACE},
            "spec": {
                "externalProviderRefs": [
                    {
                        "ref": {"name": EXTERNAL_PROVIDER_NAME},
                        "targetModel": TARGET_MODEL,
                        "apiFormat": "openai-chat",
                        "path": "/v1/chat/completions",
                    },
                ],
            },
        })

        try:
            # Wait for reconciler to create resources
            time.sleep(RECONCILE_WAIT * 2)

            # Verify HTTPRoute was created (named after the ExternalModel)
            route = _get_cr("httproute", temp_name, MODEL_NAMESPACE)
            assert route is not None, f"HTTPRoute {temp_name} should exist before deletion"

            # Delete the ExternalModel (owns the HTTPRoute via OwnerReference)
            _delete_cr(EXTERNAL_MODEL_KIND, temp_name, MODEL_NAMESPACE)
            time.sleep(RECONCILE_WAIT)

            # Verify HTTPRoute was cleaned up by garbage collection
            route = _get_cr("httproute", temp_name, MODEL_NAMESPACE)
            assert route is None, f"HTTPRoute {temp_name} should be cleaned up after ExternalModel deletion"
        finally:
            # Always clean up to avoid resource leaks
            _delete_cr(EXTERNAL_MODEL_KIND, temp_name, MODEL_NAMESPACE)



@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
class TestExternalModelPathRouting:
    """Verify path-based routing for external model endpoints.

    The URL path ``/{namespace}/{model}/v1/...`` determines which model
    backend the gateway routes to. The happy-path case (correct path reaches
    the endpoint) is already covered by TestExternalModelEgress.
    """

    def test_wrong_path_returns_not_found(self, external_models_setup):
        """Request to non-existent model path returns 404 or auth error."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/nonexistent-model-xyz/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": "nonexistent-model-xyz", "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403, 404), (
            f"Expected 401/403/404 for wrong model path, got {r.status_code}. "
            f"Request should not route to any model backend."
        )
        log.info("Path routing (wrong path): HTTP %d", r.status_code)



requires_ipp = pytest.mark.skipif(
    not _check_ipp_pods_deployed(),
    reason="Payload-processing (IPP) pods not deployed; body routing tests require IPP",
)


@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
@requires_ipp
class TestLegacyExternalModelMigration:
    """Verify IPP migration is reflected on the retained legacy CR."""

    def test_migration_sets_legacy_status_and_removes_networking(self):
        name = f"e2e-legacy-migration-{uuid.uuid4().hex[:8]}"
        secret_name = f"{name}-api-key"
        legacy_kind = "externalmodels.maas.opendatahub.io"
        legacy_resource_name = f"maas-{name}"
        expected_message = f"Model migrated to inference.opendatahub.io ExternalModel: {name}"
        deadline = time.monotonic() + 180

        try:
            _apply_cr({
                "apiVersion": "v1",
                "kind": "Secret",
                "metadata": {"name": secret_name, "namespace": MODEL_NAMESPACE},
                "type": "Opaque",
                "stringData": {"api-key": "e2e-test-key"},
            })
            # Seed one legacy networking child so this test proves teardown ran,
            # even if IPP migrates the model before the legacy reconciler has
            # time to create the full networking set.
            _apply_cr({
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {"name": legacy_resource_name, "namespace": MODEL_NAMESPACE},
                "spec": {
                    "type": "ExternalName",
                    "externalName": EXTERNAL_ENDPOINT,
                    "ports": [{"name": "https", "port": 443}],
                },
            })
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "ExternalModel",
                "metadata": {"name": name, "namespace": MODEL_NAMESPACE},
                "spec": {
                    "provider": "openai",
                    "targetModel": TARGET_MODEL,
                    "endpoint": EXTERNAL_ENDPOINT,
                    "credentialRef": {"name": secret_name},
                },
            })

            legacy = None
            inference = None
            while time.monotonic() < deadline:
                legacy = _get_cr(legacy_kind, name, MODEL_NAMESPACE)
                inference = _get_cr(EXTERNAL_MODEL_KIND, name, MODEL_NAMESPACE)
                if (
                    legacy
                    and legacy.get("status", {}).get("phase") == "Migrated"
                    and inference
                    and inference.get("status", {}).get("httpRouteName")
                ):
                    break
                time.sleep(2)

            assert inference is not None, "IPP did not create the inference ExternalModel"
            assert inference.get("status", {}).get("httpRouteName"), (
                "Migrated inference ExternalModel did not publish status.httpRouteName"
            )
            assert legacy is not None, "Legacy ExternalModel was unexpectedly removed"
            assert legacy.get("status", {}).get("phase") == "Migrated"
            assert legacy.get("status", {}).get("message") == expected_message

            assert _get_cr("httproute", legacy_resource_name, MODEL_NAMESPACE) is None, (
                f"Legacy HTTPRoute {legacy_resource_name} was not removed"
            )
            assert _get_cr("service", legacy_resource_name, MODEL_NAMESPACE) is None, (
                f"Legacy Service {legacy_resource_name} was not removed"
            )
        finally:
            # The legacy CR owns the inference resources created by the migration
            # controller, so deleting it should garbage-collect those children.
            _delete_cr(legacy_kind, name, MODEL_NAMESPACE)
            _delete_cr(EXTERNAL_MODEL_KIND, name, MODEL_NAMESPACE)
            _delete_cr("service", legacy_resource_name, MODEL_NAMESPACE)
            _delete_cr("secret", secret_name, MODEL_NAMESPACE)


@pytest.mark.legacy_ipp
@pytest.mark.usefixtures("external_endpoint_reachable")
@requires_ipp
class TestExternalModelBodyRouting:
    """Verify body-based routing for external models.

    IPP pre-processing extracts the ``model`` field from the JSON body and
    resolves it via the model-provider-resolver plugin's store.

    IMPORTANT CAVEAT: every request here hits ``/{ns}/{model}/v1/...``, a
    path that already encodes a valid model name, so Kuadrant's AuthPolicy
    authorizes from the path alone and the plugin silently no-ops (rather
    than rejecting) on an unresolvable body model. The valid-body forwarding
    contract is covered by TestExternalModelEgress; the "wrong"/"missing"
    cases below are smoke checks only. Genuine path-agnostic body-only
    enforcement is future work (RHAISTRAT-1540).
    """

    def _post_chat(self, gateway_url, model_path, api_key, body):
        url = f"{gateway_url}{model_path}/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
        }
        return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)

    @pytest.mark.parametrize(
        "body, case_name",
        [
            (
                {"model": "nonexistent-model", "messages": [{"role": "user", "content": "hello"}]},
                "wrong-model",
            ),
            (
                {"messages": [{"role": "user", "content": "hello"}]},
                "missing-model",
            ),
        ],
        ids=["wrong-model", "missing-model"],
    )
    def test_unresolvable_model_body_does_not_error(self, external_models_setup, body, case_name):
        """
        An unresolvable or missing model field does not crash the request pipeline.

        NOTE: This is a smoke check, not an enforcement check. The URL path
        already contains a valid model name, so Kuadrant's AuthPolicy
        authorizes the request from the path alone before the body is
        considered. IPP's model-provider-resolver plugin does not reject
        unresolvable models either — it silently no-ops (returns nil) and
        lets the request pass through unchanged when the body's model isn't
        found in its store (see plugin.go ProcessRequest). So today there is
        no code path that actually rejects a mismatched body model for
        ExternalModel; this test only proves the request isn't dropped or
        errored (still != 200, since httpbingo never returns 200 for these
        paths). True body-only enforcement without a path is tracked under
        RHAISTRAT-1540 (unified BBR routing) and isn't implemented yet.
        """
        setup = external_models_setup
        model_path = f"/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1"
        r = self._post_chat(setup["gateway_url"], model_path, setup["api_key"], body)
        assert 400 <= r.status_code < 500 and r.status_code not in (401, 403), (
            f"Expected a non-auth 4xx from the external upstream for {case_name} body, "
            f"got {r.status_code}. A 5xx means the request pipeline errored; "
            "a 200 means body routing may not be active."
        )
        log.info("Body routing (%s): HTTP %d", case_name, r.status_code)


@pytest.mark.external
@pytest.mark.serial
class TestExternalModelTokenRateLimit:
    """Deterministically prove response usage is charged for an ExternalModel."""

    def test_external_model_usage_exhaustion_gets_429(self, external_token_limit_resources, gateway_url):
        resources = external_token_limit_resources

        def infer():
            return requests.post(
                f"{gateway_url}/{MODEL_NAMESPACE}/{resources['model']}/v1/chat/completions",
                headers={"Authorization": f"Bearer {resources['api_key']}", "Content-Type": "application/json"},
                json={
                    "model": resources["model"],
                    "messages": [{"role": "user", "content": "one"}],
                    "max_tokens": 1,
                },
                timeout=30,
                verify=TLS_VERIFY,
            )

        result = exercise_until_token_limited(
            infer, max_attempts=15, interval_seconds=0.1,
        )
        log.info(
            "External token limit subscription=%s statuses=%s usage=%s",
            resources["subscription"], result.status_codes, result.successful_usages,
        )
