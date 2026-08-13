"""
E2E tests for MaaSModelRef model-identity-conflict detection.

Body-based routing (BBR) selects a backend using the model identity carried in
the request (the "model" field / x-gateway-model-name header) alone — it has
no notion of which MaaSModelRef or subscription the caller intended. When two
MaaSModelRefs in the same namespace resolve to the same model identity (most
commonly because two LLMInferenceServices declare the same spec.model.name),
requests can be routed to the wrong backend and/or evaluated against the wrong
subscription (observed in production as spurious "subscription does not
include model" denials for facebook/opt-125m served by two distinct
LLMInferenceServices in the "llm" namespace).

The controller surfaces this as a `ModelIdentityUnique` status condition
(informational only — it does not affect Phase) plus a Warning/Normal event on
transition. These tests create an intentional collision, assert detection and
event emission, then remove it and assert recovery.
"""

import json
import uuid

from multitenancy_helpers import DEFAULT_GATEWAY_NAME, _oc_run, wait_for_status_condition
from test_helper import (
    GATEWAY_NAMESPACE,
    MODEL_NAMESPACE,
    _create_llmis,
    _create_maas_model_ref,
    _delete_cr,
)

CONDITION = "ModelIdentityUnique"


def _events_for(name: str, namespace: str, reason: str) -> list[dict]:
    """Return Kubernetes Events for `reason` involving object `name`, or [] if none/unavailable."""
    result = _oc_run(
        [
            "get",
            "events",
            "-n",
            namespace,
            "--field-selector",
            f"involvedObject.name={name},reason={reason}",
            "-o",
            "json",
        ]
    )
    if result.returncode != 0:
        return []
    return json.loads(result.stdout).get("items") or []


def _condition(obj: dict, condition_type: str) -> dict:
    conditions = ((obj.get("status") or {}).get("conditions")) or []
    for condition in conditions:
        if condition.get("type") == condition_type:
            return condition
    raise AssertionError(f"condition {condition_type!r} not found in {conditions!r}")


class TestModelIdentityConflictDetection:
    """Two MaaSModelRefs sharing a resolved model identity must be flagged, not silently misrouted."""

    def test_colliding_model_names_flagged_then_resolved(self):
        suffix = uuid.uuid4().hex[:8]
        shared_model_name = f"test/e2e-identity-conflict-{suffix}"
        llmis_a = f"e2e-conflict-a-{suffix}"
        llmis_b = f"e2e-conflict-b-{suffix}"
        ref_a, ref_b = llmis_a, llmis_b

        try:
            # A single model with no siblings must be reported unique.
            _create_llmis(llmis_a, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME, GATEWAY_NAMESPACE, model_name=shared_model_name)
            _create_maas_model_ref(ref_a, MODEL_NAMESPACE, llmis_a)

            obj_a = wait_for_status_condition(
                "maasmodelref", ref_a, MODEL_NAMESPACE,
                condition_type=CONDITION, expected_status="True", timeout=120,
            )
            assert (obj_a.get("status") or {}).get("resolvedModelAlias"), (
                f"expected {ref_a} to have a resolvedModelAlias once the LLMIS exists"
            )

            # A second MaaSModelRef whose LLMIS declares the SAME spec.model.name
            # introduces a collision — this mirrors the reported production bug.
            _create_llmis(llmis_b, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME, GATEWAY_NAMESPACE, model_name=shared_model_name)
            _create_maas_model_ref(ref_b, MODEL_NAMESPACE, llmis_b)

            obj_a = wait_for_status_condition(
                "maasmodelref", ref_a, MODEL_NAMESPACE,
                condition_type=CONDITION, expected_status="False", timeout=120,
            )
            obj_b = wait_for_status_condition(
                "maasmodelref", ref_b, MODEL_NAMESPACE,
                condition_type=CONDITION, expected_status="False", timeout=120,
            )

            cond_a = _condition(obj_a, CONDITION)
            cond_b = _condition(obj_b, CONDITION)
            assert ref_b in cond_a["message"], f"expected {ref_a}'s condition to name {ref_b}: {cond_a['message']!r}"
            assert ref_a in cond_b["message"], f"expected {ref_b}'s condition to name {ref_a}: {cond_b['message']!r}"

            events_a = _events_for(ref_a, MODEL_NAMESPACE, "ModelNameConflict")
            events_b = _events_for(ref_b, MODEL_NAMESPACE, "ModelNameConflict")
            assert events_a or events_b, (
                "expected a Warning ModelNameConflict event on at least one of the "
                f"colliding MaaSModelRefs ({ref_a}, {ref_b})"
            )

            # Removing one sibling must clear the conflict on the survivor (exercises
            # the sibling-watch that re-reconciles on Delete) and emit a Normal event.
            _delete_cr("maasmodelref", ref_b, MODEL_NAMESPACE)
            _delete_cr("llminferenceservice", llmis_b, MODEL_NAMESPACE)

            wait_for_status_condition(
                "maasmodelref", ref_a, MODEL_NAMESPACE,
                condition_type=CONDITION, expected_status="True", timeout=120,
            )
            resolved_events = _events_for(ref_a, MODEL_NAMESPACE, "ModelNameConflictResolved")
            assert resolved_events, f"expected a Normal ModelNameConflictResolved event on {ref_a}"
        finally:
            _delete_cr("maasmodelref", ref_a, MODEL_NAMESPACE)
            _delete_cr("maasmodelref", ref_b, MODEL_NAMESPACE)
            _delete_cr("llminferenceservice", llmis_a, MODEL_NAMESPACE)
            _delete_cr("llminferenceservice", llmis_b, MODEL_NAMESPACE)
