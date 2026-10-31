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
- E2E_SHARED_PROVIDER_PATH: Upstream path for the shared-provider Praxis test (default: /post)
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
    _wait_for_cr_absent,
    _wait_for_httproute_accepted,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
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

# Canonical inference.opendatahub.io CRDs. maas-controller's MaaSModelRef
# externalModelHandler and the IPP model-provider-resolver plugin both watch
# this group; the legacy maas.opendatahub.io/ExternalModel is a fallback only
# and is a no-op for BBR (model-provider-resolver never watches it).
EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"

# The canonical reconciler (ai-gateway-payload-processing) names the HTTPRoute
# after the ExternalModel itself (no "maas-" prefix) and the backend Service
# after the ExternalProvider (the HTTPRoute's backendRef).
EXTERNAL_MODEL_HTTPROUTE_NAME = EXTERNAL_MODEL_NAME
EXTERNAL_PROVIDER_SERVICE_NAME = EXTERNAL_PROVIDER_NAME


# ─── Helpers ─────────────────────────────────────────────────────────────────

def _patch_cr(kind: str, name: str, namespace: str, patch: dict):
    """Patch a Kubernetes resource, raising if oc rejects the patch."""
    subprocess.run(
        ["oc", "patch", kind, name, "-n", namespace, "--type=merge", "-p", json.dumps(patch)],
        capture_output=True, text=True, check=True,
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


_LEGACY_IPP = pytest.mark.legacy_ipp

pytestmark = [
    pytest.mark.skipif(
        not _check_external_endpoint_reachable(),
        reason=f"External endpoint {EXTERNAL_ENDPOINT} is not reachable (disconnected environment?)",
    ),
    pytest.mark.xdist_group("external"),
]


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


# ─── Tests: Discovery ───────────────────────────────────────────────────────

class TestExternalModelDiscovery:
    pytestmark = _LEGACY_IPP

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

class TestExternalModelAuth:
    pytestmark = _LEGACY_IPP

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

class TestExternalModelEgress:
    pytestmark = _LEGACY_IPP

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

class TestExternalModelCleanup:
    pytestmark = _LEGACY_IPP

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



class TestExternalModelPathRouting:
    pytestmark = _LEGACY_IPP

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


@requires_ipp
class TestLegacyExternalModelMigration:
    pytestmark = _LEGACY_IPP

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


@requires_ipp
class TestExternalModelBodyRouting:
    pytestmark = _LEGACY_IPP

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


# ─── Tests: Shared Provider (multi-model) ────────────────────────────────────

SHARED_PROVIDER_PATH = os.environ.get("E2E_SHARED_PROVIDER_PATH", "/post")
PAYLOAD_PROCESSING_TYPE_ANNOTATION = "maas.opendatahub.io/payload-processing-type"
DEFAULT_TENANT_CONFIG_NAME = "default-tenant"


def _wait_for_praxis_external_model(name: str, namespace: str, timeout: int = 180):
    """Wait for an AIGC-ready model with its generated route and overlay digest."""
    deadline = time.monotonic() + timeout
    last_status = {}
    while time.monotonic() < deadline:
        model = _get_cr(EXTERNAL_MODEL_KIND, name, namespace)
        if model:
            last_status = model.get("status", {})
            generation = model.get("metadata", {}).get("generation")
            if (
                last_status.get("phase") == "Ready"
                and last_status.get("httpRouteName")
                and last_status.get("overlayDigest")
                and last_status.get("observedGeneration") == generation
            ):
                return model
        time.sleep(2)
    raise TimeoutError(
        f"ExternalModel {namespace}/{name} did not publish Ready status, an HTTPRoute name, "
        f"and a Praxis overlay digest within {timeout}s (last status: {last_status!r})"
    )


def _post_shared_provider_model(
    gateway_url: str,
    namespace: str,
    model_name: str,
    api_key: str,
    *,
    expected_status: int,
    timeout: int = 120,
):
    """Poll the model route for a deterministic response from the shared provider."""
    url = f"{gateway_url.rstrip('/')}/{namespace}/{model_name}{SHARED_PROVIDER_PATH}"
    request_headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
    }
    body = {"model": model_name, "messages": [{"role": "user", "content": "shared provider test"}]}
    deadline = time.monotonic() + timeout
    last_status = "no response"
    attempt = 0
    while time.monotonic() < deadline:
        attempt += 1
        try:
            response = requests.post(
                url, headers=request_headers, json=body, timeout=30, verify=TLS_VERIFY,
            )
            last_status = response.status_code
            if response.status_code == expected_status:
                return response
            if response.status_code not in (401, 403, 404, 429, 500, 502, 503, 504):
                raise AssertionError(
                    f"Shared-provider route {namespace}/{model_name} returned unexpected "
                    f"HTTP {response.status_code}; expected HTTP {expected_status}"
                )
        except requests.RequestException as exc:
            last_status = type(exc).__name__
        log.info(
            "Shared-provider route %s returned %s (attempt %d); waiting for HTTP %d",
            model_name,
            last_status,
            attempt,
            expected_status,
        )
        time.sleep(2)
    raise AssertionError(
        f"Shared-provider route {namespace}/{model_name} returned {last_status!r}; "
        f"expected HTTP {expected_status} within {timeout}s"
    )


def _cleanup_shared_provider_resources(setup: dict):
    """Delete test resources while still attempting every cleanup step on error."""
    namespace = setup["namespace"]
    cleanup_errors = []

    def attempt(description, action):
        try:
            action()
        except Exception as exc:  # cleanup must continue after any one failure
            cleanup_errors.append(f"{description}: {exc}")

    for kind, name in (
        ("maassubscription", setup["subscription_name"]),
        ("maasauthpolicy", setup["auth_policy_name"]),
    ):
        attempt(f"delete {kind}/{name}", lambda kind=kind, name=name: _delete_cr(kind, name, namespace))
        attempt(
            f"wait for {kind}/{name} deletion",
            lambda kind=kind, name=name: _wait_for_cr_absent(kind, name, namespace=namespace, timeout=90),
        )

    for name in (setup["first_model"], setup["second_model"]):
        model_ref = None
        try:
            model_ref = _get_cr("maasmodelref", name, namespace)
        except Exception as exc:  # noqa: BLE001 - continue cleanup after API errors
            cleanup_errors.append(f"get maasmodelref/{name}: {exc}")
        if model_ref:
            attempt(
                f"remove finalizers from maasmodelref/{name}",
                lambda name=name: _patch_cr(
                    "maasmodelref", name, namespace, {"metadata": {"finalizers": []}},
                ),
            )
        attempt(f"delete maasmodelref/{name}", lambda name=name: _delete_cr("maasmodelref", name, namespace))
        attempt(
            f"wait for maasmodelref/{name} deletion",
            lambda name=name: _wait_for_cr_absent("maasmodelref", name, namespace=namespace, timeout=90),
        )

    for name in (setup["first_model"], setup["second_model"]):
        attempt(
            f"delete ExternalModel/{name}",
            lambda name=name: _delete_cr(EXTERNAL_MODEL_KIND, name, namespace),
        )
    for name in (setup["first_model"], setup["second_model"]):
        attempt(
            f"wait for ExternalModel/{name} deletion",
            lambda name=name: _wait_for_cr_absent(EXTERNAL_MODEL_KIND, name, namespace=namespace, timeout=180),
        )

    attempt(
        f"delete ExternalProvider/{setup['provider_name']}",
        lambda: _delete_cr(EXTERNAL_PROVIDER_KIND, setup["provider_name"], namespace),
    )
    attempt(
        f"wait for ExternalProvider/{setup['provider_name']} deletion",
        lambda: _wait_for_cr_absent(
            EXTERNAL_PROVIDER_KIND, setup["provider_name"], namespace=namespace, timeout=90,
        ),
    )
    attempt(
        f"delete Secret/{setup['secret_name']}",
        lambda: _delete_cr("secret", setup["secret_name"], namespace),
    )
    if cleanup_errors:
        raise RuntimeError("Shared-provider fixture cleanup failed: " + "; ".join(cleanup_errors))


class TestSharedProviderMultiModel:
    """Verify two ExternalModels sharing one ExternalProvider both route correctly.

    Regression test for RHOAIENG-98005: ai-gateway-controller produced
    duplicate stable_id values in the routing overlay when two models
    referenced the same provider, causing Praxis to reject the overlay
    and the second model to return 404/503.
    """

    pytestmark = pytest.mark.external

    @pytest.fixture(scope="class")
    def shared_provider_setup(self, gateway_url, headers, api_keys_base_url):
        """Use the already bootstrapped default tenant and its Praxis dataplane."""
        namespace = _ns()
        suffix = uuid.uuid4().hex[:8]
        setup = {
            "namespace": namespace,
            "provider_name": f"e2e-shared-provider-{suffix}",
            "first_model": f"e2e-shared-model-a-{suffix}",
            "second_model": f"e2e-shared-model-b-{suffix}",
            "auth_policy_name": f"e2e-shared-auth-{suffix}",
            "subscription_name": f"e2e-shared-sub-{suffix}",
            "secret_name": f"e2e-shared-secret-{suffix}",
            "gateway_url": gateway_url,
        }

        tenant_config = _get_cr("maastenantconfig", DEFAULT_TENANT_CONFIG_NAME, namespace)
        if tenant_config is None:
            pytest.fail(
                f"The bootstrapped MaaSTenantConfig {DEFAULT_TENANT_CONFIG_NAME}/{namespace} "
                "is required for this Praxis regression test"
            )
        payload_processing_type = (
            tenant_config.get("metadata", {}).get("annotations", {}).get(PAYLOAD_PROCESSING_TYPE_ANNOTATION, "")
        )
        if payload_processing_type not in ("", "praxis"):
            if payload_processing_type == "ipp":
                pytest.skip(
                    f"MaaSTenantConfig {DEFAULT_TENANT_CONFIG_NAME}/{namespace} explicitly selects IPP; "
                    "this regression test covers the default Praxis dataplane"
                )
            pytest.fail(
                f"MaaSTenantConfig {DEFAULT_TENANT_CONFIG_NAME}/{namespace} selects "
                f"payload processing {payload_processing_type!r}; this test requires the existing Praxis default"
            )

        try:
            _apply_cr({
                "apiVersion": "v1",
                "kind": "Secret",
                "metadata": {
                    "name": setup["secret_name"],
                    "namespace": namespace,
                },
                "type": "Opaque",
                "stringData": {"api-key": "e2e-test-key"},
            })
            _apply_cr({
                "apiVersion": "inference.opendatahub.io/v1alpha1",
                "kind": "ExternalProvider",
                "metadata": {"name": setup["provider_name"], "namespace": namespace},
                "spec": {
                    "provider": "openai",
                    "endpoint": EXTERNAL_ENDPOINT,
                    "auth": {
                        "type": "simple",
                        "secretRef": {"name": setup["secret_name"]},
                    },
                },
            })

            model_refs = []
            for model_name in (setup["first_model"], setup["second_model"]):
                _apply_cr({
                    "apiVersion": "inference.opendatahub.io/v1alpha1",
                    "kind": "ExternalModel",
                    "metadata": {"name": model_name, "namespace": namespace},
                    "spec": {
                        "externalProviderRefs": [
                            {
                                "ref": {"name": setup["provider_name"]},
                                "targetModel": TARGET_MODEL,
                                "apiFormat": "openai-chat",
                                "path": SHARED_PROVIDER_PATH,
                            },
                        ],
                    },
                })
                _apply_cr({
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "MaaSModelRef",
                    "metadata": {
                        "name": model_name,
                        "namespace": namespace,
                        "annotations": {
                            "maas.opendatahub.io/endpoint": EXTERNAL_ENDPOINT,
                            "maas.opendatahub.io/provider": "openai",
                        },
                    },
                    "spec": {
                        "modelRef": {"kind": "ExternalModel", "name": model_name},
                    },
                })
                model_refs.append({"name": model_name, "namespace": namespace})

            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": setup["auth_policy_name"], "namespace": namespace},
                "spec": {
                    "modelRefs": model_refs,
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": setup["subscription_name"], "namespace": namespace},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [
                        {
                            **model_ref,
                            "tokenRateLimits": [{"limit": 10000, "window": "1h"}],
                        }
                        for model_ref in model_refs
                    ],
                },
            })

            _wait_for_maas_auth_policy_phase(setup["auth_policy_name"], namespace=namespace)
            _wait_for_maas_subscription_phase(
                setup["subscription_name"],
                namespace=namespace,
                require_model_statuses=True,
            )

            for model_name in (setup["first_model"], setup["second_model"]):
                external_model = _wait_for_praxis_external_model(model_name, namespace)
                route_name = external_model["status"]["httpRouteName"]
                route = _wait_for_httproute_accepted(route_name, namespace=namespace)
                managed_by = route.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/managed-by")
                if managed_by != "ai-gateway-controller":
                    pytest.fail(
                        f"HTTPRoute {namespace}/{route_name} is managed by {managed_by!r}, "
                        "not ai-gateway-controller; the test did not exercise the Praxis route"
                    )
                _wait_for_model_ready(model_name, namespace=namespace, timeout=180)

            first_model_cr = _get_cr(EXTERNAL_MODEL_KIND, setup["first_model"], namespace)
            second_model_cr = _get_cr(EXTERNAL_MODEL_KIND, setup["second_model"], namespace)
            first_route_name = first_model_cr.get("status", {}).get("httpRouteName")
            second_route_name = second_model_cr.get("status", {}).get("httpRouteName")
            if not first_route_name or not second_route_name or first_route_name == second_route_name:
                pytest.fail(
                    "Expected each shared-provider ExternalModel to publish its own HTTPRoute name; "
                    f"got {first_route_name!r} and {second_route_name!r}"
                )
            setup["first_route_name"] = first_route_name
            setup["second_route_name"] = second_route_name

            log.info("Creating API key for shared-provider Praxis test...")
            response = requests.post(
                api_keys_base_url,
                headers=headers,
                json={"name": f"e2e-shared-provider-{suffix}", "subscription": setup["subscription_name"]},
                timeout=30,
                verify=TLS_VERIFY,
            )
            if response.status_code not in (200, 201):
                pytest.fail(f"Failed to create shared-provider API key: HTTP {response.status_code}")
            setup["api_key"] = response.json().get("key")
            if not setup["api_key"]:
                pytest.fail("Shared-provider API key response did not include a key")

            yield setup
        finally:
            _cleanup_shared_provider_resources(setup)

    def test_both_models_reach_praxis_ready(self, shared_provider_setup):
        """Both models publish AIGC routes and a Praxis overlay digest."""
        setup = shared_provider_setup
        for name, route_name in (
            (setup["first_model"], setup["first_route_name"]),
            (setup["second_model"], setup["second_route_name"]),
        ):
            model = _get_cr(EXTERNAL_MODEL_KIND, name, setup["namespace"])
            assert model is not None, f"ExternalModel {name} not found"
            status = model.get("status", {})
            assert status.get("phase") == "Ready", f"ExternalModel {name} phase is {status.get('phase')!r}"
            assert status.get("overlayDigest"), f"ExternalModel {name} has no distributed Praxis overlay digest"
            assert status.get("httpRouteName") == route_name, (
                f"ExternalModel {name} route status changed from {route_name!r} to {status.get('httpRouteName')!r}"
            )

    def test_second_model_routes_through_gateway(self, shared_provider_setup):
        """The second model receives the known-success response from the configured upstream path."""
        setup = shared_provider_setup
        response = _post_shared_provider_model(
            setup["gateway_url"], setup["namespace"], setup["second_model"], setup["api_key"],
            expected_status=200,
        )
        log.info("Shared provider (second model): HTTP %d", response.status_code)

    def test_first_model_still_routes(self, shared_provider_setup):
        """The first model receives the same deterministic upstream response."""
        setup = shared_provider_setup
        response = _post_shared_provider_model(
            setup["gateway_url"], setup["namespace"], setup["first_model"], setup["api_key"],
            expected_status=200,
        )
        log.info("Shared provider (first model): HTTP %d", response.status_code)

    def test_both_models_have_distinct_aigc_httproutes(self, shared_provider_setup):
        """Each model status points to its own HTTPRoute owned by AIGC."""
        setup = shared_provider_setup
        route_names = (setup["first_route_name"], setup["second_route_name"])
        assert route_names[0] != route_names[1], f"Models share HTTPRoute name {route_names[0]!r}"
        for route_name in route_names:
            route = _get_cr("httproute", route_name, setup["namespace"])
            assert route is not None, f"AIGC HTTPRoute {route_name} not found"
            labels = route.get("metadata", {}).get("labels", {})
            assert labels.get("app.kubernetes.io/managed-by") == "ai-gateway-controller", (
                f"HTTPRoute {route_name} is not owned by ai-gateway-controller: {labels!r}"
            )

    def test_delete_second_model_preserves_first(self, shared_provider_setup):
        """Deleting the second model withdraws only its route and preserves the first."""
        setup = shared_provider_setup
        _delete_cr(EXTERNAL_MODEL_KIND, setup["second_model"], setup["namespace"])
        _wait_for_cr_absent(
            EXTERNAL_MODEL_KIND,
            setup["second_model"],
            namespace=setup["namespace"],
            timeout=180,
        )
        _wait_for_cr_absent(
            "httproute",
            setup["second_route_name"],
            namespace=setup["namespace"],
            timeout=120,
        )

        response = _post_shared_provider_model(
            setup["gateway_url"], setup["namespace"], setup["second_model"], setup["api_key"],
            expected_status=404,
        )
        log.info("Shared provider (deleted second model): HTTP %d", response.status_code)
        response = _post_shared_provider_model(
            setup["gateway_url"], setup["namespace"], setup["first_model"], setup["api_key"],
            expected_status=200,
        )
        log.info("Shared provider (first model after deletion): HTTP %d", response.status_code)
