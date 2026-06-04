"""E2E coverage for AITenant create/delete bootstrap behavior."""

import json
import os
import shutil
import subprocess
import time
import uuid

import pytest

AITENANT_CRD = "aitenants.maas.opendatahub.io"
AITENANT_KIND = "aitenant"
TENANT_NAME = "default-tenant"
AITENANT_NAMESPACE = os.environ.get("AITENANT_NAMESPACE", "redhat-ai-gateway-infra")
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
AITENANT_GATEWAY_CLASS_NAME = os.environ.get("AITENANT_GATEWAY_CLASS_NAME", "openshift-default")
OC_TIMEOUT = int(os.environ.get("E2E_OC_TIMEOUT", "60"))


def _oc_bin():
    path = shutil.which("oc")
    if not path:
        raise RuntimeError("`oc` binary not found in PATH")
    return path


def _oc_run(args, *, input_text=None, timeout=None):
    return subprocess.run(
        [_oc_bin(), *args],
        input=input_text,
        capture_output=True,
        text=True,
        timeout=OC_TIMEOUT if timeout is None else timeout,
        check=False,
    )


def _oc_output_not_found(result):
    combined = (result.stderr or "") + (result.stdout or "")
    return "(NotFound)" in combined or "not found" in combined.lower()


def _apply(obj):
    result = _oc_run(["apply", "-f", "-"], input_text=json.dumps(obj))
    if result.returncode != 0:
        raise RuntimeError(f"`oc apply` failed: {result.stderr.strip() or result.stdout.strip()}")


def _delete(kind, name, namespace=None, *, timeout="60s"):
    args = ["delete", kind, name, "--ignore-not-found", f"--timeout={timeout}"]
    if namespace:
        args.extend(["-n", namespace])
    result = _oc_run(args, timeout=OC_TIMEOUT + 30)
    if result.returncode != 0:
        raise RuntimeError(f"`oc {' '.join(args)}` failed: {result.stderr.strip() or result.stdout.strip()}")


def _delete_best_effort(kind, name, namespace=None, *, timeout="60s"):
    try:
        _delete(kind, name, namespace, timeout=timeout)
    except Exception as exc:  # noqa: BLE001 - cleanup must not mask the test failure
        print(f"[cleanup] failed to delete {kind}/{name}: {exc}")


def _get_json_or_none(kind, name, namespace=None):
    args = ["get", kind, name, "-o", "json"]
    if namespace:
        args.extend(["-n", namespace])
    result = _oc_run(args)
    if result.returncode == 0:
        return json.loads(result.stdout)
    if _oc_output_not_found(result):
        return None
    raise RuntimeError(f"`oc {' '.join(args)}` failed: {result.stderr.strip() or result.stdout.strip()}")


def _wait_for_json(kind, name, namespace=None, *, predicate=None, timeout=180, interval=5):
    deadline = time.time() + timeout
    last_obj = None
    while time.time() < deadline:
        obj = _get_json_or_none(kind, name, namespace)
        if obj is not None:
            last_obj = obj
            if predicate is None or predicate(obj):
                return obj
        time.sleep(interval)
    raise AssertionError(f"{kind}/{name} in {namespace or '<cluster>'} did not satisfy condition. Last object: {last_obj}")


def _wait_for_not_found(kind, name, namespace=None, *, timeout=120, interval=5):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if _get_json_or_none(kind, name, namespace) is None:
            return
        time.sleep(interval)
    raise AssertionError(f"{kind}/{name} in {namespace or '<cluster>'} still exists")


def _aitenant_ready(obj):
    status = obj.get("status") or {}
    if status.get("phase") != "Active":
        return False
    return any(
        cond.get("type") == "Ready" and cond.get("status") == "True"
        for cond in status.get("conditions") or []
    )


@pytest.fixture(scope="module", autouse=True)
def require_aitenant_crd():
    result = _oc_run(["get", "crd", AITENANT_CRD])
    if result.returncode != 0:
        if _oc_output_not_found(result):
            pytest.skip(f"Missing CRD {AITENANT_CRD}; AITenant lifecycle test is not applicable")
        pytest.fail(f"`oc get crd {AITENANT_CRD}` failed: {result.stderr.strip() or result.stdout.strip()}")

@pytest.fixture(scope="module", autouse=True)
def require_aitenant_namespace():
    _wait_for_json("namespace", AITENANT_NAMESPACE, timeout=180, interval=5)


def _new_aitenant_case():
    suffix = uuid.uuid4().hex[:8]
    aitenant_name = f"e2e-ait-{suffix}"
    return {
        "tenant_ns": f"e2e-aitenant-{suffix}",
        "aitenant_name": aitenant_name,
        "gateway_name": aitenant_name,
        "tenant_admin_role": f"aitenant-{aitenant_name}-tenant-admin",
        "object_admin_role": f"aitenant-{aitenant_name}-object-admin",
    }


def _admin_subject():
    whoami = _oc_run(["whoami"])
    if whoami.returncode == 0 and whoami.stdout.strip():
        return whoami.stdout.strip()
    return "system:authenticated"


def _apply_gateway_fixture(case):
    _apply(
        {
            "apiVersion": "gateway.networking.k8s.io/v1",
            "kind": "Gateway",
            "metadata": {
                "name": case["gateway_name"],
                "namespace": GATEWAY_NAMESPACE,
                "labels": {
                    "e2e.maas.opendatahub.io/fixture": case["aitenant_name"],
                },
            },
            "spec": {
                "gatewayClassName": AITENANT_GATEWAY_CLASS_NAME,
                "listeners": [
                    {
                        "name": "http",
                        "port": 80,
                        "protocol": "HTTP",
                    }
                ],
            },
        }
    )


def _apply_aitenant(case):
    _apply(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "AITenant",
            "metadata": {
                "name": case["aitenant_name"],
                "namespace": AITENANT_NAMESPACE,
            },
            "spec": {
                "tenantNamespace": {
                    "name": case["tenant_ns"],
                },
                "rbac": {
                    "admins": [
                        {
                            "kind": "User",
                            "name": _admin_subject(),
                        }
                    ]
                },
            },
        }
    )


def _assert_aitenant_bootstrap_resources(case):
    aitenant = _wait_for_json(
        AITENANT_KIND,
        case["aitenant_name"],
        AITENANT_NAMESPACE,
        predicate=_aitenant_ready,
    )
    assert aitenant["status"]["tenantNamespace"] == case["tenant_ns"]
    assert aitenant["status"]["gatewayRef"] == {
        "namespace": GATEWAY_NAMESPACE,
        "name": case["gateway_name"],
    }

    gateway = _wait_for_json("gateway", case["gateway_name"], GATEWAY_NAMESPACE)
    assert gateway["metadata"]["labels"]["e2e.maas.opendatahub.io/fixture"] == case["aitenant_name"]
    assert gateway["metadata"]["labels"].get("ai-gateway.opendatahub.io/tenant") is None
    assert gateway["metadata"]["labels"].get("maas.opendatahub.io/managed-by-aitenant") is None
    assert (gateway["metadata"].get("annotations") or {}).get("maas.opendatahub.io/aitenant-name") is None
    assert (gateway["metadata"].get("annotations") or {}).get("maas.opendatahub.io/aitenant-namespace") is None
    assert gateway["spec"]["gatewayClassName"] == AITENANT_GATEWAY_CLASS_NAME

    namespace = _wait_for_json("namespace", case["tenant_ns"])
    assert namespace["metadata"]["labels"]["maas.opendatahub.io/managed-by-aitenant"] == "true"
    assert namespace["metadata"]["labels"]["ai-gateway.opendatahub.io/tenant"] == case["aitenant_name"]

    tenant = _wait_for_json("tenant", TENANT_NAME, case["tenant_ns"])
    assert tenant["spec"]["gatewayRef"] == {
        "namespace": GATEWAY_NAMESPACE,
        "name": case["gateway_name"],
    }
    assert tenant["metadata"]["labels"]["maas.opendatahub.io/managed-by-aitenant"] == "true"

    assert _get_json_or_none("role", case["tenant_admin_role"], case["tenant_ns"]) is not None
    assert _get_json_or_none("rolebinding", case["tenant_admin_role"], case["tenant_ns"]) is not None
    assert _get_json_or_none("role", case["object_admin_role"], AITENANT_NAMESPACE) is not None
    assert _get_json_or_none("rolebinding", case["object_admin_role"], AITENANT_NAMESPACE) is not None


def _delete_aitenant(case):
    _delete(AITENANT_KIND, case["aitenant_name"], AITENANT_NAMESPACE)
    _wait_for_not_found(AITENANT_KIND, case["aitenant_name"], AITENANT_NAMESPACE)


class TestAITenantLifecycle:
    # TODO: Add e2e coverage that Policies, Subscriptions, Models, and inference requests
    # work end-to-end in a newly created AITenant tenant namespace.
    def test_aitenant_create_bootstrap_resources(self):
        case = _new_aitenant_case()

        try:
            _apply_gateway_fixture(case)
            _apply_aitenant(case)
            _assert_aitenant_bootstrap_resources(case)
        finally:
            _delete_best_effort(AITENANT_KIND, case["aitenant_name"], AITENANT_NAMESPACE)
            _delete_best_effort("gateway", case["gateway_name"], GATEWAY_NAMESPACE)
            _delete_best_effort("namespace", case["tenant_ns"], timeout="90s")

    def test_aitenant_delete_cleans_up_bootstrap_resources(self):
        case = _new_aitenant_case()

        try:
            _apply_gateway_fixture(case)
            _apply_aitenant(case)
            _assert_aitenant_bootstrap_resources(case)

            _delete_aitenant(case)
            _wait_for_not_found("tenant", TENANT_NAME, case["tenant_ns"])
            _wait_for_not_found("role", case["tenant_admin_role"], case["tenant_ns"])
            _wait_for_not_found("rolebinding", case["tenant_admin_role"], case["tenant_ns"])
            _wait_for_not_found("role", case["object_admin_role"], AITENANT_NAMESPACE)
            _wait_for_not_found("rolebinding", case["object_admin_role"], AITENANT_NAMESPACE)

            namespace = _get_json_or_none("namespace", case["tenant_ns"])
            assert namespace is not None
            labels = namespace["metadata"].get("labels") or {}
            annotations = namespace["metadata"].get("annotations") or {}
            assert labels.get("maas.opendatahub.io/managed-by-aitenant") is None
            assert labels.get("ai-gateway.opendatahub.io/tenant") is None
            assert annotations.get("maas.opendatahub.io/aitenant-name") is None
            assert annotations.get("maas.opendatahub.io/aitenant-namespace") is None
            gateway = _get_json_or_none("gateway", case["gateway_name"], GATEWAY_NAMESPACE)
            assert gateway is not None
            assert gateway["metadata"]["labels"]["e2e.maas.opendatahub.io/fixture"] == case["aitenant_name"]
        finally:
            _delete_best_effort(AITENANT_KIND, case["aitenant_name"], AITENANT_NAMESPACE)
            _delete_best_effort("gateway", case["gateway_name"], GATEWAY_NAMESPACE)
            _delete_best_effort("namespace", case["tenant_ns"], timeout="90s")
