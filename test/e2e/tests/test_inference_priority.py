"""E2E tests for subscription request priority (llm-d flow control).

A MaaSSubscription with ``spec.inferencePriority`` makes maas-controller reconcile one
llm-d ``InferenceObjective`` per InferencePool behind the subscription's models, report
it in ``status.flowControlStatuses`` and the ``InferenceObjectivesReady`` condition, and
have the gateway AuthPolicy inject the objective and fairness ID headers the scheduler
reads. Client-supplied copies of those headers are overwritten at the gateway.

The pool model is a test-created simulator LLMInferenceService with ``router.scheduler``,
so KServe gives it an InferencePool and an endpoint picker. The fixture models have no
scheduler and only cover the ``NotApplicable`` path.

Headers are checked in the generated gateway AuthPolicy and by sending traffic with
spoofed values. They are not observed at the backend: that needs ``--log-http``, which
writes live API keys into pod logs.

The module skips when the ``inferenceobjectives.llm-d.ai`` CRD is not installed.

Environment Variables:
- E2E_EPP_POOL_TIMEOUT: Seconds to wait for the pool backend and for the first
  successful response through it (default: 300)
- E2E_FLOW_CONTROL_TIMEOUT: Seconds to wait for flow-control status and
  InferenceObjective changes (default: 120)
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import uuid

import pytest
from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    _oc_run,
    get_gateway_authpolicy,
    get_json_or_none,
    list_json,
    wait_for_llmisvc_backend_ready,
    wait_until,
)
from test_helper import (
    MODEL_NAMESPACE,
    MODEL_REF,
    _create_api_key,
    _create_llmis,
    _create_maas_model_ref,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_governance_and_wait,
    _delete_sa,
    _gateway_url,
    _get_cr,
    _ns,
    _poll_status,
    _sa_to_user,
    _wait_for_cr_absent,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
    chat,
)

log = logging.getLogger(__name__)

INFERENCE_OBJECTIVE = "inferenceobjectives.llm-d.ai"
POOL_READY_TIMEOUT = int(os.environ.get("E2E_EPP_POOL_TIMEOUT", "300"))
FLOW_CONTROL_TIMEOUT = int(os.environ.get("E2E_FLOW_CONTROL_TIMEOUT", "120"))
CONDITION = "InferenceObjectivesReady"
SUBSCRIPTION_UID_LABEL = "maas.opendatahub.io/subscription-uid"
# Canonical and legacy llm-d flow-control headers, mapped to the subscription-info field
# each one is set from.
FLOW_CONTROL_HEADERS = {
    "x-llm-d-inference-objective": "objective",
    "x-gateway-inference-objective": "objective",
    "x-llm-d-inference-fairness-id": "fairnessId",
    "x-gateway-inference-fairness-id": "fairnessId",
}


def _crd_installed(crd_name):
    """Check if a CRD is installed; False when the cluster cannot be reached (collect-only)."""
    try:
        result = subprocess.run(["oc", "get", "crd", crd_name], check=False, capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        return False
    return result.returncode == 0


# Own xdist group, and serial because creating the MaaSAuthPolicy regenerates the shared
# gateway AuthPolicy and the pool model lives on the shared default gateway.
pytestmark = [
    pytest.mark.skipif(not _crd_installed(INFERENCE_OBJECTIVE),
                       reason=f"{INFERENCE_OBJECTIVE} CRD not installed"),
    pytest.mark.xdist_group("inference_priority"),
    pytest.mark.serial,
]


def _patch(kind, name, namespace, patch, patch_type="merge"):
    result = _oc_run(["patch", kind, name, "-n", namespace, "--type", patch_type, "-p", json.dumps(patch)])
    assert result.returncode == 0, f"oc patch {kind}/{name} failed: {result.stderr.strip()}"


def _flow_control_status(subscription, model):
    """The subscription's flowControlStatuses entry for model, or None."""
    cr = _get_cr("maassubscription", subscription) or {}
    for entry in (cr.get("status") or {}).get("flowControlStatuses") or []:
        if entry.get("name") == model and entry.get("namespace") == MODEL_NAMESPACE:
            return entry
    return None


def _condition(subscription):
    cr = _get_cr("maassubscription", subscription) or {}
    for cond in (cr.get("status") or {}).get("conditions") or []:
        if cond.get("type") == CONDITION:
            return cond
    return None


def _wait_for_flow_control_reason(subscription, model, reason):
    def reached():
        entry = _flow_control_status(subscription, model)
        assert entry and entry.get("reason") == reason, f"flowControlStatuses entry for {model}: {entry}"
        return entry

    return wait_until(reached, FLOW_CONTROL_TIMEOUT,
                      f"{subscription} never reported {reason} for {model}", interval=2)


def _wait_for_objective_priority(name, priority):
    def reached():
        objective = get_json_or_none(INFERENCE_OBJECTIVE, name, MODEL_NAMESPACE)
        assert objective, f"InferenceObjective {MODEL_NAMESPACE}/{name} not found"
        actual = (objective.get("spec") or {}).get("priority")
        assert actual == priority, f"InferenceObjective {name} priority {actual}, expected {priority}"
        return objective

    return wait_until(reached, FLOW_CONTROL_TIMEOUT,
                      f"InferenceObjective {name} never reached priority {priority}", interval=2)


def _owned_objectives(subscription_uid):
    return list_json(INFERENCE_OBJECTIVE, MODEL_NAMESPACE, labels=f"{SUBSCRIPTION_UID_LABEL}={subscription_uid}")


def _chat(api_key, path=None, extra_headers=None, model_name=None):
    """Body-routed chat completion: the model is only named in the body, so path is unused.

    The signature matches the inference_fn contract of _poll_status.
    """
    headers = {"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"}
    headers.update(extra_headers or {})
    return chat("Hello", f"{_gateway_url()}/v1", headers, model_name, max_tokens=5)


def _assert_served(api_key, model, extra_headers=None, timeout=None):
    """Poll until a body-routed completion for model succeeds, and check it has choices."""
    response = _poll_status(api_key, 200, extra_headers=extra_headers, model_name=model,
                            timeout=timeout, inference_fn=_chat)
    assert response.json().get("choices"), f"completion without choices: {response.text[:200]}"
    return response


def _log_diagnostics(llmis, subscription):
    """Log the pool and subscription state before cleanup deletes them."""
    obj = get_json_or_none("llminferenceservice", llmis, MODEL_NAMESPACE) or {}
    log.error("LLMInferenceService %s status: %s", llmis, obj.get("status"))
    sub = _get_cr("maassubscription", subscription) or {}
    log.error("MaaSSubscription %s status: %s", subscription, sub.get("status"))
    for objective in list_json(INFERENCE_OBJECTIVE, MODEL_NAMESPACE):
        if objective["metadata"]["name"].startswith("maas-"):
            log.error("InferenceObjective %s: spec=%s labels=%s", objective["metadata"]["name"],
                      objective.get("spec"), objective["metadata"].get("labels"))


@pytest.fixture(scope="class")
def pool_model():
    """A scheduler-backed model with an auth policy and a subscription at inferencePriority 10.

    Access is granted to a dedicated service account only, so no other caller sees the
    new subscription.
    """
    suffix = uuid.uuid4().hex[:8]
    llmis = f"e2e-prio-pool-{suffix}"
    policy = f"e2e-prio-access-{suffix}"
    subscription = f"e2e-prio-sub-{suffix}"
    sa_name = f"e2e-prio-{suffix}"
    served_id = f"test/e2e-prio-pool-{suffix}"

    try:
        log.info("Creating InferencePool model %s/%s", MODEL_NAMESPACE, llmis)
        _create_llmis(llmis, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME, GATEWAY_NAMESPACE,
                      model_name=served_id, scheduler=True)
        wait_for_llmisvc_backend_ready(llmis, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME, timeout=POOL_READY_TIMEOUT)

        def pool_name():
            obj = get_json_or_none("llminferenceservice", llmis, MODEL_NAMESPACE) or {}
            pool = ((((obj.get("status") or {}).get("router") or {}).get("scheduler") or {})
                    .get("inferencePool") or {})
            return pool.get("name")

        pool = wait_until(pool_name, POOL_READY_TIMEOUT, f"{llmis} never reported its InferencePool")
        _create_maas_model_ref(llmis, MODEL_NAMESPACE, llmis)

        sa_token = _create_sa_token(sa_name, duration="30m")
        sa_user = _sa_to_user(sa_name)
        _create_test_auth_policy(policy, llmis, users=[sa_user])
        _create_test_subscription(subscription, llmis, users=[sa_user], token_limit=100000, inference_priority=10)
        _wait_for_maas_auth_policy_phase(policy)
        _wait_for_maas_subscription_phase(subscription)
        _wait_for_model_ready(llmis, namespace=MODEL_NAMESPACE, timeout=120)

        # Auth needs to propagate and the pool needs its picker, so wait for the first success.
        model = f"publishers/{MODEL_NAMESPACE}/models/{served_id}"
        api_key = _create_api_key(sa_token, name=f"e2e-prio-{suffix}", subscription=subscription)
        _assert_served(api_key, model, timeout=POOL_READY_TIMEOUT)

        yield {
            "llmis": llmis,
            "pool": pool,
            "subscription": subscription,
            "api_key": api_key,
            "model": model,
        }
    except Exception:
        _log_diagnostics(llmis, subscription)
        raise
    finally:
        _delete_cr("maassubscription", subscription)
        _delete_cr("maasauthpolicy", policy)
        _delete_cr("maasmodelref", llmis, namespace=MODEL_NAMESPACE)
        _delete_cr("llminferenceservice", llmis, namespace=MODEL_NAMESPACE)
        _delete_sa(sa_name)
        _wait_for_cr_absent("maassubscription", subscription)
        _wait_for_cr_absent("maasauthpolicy", policy)
        _wait_for_cr_absent("maasmodelref", llmis, namespace=MODEL_NAMESPACE)
        _wait_for_cr_absent("llminferenceservice", llmis, namespace=MODEL_NAMESPACE, timeout=180)


@pytest.mark.models
class TestInferencePriority:
    """InferenceObjective lifecycle, status and gateway headers for a pool-backed model.

    Tests run in order and share the subscription: each one starts from the state the
    previous one left.
    """

    def test_objective_created_and_status_ready(self, pool_model):
        llmis, pool, subscription = pool_model["llmis"], pool_model["pool"], pool_model["subscription"]
        try:
            entry = _wait_for_flow_control_reason(subscription, llmis, "ObjectiveReconciled")
            assert entry.get("ready") is True, f"flow-control entry not ready: {entry}"
            assert (entry.get("inferencePool") or {}).get("name") == pool, f"wrong pool in status: {entry}"
            objective_name = entry.get("objectiveName")
            assert objective_name, f"no objectiveName in status: {entry}"

            cond = wait_until(lambda: _condition(subscription), FLOW_CONTROL_TIMEOUT,
                              f"{subscription} has no {CONDITION} condition", interval=2)
            assert cond.get("status") == "True" and cond.get("reason") == "Reconciled", f"{CONDITION}: {cond}"

            sub = _get_cr("maassubscription", subscription)
            assert sub["status"].get("phase") == "Active", f"phase {sub['status'].get('phase')}"

            objective = _wait_for_objective_priority(objective_name, 10)
            spec, meta = objective["spec"], objective["metadata"]
            assert (spec.get("poolRef") or {}).get("name") == pool, f"objective poolRef: {spec.get('poolRef')}"
            labels, annotations = meta.get("labels") or {}, meta.get("annotations") or {}
            assert labels.get("app.kubernetes.io/managed-by") == "maas-controller", labels
            assert labels.get("app.kubernetes.io/component") == "inference-objective", labels
            assert labels.get(SUBSCRIPTION_UID_LABEL) == sub["metadata"]["uid"], labels
            assert labels.get("maas.opendatahub.io/subscription-namespace") == _ns(), labels
            assert annotations.get("maas.opendatahub.io/subscription-name") == subscription, annotations
        except Exception:
            _log_diagnostics(llmis, subscription)
            raise
        pool_model["objective"] = objective_name
        pool_model["uid"] = sub["metadata"]["uid"]
        _assert_served(pool_model["api_key"], pool_model["model"])

    def test_priority_update_propagates(self, pool_model):
        _patch("maassubscription", pool_model["subscription"], _ns(), {"spec": {"inferencePriority": -5}})
        _wait_for_objective_priority(pool_model["objective"], -5)
        entry = _wait_for_flow_control_reason(pool_model["subscription"], pool_model["llmis"], "ObjectiveReconciled")
        assert entry.get("objectiveName") == pool_model["objective"], f"objective renamed on update: {entry}"
        _assert_served(pool_model["api_key"], pool_model["model"])

    def test_manual_edit_is_reverted(self, pool_model):
        _patch(INFERENCE_OBJECTIVE, pool_model["objective"], MODEL_NAMESPACE, {"spec": {"priority": 999}})
        _wait_for_objective_priority(pool_model["objective"], -5)
        _assert_served(pool_model["api_key"], pool_model["model"])

    def test_gateway_authpolicy_sets_flow_control_headers(self, pool_model):
        policy = get_gateway_authpolicy()
        assert policy, "gateway AuthPolicy not found"
        headers = ((((policy.get("spec") or {}).get("defaults") or {}).get("rules") or {})
                   .get("response", {}).get("success", {}).get("headers") or {})
        for header, field in FLOW_CONTROL_HEADERS.items():
            assert header in headers, f"gateway AuthPolicy does not set {header}: {sorted(headers)}"
            expression = (headers[header].get("plain") or {}).get("expression") or ""
            assert f'auth.metadata["subscription-info"].{field}' in expression, (
                f"{header} is not set from subscription-info {field}: {expression!r}"
            )

    def test_inference_with_spoofed_headers_succeeds(self, pool_model):
        """Spoofed flow-control headers are overwritten at the gateway and do not break routing."""
        spoofed = {
            "X-LLM-D-Inference-Objective": "e2e-spoofed-objective",
            "X-Gateway-Inference-Objective": "e2e-spoofed-objective",
            "X-LLM-D-Inference-Fairness-Id": "e2e-spoofed-tenant",
            "X-Gateway-Inference-Fairness-Id": "e2e-spoofed-tenant",
        }
        _assert_served(pool_model["api_key"], pool_model["model"], extra_headers=spoofed)
        _assert_served(pool_model["api_key"], pool_model["model"])

    def test_clearing_priority_deletes_objective(self, pool_model):
        subscription = pool_model["subscription"]
        _patch("maassubscription", subscription, _ns(),
               [{"op": "remove", "path": "/spec/inferencePriority"}], patch_type="json")
        _wait_for_cr_absent(INFERENCE_OBJECTIVE, pool_model["objective"], namespace=MODEL_NAMESPACE,
                            timeout=FLOW_CONTROL_TIMEOUT)

        entry = _wait_for_flow_control_reason(subscription, pool_model["llmis"], "PriorityUnset")
        assert entry.get("ready") is True, f"flow-control entry not ready: {entry}"
        wait_until(lambda: _condition(subscription) is None, FLOW_CONTROL_TIMEOUT,
                   f"{CONDITION} still set after clearing inferencePriority", interval=2)
        assert not _owned_objectives(pool_model["uid"]), "objectives left after clearing inferencePriority"
        _assert_served(pool_model["api_key"], pool_model["model"])

    def test_subscription_delete_removes_objective(self, pool_model):
        subscription = pool_model["subscription"]
        _patch("maassubscription", subscription, _ns(), {"spec": {"inferencePriority": 7}})
        _wait_for_objective_priority(pool_model["objective"], 7)

        _delete_cr("maassubscription", subscription)
        _wait_for_cr_absent("maassubscription", subscription, timeout=FLOW_CONTROL_TIMEOUT)
        remaining = _owned_objectives(pool_model["uid"])
        assert not remaining, f"objectives left after deleting {subscription}: {[o['metadata']['name'] for o in remaining]}"

        # The key is still valid but its subscription is gone, so the gateway rejects it.
        _poll_status(pool_model["api_key"], (401, 403), model_name=pool_model["model"], inference_fn=_chat)


@pytest.mark.models
class TestInferencePriorityWithoutScheduler:
    """A model without an inference scheduler reports NotApplicable and gets no objective."""

    def test_model_without_pool_is_not_applicable(self):
        suffix = uuid.uuid4().hex[:8]
        policy = f"e2e-prio-noscheduler-access-{suffix}"
        subscription = f"e2e-prio-noscheduler-{suffix}"
        sa_name = f"e2e-prio-noscheduler-{suffix}"
        try:
            sa_token = _create_sa_token(sa_name, duration="10m")
            sa_user = _sa_to_user(sa_name)
            _create_test_auth_policy(policy, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription, MODEL_REF, users=[sa_user],
                                      token_limit=100, inference_priority=10)
            _wait_for_maas_auth_policy_phase(policy)
            entry = _wait_for_flow_control_reason(subscription, MODEL_REF, "NotApplicable")
            assert entry.get("ready") is True, f"flow-control entry not ready: {entry}"
            assert not entry.get("objectiveName"), f"objectiveName set without a pool: {entry}"

            cond = wait_until(lambda: _condition(subscription), FLOW_CONTROL_TIMEOUT,
                              f"{subscription} has no {CONDITION} condition", interval=2)
            assert cond.get("status") == "True", f"{CONDITION}: {cond}"

            uid = _get_cr("maassubscription", subscription)["metadata"]["uid"]
            assert not _owned_objectives(uid), "objective created for a model without a pool"

            api_key = _create_api_key(sa_token, name=f"e2e-prio-noscheduler-{suffix}", subscription=subscription)
            _poll_status(api_key, 200, timeout=120)
        finally:
            _delete_governance_and_wait(subscriptions=[(subscription, _ns())], auth_policies=[(policy, _ns())])
            _delete_sa(sa_name)
