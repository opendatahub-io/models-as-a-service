"""
E2E test for in-place ExternalProvider auth Secret rotation under Praxis ExtProc.

Covers RHOAIENG-97467: when the provider auth Secret is updated in place (same
name/key, new value), the praxis ExtProc dataplane must inject the *new*
credential on upstream requests — keeping inference working *without* deleting
the ExtProc pod or the ExternalModel. This guards against the resolver stall that
was observed after credential updates (``demo.sh`` historically marked rotation
NOT_DEMONSTRATED).

How the test actually verifies the rotation (not just "egress still works"):
  The upstream is httpbingo's ``/anything`` echo endpoint, which returns the
  request it received as JSON — including the ``Authorization`` header that
  praxis-extproc's credential_inject put on the upstream request. So the test
  reads back the *exact* credential the provider would have seen, and asserts it
  flips from v1 to v2 after the in-place Secret rotation. That directly proves the
  AC: the new credential reaches the upstream, and requests keep succeeding, with
  no ExtProc/ExternalModel teardown.

Praxis topology notes (why the resources live where they do):
  * The ExternalProvider/ExternalModel, the MaaSModelRef, and the credential
    Secret all live in the tenant (subscription) namespace. The AIGC-generated
    route only attaches once that tenant Gateway's ``allowedRoutes`` admits the
    namespace, and the credential Secret has to sit next to the model it backs.
    (A ref-less ExternalModel elsewhere is implicitly attached to the default
    tenant's Gateway by ai-gateway-controller, so co-locating in the tenant
    namespace is the reliable choice here rather than a hard requirement.)
  * The route AIGC creates (``external-model-<name>``) attaches to the tenant's
    Gateway, so that Gateway's ``allowedRoutes`` must admit the tenant namespace
    (deploy.sh adds it to ALLOWED_ROUTE_NAMESPACES).
  * ExternalProvider auth must be ``type: apikey`` — the praxis reconciler rejects
    the legacy ``simple`` strategy ("only apikey is supported").
  * Credential injection runs in a dedicated per-tenant extproc Deployment
    (``payload-processing-external-model``) in the tenant namespace; the request
    body flows through the shared payload-processing / payload-pre-processing
    stack. The rotation must not restart any of them.
  * The gateway rewrites the ``/{tenant-ns}/{model}`` prefix to ``/``, so a
    ``/anything/...`` suffix on the client URL lands on httpbingo's echo endpoint
    upstream.
"""

from __future__ import annotations

import json
import logging
import os
import time

import pytest
import requests
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    LABEL_TENANT_INSTANCE,
    _oc_run,
    extproc_deployment_uses_praxis,
)
from test_helper import (
    RECONCILE_WAIT,
    TLS_VERIFY,
    _apply_cr,
    _check_ipp_pods_deployed,
    _delete_cr,
    _get_cr,
    _is_transient_gateway_response,
    _ns,
    _revoke_api_key,
    _wait_for_httproute_accepted,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
)

log = logging.getLogger(__name__)

# ─── Configuration ──────────────────────────────────────────────────────────

EXTERNAL_ENDPOINT = os.environ.get(
    "E2E_EXTERNAL_ENDPOINT",
    os.environ.get("E2E_SIMULATOR_ENDPOINT", "httpbingo.org"),
)


def _subscription_ns():
    return os.environ.get("E2E_SUBSCRIPTION_NAMESPACE", _ns())


# Praxis attaches external models to the tenant's Gateway, so the ExternalModel,
# ExternalProvider, MaaSModelRef and credential Secret must co-locate in the
# tenant (subscription) namespace — the only namespace with a MaasTenantConfig.
EXTERNAL_NS = _subscription_ns()

TARGET_MODEL = "gpt-3.5-turbo"

EXTERNAL_MODEL_NAME = "e2e-rotation-external-model"
EXTERNAL_PROVIDER_NAME = "e2e-rotation-external-provider"
SECRET_NAME = f"{EXTERNAL_MODEL_NAME}-api-key"
SECRET_KEY = "api-key"
SUBSCRIPTION_NAME = "e2e-rotation-subscription"
AUTH_POLICY_NAME = "e2e-rotation-access"

# The route AIGC generates for an ExternalModel is prefixed with "external-model-".
EXTERNAL_MODEL_ROUTE_NAME = f"external-model-{EXTERNAL_MODEL_NAME}"

# httpbingo's /anything echoes the upstream request as JSON, letting us read back
# the credential praxis-extproc injected. The gateway strips the /{ns}/{model}
# prefix, so this suffix lands on /anything/... upstream.
ECHO_PATH_SUFFIX = "/anything/v1/chat/completions"

# Same name/key, new value — the "in place" rotation the AC describes.
SECRET_VALUE_V1 = "e2e-rotation-key-v1"
SECRET_VALUE_V2 = "e2e-rotation-key-v2"

# Canonical inference.opendatahub.io CRDs (see test_external_models.py for the
# group/kind rationale).
EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"

# Default-tenant praxis ExtProc Deployments (unsuffixed) in the gateway namespace.
PROCESSING_DEPLOYMENT = "payload-processing"
PRE_PROCESSING_DEPLOYMENT = "payload-pre-processing"
# Per-tenant external-model extproc (credential injection) in the tenant namespace.
EXTERNAL_MODEL_DEPLOYMENT = "payload-processing-external-model"

# Baseline egress can race route/extproc warmup; rotation also waits for the
# mounted Secret to propagate to the extproc. That propagation is bounded by the
# kubelet sync period + cache TTL and was observed to take ~60-90s, so give the
# rotation poll generous margin to avoid flakes.
EGRESS_POLL_TIMEOUT = max(RECONCILE_WAIT * 6, 90)
ROTATION_POLL_TIMEOUT = max(RECONCILE_WAIT * 24, 240)


# ─── Helpers ─────────────────────────────────────────────────────────────────

def _check_external_endpoint_reachable() -> bool:
    """Verify the external endpoint is reachable (and httpbin-compatible)."""
    for scheme in ("https", "http"):
        try:
            r = requests.get(f"{scheme}://{EXTERNAL_ENDPOINT}/get", timeout=10, verify=False)
            if r.status_code == 200:
                return True
        except Exception:
            pass
    return False


pytestmark = [
    pytest.mark.serial,
    pytest.mark.skipif(
        not _check_external_endpoint_reachable(),
        reason=f"External endpoint {EXTERNAL_ENDPOINT} is not reachable (disconnected environment?)",
    ),
    pytest.mark.xdist_group("tenant_ipp"),
]


def _skip_unless_default_praxis():
    # Skip only when praxis-extproc isn't the configured dataplane (or the
    # Deployment is absent) — that's genuinely not this test's scenario.
    if not extproc_deployment_uses_praxis(PROCESSING_DEPLOYMENT):
        pytest.skip("default tenant payload-processing is not praxis-extproc")
    # Praxis IS the dataplane: a transient rollout must not silently skip this
    # test. Wait for the IPP pods to become ready, and fail if they never do.
    deadline = time.time() + EGRESS_POLL_TIMEOUT
    while time.time() < deadline:
        if _check_ipp_pods_deployed():
            return
        time.sleep(5)
    pytest.fail("Default payload-processing stack never became ready")


def _extproc_pod_uids(deployment: str, namespace: str, *, label: str | None = None) -> set[str]:
    """UIDs of a Deployment's current pods, to prove rotation did not replace them.

    The shared praxis stack is selected by the ``tenant-instance`` label; the
    per-tenant external-model extproc uses a plain ``app`` label.
    """
    selector = label or f"{LABEL_TENANT_INSTANCE}={deployment}"
    result = _oc_run(
        ["get", "pods", "-n", namespace, "-l", selector, "-o", "json"]
    )
    try:
        items = json.loads(result.stdout or "{}").get("items", [])
    except (json.JSONDecodeError, TypeError):
        return set()
    # Exclude terminating pods (deletionTimestamp set): a pod on its way out
    # must not count as "surviving" the rotation, or a restart caught mid-flight
    # would be masked.
    return {
        meta["uid"]
        for pod in items
        for meta in (pod.get("metadata", {}),)
        if meta.get("uid") and not meta.get("deletionTimestamp")
    }


def _all_extproc_uids() -> dict[str, set[str]]:
    """Snapshot UIDs of every ExtProc pod that must survive the rotation."""
    return {
        PROCESSING_DEPLOYMENT: _extproc_pod_uids(PROCESSING_DEPLOYMENT, GATEWAY_NAMESPACE),
        PRE_PROCESSING_DEPLOYMENT: _extproc_pod_uids(PRE_PROCESSING_DEPLOYMENT, GATEWAY_NAMESPACE),
        EXTERNAL_MODEL_DEPLOYMENT: _extproc_pod_uids(
            EXTERNAL_MODEL_DEPLOYMENT, EXTERNAL_NS, label=f"app={EXTERNAL_MODEL_DEPLOYMENT}"
        ),
    }


def _extproc_uids_for(deployment: str) -> set[str]:
    """Re-read one deployment's pod UIDs using the same selector as the snapshot."""
    if deployment == EXTERNAL_MODEL_DEPLOYMENT:
        return _extproc_pod_uids(deployment, EXTERNAL_NS, label=f"app={EXTERNAL_MODEL_DEPLOYMENT}")
    return _extproc_pod_uids(deployment, GATEWAY_NAMESPACE)


def _deployment_ns(deployment: str) -> str:
    """Namespace a given ExtProc Deployment lives in."""
    return EXTERNAL_NS if deployment == EXTERNAL_MODEL_DEPLOYMENT else GATEWAY_NAMESPACE


def _extproc_selector(deployment: str) -> str:
    """Pod label selector for an ExtProc Deployment (same as the UID snapshot)."""
    if deployment == EXTERNAL_MODEL_DEPLOYMENT:
        return f"app={deployment}"
    return f"{LABEL_TENANT_INSTANCE}={deployment}"


def _extproc_generation(deployment: str) -> str:
    """``.metadata.generation`` of an ExtProc Deployment.

    An in-place Secret data change does not touch the Deployment, so its
    generation stays constant across rotation. Anything that *restarts* the
    ExtProc (a spec change or ``rollout restart``) bumps it. Generation is
    therefore a no-restart signal that is insensitive to scale/eviction events.
    """
    result = _oc_run(
        ["get", "deployment", deployment, "-n", _deployment_ns(deployment),
         "-o", "jsonpath={.metadata.generation}"]
    )
    return (result.stdout or "").strip()


def _extproc_restart_counts(deployment: str) -> dict[tuple[str, str], int]:
    """Container restart counts keyed by ``(pod UID, container name)``.

    The pod-UID and Deployment-generation checks miss an *in-place* container
    restart: the ExtProc process crashing (or being killed by a liveness probe)
    and restarting inside the same pod bumps ``restartCount`` without changing
    the pod UID or the Deployment generation. Rotation must not restart the
    container either, so snapshot these and require them unchanged.
    """
    result = _oc_run(
        ["get", "pods", "-n", _deployment_ns(deployment),
         "-l", _extproc_selector(deployment), "-o", "json"]
    )
    try:
        items = json.loads(result.stdout or "{}").get("items", [])
    except (json.JSONDecodeError, TypeError):
        return {}
    counts: dict[tuple[str, str], int] = {}
    for pod in items:
        meta = pod.get("metadata", {})
        uid = meta.get("uid")
        if not uid or meta.get("deletionTimestamp"):
            continue
        for cs in pod.get("status", {}).get("containerStatuses", []) or []:
            name = cs.get("name")
            if name is not None:
                counts[(uid, name)] = cs.get("restartCount", 0)
    return counts


def _apply_provider_secret(value: str):
    """Create/replace the provider credential Secret in place (same name/key)."""
    _apply_cr(
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": SECRET_NAME,
                "namespace": EXTERNAL_NS,
                # Retained for parity with the legacy IPP secret store, which
                # selected credential Secrets by this label. The praxis (Rust)
                # extproc instead reads the credential from a projected Secret
                # volume rendered from the ExternalProvider spec, so the label is
                # harmless here but kept to stay compatible across dataplanes.
                "labels": {"inference.llm-d.ai/ipp-managed": "true"},
            },
            "type": "Opaque",
            "stringData": {SECRET_KEY: value},
        }
    )


def _echoed_authorization(response: requests.Response) -> str | None:
    """Pull the Authorization header the upstream received from an /anything echo.

    httpbingo echoes headers as ``{"headers": {"Authorization": ["Bearer ..."]}}``.
    """
    try:
        headers = response.json().get("headers", {})
    except ValueError:
        return None
    value = headers.get("Authorization") or headers.get("authorization")
    if isinstance(value, list):
        return value[0] if value else None
    return value


def _poll_injected_credential(url: str, headers: dict, body: dict, *, expected: str, timeout: int) -> str:
    """POST to the echo upstream until the credential praxis injected == ``expected``.

    The echoed Authorization header is exactly what credential_inject put on the
    upstream request, so this proves the provider received that credential. Rides
    out auth-propagation flakes (empty 401, proxy 500/503) and, after a rotation,
    the mounted-Secret propagation lag.
    """
    deadline = time.time() + timeout
    last_status = None
    last_cred = None
    saw_non_auth_response = False
    while time.time() < deadline:
        try:
            r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        except requests.RequestException as exc:
            log.info("Echo poll transient request error: %s", exc)
            time.sleep(2)
            continue
        last_status = r.status_code
        if r.status_code not in (401, 403) and not _is_transient_gateway_response(r):
            saw_non_auth_response = True
            last_cred = _echoed_authorization(r)
            if last_cred == expected:
                return last_cred
            # Never log the credential value itself (CWE-532): on injection
            # failure the upstream may echo the client's Authorization header,
            # which carries the test's API key. Report presence only.
            log.info(
                "Echo poll: upstream credential did not match expected "
                "(credential present=%s), retrying...",
                last_cred is not None,
            )
        else:
            log.info("Echo poll got transient %d, retrying...", r.status_code)
        time.sleep(2)
    # Persistent 401/403 (never a single authorized response) is an authorization
    # problem — AuthPolicy not enforced, or the key not bound to the subscription —
    # not a credential-rotation failure. Call it out so the two don't get confused.
    hint = ""
    if not saw_non_auth_response and last_status in (401, 403):
        hint = (
            " — every response was 401/403, which indicates an authorization problem "
            "(AuthPolicy not enforced or API key not bound to the subscription), "
            "not the credential rotation"
        )
    raise AssertionError(
        f"upstream never received the expected credential within {timeout}s "
        f"(last status={last_status}, credential present={last_cred is not None}){hint}"
    )


# ─── Fixture: external provider + model backed by a credential Secret ─────────

@pytest.fixture(scope="module")
def rotation_setup(gateway_url, headers, api_keys_base_url):
    """Stand up an external egress route backed by a rotatable credential Secret."""
    _skip_unless_default_praxis()

    log.info("Setting up external provider rotation fixture (endpoint: %s, ns: %s)...",
             EXTERNAL_ENDPOINT, EXTERNAL_NS)
    api_key_id = None
    try:
        _apply_provider_secret(SECRET_VALUE_V1)

        _apply_cr(
            {
                "apiVersion": "inference.opendatahub.io/v1alpha1",
                "kind": "ExternalProvider",
                "metadata": {"name": EXTERNAL_PROVIDER_NAME, "namespace": EXTERNAL_NS},
                "spec": {
                    "provider": "openai",
                    "endpoint": EXTERNAL_ENDPOINT,
                    "auth": {
                        # praxis reconciler only implements apikey; "simple" is rejected.
                        "type": "apikey",
                        "secretRef": {"name": SECRET_NAME},
                    },
                },
            }
        )

        _apply_cr(
            {
                "apiVersion": "inference.opendatahub.io/v1alpha1",
                "kind": "ExternalModel",
                "metadata": {"name": EXTERNAL_MODEL_NAME, "namespace": EXTERNAL_NS},
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
            }
        )

        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSModelRef",
                "metadata": {
                    "name": EXTERNAL_MODEL_NAME,
                    "namespace": EXTERNAL_NS,
                    "annotations": {
                        "maas.opendatahub.io/endpoint": EXTERNAL_ENDPOINT,
                        "maas.opendatahub.io/provider": "openai",
                    },
                },
                "spec": {
                    "modelRef": {"kind": "ExternalModel", "name": EXTERNAL_MODEL_NAME},
                },
            }
        )

        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": AUTH_POLICY_NAME, "namespace": EXTERNAL_NS},
                "spec": {
                    "modelRefs": [{"name": EXTERNAL_MODEL_NAME, "namespace": EXTERNAL_NS}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            }
        )

        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": SUBSCRIPTION_NAME, "namespace": EXTERNAL_NS},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [
                        {
                            "name": EXTERNAL_MODEL_NAME,
                            "namespace": EXTERNAL_NS,
                            "tokenRateLimits": [{"limit": 10000, "window": "1h"}],
                        },
                    ],
                },
            }
        )

        _wait_for_maas_auth_policy_phase(AUTH_POLICY_NAME, namespace=EXTERNAL_NS)
        _wait_for_maas_subscription_phase(SUBSCRIPTION_NAME, namespace=EXTERNAL_NS)
        _wait_for_httproute_accepted(EXTERNAL_MODEL_ROUTE_NAME, namespace=EXTERNAL_NS)
        _wait_for_model_ready(EXTERNAL_MODEL_NAME, namespace=EXTERNAL_NS, timeout=120)

        log.info("Creating API key for rotation tests...")
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "e2e-rotation-key", "subscription": SUBSCRIPTION_NAME},
            timeout=30,
            verify=TLS_VERIFY,
        )
        if r.status_code not in (200, 201):
            pytest.fail(f"Failed to create API key: {r.status_code} {r.text}")
        api_key = r.json().get("key")
        api_key_id = r.json().get("id")
        if not api_key:
            pytest.fail("API key creation response missing 'key' field")

        yield {"api_key": api_key, "gateway_url": gateway_url}
    finally:
        log.info("Cleaning up external provider rotation fixtures...")
        # Best-effort: deleting the subscription invalidates the key, but revoke
        # it explicitly so repeated runs don't accumulate key records.
        if api_key_id:
            oc_token = headers.get("Authorization", "").removeprefix("Bearer ").strip()
            if oc_token:
                _revoke_api_key(oc_token, api_key_id)
        _delete_cr("maasauthpolicy", AUTH_POLICY_NAME, EXTERNAL_NS)
        _delete_cr("maassubscription", SUBSCRIPTION_NAME, EXTERNAL_NS)
        _delete_cr("maasmodelref", EXTERNAL_MODEL_NAME, EXTERNAL_NS)
        _delete_cr(EXTERNAL_MODEL_KIND, EXTERNAL_MODEL_NAME, EXTERNAL_NS)
        _delete_cr(EXTERNAL_PROVIDER_KIND, EXTERNAL_PROVIDER_NAME, EXTERNAL_NS)
        _delete_cr("secret", SECRET_NAME, EXTERNAL_NS)


# ─── Test ──────────────────────────────────────────────────────────────────

class TestPraxisExtProcSecretRotation:
    """In-place provider Secret rotation keeps egress alive (RHOAIENG-97467)."""

    def _echo_request(self, setup):
        url = f"{setup['gateway_url']}/{EXTERNAL_NS}/{EXTERNAL_MODEL_NAME}{ECHO_PATH_SUFFIX}"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}
        return url, headers, body

    def test_in_place_secret_rotation_keeps_egress_without_extproc_restart(self, rotation_setup):
        setup = rotation_setup
        url, headers, body = self._echo_request(setup)

        _poll_injected_credential(
            url, headers, body, expected=f"Bearer {SECRET_VALUE_V1}", timeout=EGRESS_POLL_TIMEOUT
        )
        log.info("Baseline: upstream receives the v1 credential")

        before_uids = _all_extproc_uids()
        for deployment, uids in before_uids.items():
            assert uids, f"no {deployment} pods found before rotation"
        before_gens = {d: _extproc_generation(d) for d in before_uids}
        before_restarts = {d: _extproc_restart_counts(d) for d in before_uids}

        model_before = _get_cr(EXTERNAL_MODEL_KIND, EXTERNAL_MODEL_NAME, EXTERNAL_NS)
        assert model_before is not None, "ExternalModel missing before rotation"
        model_uid_before = model_before["metadata"]["uid"]

        _apply_provider_secret(SECRET_VALUE_V2)
        assert _get_cr("secret", SECRET_NAME, EXTERNAL_NS) is not None, (
            "provider Secret vanished after in-place rotation"
        )
        log.info("Provider Secret rotated in place (same name/key, new value)")

        _poll_injected_credential(
            url, headers, body, expected=f"Bearer {SECRET_VALUE_V2}", timeout=ROTATION_POLL_TIMEOUT
        )
        log.info("After rotation: upstream receives the v2 credential")

        for deployment, uids in before_uids.items():
            # No rollout was triggered: an in-place Secret change must not bump
            # the Deployment generation. This catches a restart without being
            # sensitive to scale/eviction events (unlike exact pod-set equality).
            after_gen = _extproc_generation(deployment)
            assert after_gen == before_gens[deployment], (
                f"{deployment} Deployment generation changed during in-place rotation "
                f"({before_gens[deployment]} -> {after_gen}); rotation must not roll ExtProc"
            )
            # Every pod that existed before must still be present (excluding
            # terminating pods), so none were replaced.
            after_uids = _extproc_uids_for(deployment)
            assert uids <= after_uids, (
                f"{deployment} pods were replaced during in-place rotation "
                f"(before={uids}, after={after_uids}); rotation must not restart ExtProc"
            )
            # No in-place container restart: every container we saw before must
            # still be present with the same restartCount (a crash/liveness kill
            # within the same pod would bump it without changing pod UID/gen).
            after_restarts = _extproc_restart_counts(deployment)
            for key, count in before_restarts[deployment].items():
                pod_uid, container = key
                assert after_restarts.get(key) == count, (
                    f"{deployment} container {container} restarted during in-place rotation "
                    f"(pod {pod_uid}: {count} -> {after_restarts.get(key)}); "
                    "rotation must not restart ExtProc"
                )

        model_after = _get_cr(EXTERNAL_MODEL_KIND, EXTERNAL_MODEL_NAME, EXTERNAL_NS)
        assert model_after is not None, (
            "ExternalModel disappeared during in-place Secret rotation"
        )
        assert model_after["metadata"]["uid"] == model_uid_before, (
            "ExternalModel was deleted and recreated during in-place Secret rotation "
            "(UID changed); rotation must not recreate the ExternalModel"
        )
