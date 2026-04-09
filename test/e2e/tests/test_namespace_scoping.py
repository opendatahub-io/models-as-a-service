"""
E2E tests for namespace scoping in MaaS.

Tests that (1) MaaS API and controller only watch the subscription namespace for MaaSAuthPolicy and
MaaSSubscription, and (2) when those CRs reference a model by (name, namespace), only that exact
model receives the generated AuthPolicy or TokenRateLimitPolicy.

Requires:
  - GATEWAY_HOST env var (e.g. maas.apps.cluster.example.com)
  - MAAS_API_BASE_URL env var (e.g. https://maas.apps.cluster.example.com/maas-api)
  - maas-controller deployed
  - LLMInferenceService deployed in llm namespace (facebook-opt-125m-simulated)
  - oc/kubectl access with cluster-admin or sufficient RBAC permissions

Environment variables (all optional, with defaults):
  - GATEWAY_HOST: Gateway hostname (required)
  - MAAS_API_BASE_URL: MaaS API URL (required)
  - MAAS_SUBSCRIPTION_NAMESPACE: MaaS subscription namespace (default: models-as-a-service)
  - E2E_TIMEOUT: Request timeout in seconds (default: 30)
  - E2E_RECONCILE_WAIT: Wait time for controller reconciliation (default: 8)
  - E2E_SKIP_TLS_VERIFY: Set to "true" to skip TLS verification
  - E2E_MODEL_REF: Model ref for tests (default: facebook-opt-125m-simulated)
  - E2E_MODEL_NAMESPACE: Namespace where model MaaSModelRef lives (default: llm)
"""

import json
import logging
import os
import subprocess
import time
import uuid
from typing import Optional

import pytest
import requests

log = logging.getLogger(__name__)

# Constants
TIMEOUT = int(os.environ.get("E2E_TIMEOUT", "30"))
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "8"))
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"
MODEL_REF = os.environ.get("E2E_MODEL_REF", "facebook-opt-125m-simulated")
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")


def _ns():
    """Default MaaS subscription namespace."""
    return os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")


def _maas_api_url():
    """MaaS API base URL."""
    url = os.environ.get("MAAS_API_BASE_URL", "")
    if not url:
        host = os.environ.get("GATEWAY_HOST", "")
        if not host:
            raise RuntimeError("MAAS_API_BASE_URL or GATEWAY_HOST env var is required")
        scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
        url = f"{scheme}://{host}/maas-api"
    return url


def _get_token():
    """Get OC token for authentication."""
    token = os.environ.get("TOKEN", "")
    if token:
        return token
    result = subprocess.run(["oc", "whoami", "-t"], capture_output=True, text=True)
    token = result.stdout.strip()
    if not token:
        raise RuntimeError("Could not get token via `oc whoami -t`")
    return token


def _create_api_key(name: str = None) -> tuple[str, str]:
    """Create an API key and return (key_id, plaintext_key)."""
    token = _get_token()
    url = f"{_maas_api_url()}/v1/api-keys"
    key_name = name or f"e2e-ns-test-{uuid.uuid4().hex[:8]}"

    r = requests.post(
        url,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        json={"name": key_name},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )
    if r.status_code not in (200, 201):
        raise RuntimeError(f"Failed to create API key: {r.status_code} {r.text}")

    data = r.json()
    return data.get("id"), data.get("key")


def _apply_cr(cr_dict: dict):
    """Apply CR from dict."""
    subprocess.run(
        ["oc", "apply", "-f", "-"],
        input=json.dumps(cr_dict),
        capture_output=True,
        text=True,
        check=True,
    )


def _delete_cr(kind: str, name: str, namespace: str):
    """Delete CR (best effort)."""
    subprocess.run(
        ["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"],
        capture_output=True,
        text=True,
    )


def _create_external_model(name: str, 
                           namespace: str, 
                           provider: str = "openai",
                           endpoint: str = "test.example.com", 
                           target_model: Optional[str] = None):
    """ Create an ExternalModel CR with the given name and namespace.
    Note: targetModel is required by the ExternalModel CRD. """
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "ExternalModel",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "provider": provider,
            "endpoint": endpoint,
            "credentialRef": {"name": f"{name}-credentials"},
            "targetModel": target_model or name
        },
    })


def _get_cr(kind: str, name: str, namespace: str) -> Optional[dict]:
    """Get CR as dict, or None if not found."""
    result = subprocess.run(
        ["oc", "get", kind, name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return None
    return json.loads(result.stdout)


def _create_namespace(name: str):
    """Create namespace if it doesn't exist."""
    result = subprocess.run(
        ["oc", "create", "namespace", name],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0 and "already exists" not in result.stderr:
        raise RuntimeError(f"Failed to create namespace {name}: {result.stderr}")


def _delete_namespace(name: str):
    """Delete namespace (best effort)."""
    subprocess.run(
        ["oc", "delete", "namespace", name, "--ignore-not-found", "--timeout=60s"],
        capture_output=True,
        text=True,
    )


def _call_subscriptions_select(api_key: str, username: str, groups: list, requested_subscription: str = "") -> requests.Response:
    """Call MaaS API POST /v1/subscriptions/select. Returns the response (always 200 with body)."""
    url = f"{_maas_api_url()}/internal/v1/subscriptions/select"
    headers = {"Authorization": f"Bearer {api_key}"}
    payload = {"username": username, "groups": groups}
    if requested_subscription:
        payload["requestedSubscription"] = requested_subscription
    return requests.post(
        url,
        headers=headers,
        json=payload,
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _wait_reconcile(seconds=None):
    """Wait for controller reconciliation."""
    time.sleep(seconds or RECONCILE_WAIT)


def _get_cr_annotation(kind: str, name: str, namespace: str, key: str):
    """Return the annotation value for key on the CR, or \"\" if not found."""
    result = subprocess.run(
        ["oc", "get", kind, name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return ""
    obj = json.loads(result.stdout)
    annotations = obj.get("metadata", {}).get("annotations") or {}
    return annotations.get(key, "") or ""


@pytest.fixture(scope="module")
def api_key():
    """Create an API key for tests."""
    _, key = _create_api_key("e2e-ns-scoping-key")
    return key


class TestMaaSAPIWatchNamespace:
    """Test that MaaS API only gets MaaSSubscription from the subscription namespace (MAAS_SUBSCRIPTION_NAMESPACE)."""

    def test_subscription_in_subscription_namespace_visible_to_api(self, api_key):
        """
        MaaSSubscription in the subscription namespace should be visible to the API.
        POST /v1/subscriptions/select with that subscription name should succeed.
        """
        sub_name = f"e2e-api-visible-{uuid.uuid4().hex[:6]}"
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": sub_name, "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 1, "window": "1m"}]}],
                },
            })
            _wait_reconcile()

            r = _call_subscriptions_select(api_key, "e2e-api-user", ["system:authenticated"], requested_subscription=sub_name)
            assert r.status_code == 200, f"subscriptions/select failed: {r.status_code} {r.text}"
            data = r.json()
            assert data.get("error") != "not_found", (
                f"Subscription {sub_name} in subscription namespace should be visible to API, got: {data}"
            )
            assert data.get("name") == sub_name, (
                f"Expected name={sub_name}, got: {data}"
            )
            log.info(f"✓ Subscription {sub_name} in {ns} is visible to MaaS API")
        finally:
            _delete_cr("MaaSSubscription", sub_name, ns)
            _wait_reconcile()

    def test_subscription_in_another_namespace_not_visible_to_api(self, api_key):
        """
        MaaSSubscription in a namespace other than the subscription namespace should NOT be visible to the API.
        POST /v1/subscriptions/select with that subscription name should return not_found.
        """
        sub_name = f"e2e-api-hidden-{uuid.uuid4().hex[:6]}"
        other_ns = "e2e-api-unwatched-ns"
        _create_namespace(other_ns)
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": sub_name, "namespace": other_ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 1, "window": "1m"}]}],
                },
            })
            _wait_reconcile()

            r = _call_subscriptions_select(api_key, "e2e-api-user", ["system:authenticated"], requested_subscription=sub_name)
            assert r.status_code == 200, f"subscriptions/select failed: {r.status_code} {r.text}"
            data = r.json()
            assert data.get("error") == "not_found", (
                f"Subscription {sub_name} in {other_ns} should NOT be visible to API (expected not_found), got: {data}"
            )
            log.info(f"✓ Subscription {sub_name} in {other_ns} is correctly not visible to MaaS API")
        finally:
            _delete_cr("MaaSSubscription", sub_name, other_ns)
            _delete_namespace(other_ns)
            _wait_reconcile()


class TestMaaSControllerWatchNamespace:
    """Verifies MaaS controller only reconciles MaaSAuthPolicy and MaaSSubscription in the subscription namespace."""

    def test_authpolicy_and_subscription_in_maas_subscription_namespace(self):
        """MaaSAuthPolicy and MaaSSubscription in MaaS subscription namespace should be reconciled
        and should appear in the AuthPolicy and TRLP annotations for the model."""
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-watched-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-watched-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 1, "window": "1m"}]}],
                },
            })
            _wait_reconcile(15)

            auth_name = f"maas-auth-{MODEL_REF}"
            auth_policies = [x.strip() for x in (_get_cr_annotation("authpolicy", auth_name, MODEL_NAMESPACE, "maas.opendatahub.io/auth-policies") or "").split(",") if x.strip()]
            assert "e2e-watched-auth" in auth_policies, (
                f"AuthPolicy {auth_name} not found or MaaSAuthPolicy e2e-watched-auth not reconciled"
            )

            trlp_name = f"maas-trlp-{MODEL_REF}"
            subscriptions = [x.strip() for x in (_get_cr_annotation("tokenratelimitpolicy", trlp_name, MODEL_NAMESPACE, "maas.opendatahub.io/subscriptions") or "").split(",") if x.strip()]
            assert "e2e-watched-sub" in subscriptions, (
                f"TRLP {trlp_name} not found or MaaSSubscription e2e-watched-sub not reconciled"
            )
        finally:
            _delete_cr("MaaSAuthPolicy", "e2e-watched-auth", ns)
            _delete_cr("MaaSSubscription", "e2e-watched-sub", ns)
            _wait_reconcile()

    def test_authpolicy_and_subscription_in_another_namespace(self):
        """MaaSAuthPolicy and MaaSSubscription in another namespace should not be reconciled
        and should not appear in the AuthPolicy and TRLP annotations for the model."""
        ns = "e2e-unwatched-ns"
        _create_namespace(ns)
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-unwatched-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-unwatched-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 1, "window": "1m"}]}],
                },
            })
            _wait_reconcile(15)

            auth_name = f"maas-auth-{MODEL_REF}"
            auth_policies = [x.strip() for x in (_get_cr_annotation("authpolicy", auth_name, MODEL_NAMESPACE, "maas.opendatahub.io/auth-policies") or "").split(",") if x.strip()]
            assert "e2e-unwatched-auth" not in auth_policies, (
                "MaaSAuthPolicy e2e-unwatched-auth reconciled"
            )

            trlp_name = f"maas-trlp-{MODEL_REF}"
            subscriptions = [x.strip() for x in (_get_cr_annotation("tokenratelimitpolicy", trlp_name, MODEL_NAMESPACE, "maas.opendatahub.io/subscriptions") or "").split(",") if x.strip()]
            assert "e2e-unwatched-sub" not in subscriptions, (
                "MaaSSubscription e2e-unwatched-sub reconciled"
            )
        finally:
            _delete_cr("MaaSAuthPolicy", "e2e-unwatched-auth", ns)
            _delete_cr("MaaSSubscription", "e2e-unwatched-sub", ns)
            _wait_reconcile()
            _delete_namespace(ns)


class TestModelRef:
    """Test model ref scoping: MaaSAuthPolicy and MaaSSubscription only reconcile into the referenced model's namespace."""

    def test_auth_policy_model_ref(self):
        """
        Create a new namespace and two MaaSModelRefs: MODEL_REF in the new namespace, and another
        name in MODEL_NAMESPACE. Create MaaSAuthPolicy referencing MODEL_REF in MODEL_NAMESPACE.
        Verify it is reconciled into MODEL_REF's AuthPolicy in MODEL_NAMESPACE, and the other two
        models' AuthPolicies do not exist.
        """
        other_ns = f"e2e-modelref-{uuid.uuid4().hex[:6]}"
        other_model_ref = f"e2e-other-model-{uuid.uuid4().hex[:6]}"
        policy_name = f"e2e-auth-ref-{uuid.uuid4().hex[:6]}"
        ns = _ns()

        _create_namespace(other_ns)
        try:
            # Create ExternalModel CRs in both namespaces
            _create_external_model("test-backend", other_ns)
            _create_external_model("test-backend", MODEL_NAMESPACE)

            # MaaSModelRef in the new namespace with same name as MODEL_REF
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSModelRef",
                "metadata": {"name": MODEL_REF, "namespace": other_ns},
                "spec": {"modelRef": {"kind": "ExternalModel", "name": "test-backend"}},
            })
            # MaaSModelRef in MODEL_NAMESPACE with a different name (not referenced by policy)
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSModelRef",
                "metadata": {"name": other_model_ref, "namespace": MODEL_NAMESPACE},
                "spec": {"modelRef": {"kind": "ExternalModel", "name": "test-backend"}},
            })

            # MaaSAuthPolicy referencing only MODEL_REF in MODEL_NAMESPACE
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": policy_name, "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })

            _wait_reconcile(15)

            auth_name = f"maas-auth-{MODEL_REF}"
            auth_name_other = f"maas-auth-{other_model_ref}"

            # Verify: policy is reconciled into MODEL_REF's AuthPolicy in MODEL_NAMESPACE
            auth_policies_reconciled = [x.strip() for x in (_get_cr_annotation("authpolicy", auth_name, MODEL_NAMESPACE, "maas.opendatahub.io/auth-policies") or "").split(",") if x.strip()]
            assert policy_name in auth_policies_reconciled, (
                f"MaaSAuthPolicy {policy_name} should be in AuthPolicy {auth_name} in {MODEL_NAMESPACE}, got: {auth_policies_reconciled}"
            )

            # Verify: MODEL_REF's AuthPolicy in the new namespace does not exist
            auth_in_other_ns = _get_cr("AuthPolicy", auth_name, other_ns)
            assert auth_in_other_ns is None, (
                f"AuthPolicy {auth_name} should NOT exist in {other_ns}"
            )

            # Verify: other model's AuthPolicy in MODEL_NAMESPACE does not exist
            auth_other_in_model_ns = _get_cr("AuthPolicy", auth_name_other, MODEL_NAMESPACE)
            assert auth_other_in_model_ns is None, (
                f"AuthPolicy {auth_name_other} should NOT exist in {MODEL_NAMESPACE} (policy references MODEL_REF only)"
            )
            log.info("✓ MaaSAuthPolicy reconciled into MODEL_REF in MODEL_NAMESPACE only; other AuthPolicies do not exist")
        finally:
            _delete_cr("MaaSAuthPolicy", policy_name, ns)
            _delete_cr("MaaSModelRef", MODEL_REF, other_ns)
            _delete_cr("MaaSModelRef", other_model_ref, MODEL_NAMESPACE)
            _delete_cr("ExternalModel", "test-backend", other_ns)
            _delete_cr("ExternalModel", "test-backend", MODEL_NAMESPACE)
            _delete_namespace(other_ns)
            _wait_reconcile()

    def test_subscription_model_ref(self):
        """
        Create a new namespace and two MaaSModelRefs: MODEL_REF in the new namespace, and another
        name in MODEL_NAMESPACE. Create MaaSSubscription referencing MODEL_REF in MODEL_NAMESPACE.
        Verify it is reconciled into MODEL_REF's TRLP in MODEL_NAMESPACE, and the other two
        models' TRLPs do not exist.
        """
        other_ns = f"e2e-modelref-{uuid.uuid4().hex[:6]}"
        other_model_ref = f"e2e-other-model-{uuid.uuid4().hex[:6]}"
        sub_name = f"e2e-sub-ref-{uuid.uuid4().hex[:6]}"
        ns = _ns()

        _create_namespace(other_ns)
        try:
            # Create ExternalModel CRs in both namespaces
            _create_external_model("test-backend", other_ns)
            _create_external_model("test-backend", MODEL_NAMESPACE)

            # MaaSModelRef in the new namespace with same name as MODEL_REF
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSModelRef",
                "metadata": {"name": MODEL_REF, "namespace": other_ns},
                "spec": {"modelRef": {"kind": "ExternalModel", "name": "test-backend"}},
            })
            # MaaSModelRef in MODEL_NAMESPACE with a different name
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSModelRef",
                "metadata": {"name": other_model_ref, "namespace": MODEL_NAMESPACE},
                "spec": {"modelRef": {"kind": "ExternalModel", "name": "test-backend"}},
            })

            # MaaSSubscription referencing only MODEL_REF in MODEL_NAMESPACE
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": sub_name, "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [
                        {"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 100, "window": "1m"}]},
                    ],
                },
            })

            _wait_reconcile(15)

            trlp_name = f"maas-trlp-{MODEL_REF}"
            trlp_name_other = f"maas-trlp-{other_model_ref}"

            # Verify: subscription is reconciled into MODEL_REF's TRLP in MODEL_NAMESPACE
            subscriptions_in_model_ns = [x.strip() for x in (_get_cr_annotation("tokenratelimitpolicy", trlp_name, MODEL_NAMESPACE, "maas.opendatahub.io/subscriptions") or "").split(",") if x.strip()]
            assert sub_name in subscriptions_in_model_ns, (
                f"MaaSSubscription {sub_name} should be in TRLP {trlp_name} in {MODEL_NAMESPACE}, got: {subscriptions_in_model_ns}"
            )

            # Verify: MODEL_REF's TRLP in the new namespace does not exist
            trlp_in_other_ns = _get_cr("tokenratelimitpolicy", trlp_name, other_ns)
            assert trlp_in_other_ns is None, (
                f"TokenRateLimitPolicy {trlp_name} should NOT exist in {other_ns}"
            )

            # Verify: other model's TRLP in MODEL_NAMESPACE does not exist
            trlp_other_in_model_ns = _get_cr("tokenratelimitpolicy", trlp_name_other, MODEL_NAMESPACE)
            assert trlp_other_in_model_ns is None, (
                f"TokenRateLimitPolicy {trlp_name_other} should NOT exist in {MODEL_NAMESPACE} (subscription references MODEL_REF only)"
            )
            log.info("✓ MaaSSubscription reconciled into MODEL_REF in MODEL_NAMESPACE only; other TRLPs do not exist")
        finally:
            _delete_cr("MaaSSubscription", sub_name, ns)
            _delete_cr("MaaSModelRef", MODEL_REF, other_ns)
            _delete_cr("MaaSModelRef", other_model_ref, MODEL_NAMESPACE)
            _delete_cr("ExternalModel", "test-backend", other_ns)
            _delete_cr("ExternalModel", "test-backend", MODEL_NAMESPACE)
            _delete_namespace(other_ns)
            _wait_reconcile()
