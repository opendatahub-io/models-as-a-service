"""Cluster-free regressions for tenant fixture readiness and backend switching."""

import copy
from types import SimpleNamespace

import multitenancy_helpers as helpers
import pytest
import test_external_model_routing as routing


def deployment(image="quay.io/opendatahub/odh-payload-processing:test"):
    return {
        "metadata": {"generation": 2},
        "spec": {"replicas": 1, "template": {"spec": {"containers": [{"image": image}]}}},
        "status": {"observedGeneration": 2, "replicas": 1, "updatedReplicas": 1, "availableReplicas": 1},
    }


@pytest.mark.parametrize(
    "mutation",
    [
        {"observedGeneration": 1},
        {"updatedReplicas": 0},
        {"availableReplicas": 0},
        {"replicas": 2},
    ],
)
def test_old_or_incomplete_rollout_is_not_ready(mutation):
    obj = deployment()
    obj["status"].update(mutation)
    assert not helpers.payload_processing_rollout_ready(obj, helpers.PAYLOAD_PROCESSING_TYPE_IPP)


@pytest.mark.parametrize("selected", [helpers.PAYLOAD_PROCESSING_TYPE_IPP, helpers.PAYLOAD_PROCESSING_TYPE_PRAXIS])
def test_shared_deployment_name_does_not_make_the_wrong_backend_ready(selected):
    ipp = deployment()
    praxis = deployment("quay.io/opendatahub/odh-praxis-extproc:test")
    assert helpers.payload_processing_rollout_ready(ipp, selected) == (selected == helpers.PAYLOAD_PROCESSING_TYPE_IPP)
    assert helpers.payload_processing_rollout_ready(praxis, selected) == (selected == helpers.PAYLOAD_PROCESSING_TYPE_PRAXIS)


def test_custom_praxis_image_is_identified_by_its_config_argument():
    obj = deployment("localhost/custom-processor:test")
    obj["spec"]["template"]["spec"]["containers"][0]["args"] = ["--config", "/etc/praxis/extproc.yaml"]
    assert not helpers.payload_processing_rollout_ready(obj, helpers.PAYLOAD_PROCESSING_TYPE_IPP)
    assert helpers.payload_processing_rollout_ready(obj, helpers.PAYLOAD_PROCESSING_TYPE_PRAXIS)


def test_a_terminating_backend_is_not_ready():
    obj = deployment()
    obj["metadata"]["deletionTimestamp"] = "2026-11-01T09:10:44Z"
    assert not helpers.payload_processing_rollout_ready(obj, helpers.PAYLOAD_PROCESSING_TYPE_IPP)


@pytest.mark.parametrize("stale_or_deleting", ["stale", "deleting"])
def test_aitenant_active_stamp_must_be_current_and_not_deleting(stale_or_deleting):
    obj = {
        "metadata": {"generation": 2},
        "status": {"phase": "Active", "conditions": [{"type": "Ready", "status": "True", "observedGeneration": 2}]},
    }
    assert helpers.aitenant_ready(obj)
    changed = copy.deepcopy(obj)
    if stale_or_deleting == "stale":
        changed["status"]["conditions"][0]["observedGeneration"] = 1
    else:
        changed["metadata"]["deletionTimestamp"] = "2026-11-01T09:10:44Z"
    assert not helpers.aitenant_ready(changed)


def test_backend_is_selected_before_waiting_for_tenant_readiness(monkeypatch):
    """The initial default backend must not be mistaken for the requested one."""
    case = {"tenant_label_name": "team-a", "tenant_ns": "ai-tenant-team-a"}
    observed = {"config_exists": False, "backend": None, "ready_checked": False}
    monkeypatch.setattr(helpers, "apply_aitenant", lambda *args, **kwargs: None)

    def select_backend(namespace, payload_type):
        assert observed["config_exists"]
        assert namespace == case["tenant_ns"]
        observed["backend"] = payload_type

    def wait(kind, *args, **kwargs):
        if kind == "maastenantconfig":
            observed["config_exists"] = True
        elif kind == helpers.AITENANT_KIND:
            assert observed["backend"] == helpers.PAYLOAD_PROCESSING_TYPE_IPP
            observed["ready_checked"] = True
        else:
            pytest.fail(f"unexpected wait: {kind}")
        return {}

    monkeypatch.setattr(helpers, "ensure_payload_processing_type_on_tenant_config", select_backend)
    monkeypatch.setattr(helpers, "wait_for_json", wait)
    helpers.bootstrap_aitenant_tenant(case, use_default_gateway=True, payload_processing_type=helpers.PAYLOAD_PROCESSING_TYPE_IPP)
    assert observed["ready_checked"]


@pytest.mark.parametrize("status", [200, 503, 429])
def test_routing_fixture_requires_a_successful_first_inference(monkeypatch, status):
    monkeypatch.setattr(routing, "wait_for_external_processor_ready", lambda *args: None)
    monkeypatch.setattr(routing, "make_tenant_model_accessible", lambda *args, **kwargs: None)
    monkeypatch.setattr(routing, "_get_cluster_token", lambda: "test-token")
    monkeypatch.setattr(routing, "_create_api_key", lambda *args: "test-key")
    probes = []

    def infer(*args, **kwargs):
        probes.append(kwargs)
        return SimpleNamespace(status_code=status, text="model-client")

    monkeypatch.setattr(routing, "_infer", infer)
    env = {"resources": SimpleNamespace(track=lambda *args: None), "subscription_ns": "tenant", "model_ns": "models"}
    model = {"name": "model", "client_tag": "model-client"}
    if status == 200:
        assert routing._serve(env, model) == "test-key"
    else:
        with pytest.raises(AssertionError, match="expected 200"):
            routing._serve(env, model)
    assert probes == [{"readiness_timeout": routing.TENANT_EXTERNAL_MODEL_READY_TIMEOUT}]
