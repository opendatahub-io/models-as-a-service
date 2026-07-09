"""E2E coverage for AITenant create/delete bootstrap behavior."""

import json
import os
import shutil
import subprocess
import time
import uuid

import pytest

from test_helper import DEPLOYMENT_NAMESPACE, MAAS_API_DEPLOYMENT_NAMESPACE

AITENANT_CRD = "aitenants.maas.opendatahub.io"
AITENANT_KIND = "aitenant"
CONFIG_CRD = "configs.maas.opendatahub.io"
CONFIG_NAME = "default"
DEFAULT_AITENANT_BOOTSTRAPPED_ANNOTATION = "maas.opendatahub.io/default-aitenant-bootstrapped"
ANNOTATION_AITENANT_NAME = "maas.opendatahub.io/aitenant-name"
ANNOTATION_AITENANT_NAMESPACE = "maas.opendatahub.io/aitenant-namespace"
TENANT_NAME = "default-tenant"
DEFAULT_AITENANT_NAME = "models-as-a-service"
AITENANT_NAMESPACE = os.environ.get("AITENANT_NAMESPACE", "ai-tenants")
MAAS_SUBSCRIPTION_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
GATEWAY_NAME = os.environ.get("GATEWAY_NAME", "maas-default-gateway")
INFRA_NAMESPACE = MAAS_API_DEPLOYMENT_NAMESPACE
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
        "tenant_ns": f"ai-tenant-e2e-ait-{suffix}",
        "aitenant_name": aitenant_name,
        "gateway_name": aitenant_name,
        "tenant_admin_role": f"aitenant-{aitenant_name}-tenant-admin",
        "object_admin_role": f"aitenant-{aitenant_name}-object-admin",
    }


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
            "spec": {},
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
    labels = tenant["metadata"].get("labels") or {}
    annotations = tenant["metadata"].get("annotations") or {}
    assert labels["maas.opendatahub.io/managed-by-aitenant"] == "true"
    assert labels["ai-gateway.opendatahub.io/tenant"] == case["aitenant_name"]
    assert labels["maas.opendatahub.io/tenant-name"] == case["aitenant_name"]
    assert labels["maas.opendatahub.io/tenant-namespace"] == case["tenant_ns"]
    assert annotations[ANNOTATION_AITENANT_NAME] == case["aitenant_name"]
    assert annotations[ANNOTATION_AITENANT_NAMESPACE] == AITENANT_NAMESPACE
    # AITenant-managed bridge Tenants keep legacy/default spec values. The
    # tenant Gateway under test is reported in AITenant.status.gatewayRef.
    assert tenant["spec"]["gatewayRef"] == {
        "namespace": GATEWAY_NAMESPACE,
        "name": GATEWAY_NAME,
    }
    assert tenant["spec"]["gatewayRef"]["name"] != case["gateway_name"]

    assert _get_json_or_none("role", case["tenant_admin_role"], case["tenant_ns"]) is not None
    assert _get_json_or_none("rolebinding", case["tenant_admin_role"], case["tenant_ns"]) is None
    assert _get_json_or_none("role", case["object_admin_role"], AITENANT_NAMESPACE) is not None
    assert _get_json_or_none("rolebinding", case["object_admin_role"], AITENANT_NAMESPACE) is None


def _delete_aitenant(case):
    _delete(AITENANT_KIND, case["aitenant_name"], AITENANT_NAMESPACE)
    _wait_for_not_found(AITENANT_KIND, case["aitenant_name"], AITENANT_NAMESPACE)


class TestAITenantLifecycle:
    # TODO: Add e2e coverage that Policies, Subscriptions, Models, and inference requests
    # work end-to-end in a newly created AITenant tenant namespace.
    def test_default_aitenant_bootstraps_legacy_tenant_without_gateway_mutation(self):
        aitenant = _wait_for_json(
            AITENANT_KIND,
            DEFAULT_AITENANT_NAME,
            AITENANT_NAMESPACE,
            predicate=_aitenant_ready,
            timeout=240,
        )
        assert aitenant["status"]["tenantNamespace"] == MAAS_SUBSCRIPTION_NAMESPACE
        assert aitenant["status"]["gatewayRef"] == {
            "namespace": GATEWAY_NAMESPACE,
            "name": GATEWAY_NAME,
        }
        _wait_for_json(
            CONFIG_CRD,
            CONFIG_NAME,
            predicate=lambda obj: (
                (obj.get("metadata", {}).get("annotations") or {}).get(DEFAULT_AITENANT_BOOTSTRAPPED_ANNOTATION)
                == "true"
            ),
            timeout=180,
        )

        gateway = _wait_for_json("gateway", GATEWAY_NAME, GATEWAY_NAMESPACE, timeout=180)
        gateway_labels = gateway["metadata"].get("labels") or {}
        gateway_annotations = gateway["metadata"].get("annotations") or {}
        assert gateway_labels.get("ai-gateway.opendatahub.io/tenant") is None
        assert gateway_labels.get("maas.opendatahub.io/managed-by-aitenant") is None
        assert gateway_annotations.get("maas.opendatahub.io/aitenant-name") is None
        assert gateway_annotations.get("maas.opendatahub.io/aitenant-namespace") is None

        namespace = _wait_for_json("namespace", MAAS_SUBSCRIPTION_NAMESPACE, timeout=180)
        namespace_labels = namespace["metadata"].get("labels") or {}
        assert namespace_labels["maas.opendatahub.io/managed-by-aitenant"] == "true"
        assert namespace_labels["ai-gateway.opendatahub.io/tenant"] == DEFAULT_AITENANT_NAME
        assert namespace_labels["maas.opendatahub.io/tenant-name"] == DEFAULT_AITENANT_NAME
        assert namespace_labels["maas.opendatahub.io/tenant-namespace"] == MAAS_SUBSCRIPTION_NAMESPACE

        tenant = _wait_for_json("tenant", TENANT_NAME, MAAS_SUBSCRIPTION_NAMESPACE, timeout=180)
        assert tenant["metadata"]["labels"]["maas.opendatahub.io/managed-by-aitenant"] == "true"
        assert tenant["metadata"]["labels"]["ai-gateway.opendatahub.io/tenant"] == DEFAULT_AITENANT_NAME
        assert tenant["metadata"]["labels"]["maas.opendatahub.io/tenant-name"] == DEFAULT_AITENANT_NAME
        tenant_annotations = tenant["metadata"].get("annotations") or {}
        assert tenant_annotations[ANNOTATION_AITENANT_NAME] == DEFAULT_AITENANT_NAME
        assert tenant_annotations[ANNOTATION_AITENANT_NAMESPACE] == AITENANT_NAMESPACE
        assert tenant["spec"]["gatewayRef"] == {
            "namespace": GATEWAY_NAMESPACE,
            "name": GATEWAY_NAME,
        }

        assert _wait_for_json("deployment", "maas-api", INFRA_NAMESPACE, timeout=180) is not None
        assert _wait_for_json("service", "maas-api", INFRA_NAMESPACE, timeout=180) is not None
        assert _wait_for_json("httproute", "maas-api-route", INFRA_NAMESPACE, timeout=180) is not None
        assert _get_json_or_none("deployment", "maas-api-models-as-a-service", INFRA_NAMESPACE) is None
        assert _get_json_or_none("service", "maas-api-models-as-a-service", DEPLOYMENT_NAMESPACE) is None
        assert _get_json_or_none("httproute", "maas-api-route-models-as-a-service", DEPLOYMENT_NAMESPACE) is None

        if os.environ.get("EXTERNAL_OIDC") == "true" and os.environ.get("OIDC_ISSUER_URL"):
            expected_issuer = os.environ["OIDC_ISSUER_URL"]
            expected_client_id = os.environ.get("OIDC_CLIENT_ID")

            def aitenant_oidc_converged(obj):
                oidc = obj.get("spec", {}).get("oidc") or {}
                return oidc.get("issuerUrl") == expected_issuer and (
                    not expected_client_id or oidc.get("clientId") == expected_client_id
                )

            _wait_for_json(
                AITENANT_KIND,
                DEFAULT_AITENANT_NAME,
                AITENANT_NAMESPACE,
                predicate=aitenant_oidc_converged,
                timeout=180,
            )

    def test_aitenant_rejected_outside_ai_tenants_namespace(self):
        suffix = uuid.uuid4().hex[:8]
        wrong_ns = f"e2e-ait-wrong-{suffix}"
        tenant_ns = f"e2e-ait-wrong-tenant-{suffix}"
        aitenant_name = f"e2e-ait-wrong-{suffix}"

        try:
            result = _oc_run(["create", "namespace", wrong_ns])
            assert result.returncode == 0, f"Failed to create namespace: {result.stderr.strip() or result.stdout.strip()}"

            result = _oc_run(
                ["apply", "-f", "-"],
                input_text=json.dumps(
                    {
                        "apiVersion": "maas.opendatahub.io/v1alpha1",
                        "kind": "AITenant",
                        "metadata": {
                            "name": aitenant_name,
                            "namespace": wrong_ns,
                        },
                        "spec": {
                            "tenantNamespace": {
                                "name": tenant_ns,
                            },
                        },
                    }
                ),
            )

            assert result.returncode != 0, "Expected webhook to reject AITenant outside the configured infra namespace"
            combined = f"{result.stderr or ''}\n{result.stdout or ''}"
            assert "admission webhook" in combined.lower(), \
                f"Expected webhook rejection, got: {combined}"
            assert "configured AITenant infrastructure namespace" in combined, \
                f"Expected namespace placement error, got: {combined}"
            assert AITENANT_NAMESPACE in combined, \
                f"Expected configured infra namespace in error, got: {combined}"

            assert _get_json_or_none(AITENANT_KIND, aitenant_name, wrong_ns) is None

        finally:
            _delete_best_effort(AITENANT_KIND, aitenant_name, wrong_ns)
            _delete_best_effort("namespace", wrong_ns, timeout="90s")

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
            _wait_for_not_found("role", case["object_admin_role"], AITENANT_NAMESPACE)

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

    def test_aitenant_derives_non_default_tenant_namespace(self):
        """RHOAIENG-66836: non-default AITenant must not use models-as-a-service tenant namespace."""
        suffix = uuid.uuid4().hex[:8]
        aitenant_name = f"e2e-derive-{suffix}"
        reserved_ns = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
        expected_ns = f"ai-tenant-{aitenant_name}"
        gateway_name = aitenant_name

        try:
            _apply_gateway_fixture({"gateway_name": gateway_name, "aitenant_name": aitenant_name})
            _apply(
                {
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "AITenant",
                    "metadata": {"name": aitenant_name, "namespace": AITENANT_NAMESPACE},
                    "spec": {},
                }
            )
            aitenant = _wait_for_json(
                AITENANT_KIND,
                aitenant_name,
                AITENANT_NAMESPACE,
                predicate=_aitenant_ready,
                timeout=120,
            )
            assert aitenant["status"]["tenantNamespace"] == expected_ns
            assert aitenant["status"]["tenantNamespace"] != reserved_ns
            assert _get_json_or_none("tenant", TENANT_NAME, expected_ns) is not None
        finally:
            _delete_best_effort(AITENANT_KIND, aitenant_name, AITENANT_NAMESPACE)
            _delete_best_effort("gateway", gateway_name, GATEWAY_NAMESPACE)
            _delete_best_effort("namespace", expected_ns, timeout="90s")
