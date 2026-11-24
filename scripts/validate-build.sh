#!/usr/bin/env bash
# PR build simulator for the three Konflux images and four Go modules.
# Usage: ./scripts/validate-build.sh {static|hermetic|image|smoke-image|manifests|kind}
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHON="${PYTHON:-python3}"
KUSTOMIZE_VERSION="5.7.1"
KUSTOMIZE_SHA256="ea375e7372f9aa029129d4b2d16c66b7750b7f1213c4f66f910d981c895818d8"
KUBECONFORM_VERSION="0.7.0"
KUBECONFORM_SHA256="c31518ddd122663b3f3aa874cfe8178cb0988de944f29c74a0b9260920d115d3"
HERMETIC_TOOLSET_IMAGE="${HERMETIC_TOOLSET_IMAGE:-registry.access.redhat.com/ubi9/go-toolset@sha256:1a755651ffe1a438f137418d183f18ebad527ec929206c17804f93490a97869e}"
TOOLS_DIR="${TOOLS_DIR:-${RUNNER_TEMP:-/tmp}/maas-konflux-tools}"

log() { printf '\n== %s ==\n' "$*"; }

ensure_kustomize() {
  if [[ -n "${KUSTOMIZE_BIN:-}" ]]; then
    return
  fi
  if [[ "$(uname -s)" != "Linux" ]]; then
    return
  fi
  mkdir -p "$TOOLS_DIR"
  if [[ ! -x "$TOOLS_DIR/kustomize" ]]; then
    local archive="$TOOLS_DIR/kustomize.tar.gz"
    curl -fsSL --retry 3 -o "$archive" \
      "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_amd64.tar.gz"
    echo "${KUSTOMIZE_SHA256}  ${archive}" | sha256sum -c -
    tar -xzf "$archive" -C "$TOOLS_DIR" kustomize
  fi
  KUSTOMIZE_BIN="$TOOLS_DIR/kustomize"
  export KUSTOMIZE_BIN
}

ensure_kubeconform() {
  if [[ -n "${KUBECONFORM_BIN:-}" ]]; then
    return
  fi
  if [[ "$(uname -s)" != "Linux" ]]; then
    return
  fi
  mkdir -p "$TOOLS_DIR"
  if [[ ! -x "$TOOLS_DIR/kubeconform" ]]; then
    local archive="$TOOLS_DIR/kubeconform.tar.gz"
    curl -fsSL --retry 3 -o "$archive" \
      "https://github.com/yannh/kubeconform/releases/download/v${KUBECONFORM_VERSION}/kubeconform-linux-amd64.tar.gz"
    echo "${KUBECONFORM_SHA256}  ${archive}" | sha256sum -c -
    tar -xzf "$archive" -C "$TOOLS_DIR" kubeconform
  fi
  KUBECONFORM_BIN="$TOOLS_DIR/kubeconform"
  export KUBECONFORM_BIN
}

run_static() {
  log "Static Tekton, Dockerfile, embedded-tree, RELATED_IMAGE and codegen contracts"
  cd "$ROOT"
  "$PYTHON" scripts/konflux_targets.py static
}

run_hermetic_module() {
  local module="$1"
  local target=""
  case "$module" in
    maas-api) target="./cmd/" ;;
    maas-controller) target="./cmd/manager" ;;
    maas-common) target="./..." ;;
    maas-discovery) target="./cmd/discovery" ;;
    *) echo "unknown module: $module" >&2; return 2 ;;
  esac
  local work
  work="$(mktemp -d)"
  local modcache="$work/modcache"
  local gocache="$work/buildcache"
  local output="$work/out"
  mkdir -p "$modcache" "$gocache" "$output"
  log "Hermetic preflight: $module ($target)"
  local build_args=(-a -trimpath)
  if [[ "$target" != "./..." ]]; then
    build_args+=(-o "/out/${module}")
  fi
  docker run --rm --platform linux/amd64 \
    --user "$(id -u):$(id -g)" \
    -e HOME=/tmp \
    -e GOTOOLCHAIN=local \
    -e GOMODCACHE=/gomodcache \
    -e GOCACHE=/gocache \
    -e CGO_ENABLED=1 \
    -e GOEXPERIMENT=strictfipsruntime \
    -e GOOS=linux \
    -e GOARCH=amd64 \
    -v "$ROOT/$module:/src:ro" \
    -v "$modcache:/gomodcache" \
    -v "$gocache:/gocache" \
    -v "$output:/out" \
    -w /src \
    --entrypoint /bin/bash \
    "$HERMETIC_TOOLSET_IMAGE" \
    -lc 'set -euo pipefail; GOFLAGS=-mod=readonly go mod download; GOPROXY=off GOSUMDB=off go build "$@"' \
    _ "${build_args[@]}" "$target"
  echo "Hermetic build passed: $module (Red Hat toolset, isolated cache, GOPROXY=off)"
}

run_hermetic() {
  local only="${1:-}"
  if [[ -n "$only" ]]; then
    run_hermetic_module "$only"
    return
  fi
  for module in maas-api maas-controller maas-common maas-discovery; do
    run_hermetic_module "$module"
  done
}

target_fields() {
  local name="$1"
  cd "$ROOT"
  "$PYTHON" scripts/konflux_targets.py target "$name"
}

run_image() {
  local name="${1:?image name required}"
  local context dockerfile output
  IFS=$'\t' read -r context dockerfile output < <(target_fields "$name")
  log "Build Tekton image $name: $output"
  docker build --pull=false -f "$ROOT/$dockerfile" -t "$output" "$ROOT/$context"
  docker image inspect "$output" >/dev/null
  echo "$output"
}

image_tag() {
  local name="$1"
  local context dockerfile output
  IFS=$'\t' read -r context dockerfile output < <(target_fields "$name")
  echo "$output"
}

run_smoke_image() {
  local name="${1:?image name required}"
  local image
  image="$(image_tag "$name")"
  log "Runtime smoke: $name ($image)"
  case "$name" in
    maas-controller)
      docker run --rm --entrypoint /bin/sh "$image" -c '
        set -eu
        test "$(id -u)" = "1001"
        for path in /maas-api/deploy /deployment/base/maas-api /deployment/base/maas-controller/policies \
                    /deployment/base/payload-processing /deployment/base/maas-discovery /deployment/components; do
          test -d "$path" || { echo "missing embedded tree: $path"; exit 1; }
          test -r "$path" || { echo "embedded tree is not readable: $path"; exit 1; }
        done
        test -r /deployment/components/observability/usage-logs/dsci-managed/envoy-otel-access-log.yaml
        echo "controller image is non-root and embedded trees are readable"
      '
      local help_output
      help_output=$(docker run --rm --entrypoint /manager "$image" --help 2>&1 || true)
      grep -q -- '-health-probe-bind-address' <<<"$help_output" || {
        printf '%s\n' "$help_output" >&2
        echo "controller /manager --help does not expose the health probe flag" >&2
        return 1
      }
      ;;
    maas-api)
      docker run --rm --entrypoint /bin/sh "$image" -c '
        set -eu
        test "$(id -u)" = "1001"
        test -x /app/maas-api
        echo "maas-api image is non-root with an executable binary; /health is exercised in Kind"
      '
      ;;
    maas-discovery)
      docker run --rm --entrypoint /bin/sh "$image" -c '
        set -eu
        test "$(id -u)" = "1001"
        test -x /app/discovery
        echo "discovery image is non-root with an executable binary"
      '
      docker rm -f maas-discovery-smoke >/dev/null 2>&1 || true
      docker run -d --name maas-discovery-smoke -p 18443:8443 "$image" --self-signed --addr=:8443 >/dev/null
      for _ in $(seq 1 45); do
        if curl -fsSk https://127.0.0.1:18443/healthz >/dev/null 2>&1; then
          echo "discovery /healthz passed"
          docker rm -f maas-discovery-smoke >/dev/null
          return 0
        fi
        sleep 1
      done
      docker logs maas-discovery-smoke >&2 || true
      docker rm -f maas-discovery-smoke >/dev/null 2>&1 || true
      echo "discovery did not become healthy" >&2
      return 1
      ;;
    *) echo "unknown image: $name" >&2; return 2 ;;
  esac
}

run_manifests() {
  ensure_kustomize
  ensure_kubeconform
  local out="${RENDER_DIR:-$(mktemp -d)}"
  mkdir -p "$out"
  local require_kubeconform=0
  if [[ -n "${KUBECONFORM_BIN:-}" ]]; then
    require_kubeconform=1
  fi
  log "Render every kustomize entry point and validate schemas, env names and RBAC closure"
  cd "$ROOT"
  KUSTOMIZE_BIN="${KUSTOMIZE_BIN:-}" KUBECONFORM_BIN="${KUBECONFORM_BIN:-}" \
    REQUIRE_KUBECONFORM="$require_kubeconform" \
    "$PYTHON" scripts/konflux_targets.py manifests --output-dir "$out"
}

create_namespace() {
  kubectl create namespace "$1" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

download_crds() {
  local url="$1" name="$2"
  local source="$KIND_WORK/$name.source.yaml" crds="$KIND_WORK/$name.crds.yaml"
  curl -fsSL --retry 3 -o "$source" "$url"
  "$PYTHON" - "$source" "$crds" <<'PY'
import sys, yaml
source, target = sys.argv[1:3]
docs = [d for d in yaml.safe_load_all(open(source, encoding="utf-8")) if isinstance(d, dict)]
crds = [d for d in docs if d.get("kind") == "CustomResourceDefinition"]
if not crds:
    raise SystemExit(f"no CRDs found in {source}")
with open(target, "w", encoding="utf-8") as h:
    yaml.safe_dump_all(crds, h, sort_keys=False)
print(f"installing {len(crds)} CRDs from {target}")
PY
  kubectl apply --server-side --force-conflicts -f "$crds" >/dev/null
}

dump_controller_state() {
  echo "--- controller pods ---" >&2
  kubectl -n opendatahub get pods -l control-plane=maas-controller -o wide >&2 || true
  echo "--- controller pod description ---" >&2
  kubectl -n opendatahub describe pod -l control-plane=maas-controller >&2 || true
  echo "--- controller deployment ---" >&2
  kubectl -n opendatahub describe deployment maas-controller >&2 || true
  echo "--- controller termination ---" >&2
  kubectl -n opendatahub get pods -l control-plane=maas-controller -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{range .status.containerStatuses[*]}{.name}{": exit="}{.lastState.terminated.exitCode}{" reason="}{.lastState.terminated.reason}{" message="}{.lastState.terminated.message}{"\n"}{end}{end}' >&2 || true
  echo "--- controller logs ---" >&2
  kubectl -n opendatahub logs -l control-plane=maas-controller --all-containers --tail=200 --prefix >&2 || true
  echo "--- previous controller logs ---" >&2
  kubectl -n opendatahub logs -l control-plane=maas-controller --all-containers --tail=200 --previous --prefix >&2 || true
}

assert_controller_stable() {
  local phase="$1"
  kubectl -n opendatahub wait --for=condition=Available deployment/maas-controller --timeout=120s >/dev/null
  kubectl -n opendatahub wait --for=condition=Ready pod -l control-plane=maas-controller --timeout=120s >/dev/null
  "$PYTHON" - "$phase" <<'PY'
import json, subprocess, sys

phase = sys.argv[1]
pods = json.loads(subprocess.check_output([
    "kubectl", "-n", "opendatahub", "get", "pods",
    "-l", "control-plane=maas-controller", "-o", "json",
], text=True))
problems = []
for pod in pods.get("items", []):
    name = pod.get("metadata", {}).get("name", "<unknown>")
    if pod.get("status", {}).get("phase") != "Running":
        problems.append(f"{name} phase={pod.get('status', {}).get('phase')}")
    for status in pod.get("status", {}).get("containerStatuses", []) or []:
        if not status.get("ready"):
            problems.append(f"{name}/{status.get('name')} is not ready")
        if status.get("restartCount", 0):
            problems.append(f"{name}/{status.get('name')} restarted {status['restartCount']} time(s)")
if not pods.get("items"):
    problems.append("no maas-controller pods found")
if problems:
    raise SystemExit(f"controller stability failed {phase}: {'; '.join(problems)}")
print(f"controller remained running without restarts {phase}")
PY
}

run_kind() {
  local controller_image="${CONTROLLER_IMAGE:?CONTROLLER_IMAGE is required}"
  local maas_api_image="${MAAS_API_IMAGE:?MAAS_API_IMAGE is required}"
  local cluster="${KIND_CLUSTER_NAME:-maas-konflux}"
  local namespace="opendatahub"
  local tenant_ns="models-as-a-service"
  local infra_ns="odh-ai-gateway-infra"
  local gateway_ns="openshift-ingress"
  local tenant="ci-tenant"
  local tenant_source="ci-source"
  KIND_WORK="$(mktemp -d)"
  trap 'rm -rf "$KIND_WORK"' EXIT
  log "Controller Kind integration on cluster $cluster"
  ensure_kustomize
  : "${KUSTOMIZE_BIN:?KUSTOMIZE_BIN is required for Kind integration}"
  kubectl config use-context "kind-$cluster" >/dev/null

  for ns in "$namespace" "$tenant_ns" "$infra_ns" "$gateway_ns" ai-tenants; do
    create_namespace "$ns"
  done

  log "Install repository CRDs"
  kubectl apply --server-side --force-conflicts -k "$ROOT/deployment/base/maas-controller/crd" >/dev/null
  kubectl wait --for=condition=Established --timeout=180s \
    crd/aitenants.maas.opendatahub.io \
    crd/configs.maas.opendatahub.io \
    crd/maasauthpolicies.maas.opendatahub.io \
    crd/maasmodelrefs.maas.opendatahub.io \
    crd/maassubscriptions.maas.opendatahub.io \
    crd/maastenantconfigs.maas.opendatahub.io \
    crd/externalmodels.maas.opendatahub.io \
    crd/externalproviders.inference.opendatahub.io

  log "Install the exact upstream CRDs the controller renders against"
  download_crds "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml" gateway-api
  download_crds "https://github.com/cert-manager/cert-manager/releases/download/v1.18.2/cert-manager.crds.yaml" cert-manager
  download_crds "https://raw.githubusercontent.com/Kuadrant/authorino/v0.22.0/install/crd/authorino.kuadrant.io_authconfigs.yaml" authorino
  # The Kuadrant v1.3.1 release publishes only a Helm chart, so the CRDs come
  # from the bundle at the tag. The controller watches both the kuadrant.io and
  # extensions.kuadrant.io groups.
  for crd in \
    extensions.kuadrant.io_oidcpolicies.yaml \
    extensions.kuadrant.io_planpolicies.yaml \
    extensions.kuadrant.io_telemetrypolicies.yaml \
    kuadrant.io_authpolicies.yaml \
    kuadrant.io_dnspolicies.yaml \
    kuadrant.io_kuadrants.yaml \
    kuadrant.io_ratelimitpolicies.yaml \
    kuadrant.io_tlspolicies.yaml \
    kuadrant.io_tokenratelimitpolicies.yaml; do
    download_crds "https://raw.githubusercontent.com/Kuadrant/kuadrant-operator/v1.3.1/bundle/manifests/${crd}" "kuadrant-${crd%.yaml}"
  done
  download_crds "https://raw.githubusercontent.com/istio/istio/1.26.3/manifests/charts/base/files/crd-all.gen.yaml" istio
  # The production bundle renders a ServiceMonitor, and a vanilla Kind cluster
  # has no Prometheus operator CRDs to accept it.
  download_crds "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.89.0/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml" prometheus

  log "Create the webhook serving certificate"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout "$KIND_WORK/tls.key" -out "$KIND_WORK/tls.crt" \
    -subj "/CN=maas-controller-webhook-service.$namespace.svc" \
    -addext "subjectAltName=DNS:maas-controller-webhook-service.$namespace.svc,DNS:maas-controller-webhook-service.$namespace.svc.cluster.local" \
    >/dev/null 2>&1
  kubectl -n "$namespace" create secret tls maas-controller-webhook-cert \
    --cert="$KIND_WORK/tls.crt" --key="$KIND_WORK/tls.key" --dry-run=client -o yaml \
    | kubectl apply -f - >/dev/null

  log "Render and patch the production controller bundle"
  "$KUSTOMIZE_BIN" build --load-restrictor LoadRestrictionsNone "$ROOT/deployment/base/maas-controller/default" > "$KIND_WORK/controller.yaml"
  WEBHOOK_CA_B64="$(base64 < "$KIND_WORK/tls.crt" | tr -d '\n')" \
  CONTROLLER_IMAGE="$controller_image" MAAS_API_IMAGE="$maas_api_image" \
    "$PYTHON" - "$KIND_WORK/controller.yaml" "$KIND_WORK/controller-patched.yaml" <<'PY'
import os, sys, yaml
source, target = sys.argv[1:3]
controller_image = os.environ["CONTROLLER_IMAGE"]
maas_api_image = os.environ["MAAS_API_IMAGE"]
ca_bundle = os.environ["WEBHOOK_CA_B64"]
docs = [d for d in yaml.safe_load_all(open(source, encoding="utf-8")) if isinstance(d, dict)]
for doc in docs:
    kind = doc.get("kind")
    name = doc.get("metadata", {}).get("name")
    if kind == "Service" and name == "maas-controller-webhook-service":
        doc["metadata"]["namespace"] = "opendatahub"
    if kind == "ValidatingWebhookConfiguration":
        for webhook in doc.get("webhooks", []):
            webhook.setdefault("clientConfig", {}).setdefault("service", {})["namespace"] = "opendatahub"
            webhook["clientConfig"]["caBundle"] = ca_bundle
    if kind == "ConfigMap" and name == "maas-parameters":
        doc.setdefault("data", {})["maas-api-image"] = maas_api_image
        doc["data"]["maas-controller-image"] = controller_image
    if kind == "Deployment" and name == "maas-controller":
        pod = doc["spec"]["template"]["spec"]
        container = pod["containers"][0]
        container["image"] = controller_image
        container["imagePullPolicy"] = "IfNotPresent"
        args = []
        for arg in container.get("args", []):
            if arg.startswith("--metrics-secure="):
                continue
            if arg.startswith("--infra-namespace="):
                arg = "--infra-namespace=odh-ai-gateway-infra"
            args.append(arg)
        args.append("--metrics-secure=false")
        container["args"] = args
        container["volumeMounts"] = [
            mount for mount in container.get("volumeMounts", []) if mount.get("name") != "metrics-certs"
        ]
        pod["volumes"] = [volume for volume in pod.get("volumes", []) if volume.get("name") != "metrics-certs"]
        for env in container.get("env", []):
            if env.get("name") == "INFRA_NAMESPACE":
                env["value"] = "odh-ai-gateway-infra"
with open(target, "w", encoding="utf-8") as handle:
    yaml.safe_dump_all(docs, handle, sort_keys=False)
PY
  # The generated maas-parameters ConfigMap carries no explicit namespace, so the
  # bundle must be applied with -n or it lands in the caller's default namespace.
  # The Deployment reads it through configMapKeyRef; when the lookup misses, the
  # $(VAR) args stay literal and the manager exits before the logger is set,
  # producing a silent CrashLoopBackOff.
  kubectl -n "$namespace" apply --server-side --force-conflicts -f "$KIND_WORK/controller-patched.yaml" >/dev/null
  if ! kubectl -n "$namespace" get configmap maas-parameters >/dev/null 2>&1; then
    echo "maas-parameters ConfigMap is missing from namespace $namespace; the controller would exit before logging" >&2
    return 1
  fi
  if ! kubectl -n "$namespace" rollout status deployment/maas-controller --timeout=240s; then
    dump_controller_state
    return 1
  fi

  log "Wait for Config/default created by the controller"
  for _ in $(seq 1 60); do
    if kubectl get configs.maas.opendatahub.io/default >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
  if ! kubectl get configs.maas.opendatahub.io/default >/dev/null 2>&1; then
    dump_controller_state
    echo "controller did not create Config/default" >&2
    return 1
  fi
  assert_controller_stable "after Config/default bootstrap"

  log "Deploy a test PostgreSQL instance and the database secret"
  kubectl -n "$infra_ns" apply -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  replicas: 1
  selector:
    matchLabels: {app: postgres}
  template:
    metadata:
      labels: {app: postgres}
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - {name: POSTGRES_USER, value: maas}
            - {name: POSTGRES_PASSWORD, value: maas}
            - {name: POSTGRES_DB, value: maas}
          ports:
            - {containerPort: 5432}
          readinessProbe:
            exec:
              command: [pg_isready, -U, maas]
            initialDelaySeconds: 3
            periodSeconds: 3
          volumeMounts:
            - {name: data, mountPath: /var/lib/postgresql/data}
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
spec:
  selector: {app: postgres}
  ports:
    - {port: 5432, targetPort: 5432}
YAML
  kubectl -n "$infra_ns" rollout status deployment/postgres --timeout=180s
  kubectl -n "$infra_ns" create secret generic maas-db-config \
    --from-literal=DB_CONNECTION_URL="postgresql://maas:maas@postgres.$infra_ns.svc.cluster.local:5432/maas?sslmode=disable" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$tenant_ns" create secret generic maas-db-config \
    --from-literal=DB_CONNECTION_URL="postgresql://maas:maas@postgres.$infra_ns.svc.cluster.local:5432/maas?sslmode=disable" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  log "Create test-only TLS material normally supplied by OpenShift service-ca"
  kubectl -n "$infra_ns" create secret tls "maas-api-$tenant-serving-cert" \
    --cert="$KIND_WORK/tls.crt" --key="$KIND_WORK/tls.key" --dry-run=client -o yaml \
    | kubectl apply -f - >/dev/null
  kubectl -n "$infra_ns" create secret tls maas-api-metrics-tls \
    --cert="$KIND_WORK/tls.crt" --key="$KIND_WORK/tls.key" --dry-run=client -o yaml \
    | kubectl apply -f - >/dev/null
  kubectl -n "$infra_ns" create configmap openshift-service-ca.crt \
    --from-file=service-ca.crt="$KIND_WORK/tls.crt" --dry-run=client -o yaml \
    | kubectl apply -f - >/dev/null

  log "Create the Gateway and its Service, then the AITenant-backed tenant config"
  kubectl -n "$gateway_ns" apply -f - >/dev/null <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
spec:
  gatewayClassName: istio
  listeners:
    - name: http
      port: 80
      protocol: HTTP
YAML
  local gateway_uid
  gateway_uid="$(kubectl -n "$gateway_ns" get gateway maas-default-gateway -o jsonpath='{.metadata.uid}')"
  GATEWAY_UID="$gateway_uid" "$PYTHON" - "$KIND_WORK/gateway-service.yaml" <<'PY'
import os, sys, yaml
doc = {
    "apiVersion": "v1",
    "kind": "Service",
    "metadata": {
        "name": "maas-default-gateway",
        "namespace": "openshift-ingress",
        "labels": {"gateway.networking.k8s.io/gateway-name": "maas-default-gateway"},
        "ownerReferences": [{
            "apiVersion": "gateway.networking.k8s.io/v1",
            "kind": "Gateway",
            "name": "maas-default-gateway",
            "uid": os.environ["GATEWAY_UID"],
        }],
    },
    "spec": {
        "selector": {"istio": "ingressgateway"},
        "ports": [{"name": "https", "port": 443, "targetPort": 8443}],
    },
}
yaml.safe_dump(doc, open(sys.argv[1], "w", encoding="utf-8"))
PY
  kubectl apply -f "$KIND_WORK/gateway-service.yaml" >/dev/null
  kubectl -n ai-tenants apply -f - >/dev/null <<YAML
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: $tenant_source
spec:
  gateway:
    name: maas-default-gateway
YAML
  kubectl -n ai-tenants patch aitenant "$tenant_source" --subresource=status --type=merge \
    -p '{"status":{"gatewayRef":{"name":"maas-default-gateway","namespace":"openshift-ingress"}}}' >/dev/null
  kubectl -n "$tenant_ns" apply -f - >/dev/null <<YAML
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: $tenant
  labels:
    maas.opendatahub.io/managed-by-aitenant: "true"
    maas.opendatahub.io/tenant-name: $tenant
  annotations:
    maas.opendatahub.io/aitenant-name: $tenant_source
    maas.opendatahub.io/aitenant-namespace: ai-tenants
spec: {}
YAML

  log "Assert the tenant render uses the PR-built maas-api image"
  for _ in $(seq 1 90); do
    rendered_image="$(kubectl -n "$infra_ns" get deployment "maas-api-$tenant" -o jsonpath='{.spec.template.spec.containers[?(@.name=="maas-api")].image}' 2>/dev/null || true)"
    if [[ "$rendered_image" == "$maas_api_image" ]]; then
      break
    fi
    sleep 2
  done
  if [[ "$rendered_image" != "$maas_api_image" ]]; then
    echo "tenant did not render the PR maas-api image" >&2
    echo "expected: $maas_api_image" >&2
    echo "actual:   ${rendered_image:-<deployment not found>}" >&2
    kubectl -n "$tenant_ns" get maastenantconfigs.maas.opendatahub.io "$tenant" -o yaml >&2 || true
    kubectl -n "$infra_ns" describe deployment "maas-api-$tenant" >&2 || true
    dump_controller_state
    return 1
  fi
  if [[ "$rendered_image" == *":latest" ]]; then
    echo "tenant render used :latest instead of the PR image" >&2
    return 1
  fi
  echo "tenant render image: $rendered_image"
  assert_controller_stable "after Tenant reconciliation"

  log "Assert maas-api starts and serves its health endpoint"
  kubectl -n "$infra_ns" rollout status deployment/"maas-api-$tenant" --timeout=240s
  kubectl -n "$infra_ns" port-forward svc/"maas-api-$tenant" 18444:8443 >"$KIND_WORK/port-forward.log" 2>&1 &
  local forward_pid=$!
  trap 'kill "$forward_pid" 2>/dev/null || true; rm -rf "$KIND_WORK"' EXIT
  for _ in $(seq 1 45); do
    if curl -fsSk https://127.0.0.1:18444/health >/dev/null 2>&1; then
      echo "maas-api /health passed"
      kill "$forward_pid" 2>/dev/null || true
      trap 'rm -rf "$KIND_WORK"' EXIT
      return 0
    fi
    sleep 1
  done
  cat "$KIND_WORK/port-forward.log" >&2 || true
  kubectl -n "$infra_ns" logs deployment/"maas-api-$tenant" --tail=200 >&2 || true
  echo "maas-api did not become healthy" >&2
  return 1
}

main() {
  local command="${1:-}"
  case "$command" in
    static) run_static ;;
    hermetic) run_hermetic "${2:-}" ;;
    image) run_image "${2:-}" ;;
    smoke-image) run_smoke_image "${2:-}" ;;
    manifests) run_manifests ;;
    kind) run_kind ;;
    *) echo "usage: $0 {static|hermetic [module]|image <name>|smoke-image <name>|manifests|kind}" >&2; exit 2 ;;
  esac
}

main "$@"
