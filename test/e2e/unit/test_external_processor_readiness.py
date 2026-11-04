"""Regressions for the separately rolled-out credential-bearing ExtProc hop."""

import copy
import hashlib
import json
from types import SimpleNamespace

import external_model_helpers as external
import pytest
import tenant_external_model_helpers as tenants
from test_tenant_bootstrap import deployment


def configured_processor():
    obj = deployment("quay.io/opendatahub/odh-praxis-extproc:test")
    config = "filter_chains:\n  - name: post-auth\n"
    obj["spec"]["template"]["metadata"] = {
        "annotations": {
            "ai-gateway-controller.opendatahub.io/extproc-config-sha256": hashlib.sha256(config.encode()).hexdigest()
        }
    }
    return obj, {"data": {"extproc.yaml": config}}


def test_previous_ready_configuration_cannot_serve_a_new_credential_set():
    obj, config = configured_processor()
    assert external.external_processor_config_ready(obj, config)
    updated = copy.deepcopy(config)
    updated["data"]["extproc.yaml"] += "# newly added credential\n"
    assert not external.external_processor_config_ready(obj, updated)


@pytest.mark.parametrize("status_field", ["observedGeneration", "updatedReplicas", "availableReplicas"])
def test_current_config_hash_does_not_make_an_unfinished_rollout_ready(status_field):
    obj, config = configured_processor()
    obj["status"][status_field] = 0
    assert not external.external_processor_config_ready(obj, config)


def test_shared_processor_does_not_establish_external_processor_readiness():
    obj = deployment("quay.io/opendatahub/odh-praxis-extproc:test")
    _, config = configured_processor()
    assert not external.external_processor_config_ready(obj, config)


@pytest.mark.parametrize("namespace,tenant,suffix", [("models-as-a-service", "", ""), ("ai-tenant-blue", "blue", "-blue")])
def test_external_rollout_wait_reads_the_tenant_local_processor(monkeypatch, namespace, tenant, suffix):
    obj, config = configured_processor()
    reads = []

    def get(kind, name, ns):
        reads.append((kind, name, ns))
        return obj if kind == "deployment" else config

    monkeypatch.setattr(external, "get_json_or_none", get)
    external.wait_for_external_processor_ready(namespace, tenant)
    assert reads == [
        ("deployment", f"payload-processing-external-model{suffix}", namespace),
        ("configmap", f"payload-processing-external-model-plugins{suffix}", namespace),
    ]


@pytest.mark.parametrize("ready", [True, False])
def test_tenant_setup_requires_external_rollout_before_minting_a_key(monkeypatch, ready):
    events = []
    monkeypatch.setattr(tenants, "make_tenant_model_accessible", lambda *args, **kwargs: events.append("governance"))
    monkeypatch.setattr(tenants.TenantExternalModelResources, "cleanup", lambda self: events.append("cleanup"))

    def wait(namespace, tenant):
        assert (namespace, tenant) == ("ai-tenant-blue", "blue")
        events.append("rollout")
        if not ready:
            raise AssertionError("processor has not installed current config")

    monkeypatch.setattr(tenants, "wait_for_external_processor_ready", wait)
    monkeypatch.setattr(tenants, "mint_tenant_api_key", lambda *args: events.append("key") or "test-key")
    case = {"tenant_ns": "ai-tenant-blue", "tenant_label_name": "blue", "gateway_name": "blue"}
    if ready:
        assert tenants.serve_external_model_in_tenant(case, "model", "models")["api_key"] == "test-key"
        assert events == ["governance", "rollout", "key"]
    else:
        with pytest.raises(AssertionError, match="current config"):
            tenants.serve_external_model_in_tenant(case, "model", "models")
        assert events == ["governance", "rollout", "cleanup"]


def test_failure_capture_keeps_runtime_status_and_redacts_credentials(monkeypatch, tmp_path):
    pod = {
        "metadata": {"namespace": "tenant", "name": "processor"},
        "status": {"phase": "Pending"},
        "spec": {"containers": [{"env": [{"name": "SECRET", "value": "must-not-be-exported"}]}]},
    }
    commands = []

    def run(command, **kwargs):
        commands.append(command)
        assert kwargs == {"timeout": 5, "retry_budget": 0}
        stdout = json.dumps({"items": [pod]}) if command[1] == "get" else "Authorization: Bearer sk-oai-do-not-export"
        return SimpleNamespace(stdout=stdout, stderr="", check_returncode=lambda: None)

    monkeypatch.setenv("ARTIFACTS_DIR", str(tmp_path))
    monkeypatch.setattr(external, "_run_oc", run)
    external.capture_external_processor_failure("tests/test_external_model_routing.py::test_route")
    evidence = next(tmp_path.glob("external-processor-failure-*.txt")).read_text()
    assert "Pending" in evidence
    assert "must-not-be-exported" not in evidence
    assert "sk-oai-do-not-export" not in evidence
    assert len(commands) == 2


def test_failure_capture_does_not_replace_an_unavailable_cluster_error(monkeypatch, tmp_path):
    monkeypatch.setenv("ARTIFACTS_DIR", str(tmp_path))

    def unavailable(*args, **kwargs):
        raise OSError("cluster unavailable")

    monkeypatch.setattr(external, "_run_oc", unavailable)
    external.capture_external_processor_failure("tests/test_external_model_routing.py::test_route")
    assert not list(tmp_path.iterdir())
