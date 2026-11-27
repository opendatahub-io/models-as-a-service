#!/usr/bin/env python3
"""Resolve the Konflux PR targets and enforce the repository build contracts.

This file intentionally owns the checks that are specific to models-as-a-service.
It does not run tests, lint, or generated-code targets that the repository's
existing workflows already run; static mode asserts that those existing checks
are still wired instead of duplicating them.
"""

from __future__ import annotations

import argparse
import glob
import json
import os
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any, Iterable

try:
    import yaml
except ImportError as exc:  # pragma: no cover - exercised by the workflow setup
    raise SystemExit("PyYAML is required: pip install pyyaml==6.0.3") from exc


ROOT = Path(__file__).resolve().parent.parent
TEKTON_DIR = ROOT / ".tekton"
GO_TOOLSET_DIGEST = "sha256:1a755651ffe1a438f137418d183f18ebad527ec929206c17804f93490a97869e"

EXPECTED_TARGETS: dict[str, dict[str, str]] = {
    "maas-api": {
        "tekton": "odh-maas-api-pull-request.yaml",
        "dockerfile": "maas-api/Dockerfile",
        "context": "maas-api",
        "output": "quay.io/opendatahub/maas-api:odh-pr",
        "build_target": "./cmd/",
        "binary": "maas-api",
    },
    "maas-controller": {
        "tekton": "odh-maas-controller-pull-request.yaml",
        "dockerfile": "maas-controller/Dockerfile",
        "context": ".",
        "output": "quay.io/opendatahub/maas-controller:odh-pr",
        "build_target": "./cmd/manager",
        "binary": "manager",
    },
    "maas-discovery": {
        "tekton": "odh-maas-discovery-pull-request.yaml",
        "dockerfile": "maas-discovery/Dockerfile",
        "context": "maas-discovery",
        "output": "quay.io/opendatahub/odh-maas-discovery:odh-pr",
        "build_target": "./cmd/discovery",
        "binary": "discovery",
    },
}

CONTROLLER_TREES = {
    "maas-api/deploy": "/maas-api/deploy",
    "deployment/base/maas-api": "/deployment/base/maas-api",
    "deployment/base/maas-controller/policies": "/deployment/base/maas-controller/policies",
    "deployment/base/payload-processing": "/deployment/base/payload-processing",
    "deployment/base/maas-discovery": "/deployment/base/maas-discovery",
    "deployment/components": "/deployment/components",
}

CONTROLLER_REQUIRED_SUBPATHS = (
    "maas-api/deploy/overlays/odh",
    "maas-api/deploy/overlays/xks",
    "deployment/base/maas-api/default",
    "deployment/base/maas-api/core",
    "deployment/base/maas-controller/policies",
    "deployment/base/payload-processing/default",
    "deployment/base/maas-discovery/default",
    "deployment/components/observability/observability/dashboards",
    "deployment/components/observability/usage-logs",
    "deployment/components/observability/usage-logs/dsci-managed/envoy-otel-access-log.yaml",
)

REQUIRED_KUSTOMIZATION_ROOTS = (
    "deployment/base/maas-controller/default",
    "deployment/base/maas-controller/crd",
    "deployment/base/maas-controller/rbac",
    "deployment/base/maas-controller/rbac/ocp",
    "deployment/base/maas-controller/manager",
    "deployment/base/maas-controller/monitoring",
    "deployment/base/maas-controller/webhook",
    "deployment/base/maas-controller/policies",
    "deployment/base/maas-controller/overlays/xks",
    "deployment/base/maas-api/default",
    "deployment/base/maas-api/overlays/tls",
    "deployment/base/maas-api/core",
    "deployment/base/maas-api/rbac",
    "deployment/base/maas-api/networking",
    "deployment/base/maas-api/policies",
    "deployment/base/maas-api/monitoring",
    "maas-api/deploy/overlays/odh",
    "maas-api/deploy/overlays/xks",
    "maas-api/deploy/overlays/dev",
    "deployment/base/maas-discovery/default",
    "deployment/base/maas-discovery/core",
    "deployment/base/maas-discovery/rbac",
    "deployment/base/payload-processing/default",
    "deployment/base/payload-processing/after",
    "deployment/base/payload-processing/manager",
    "deployment/base/payload-processing/instance",
    "deployment/base/payload-processing/networking",
    "deployment/base/payload-processing/pre-processing",
    "deployment/base/payload-processing/rbac",
    "deployment/base/networking",
    "deployment/base/networking/maas",
    "deployment/base/networking/odh",
    "deployment/base/observability",
    "deployment/overlays/openshift",
    "deployment/overlays/xks",
    "deployment/components/observability/observability/dashboards",
    "deployment/components/observability/usage-logs",
    "deployment/components/observability/usage-logs/dsci-managed",
)

EXTERNAL_SERVICE_ACCOUNTS = {
    ("opendatahub", "usage-logs-collector"),
}

PRODUCTION_BUNDLES = (
    "deployment/base/maas-controller/default",
    "deployment/base/maas-controller/overlays/xks",
    "deployment/overlays/openshift",
    "deployment/overlays/xks",
    "maas-api/deploy/overlays/odh",
    "maas-api/deploy/overlays/xks",
    "deployment/base/maas-discovery/default",
    "deployment/base/payload-processing/default",
)

# Objects rendered from these trees are server-side applied by maas-controller.
APPLIED_TREES = (
    "maas-api/deploy/overlays/odh",
    "maas-api/deploy/overlays/xks",
    "deployment/components/observability/observability/dashboards",
    "deployment/components/observability/usage-logs/dsci-managed",
    "deployment/base/maas-discovery/default",
    "deployment/base/payload-processing/default",
)

REQUIRED_APPLIED_ROLES = {
    "maas-api",
    "maas-api-supplemental",
    "payload-processing-reader",
}

# Evaluate escalation against resources that are applied together for one
# platform. Flattening every overlay into one permission union can hide a gap in
# one environment behind a rule that only exists in the other.
ESCALATION_BUNDLES = {
    "openshift": (
        "deployment/base/maas-controller/default",
        "maas-api/deploy/overlays/odh",
        "deployment/components/observability/observability/dashboards",
        "deployment/components/observability/usage-logs/dsci-managed",
    ),
    "xks": (
        "deployment/base/maas-controller/overlays/xks",
        "maas-api/deploy/overlays/xks",
        "deployment/components/observability/observability/dashboards",
        "deployment/components/observability/usage-logs/dsci-managed",
    ),
}

DISCOVERY_APPLIED_ROOT = "deployment/base/maas-discovery/default"

BUILTIN_KUBERNETES_API_GROUPS = {
    "",
    "admissionregistration.k8s.io",
    "apiextensions.k8s.io",
    "apiregistration.k8s.io",
    "apps",
    "authentication.k8s.io",
    "authorization.k8s.io",
    "autoscaling",
    "batch",
    "certificates.k8s.io",
    "coordination.k8s.io",
    "discovery.k8s.io",
    "events.k8s.io",
    "extensions",
    "flowcontrol.apiserver.k8s.io",
    "networking.k8s.io",
    "node.k8s.io",
    "policy",
    "rbac.authorization.k8s.io",
    "scheduling.k8s.io",
    "storage.k8s.io",
}

# These bases are fragments consumed by deployable overlays. They intentionally
# omit spec.selector. Permit only that exact schema error; every other schema
# error still fails. This keeps all entry points schema-checked without
# weakening validation globally or mutating the rendered manifests.
PARTIAL_SCHEMA_FRAGMENTS = {
    "deployment/base/maas-api/core": {
        (
            "Deployment",
            "maas-api",
            "apps/v1",
            (("/spec", "missing property 'selector'"),),
        ),
    },
    "deployment/base/maas-discovery/core": {
        (
            "Deployment",
            "maas-discovery",
            "apps/v1",
            (("/spec", "missing property 'selector'"),),
        ),
    },
}

ERRORS: list[str] = []
WARNINGS: list[str] = []


def fail(message: str) -> None:
    ERRORS.append(message)
    print(f"FAIL: {message}")


def warn(message: str) -> None:
    WARNINGS.append(message)
    print(f"WARN: {message}")


def ok(message: str) -> None:
    print(f"OK: {message}")


def resolve_inside_root(path: Path) -> Path | None:
    resolved = path.resolve()
    try:
        resolved.relative_to(ROOT)
    except ValueError:
        return None
    return resolved


def read_yaml(path: Path) -> list[dict[str, Any]]:
    try:
        with path.open(encoding="utf-8") as handle:
            return [doc for doc in yaml.safe_load_all(handle) if isinstance(doc, dict)]
    except yaml.YAMLError as exc:
        fail(f"{path.relative_to(ROOT)} is not valid YAML: {exc}")
        return []


def params_to_dict(pipeline_run: dict[str, Any]) -> dict[str, Any]:
    values: dict[str, Any] = {}
    for item in pipeline_run.get("spec", {}).get("params", []) or []:
        if isinstance(item, dict) and "name" in item:
            values[item["name"]] = item.get("value")
    return values


def tekton_targets() -> dict[str, dict[str, Any]]:
    files = sorted(TEKTON_DIR.glob("*pull-request.yaml"))
    found: dict[str, dict[str, Any]] = {}
    for path in files:
        docs = read_yaml(path)
        pipeline_runs = [doc for doc in docs if doc.get("kind") == "PipelineRun"]
        if len(pipeline_runs) != 1:
            fail(
                f"{path.name}: expected exactly one PipelineRun document, "
                f"found {len(pipeline_runs)}"
            )
            continue
        pipeline_run = pipeline_runs[0]
        params = params_to_dict(pipeline_run)
        output = str(params.get("output-image", ""))
        image_name = output.rsplit("/", 1)[-1].split(":", 1)[0]
        if image_name == "odh-maas-discovery":
            image_name = "maas-discovery"
        if image_name in found:
            fail(
                f"duplicate Tekton PR target {image_name!r}: "
                f"{found[image_name]['tekton']} and {path.name}"
            )
            continue
        found[image_name] = {
            "tekton": path.name,
            "dockerfile": str(params.get("dockerfile", "")),
            "context": str(params.get("path-context", "")),
            "output": output,
            "params": params,
            "pipeline_ref": pipeline_run.get("spec", {}).get("pipelineRef", {}),
        }
    return found


def resolve_target(name: str) -> dict[str, Any]:
    targets = tekton_targets()
    if name not in targets:
        raise SystemExit(f"unknown target {name!r}; known: {', '.join(sorted(targets))}")
    target = targets[name]
    expected = EXPECTED_TARGETS.get(name)
    if not expected:
        raise SystemExit(f"target {name!r} has no static contract")
    for field in ("dockerfile", "context", "output"):
        if target[field] != expected[field]:
            raise SystemExit(
                f"target {name!r} {field} is {target[field]!r}, expected {expected[field]!r}"
            )
    return {**target, **expected}


def join_continuations(lines: Iterable[str]) -> list[str]:
    result: list[str] = []
    current = ""
    for raw in lines:
        line = raw.rstrip("\n")
        if current:
            current += " " + line.strip()
        else:
            current = line.strip()
        if current.endswith("\\"):
            current = current[:-1].rstrip()
            continue
        if current:
            result.append(current)
        current = ""
    if current:
        result.append(current)
    return result


def parse_dockerfile(path: Path) -> dict[str, Any]:
    stages: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None
    for line in join_continuations(path.read_text(encoding="utf-8").splitlines()):
        upper = line.upper()
        if upper.startswith("FROM "):
            try:
                tokens = shlex.split(line)
            except ValueError:
                tokens = line.split()
            stage_tokens = [token for token in tokens[1:] if not token.startswith("--")]
            image = stage_tokens[0] if stage_tokens else ""
            alias = stage_tokens[2] if len(stage_tokens) > 2 and stage_tokens[1].lower() == "as" else None
            current = {"image": image, "alias": alias, "copy": [], "user": [], "run": []}
            stages.append(current)
        elif current is None:
            continue
        elif upper.startswith("COPY "):
            current["copy"].append(line)
        elif upper.startswith("USER "):
            current["user"].append(line.split(None, 1)[1].strip())
        elif upper.startswith("RUN "):
            current["run"].append(line[4:].strip())
    return {"path": path, "stages": stages}


def copy_tokens(line: str) -> tuple[list[str], list[str]]:
    body = line.split(None, 1)[1].strip()
    if body.startswith("["):
        try:
            values = json.loads(body)
            return [], [str(value) for value in values]
        except json.JSONDecodeError:
            pass
    try:
        tokens = shlex.split(body)
    except ValueError:
        tokens = body.split()
    flags = [token for token in tokens if token.startswith("--")]
    args = [token for token in tokens if not token.startswith("--")]
    return flags, args


def context_source_exists(context: Path, source: str) -> tuple[bool, str]:
    if source.startswith("/"):
        return False, f"absolute COPY source {source!r} is outside the build context"
    candidate = context / source
    if any(char in source for char in "*?["):
        matches = glob.glob(str(candidate), recursive=True)
        if not matches:
            return False, f"COPY glob {source!r} matched nothing under {context}"
        return True, ""
    if not candidate.exists():
        return False, f"COPY source {source!r} does not exist under {context}"
    return True, ""


def validate_dockerfile(name: str, dockerfile: Path, context: Path, konflux: bool) -> None:
    if not dockerfile.is_file():
        fail(f"{name}: Dockerfile {dockerfile.relative_to(ROOT)} does not exist")
        return
    if not context.is_dir():
        fail(f"{name}: build context {context.relative_to(ROOT)} does not exist")
        return

    parsed = parse_dockerfile(dockerfile)
    stages = parsed["stages"]
    if not stages:
        fail(f"{dockerfile.relative_to(ROOT)}: no FROM stage found")
        return

    aliases = {stage["alias"].lower() for stage in stages if stage.get("alias")}
    for stage in stages:
        for line in stage["copy"]:
            flags, args = copy_tokens(line)
            if len(args) < 2:
                fail(f"{dockerfile.relative_to(ROOT)}: cannot parse COPY: {line}")
                continue
            sources, destination = args[:-1], args[-1]
            from_flag = next((flag for flag in flags if flag.startswith("--from=")), None)
            if from_flag:
                from_value = from_flag.split("=", 1)[1].lower()
                if from_value in aliases:
                    continue
                continue  # External stage/image source; its contents are not in this context.
            for source in sources:
                exists, reason = context_source_exists(context, source)
                if not exists:
                    fail(f"{dockerfile.relative_to(ROOT)}: {reason}")
            if destination.startswith("/maas-api") or destination.startswith("/deployment"):
                if destination not in CONTROLLER_TREES.values():
                    fail(
                        f"{dockerfile.relative_to(ROOT)}: unexpected embedded deployment destination {destination!r}"
                    )

    if konflux:
        for stage in stages:
            if "@sha256:" not in stage["image"]:
                fail(
                    f"{dockerfile.relative_to(ROOT)}: Konflux base image {stage['image']!r} is not digest-pinned"
                )
            elif "go-toolset" in stage["image"] and GO_TOOLSET_DIGEST not in stage["image"]:
                fail(
                    f"{dockerfile.relative_to(ROOT)}: go-toolset builder {stage['image']!r} "
                    f"does not match the hermetic runner digest {GO_TOOLSET_DIGEST}"
                )
    else:
        for stage in stages:
            if re.search(r":latest(?:\s|$)", stage["image"]):
                warn(f"{dockerfile.relative_to(ROOT)}: floating base image tag {stage['image']!r}")

    final = stages[-1]
    if not final["user"] or final["user"][-1].strip() != "1001":
        fail(f"{dockerfile.relative_to(ROOT)}: final stage must end with USER 1001")

    run_text = "\n".join(final["run"])
    if name == "maas-controller":
        if "chmod -R g=u /maas-api /deployment" not in run_text:
            fail(
                f"{dockerfile.relative_to(ROOT)}: final stage must make embedded trees group-readable/writable"
            )
        copies: dict[str, str] = {}
        for line in final["copy"]:
            flags, args = copy_tokens(line)
            if any(flag.startswith("--from=") for flag in flags) or len(args) < 2:
                continue
            copies[args[-1]] = args[0]
        expected_copies = {destination: source for source, destination in CONTROLLER_TREES.items()}
        if copies != expected_copies:
            fail(
                f"{dockerfile.relative_to(ROOT)}: embedded deployment trees differ from the reconciled contract: "
                f"got {copies!r}, expected {expected_copies!r}"
            )
    else:
        if "chmod -R g=u /app" not in run_text:
            fail(f"{dockerfile.relative_to(ROOT)}: final stage must make /app group-accessible")

    # Prove the image build command still matches the hermetic preflight contract.
    build_runs = [run for stage in stages for run in stage["run"] if "go build" in run]
    if len(build_runs) != 1:
        fail(f"{dockerfile.relative_to(ROOT)}: expected exactly one go build command, found {len(build_runs)}")
        return
    build = build_runs[0]
    expected = EXPECTED_TARGETS[name]
    if expected["build_target"] not in build:
        fail(
            f"{dockerfile.relative_to(ROOT)}: go build target {expected['build_target']!r} is missing from {build!r}"
        )
    if "-tags" in build:
        fail(f"{dockerfile.relative_to(ROOT)}: build tags are not covered by the hermetic contract: {build!r}")
    if "CGO_ENABLED=${CGO_ENABLED}" not in build:
        fail(f"{dockerfile.relative_to(ROOT)}: CGO_ENABLED is not propagated into go build")
    if "GOEXPERIMENT=strictfipsruntime" not in build and "GOEXPERIMENT=${GOEXPERIMENT}" not in build:
        fail(f"{dockerfile.relative_to(ROOT)}: strictfipsruntime is not propagated into go build")


def check_tekton() -> dict[str, dict[str, Any]]:
    targets = tekton_targets()
    expected_names = set(EXPECTED_TARGETS)
    found_names = set(targets)
    if found_names != expected_names:
        fail(
            "Tekton PR target set changed; onboard the simulator for every image: "
            f"found {sorted(found_names)}, expected {sorted(expected_names)}"
        )
    for name, expected in EXPECTED_TARGETS.items():
        target = targets.get(name)
        if not target:
            fail(f"{name}: missing .tekton/{expected['tekton']}")
            continue
        for field in ("dockerfile", "context", "output"):
            if target[field] != expected[field]:
                fail(
                    f"{name}: Tekton {field} is {target[field]!r}, expected {expected[field]!r}"
                )
        params = target["params"]
        for missing in ("build-platforms", "hermetic", "prefetch-input"):
            if missing not in params:
                warn(
                    f"{name}: Tekton params omit {missing!r}; the simulator cannot prove that "
                    "Konflux multi-arch/hermetic/prefetch behaviour is configured"
                )
        pipeline_path = str(
            target.get("pipeline_ref", {}).get("params", [{}])[-1].get("value", "")
        ) if target.get("pipeline_ref", {}).get("params") else ""
        if not pipeline_path:
            pipeline_path = json.dumps(target.get("pipeline_ref", {}))
        if "multi-arch-container-build.yaml" not in pipeline_path:
            fail(f"{name}: unexpected Tekton pipeline: {pipeline_path!r}")
        if not str(target["output"]).endswith(":odh-pr"):
            fail(f"{name}: PR output image must use the odh-pr tag, got {target['output']!r}")

        context = ROOT / expected["context"]
        dockerfile = ROOT / expected["dockerfile"]
        validate_dockerfile(name, dockerfile, context, konflux=False)
        validate_dockerfile(
            name, dockerfile.with_name(dockerfile.name + ".konflux"), context, konflux=True
        )
    return targets


def check_controller_trees() -> None:
    for source, destination in CONTROLLER_TREES.items():
        if not (ROOT / source).exists():
            fail(f"controller embedded tree source {source} does not exist")
    for path in CONTROLLER_REQUIRED_SUBPATHS:
        if not (ROOT / path).exists():
            fail(f"controller image contract requires {path}, but it is absent")

    source_literals: set[str] = set()
    for go_file in (ROOT / "maas-controller").rglob("*.go"):
        if go_file.name.endswith("_test.go"):
            continue
        for match in re.finditer(r'"(/(?:deployment|maas-api)/[^"\s]+)"', go_file.read_text(encoding="utf-8")):
            literal = match.group(1)
            if literal.startswith("/maas-api/") and not literal.startswith("/maas-api/deploy"):
                continue  # HTTP endpoint, not an image path.
            source_literals.add(literal)
    for literal in sorted(source_literals):
        candidate = ROOT / literal.lstrip("/")
        if not candidate.exists():
            fail(f"controller source references embedded path {literal}, but it does not exist")
    ok(f"controller embedded path literals resolved ({len(source_literals)} checked)")


def check_manager_contract() -> None:
    manager_path = ROOT / "deployment/base/maas-controller/manager/manager.yaml"
    params_path = ROOT / "deployment/base/maas-controller/default/params.env"
    docs = read_yaml(manager_path)
    if not docs:
        fail("controller manager Deployment is missing")
        return
    deployment = docs[0]
    containers = deployment.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
    manager = next((container for container in containers if container.get("name") == "manager"), None)
    if not manager:
        fail("maas-controller Deployment has no manager container")
        return
    params: dict[str, str] = {}
    for line in params_path.read_text(encoding="utf-8").splitlines():
        if "=" in line and not line.lstrip().startswith("#"):
            key, value = line.split("=", 1)
            params[key] = value
    env_names: list[str] = []
    env_by_name: dict[str, Any] = {}
    for item in manager.get("env", []) or []:
        env_name = item.get("name")
        if env_name:
            env_names.append(env_name)
            env_by_name[env_name] = item
    duplicates = sorted({env_name for env_name in env_names if env_names.count(env_name) > 1})
    if duplicates:
        fail(f"duplicate manager env names: {', '.join(duplicates)}")
    for item in manager.get("env", []) or []:
        ref = item.get("valueFrom", {}).get("configMapKeyRef")
        if not ref:
            continue
        if ref.get("name") != "maas-parameters":
            fail(f"manager env {item.get('name')} reads unexpected ConfigMap {ref.get('name')!r}")
        if ref.get("key") not in params:
            fail(f"manager env {item.get('name')} reads missing maas-parameters key {ref.get('key')!r}")

    api_env = env_by_name.get("RELATED_IMAGE_ODH_MAAS_API_IMAGE", {})
    api_ref = api_env.get("valueFrom", {}).get("configMapKeyRef", {})
    if api_ref.get("name") != "maas-parameters" or api_ref.get("key") != "maas-api-image":
        fail("RELATED_IMAGE_ODH_MAAS_API_IMAGE must read maas-parameters/maas-api-image")
    if not params.get("maas-api-image"):
        fail("maas-parameters must define a non-empty maas-api-image")
    elif params["maas-api-image"].endswith(":latest"):
        warn("maas-parameters/maas-api-image uses a floating :latest tag")

    constants = (ROOT / "maas-controller/pkg/platform/tenantreconcile/constants.go").read_text(
        encoding="utf-8"
    )
    default_match = re.search(r'DefaultMaaSAPIImage\s*=\s*"([^"]+)"', constants)
    if not default_match:
        fail("DefaultMaaSAPIImage is missing from tenantreconcile constants")
    else:
        default_image = default_match.group(1)
        if not re.match(r"^[a-z0-9.-]+(?::[0-9]+)?/[^:\s]+:[^:\s]+$", default_image):
            fail(f"DefaultMaaSAPIImage is not a complete image reference: {default_image!r}")
        if default_image.endswith(":latest"):
            warn("DefaultMaaSAPIImage is :latest; production must supply RELATED_IMAGE_ODH_MAAS_API_IMAGE")
    params_go = (ROOT / "maas-controller/pkg/platform/tenantreconcile/params.go").read_text(encoding="utf-8")
    if 'os.Getenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE")' not in params_go:
        fail("params.go does not read RELATED_IMAGE_ODH_MAAS_API_IMAGE")
    if "firstNonEmpty(os.Getenv(\"RELATED_IMAGE_ODH_MAAS_API_IMAGE\"), DefaultMaaSAPIImage)" not in params_go:
        fail("RELATED_IMAGE_ODH_MAAS_API_IMAGE does not fall back to DefaultMaaSAPIImage")
    if 'setContainerImage(r, "maas-api", params.MaaSAPIImage)' not in params_go:
        fail("tenant render does not patch the maas-api container image from MaaSAPIImage")

    discovery_constants = (ROOT / "maas-controller/pkg/controller/maas/constants.go").read_text(
        encoding="utf-8"
    )
    discovery_match = re.search(r'DefaultMaaSDiscoveryImage\s*=\s*"([^"]+)"', discovery_constants)
    if not discovery_match:
        fail("DefaultMaaSDiscoveryImage is missing")
    elif "@sha256:" not in discovery_match.group(1):
        warn("DefaultMaaSDiscoveryImage is not digest-pinned")


def workflow_working_directory(job: dict[str, Any], step: dict[str, Any]) -> str:
    step_value = step.get("working-directory")
    if step_value is not None:
        return str(step_value).rstrip("/")
    job_defaults = (job.get("defaults", {}) or {}).get("run", {}) or {}
    return str(job_defaults.get("working-directory", "")).rstrip("/")


def check_existing_codegen_contract() -> None:
    makefile = (ROOT / "maas-controller/Makefile").read_text(encoding="utf-8")
    if not re.search(r"^verify-codegen:", makefile, re.MULTILINE):
        fail("maas-controller/Makefile no longer exposes verify-codegen")
    if "generate manifests" not in makefile.split("verify-codegen:", 1)[1].splitlines()[0]:
        fail("verify-codegen no longer regenerates CRD/RBAC/deepcopy artifacts")

    workflow_path = ROOT / ".github/workflows/build-test.yml"
    docs = read_yaml(workflow_path)
    if not docs:
        fail("build-test.yml could not be parsed")
        return
    active_step = False
    for job in (docs[0].get("jobs", {}) or {}).values():
        if job.get("if"):
            continue
        for step in job.get("steps", []) or []:
            run = step.get("run", "")
            if "make verify-codegen" not in str(run):
                continue
            if step.get("if"):
                continue
            if workflow_working_directory(job, step) != "maas-controller":
                continue
            active_step = True
    if not active_step:
        fail(
            "build-test.yml no longer runs make verify-codegen in an active maas-controller "
            "step; generated drift would be unguarded"
        )
    ok("generated CRD/RBAC/code drift remains owned by the existing build-test verify-codegen job")


def check_external_facts(targets: dict[str, dict[str, Any]]) -> None:
    for module in ("maas-api", "maas-controller", "maas-common", "maas-discovery"):
        directory = ROOT / module
        if not (directory / "go.mod").is_file():
            fail(f"{module}/go.mod is missing")
        if not (directory / "go.sum").is_file():
            fail(f"{module}/go.sum is missing")
    if (ROOT / "go.mod").exists():
        fail("root go.mod must not exist; the repository is a multi-module repo")
    if (ROOT / "Makefile").exists():
        fail("root Makefile must not exist; run tooling in the module directory")

    runner = (ROOT / "scripts/validate-build.sh").read_text(encoding="utf-8")
    if GO_TOOLSET_DIGEST not in runner:
        fail(
            "scripts/validate-build.sh does not use the same pinned go-toolset digest "
            "as the Konflux Dockerfiles"
        )

    for module in ("maas-api", "maas-controller", "maas-discovery"):
        go_mod_path = ROOT / module / "go.mod"
        if not go_mod_path.is_file():
            continue
        go_mod = go_mod_path.read_text(encoding="utf-8")
        for line in go_mod.splitlines():
            if line.startswith("replace ") and "=>" in line:
                replacement = line.split("=>", 1)[1].strip()
                if replacement.startswith(".") or replacement.startswith("/"):
                    warn(f"{module}: local replace pins vendored code: {line.strip()}")
                elif not re.search(r"\bv\d", replacement):
                    fail(f"{module}: remote replace is not version-pinned: {line.strip()}")

    operator_config = ROOT.parent / "opendatahub-operator" / "manifests-config.yaml"
    if operator_config.is_file():
        text = operator_config.read_text(encoding="utf-8")
        for env_name in (
            "RELATED_IMAGE_ODH_MAAS_API_IMAGE",
            "RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE",
            "RELATED_IMAGE_ODH_MAAS_DISCOVERY_IMAGE",
        ):
            if env_name not in text:
                fail(f"opendatahub-operator manifests-config.yaml is missing {env_name}")
        ok("local opendatahub-operator manifests-config.yaml declares all three MaaS images")
    else:
        warn(
            "EXTERNAL_UNASSERTED: opendatahub-operator manifests-config.yaml is not in this checkout; "
            "operator image pinning must be asserted in that repository"
        )

    ai_gateway = ROOT.parent / "ai-gateway-operator"
    if ai_gateway.is_dir():
        try:
            hits = subprocess.run(
                ["rg", "-l", "maas-api|maas-controller|odh-maas-discovery", str(ai_gateway)],
                text=True,
                capture_output=True,
            )
        except FileNotFoundError:
            warn(
                "EXTERNAL_UNASSERTED: ripgrep is unavailable, so the ai-gateway-operator "
                "MaaS image references could not be searched"
            )
        else:
            if hits.returncode == 1:
                warn(
                    "ai-gateway-operator checkout does not reference the MaaS images; "
                    "coverage status: not shipped there"
                )
            elif hits.returncode != 0:
                warn(
                    "EXTERNAL_UNASSERTED: search of ai-gateway-operator failed "
                    f"(exit {hits.returncode}): {hits.stderr.strip() or 'no stderr'}"
                )
            elif hits.stdout.strip():
                ok(
                    "ai-gateway-operator checkout references the MaaS image names; "
                    "full pinning still requires its CI"
                )
            else:
                warn(
                    "EXTERNAL_UNASSERTED: search of ai-gateway-operator returned no output"
                )
    else:
        warn(
            "EXTERNAL_UNASSERTED: ai-gateway-operator is not in this checkout; "
            "its image pinning and RBAC consumption can only be asserted upstream"
        )


def check_static() -> None:
    targets = check_tekton()
    check_controller_trees()
    check_manager_contract()
    check_existing_codegen_contract()
    check_external_facts(targets)
    if ERRORS:
        raise SystemExit(1)
    print("Static Konflux contracts passed.")


def kustomize_command() -> list[str]:
    explicit = os.environ.get("KUSTOMIZE_BIN")
    if explicit:
        return [explicit]
    if shutil.which("kustomize"):
        return ["kustomize"]
    if shutil.which("kubectl"):
        return ["kubectl", "kustomize"]
    raise SystemExit("kustomize is required (set KUSTOMIZE_BIN)")


RENDER_TREES = ("deployment", "maas-api/deploy")


def kustomization_files() -> list[Path]:
    files: list[Path] = []
    for base in (ROOT / tree for tree in RENDER_TREES):
        files.extend(sorted(base.rglob("kustomization.yaml")))
    return files


def is_remote_ref(ref: str) -> bool:
    return (
        "://" in ref
        or ref.startswith("github.com/")
        or ref.startswith("git@")
        or "?ref=" in ref
    )


def is_inline_kustomize_yaml(ref: str) -> bool:
    return "\n" in ref


def check_local_ref(
    kustomization: Path,
    relative: Path,
    label: str,
    ref: str,
    *,
    expect_dir: bool = False,
) -> Path | None:
    target = (kustomization.parent / ref).resolve()
    if resolve_inside_root(target) is None:
        fail(f"{relative}: {label} {ref!r} resolves outside the repository")
        return None
    if not target.exists():
        fail(f"{relative}: {label} {ref!r} does not exist")
        return None
    if expect_dir and not target.is_dir():
        fail(f"{relative}: {label} {ref!r} is not a directory")
        return None
    return target


def child_kustomization(target: Path) -> Path | None:
    if not target.is_dir():
        return None
    for name in ("kustomization.yaml", "kustomization.yml", "Kustomization"):
        candidate = target / name
        if candidate.is_file():
            return candidate
    return None


def transitive_remote_refs(kustomization: Path) -> list[str]:
    remote_refs: list[str] = []
    queue = [kustomization]
    seen: set[Path] = set()
    while queue:
        path = queue.pop()
        resolved = path.resolve()
        if resolved in seen:
            continue
        seen.add(resolved)
        docs = read_yaml(path)
        if not docs:
            continue
        doc = docs[0]
        for field in ("resources", "components", "bases"):
            for ref in doc.get(field, []) or []:
                if not isinstance(ref, str):
                    continue
                if is_remote_ref(ref):
                    if ref not in remote_refs:
                        remote_refs.append(ref)
                    continue
                target = (path.parent / ref).resolve()
                if resolve_inside_root(target) is None or not target.exists():
                    continue
                child = child_kustomization(target)
                if child:
                    queue.append(child)
    return sorted(remote_refs)


def check_kustomization_refs(files: list[Path]) -> None:
    queue = list(files)
    seen: set[Path] = set()
    while queue:
        path = queue.pop()
        resolved = path.resolve()
        if resolve_inside_root(resolved) is None:
            fail(f"{path}: kustomization file resolves outside the repository")
            continue
        if resolved in seen:
            continue
        seen.add(resolved)
        docs = read_yaml(path)
        if not docs:
            continue
        doc = docs[0]
        relative = path.relative_to(ROOT)

        for field in ("resources", "components", "bases"):
            for ref in doc.get(field, []) or []:
                if not isinstance(ref, str) or is_remote_ref(ref):
                    continue
                target = check_local_ref(path, relative, f"{field} entry", ref)
                if target:
                    child = child_kustomization(target)
                    if child:
                        queue.append(child)

        for generator in ("configMapGenerator", "secretGenerator"):
            for ref in doc.get(generator, []) or []:
                if not isinstance(ref, dict):
                    continue
                for env_file in ref.get("envs", []) or []:
                    if isinstance(env_file, str):
                        check_local_ref(path, relative, f"{generator} env file", env_file)
                env_file = ref.get("env")
                if isinstance(env_file, str):
                    check_local_ref(path, relative, f"{generator} env file", env_file)
                for file_ref in ref.get("files", []) or []:
                    file_path = str(file_ref).split("=", 1)[-1]
                    check_local_ref(path, relative, f"{generator} file", file_path)

        for ref in doc.get("patchesStrategicMerge", []) or []:
            if not isinstance(ref, str):
                continue
            stripped = ref.lstrip()
            if "\n" in ref or stripped.startswith(
                ("{", "[", "-", "apiVersion:", "kind:", "metadata:")
            ):
                continue
            check_local_ref(path, relative, "patchesStrategicMerge entry", ref)

        for field in ("patchesJson6902", "patches", "replacements"):
            for ref in doc.get(field, []) or []:
                if isinstance(ref, dict) and isinstance(ref.get("path"), str):
                    check_local_ref(path, relative, f"{field} path", ref["path"])

        for field in ("configurations", "crds"):
            for ref in doc.get(field, []) or []:
                if isinstance(ref, str) and not is_remote_ref(ref):
                    check_local_ref(path, relative, f"{field} entry", ref)

        for field in ("generators", "transformers"):
            for ref in doc.get(field, []) or []:
                if not isinstance(ref, str) or is_remote_ref(ref):
                    continue
                if is_inline_kustomize_yaml(ref):
                    continue
                check_local_ref(path, relative, f"{field} entry", ref)

        openapi = doc.get("openapi")
        if isinstance(openapi, dict) and isinstance(openapi.get("path"), str):
            check_local_ref(path, relative, "openapi path", openapi["path"])

        for chart in doc.get("helmCharts", []) or []:
            if not isinstance(chart, dict):
                continue
            if isinstance(chart.get("valuesFile"), str):
                check_local_ref(path, relative, "helmCharts valuesFile", chart["valuesFile"])
            for ref in chart.get("additionalValuesFiles", []) or []:
                if isinstance(ref, str):
                    check_local_ref(
                        path, relative, "helmCharts additionalValuesFiles entry", ref
                    )
            if isinstance(chart.get("chartHome"), str):
                check_local_ref(
                    path,
                    relative,
                    "helmCharts chartHome",
                    chart["chartHome"],
                    expect_dir=True,
                )

        helm_globals = doc.get("helmGlobals")
        if isinstance(helm_globals, dict) and isinstance(helm_globals.get("chartHome"), str):
            check_local_ref(
                path,
                relative,
                "helmGlobals chartHome",
                helm_globals["chartHome"],
                expect_dir=True,
            )


def display_path(path: Path) -> Path:
    try:
        return path.relative_to(ROOT)
    except ValueError:
        return path


def walk_render_tree(
    source: Path,
    destination: Path | None,
    active_dirs: set[tuple[int, int]],
) -> None:
    label = display_path(source)
    try:
        resolved = source.resolve()
    except (OSError, RuntimeError) as exc:
        fail(f"{label}: cannot resolve source: {exc}")
        return
    if resolve_inside_root(resolved) is None:
        fail(f"{label}: source resolves outside the repository")
        return
    try:
        source_stat = resolved.stat()
    except OSError as exc:
        fail(f"{label}: cannot stat source: {exc}")
        return
    if not stat.S_ISDIR(source_stat.st_mode):
        fail(f"{label}: render tree entry is not a directory")
        return
    identity = (source_stat.st_dev, source_stat.st_ino)
    if identity in active_dirs:
        fail(f"{label}: directory symlink cycle detected")
        return
    active_dirs.add(identity)
    try:
        if destination is not None:
            destination.mkdir(parents=True, exist_ok=True)
        try:
            entries = sorted(resolved.iterdir(), key=lambda entry: entry.name)
        except OSError as exc:
            fail(f"{label}: cannot list source directory: {exc}")
            return
        for entry in entries:
            entry_label = display_path(entry)
            try:
                target = entry.resolve()
            except (OSError, RuntimeError) as exc:
                fail(f"{entry_label}: cannot resolve source: {exc}")
                continue
            if resolve_inside_root(target) is None:
                fail(f"{entry_label}: source resolves outside the repository")
                continue
            try:
                entry_stat = target.stat()
            except OSError as exc:
                fail(f"{entry_label}: cannot stat source: {exc}")
                continue
            if stat.S_ISDIR(entry_stat.st_mode):
                walk_render_tree(
                    entry,
                    destination / entry.name if destination is not None else None,
                    active_dirs,
                )
            elif stat.S_ISREG(entry_stat.st_mode):
                if destination is None:
                    continue
                try:
                    shutil.copy2(target, destination / entry.name)
                except OSError as exc:
                    fail(f"{entry_label}: cannot copy source: {exc}")
            else:
                fail(f"{entry_label}: unsupported file type in render tree")
    finally:
        active_dirs.remove(identity)


def check_render_tree_sources() -> None:
    for tree in RENDER_TREES:
        walk_render_tree(ROOT / tree, None, set())


def render_all(
    output_dir: Path,
) -> tuple[dict[str, list[dict[str, Any]]], list[tuple[str, Path]]]:
    command = kustomize_command()
    files = kustomization_files()
    check_render_tree_sources()
    check_kustomization_refs(files)
    if ERRORS:
        print("Aborting before any kustomize invocation; no render output was written.")
        return {}, []
    rendered: dict[str, list[dict[str, Any]]] = {}
    render_outputs: list[tuple[str, Path]] = []
    with tempfile.TemporaryDirectory(prefix="maas-kustomize-") as temp_dir:
        temp_root = Path(temp_dir)
        for index, path in enumerate(files):
            docs = read_yaml(path)
            if not docs:
                continue
            if docs[0].get("kind") == "Component":
                continue
            remote_refs = transitive_remote_refs(path)
            if remote_refs:
                warn(
                    f"{path.parent.relative_to(ROOT)}: skipped render because it references external kustomize "
                    f"resources (assert these in the consuming operator): {', '.join(remote_refs)}"
                )
                continue
            relative = path.parent.relative_to(ROOT).as_posix()
            sandbox = temp_root / f"{index:04d}"
            sandbox.mkdir()
            stage_errors = len(ERRORS)
            for tree in RENDER_TREES:
                destination = sandbox / tree
                walk_render_tree(ROOT / tree, destination, set())
            if len(ERRORS) > stage_errors:
                return rendered, render_outputs
            (sandbox / "kustomization.yaml").write_text(
                yaml.safe_dump({"resources": [relative]}, sort_keys=False),
                encoding="utf-8",
            )
            if Path(command[0]).name == "kubectl":
                build_command = [
                    *command,
                    "--load-restrictor",
                    "LoadRestrictionsRootOnly",
                    str(sandbox),
                ]
            else:
                build_command = [
                    *command,
                    "build",
                    "--load-restrictor",
                    "LoadRestrictionsRootOnly",
                    str(sandbox),
                ]
            result = subprocess.run(
                build_command,
                text=True,
                capture_output=True,
            )
            if result.returncode != 0:
                fail(f"kustomize build failed for {relative}:\n{result.stderr.strip()}")
                continue
            if result.stderr.strip():
                warn(f"{relative}: kustomize warning: {result.stderr.strip()}")
            slug = relative.replace("/", "__")
            output_dir.mkdir(parents=True, exist_ok=True)
            target = output_dir / f"{index:04d}-{slug}.yaml"
            target.write_text(result.stdout, encoding="utf-8")
            parsed = [doc for doc in yaml.safe_load_all(result.stdout) if isinstance(doc, dict)]
            rendered[relative] = parsed
            render_outputs.append((relative, target))
    return rendered, render_outputs


def check_kubeconform(render_outputs: list[tuple[str, Path]]) -> None:
    binary = os.environ.get("KUBECONFORM_BIN")
    if not binary:
        if os.environ.get("REQUIRE_KUBECONFORM") == "1":
            fail("KUBECONFORM_BIN is not set; schema validation is required")
        else:
            warn("kubeconform not installed locally; CI sets KUBECONFORM_BIN and REQUIRE_KUBECONFORM=1")
        return
    for root, path in render_outputs:
        if path.stat().st_size == 0:
            warn(f"{root} rendered no resources (intentionally empty)")
            continue
        result = subprocess.run(
            [
                binary,
                "-strict",
                "-ignore-missing-schemas",
                "-verbose",
                "-kubernetes-version",
                "1.31.0",
                "-output",
                "json",
                str(path),
            ],
            text=True,
            capture_output=True,
        )
        if not result.stdout.strip():
            fail(
                f"kubeconform produced no report for {path.name} "
                f"(exit {result.returncode}):\n{result.stderr}"
            )
            continue
        try:
            report = json.loads(result.stdout)
        except json.JSONDecodeError as exc:
            fail(f"kubeconform report for {path.name} is not JSON: {exc}")
            continue
        if not isinstance(report, dict) or not isinstance(report.get("resources"), list):
            fail(f"kubeconform report for {path.name} has an unexpected shape")
            continue

        allowed_partial = PARTIAL_SCHEMA_FRAGMENTS.get(root, set())
        allowed_seen = False
        skipped_builtins: list[str] = []
        invalid: list[str] = []
        other: list[str] = []
        for resource in report.get("resources", []):
            status = resource.get("status")
            if status == "statusValid":
                continue
            if status == "statusSkipped":
                if (
                    resource.get("version") == "apiextensions.k8s.io/v1"
                    and resource.get("kind") == "CustomResourceDefinition"
                ):
                    continue
                version = str(resource.get("version", ""))
                group = version.split("/", 1)[0] if "/" in version else ""
                if group in BUILTIN_KUBERNETES_API_GROUPS:
                    skipped_builtins.append(f"{resource.get('kind')} ({version})")
                continue
            signature = (
                resource.get("kind"),
                resource.get("name"),
                str(resource.get("version", "")),
                tuple(
                    sorted(
                        (str(error.get("path", "")), str(error.get("msg", "")))
                        for error in resource.get("validationErrors", []) or []
                    )
                ),
            )
            if status == "statusInvalid" and signature in allowed_partial:
                allowed_seen = True
                continue
            if status == "statusInvalid":
                invalid.append(
                    f"{resource.get('kind')}/{resource.get('name')} "
                    f"({resource.get('version')}): {resource.get('msg', 'schema invalid')}"
                )
                continue
            other.append(
                f"{resource.get('kind')}/{resource.get('name')} "
                f"({resource.get('version')}): {status}: {resource.get('msg', '')}"
            )

        if result.returncode not in {0, 1}:
            fail(
                f"kubeconform failed to validate {path.name} "
                f"(exit {result.returncode}):\n{result.stderr}"
            )
        if invalid:
            fail(f"kubeconform reported invalid resources in {path.name}: {'; '.join(invalid)}")
        if skipped_builtins:
            fail(
                f"kubeconform skipped built-in kinds in {path.name} "
                f"(likely a kind/apiVersion typo): {', '.join(skipped_builtins)}"
            )
        if other:
            fail(f"kubeconform returned non-schema statuses for {path.name}: {'; '.join(other)}")
        if result.returncode == 1 and not (invalid or skipped_builtins or other or allowed_seen):
            fail(
                f"kubeconform failed on {path.name} (exit 1 without a classified resource): "
                f"{result.stderr.strip()}"
            )
        if allowed_seen:
            signatures = ", ".join(
                f"{kind}/{name} ({version}): {'; '.join(f'{path}: {msg}' for path, msg in errors)}"
                for kind, name, version, errors in allowed_partial
            )
            warn(
                f"{root} is an overlay fragment; schema validation permits only: {signatures}"
            )


def check_duplicate_env(rendered: dict[str, list[dict[str, Any]]]) -> None:
    workload_kinds = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}
    for root, docs in rendered.items():
        for doc in docs:
            if doc.get("kind") not in workload_kinds:
                continue
            spec = doc.get("spec", {})
            pod_spec = spec.get("template", {}).get("spec", {}) if doc.get("kind") != "CronJob" else spec.get("jobTemplate", {}).get("spec", {}).get("template", {}).get("spec", {})
            for container in pod_spec.get("containers", []) + pod_spec.get("initContainers", []):
                names = [env.get("name") for env in container.get("env", []) if env.get("name")]
                duplicates = sorted({name for name in names if names.count(name) > 1})
                if duplicates:
                    fail(
                        f"{root}: {doc.get('kind')}/{doc.get('metadata', {}).get('name')} "
                        f"container {container.get('name')} has duplicate env names: {', '.join(duplicates)}"
                    )


def doc_key(doc: dict[str, Any]) -> tuple[str, str, str]:
    metadata = doc.get("metadata", {})
    return (doc.get("kind", ""), metadata.get("namespace", ""), metadata.get("name", ""))


def check_rbac_identity(all_docs: list[dict[str, Any]], scope: str) -> None:
    service_accounts = {
        (doc.get("metadata", {}).get("namespace", ""), doc.get("metadata", {}).get("name", ""))
        for doc in all_docs
        if doc.get("kind") == "ServiceAccount"
    }
    roles = {
        doc_key(doc)
        for doc in all_docs
        if doc.get("kind") in {"Role", "ClusterRole"}
    }
    for doc in all_docs:
        if doc.get("kind") not in {"RoleBinding", "ClusterRoleBinding"}:
            continue
        metadata = doc.get("metadata", {})
        binding_ns = metadata.get("namespace", "")
        label = f"{doc.get('kind')}/{binding_ns + '/' if binding_ns else ''}{metadata.get('name')}"
        role_ref = doc.get("roleRef", {})
        role_kind = role_ref.get("kind", "")
        role_ns = binding_ns if role_kind == "Role" else ""
        role_key = (role_kind, role_ns, role_ref.get("name", ""))
        if role_key not in roles and not str(role_ref.get("name", "")).startswith("system:"):
            fail(
                f"{scope}: {label}: roleRef {role_kind}/{role_ref.get('name')} "
                "is not present in the applied union"
            )
        subjects = doc.get("subjects", []) or []
        if not subjects:
            fail(f"{scope}: {label}: binding has no subjects")
        for subject in subjects:
            if subject.get("kind") != "ServiceAccount":
                continue
            namespace = subject.get("namespace") or binding_ns
            if not namespace:
                fail(f"{label}: ServiceAccount subject {subject.get('name')!r} has no namespace")
                continue
            if (namespace, subject.get("name", "")) not in service_accounts and (
                namespace,
                subject.get("name", ""),
            ) not in EXTERNAL_SERVICE_ACCOUNTS:
                fail(
                    f"{scope}: {label}: ServiceAccount subject {namespace}/{subject.get('name')} "
                    "is not created by any resource applied with this binding"
                )


def selector_matches(role: dict[str, Any], selector: dict[str, Any]) -> bool:
    labels = role.get("metadata", {}).get("labels", {}) or {}
    if any(labels.get(key) != value for key, value in (selector.get("matchLabels") or {}).items()):
        return False
    for expression in selector.get("matchExpressions", []) or []:
        key = expression.get("key")
        operator = expression.get("operator")
        values = expression.get("values", []) or []
        if operator == "In" and labels.get(key) not in values:
            return False
        if operator == "NotIn" and labels.get(key) in values:
            return False
        if operator == "Exists" and key not in labels:
            return False
        if operator == "DoesNotExist" and key in labels:
            return False
    return True


def role_rules(role: dict[str, Any], all_docs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    rules = list(role.get("rules", []) or [])
    selectors = role.get("aggregationRule", {}).get("clusterRoleSelectors", []) or []
    if not selectors:
        return rules
    for candidate in all_docs:
        if candidate.get("kind") != "ClusterRole":
            continue
        if any(selector_matches(candidate, selector) for selector in selectors):
            rules.extend(candidate.get("rules", []) or [])
    return rules


def object_identity(doc: dict[str, Any]) -> tuple[str, str, str]:
    metadata = doc.get("metadata", {})
    return (doc.get("kind", ""), metadata.get("namespace", ""), metadata.get("name", ""))


def controller_identity(all_docs: list[dict[str, Any]]) -> tuple[str, str] | None:
    for doc in all_docs:
        if doc.get("kind") != "Deployment" or doc.get("metadata", {}).get("name") != "maas-controller":
            continue
        namespace = doc.get("metadata", {}).get("namespace", "")
        pod_spec = doc.get("spec", {}).get("template", {}).get("spec", {})
        return namespace, pod_spec.get("serviceAccountName") or "default"
    return None


def controller_env(all_docs: list[dict[str, Any]]) -> dict[str, str]:
    deployment = next(
        (
            doc
            for doc in all_docs
            if doc.get("kind") == "Deployment"
            and doc.get("metadata", {}).get("name") == "maas-controller"
        ),
        None,
    )
    if not deployment:
        return {}
    namespace = deployment.get("metadata", {}).get("namespace", "")
    configmaps = {
        (doc.get("metadata", {}).get("namespace", ""), doc.get("metadata", {}).get("name", "")): (
            doc.get("data", {}) or {}
        )
        for doc in all_docs
        if doc.get("kind") == "ConfigMap"
    }
    pod_spec = deployment.get("spec", {}).get("template", {}).get("spec", {})
    values: dict[str, str] = {}
    for container in pod_spec.get("containers", []) or []:
        for env in container.get("env", []) or []:
            name = env.get("name")
            if not name:
                continue
            if "value" in env:
                values[name] = str(env["value"])
                continue
            key_ref = (env.get("valueFrom", {}) or {}).get("configMapKeyRef", {}) or {}
            config_name = key_ref.get("name", "")
            config_data = configmaps.get((namespace, config_name))
            if config_data is None:
                config_data = configmaps.get(("", config_name), {})
            if key_ref.get("key") in config_data:
                values[name] = str(config_data[key_ref["key"]])
    return values


def effective_rules(
    all_docs: list[dict[str, Any]], namespace: str, service_account: str
) -> list[tuple[str, dict[str, Any]]]:
    by_identity = {object_identity(doc): doc for doc in all_docs}
    rules: list[tuple[str, dict[str, Any]]] = []
    for doc in all_docs:
        if doc.get("kind") not in {"RoleBinding", "ClusterRoleBinding"}:
            continue
        binding_kind = doc.get("kind", "")
        binding_ns = doc.get("metadata", {}).get("namespace", "")
        if binding_kind == "RoleBinding" and not binding_ns:
            # A namespace-less RoleBinding has no resolvable scope. Never treat
            # its grants as cluster-wide; an apply with -n would place it in one
            # namespace, which the static render cannot infer.
            continue
        subjects = doc.get("subjects", []) or []
        for subject in subjects:
            if subject.get("kind") != "ServiceAccount":
                continue
            if subject.get("name") != service_account:
                continue
            # Kubernetes resolves an unqualified ServiceAccount subject in a
            # RoleBinding to the binding namespace; keep explicit namespaces.
            subject_namespace = subject.get("namespace") or (
                binding_ns if binding_kind == "RoleBinding" else ""
            )
            if subject_namespace == namespace:
                break
        else:
            continue
        ref = doc.get("roleRef", {})
        role_kind = ref.get("kind", "")
        role_ns = binding_ns if role_kind == "Role" else ""
        role = by_identity.get((role_kind, role_ns, ref.get("name", "")))
        if role:
            # A RoleBinding limits even a ClusterRole reference to its own namespace;
            # only a ClusterRoleBinding grants the rule cluster-wide.
            scope = "" if binding_kind == "ClusterRoleBinding" else binding_ns
            rules.extend((scope, rule) for rule in role_rules(role, all_docs))
    return rules


def values_match(allowed: list[str] | None, required: list[str] | None) -> bool:
    allowed_values = allowed or []
    required_values = required or []
    if "*" in allowed_values:
        return True
    if not required_values:
        return not allowed_values
    return all(value in allowed_values for value in required_values)


def non_resource_url_covered(allowed: list[str] | None, required: str) -> bool:
    for pattern in allowed or []:
        if pattern == required:
            return True
        if pattern.endswith("*") and required.startswith(pattern[:-1]):
            return True
    return False


def permission_rule_covers(
    effective: dict[str, Any],
    group: str,
    resource: str,
    verb: str,
    resource_name: str | None,
    permission_kind: str,
) -> bool:
    if not values_match(effective.get("verbs") or [], [verb]):
        return False
    if permission_kind == "url":
        return non_resource_url_covered(effective.get("nonResourceURLs"), resource)
    if effective.get("nonResourceURLs"):
        return False
    if not values_match(effective.get("apiGroups") or [""], [group]):
        return False
    if not values_match(effective.get("resources") or [], [resource]):
        return False
    allowed_names = effective.get("resourceNames") or []
    if resource_name is None:
        return not allowed_names
    return not allowed_names or resource_name in allowed_names


def rule_permissions(rule: dict[str, Any]) -> list[tuple[str, str, str, str | None, str]]:
    verbs = rule.get("verbs") or []
    permissions: list[tuple[str, str, str, str | None, str]] = []
    if rule.get("nonResourceURLs"):
        for url in rule["nonResourceURLs"]:
            for verb in verbs:
                permissions.append(("", url, verb, None, "url"))
        return permissions
    groups = rule.get("apiGroups") or [""]
    resources = rule.get("resources") or []
    names = rule.get("resourceNames") or [None]
    for group in groups:
        for resource in resources:
            for verb in verbs:
                for name in names:
                    permissions.append((group, resource, verb, name, "resource"))
    return permissions


def rule_covers(effective: dict[str, Any], required: dict[str, Any]) -> bool:
    return all(
        permission_rule_covers(effective, group, resource, verb, name, kind)
        for group, resource, verb, name, kind in rule_permissions(required)
    )


def has_escalate_for_role(
    rules: list[dict[str, Any]], role_kind: str, role_name: str
) -> bool:
    resource = "clusterroles" if role_kind == "ClusterRole" else "roles"
    return any(
        permission_rule_covers(
            rule,
            "rbac.authorization.k8s.io",
            resource,
            "escalate",
            role_name,
            "resource",
        )
        for rule in rules
    )


def describe_rule(rule: dict[str, Any]) -> str:
    groups = ",".join(rule.get("apiGroups") or [""])
    resources = ",".join(rule.get("resources") or rule.get("nonResourceURLs") or [])
    verbs = ",".join(rule.get("verbs") or [])
    names = rule.get("resourceNames")
    suffix = f" names={','.join(names)}" if names else ""
    return f"{groups}/{resources}:{verbs}{suffix}"


def check_escalation_closure(rendered: dict[str, list[dict[str, Any]]]) -> None:
    for bundle, base_roots in ESCALATION_BUNDLES.items():
        roots = list(base_roots)
        missing_roots = [root for root in roots if root not in rendered]
        if missing_roots:
            fail(f"{bundle}: escalation closure cannot run; missing renders: {', '.join(missing_roots)}")
            continue
        if DISCOVERY_APPLIED_ROOT not in rendered:
            fail(f"{bundle}: escalation closure cannot run; missing render: {DISCOVERY_APPLIED_ROOT}")
            continue
        all_docs = [doc for root in roots for doc in rendered[root]]
        env = controller_env(rendered[base_roots[0]])
        discovery_enabled = env.get("MAAS_DISCOVERY_ENABLED", "").lower() == "true"
        if discovery_enabled:
            roots.append(DISCOVERY_APPLIED_ROOT)
            all_docs.extend(rendered[DISCOVERY_APPLIED_ROOT])
        else:
            ok(
                f"{bundle}: MAAS_DISCOVERY_ENABLED is false; discovery RBAC is not applied "
                "and is excluded from escalation closure"
            )

        identity = controller_identity(all_docs)
        if not identity:
            fail(f"{bundle}: cannot find the deployed maas-controller Deployment")
            continue
        namespace, service_account = identity
        # Only RBAC installed with the controller bundle can authorize a write.
        # Roles under APPLIED_TREES are the proposed state; crediting their rules
        # here would let a role write satisfy its own coverage check.
        installed_docs = [
            doc for root in roots if root not in APPLIED_TREES for doc in rendered[root]
        ]
        effective = effective_rules(installed_docs, namespace, service_account)
        if not effective:
            fail(
                f"{bundle}: maas-controller ServiceAccount "
                f"{namespace}/{service_account} has no effective RBAC rules"
            )

        applied_roles: list[tuple[str, dict[str, Any]]] = []
        for root in roots:
            if root not in APPLIED_TREES:
                continue
            for doc in rendered[root]:
                if doc.get("kind") in {"Role", "ClusterRole"}:
                    applied_roles.append((root, doc))
        applied_role_names = {
            role.get("metadata", {}).get("name", "") for _, role in applied_roles
        }
        missing_roles = REQUIRED_APPLIED_ROLES - applied_role_names
        if missing_roles:
            fail(
                f"{bundle}: required applied RBAC roles are missing: "
                f"{', '.join(sorted(missing_roles))}"
            )
        if not applied_roles:
            fail(f"{bundle}: no applied RBAC roles were found under the operator-rendered trees")
            continue

        checked = 0
        escalated = 0
        for root, role in applied_roles:
            role_kind = str(role.get("kind", ""))
            role_bare_name = str(role.get("metadata", {}).get("name", ""))
            role_name = f"{role_kind}/{role_bare_name}"
            role_scope = (
                ""
                if role_kind == "ClusterRole"
                else str(role.get("metadata", {}).get("namespace", ""))
            )
            usable_rules = [
                rule for scope, rule in effective if not scope or scope == role_scope
            ]
            # A matching escalate grant lets Kubernetes accept the role write
            # without checking each rule against the controller's own grants.
            if has_escalate_for_role(usable_rules, role_kind, role_bare_name):
                escalated += 1
                continue
            if role.get("aggregationRule") and not any(
                permission_rule_covers(
                    rule,
                    "rbac.authorization.k8s.io",
                    "clusterroles",
                    "escalate",
                    None,
                    "resource",
                )
                for rule in usable_rules
            ):
                fail(
                    f"{bundle}: RBAC escalation closure: maas-controller "
                    f"({namespace}/{service_account}) server-side-applies aggregate {role_name} "
                    f"from {root} without escalate on clusterroles"
                )
            for required_rule in role_rules(role, all_docs):
                checked += 1
                missing = [
                    permission
                    for permission in rule_permissions(required_rule)
                    if not any(
                        permission_rule_covers(rule, *permission) for rule in usable_rules
                    )
                ]
                if missing:
                    fail(
                        f"{bundle}: RBAC escalation closure: maas-controller "
                        f"({namespace}/{service_account}) server-side-applies {role_name} "
                        f"from {root}, but does not hold rule {describe_rule(required_rule)} "
                        f"in scope {role_scope or 'cluster'} (missing {missing})"
                    )
        ok(
            f"{bundle}: RBAC escalation closure checked {checked} applied role rules "
            f"against {len(effective)} scoped effective controller rules"
            + (f" ({escalated} role writes allowed by escalate)" if escalated else "")
        )


def check_manifests(output_dir: Path) -> None:
    rendered, render_outputs = render_all(output_dir)
    if ERRORS:
        raise SystemExit(1)
    for root in REQUIRED_KUSTOMIZATION_ROOTS:
        if root not in rendered:
            if not (ROOT / root / "kustomization.yaml").exists():
                fail(f"required manifest entry point {root} is missing")
            else:
                fail(f"required manifest entry point {root} did not render")
    check_duplicate_env(rendered)
    for bundle in PRODUCTION_BUNDLES:
        if bundle in rendered:
            check_rbac_identity(rendered[bundle], bundle)
    check_escalation_closure(rendered)
    check_kubeconform(render_outputs)
    if ERRORS:
        raise SystemExit(1)
    print(f"Manifest contracts passed for {len(rendered)} kustomize entry points.")


def main() -> None:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("static")
    subparsers.add_parser("targets")
    target_parser = subparsers.add_parser("target")
    target_parser.add_argument("name")
    manifests = subparsers.add_parser("manifests")
    manifests.add_argument("--output-dir", default=tempfile.mkdtemp(prefix="maas-manifests-"))
    args = parser.parse_args()

    if args.command == "static":
        check_static()
    elif args.command == "targets":
        for name, target in sorted(tekton_targets().items()):
            print(f"{name}\t{target['context']}\t{target['dockerfile']}\t{target['output']}")
    elif args.command == "target":
        target = resolve_target(args.name)
        print(f"{target['context']}\t{target['dockerfile']}\t{target['output']}")
    elif args.command == "manifests":
        check_manifests(Path(args.output_dir))
    if ERRORS:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
