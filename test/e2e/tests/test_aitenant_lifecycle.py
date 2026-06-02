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
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
GATEWAY_NAME = os.environ.get("GATEWAY_NAME", "maas-default-gateway")
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


def _oc_json(args):
    result = _oc_run(args)
    if result.returncode != 0:
        raise RuntimeError(
            f"`oc {' '.join(args)}` failed: {((result.stderr or '') + (result.stdout or '')).strip()}"
        )
    return json.loads(result.stdout)


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


def _create_namespace(name):
    result = _oc_run(["create", "namespace", name])
    if result.returncode != 0 and "already exists" not in (result.stderr or ""):
        raise RuntimeError(f"create namespace {name}: {result.stderr.strip() or result.stdout.strip()}")


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


def _metadata_values(obj, keys):
    meta = obj.get("metadata") or {}
    labels = meta.get("labels") or {}
    annotations = meta.get("annotations") or {}
    return {
        "labels": {key: labels.get(key) for key in keys},
        "annotations": {key: annotations.get(key) for key in keys},
    }


@pytest.fixture(scope="module", autouse=True)
def require_aitenant_crd():
    result = _oc_run(["get", "crd", AITENANT_CRD])
    if result.returncode != 0:
        if _oc_output_not_found(result):
            pytest.skip(f"Missing CRD {AITENANT_CRD}; AITenant lifecycle test is not applicable")
        pytest.fail(f"`oc get crd {AITENANT_CRD}` failed: {result.stderr.strip() or result.stdout.strip()}")


@pytest.fixture(scope="module", autouse=True)
def require_existing_gateway():
    result = _oc_run(["get", "gateway", GATEWAY_NAME, "-n", GATEWAY_NAMESPACE])
    if result.returncode != 0:
        if _oc_output_not_found(result):
            pytest.skip(
                f"Gateway {GATEWAY_NAMESPACE}/{GATEWAY_NAME} does not exist; "
                "AITenant requires a pre-existing Gateway"
            )
        pytest.fail(
            f"`oc get gateway {GATEWAY_NAME} -n {GATEWAY_NAMESPACE}` failed: "
            f"{result.stderr.strip() or result.stdout.strip()}"
        )


class TestAITenantLifecycle:
    def test_aitenant_create_delete_bootstrap_resources(self):
        suffix = uuid.uuid4().hex[:8]
        infra_ns = f"e2e-aitenant-infra-{suffix}"
        tenant_ns = f"e2e-aitenant-{suffix}"
        aitenant_name = f"e2e-ait-{suffix}"
        tenant_admin_role = f"aitenant-{aitenant_name}-tenant-admin"
        object_admin_role = f"aitenant-{aitenant_name}-object-admin"

        whoami = _oc_run(["whoami"])
        admin_subject = whoami.stdout.strip() if whoami.returncode == 0 and whoami.stdout.strip() else "system:authenticated"

        watched_metadata_keys = [
            "ai-gateway.opendatahub.io/tenant",
            "maas.opendatahub.io/aitenant-name",
            "maas.opendatahub.io/aitenant-namespace",
        ]
        gateway_before = _metadata_values(
            _oc_json(["get", "gateway", GATEWAY_NAME, "-n", GATEWAY_NAMESPACE, "-o", "json"]),
            watched_metadata_keys,
        )

        try:
            _create_namespace(infra_ns)

            _apply(
                {
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "AITenant",
                    "metadata": {
                        "name": aitenant_name,
                        "namespace": infra_ns,
                    },
                    "spec": {
                        "tenantNamespace": {
                            "name": tenant_ns,
                        },
                        "gateway": {
                            "name": GATEWAY_NAME,
                        },
                        "rbac": {
                            "admins": [
                                {
                                    "kind": "User",
                                    "name": admin_subject,
                                }
                            ]
                        },
                    },
                }
            )

            aitenant = _wait_for_json(AITENANT_KIND, aitenant_name, infra_ns, predicate=_aitenant_ready)
            assert aitenant["status"]["tenantNamespace"] == tenant_ns
            assert aitenant["status"]["gatewayRef"] == {
                "namespace": GATEWAY_NAMESPACE,
                "name": GATEWAY_NAME,
            }

            namespace = _wait_for_json("namespace", tenant_ns)
            assert namespace["metadata"]["labels"]["maas.opendatahub.io/managed-by-aitenant"] == "true"
            assert namespace["metadata"]["labels"]["ai-gateway.opendatahub.io/tenant"] == aitenant_name

            tenant = _wait_for_json("tenant", TENANT_NAME, tenant_ns)
            assert tenant["spec"]["gatewayRef"] == {
                "namespace": GATEWAY_NAMESPACE,
                "name": GATEWAY_NAME,
            }
            assert tenant["metadata"]["labels"]["maas.opendatahub.io/managed-by-aitenant"] == "true"

            assert _get_json_or_none("role", tenant_admin_role, tenant_ns) is not None
            assert _get_json_or_none("rolebinding", tenant_admin_role, tenant_ns) is not None
            assert _get_json_or_none("role", object_admin_role, infra_ns) is not None
            assert _get_json_or_none("rolebinding", object_admin_role, infra_ns) is not None

            gateway_after_create = _metadata_values(
                _oc_json(["get", "gateway", GATEWAY_NAME, "-n", GATEWAY_NAMESPACE, "-o", "json"]),
                watched_metadata_keys,
            )
            assert gateway_after_create == gateway_before, "AITenant must not mutate Gateway metadata"

            _delete(AITENANT_KIND, aitenant_name, infra_ns)
            _wait_for_not_found(AITENANT_KIND, aitenant_name, infra_ns)
            _wait_for_not_found("tenant", TENANT_NAME, tenant_ns)
            _wait_for_not_found("role", tenant_admin_role, tenant_ns)
            _wait_for_not_found("rolebinding", tenant_admin_role, tenant_ns)
            _wait_for_not_found("role", object_admin_role, infra_ns)
            _wait_for_not_found("rolebinding", object_admin_role, infra_ns)

            assert _get_json_or_none("namespace", tenant_ns) is not None
            assert _get_json_or_none("gateway", GATEWAY_NAME, GATEWAY_NAMESPACE) is not None

            gateway_after_delete = _metadata_values(
                _oc_json(["get", "gateway", GATEWAY_NAME, "-n", GATEWAY_NAMESPACE, "-o", "json"]),
                watched_metadata_keys,
            )
            assert gateway_after_delete == gateway_before, "AITenant deletion must not mutate Gateway metadata"
        finally:
            _delete_best_effort(AITENANT_KIND, aitenant_name, infra_ns)
            _delete_best_effort("namespace", tenant_ns, timeout="90s")
            _delete_best_effort("namespace", infra_ns, timeout="90s")
