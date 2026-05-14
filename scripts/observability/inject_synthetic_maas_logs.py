#!/usr/bin/env python3
"""
Dev-only synthetic OTLP logs for the maas-gateway → OTel collector → Loki path.

**Do not run against production** unless you intend to pollute billing/usage data.

Canonical field list and Loki shape live in repo root:
  `envoy-otel-structured-logs-plan.md` (stream labels vs structured attributes, sample
  `query_range` JSON under "Full Loki query response JSON").

Envoy emits these log **attributes** (see `deployment/components/observability/otel-collector/envoy-otel-access-log.yaml`):
  user_id, subscription, tokens_total, tokens_prompt, tokens_completion, model,
  response_code, method, path, duration_ms, request_id, authority, route_name,
  downstream_remote_address, upstream_cluster, bytes_received, bytes_sent,
  response_code_details

The collector (`otel-collector-configmap.yaml`) upserts resource attributes
(service.name=maas-gateway, service.namespace, kubernetes_namespace_name, …) and runs
`groupbyattrs` on subscription, model, response_code, method so they align with
dashboard selectors like `{service_name="maas-gateway", ...} | unwrap tokens_total`.

Prerequisite: reach the collector OTLP **gRPC** receiver (this repo exposes 4317 only):
  kubectl port-forward -n openshift-ingress svc/otel-collector 4317:4317

Install:
  python3 -m venv .venv && . .venv/bin/activate
  pip install -r scripts/observability/requirements-synthetic-maas-logs.txt

Example (recent timestamps only — required for Loki ingest; 7d spread will be rejected):
  python3 scripts/observability/inject_synthetic_maas_logs.py \\
    --endpoint localhost:4317 --insecure --count 500 --spread-hours 1

If the script prints success but Loki shows nothing, check the collector:
  kubectl logs -n openshift-ingress deploy/otel-collector | grep -i 'Dropping data\\|otlphttp/loki'
  Common causes: Loki gateway timeouts (see repo NetworkPolicy otel→gateway) or exporter batch too large.

Non-admin Perses users only see their own ``user_id`` (Loki proxy). Use ``--user-id`` only for dev demos
matching an identity you can query (avoid production clusters).
"""

from __future__ import annotations

import argparse
import random
import sys
import time
import uuid
from datetime import datetime, timedelta, timezone

from opentelemetry._logs import LogRecord, SeverityNumber, get_logger, set_logger_provider
from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.resources import Resource

# Fixed synthetic identity so operators can filter or drop in LogQL if needed.
SYNTH_USER_ID = "synthetic-dev-maas-loadgen"

MODELS = (
    "facebook/opt-125m",
    "the-best-gpt-3.5-m",
    "meta-llama/Llama-3.2-1B-Instruct",
)
SUBSCRIPTIONS = (
    "simulator-subscription",
    "standard-subscription",
    "dev-subscription-a",
    "dev-subscription-b",
)


def _build_attributes(
    *,
    user_id: str,
    model: str,
    subscription: str,
    response_code: str,
    path: str,
) -> dict[str, str]:
    tokens_prompt = random.randint(8, 120)
    tokens_completion = random.randint(10, 200)
    tokens_total = str(tokens_prompt + tokens_completion)
    duration_ms = str(random.randint(12, 180))
    bytes_received = str(random.randint(200, 900))
    bytes_sent = str(random.randint(300, 1200))
    route_slug = model.replace("/", "-").replace(".", "-").lower()[:48]
    return {
        "user_id": user_id,
        "subscription": subscription,
        "tokens_total": tokens_total,
        "tokens_prompt": str(tokens_prompt),
        "tokens_completion": str(tokens_completion),
        "model": model,
        "response_code": response_code,
        "method": "POST",
        "path": path,
        "duration_ms": duration_ms,
        "request_id": str(uuid.uuid4()),
        "authority": "maas-synthetic.local",
        "route_name": f"llm.{route_slug}-simulated-kserve-route.1",
        "downstream_remote_address": "10.0.0.1:12345",
        "upstream_cluster": f"outbound|8000||{route_slug}-workload-svc.llm.svc.cluster.local;",
        "bytes_received": bytes_received,
        "bytes_sent": bytes_sent,
        "response_code_details": "via_upstream",
    }


def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument(
        "--endpoint",
        default="localhost:4317",
        help="OTLP gRPC host:port (default localhost:4317 after port-forward).",
    )
    p.add_argument(
        "--insecure",
        action="store_true",
        help="Use plaintext gRPC (typical for local port-forward).",
    )
    p.add_argument("--count", type=int, default=400, help="Number of log records to emit.")
    p.add_argument(
        "--spread-hours",
        type=float,
        default=1.0,
        help="Spread timestamps over [now-spread, now]. Keep small (e.g. <=24h): Loki rejects "
        "OTLP logs older than the stack's allowed past skew (wide spreads return HTTP 400 and the "
        "collector drops the batch).",
    )
    p.add_argument(
        "--error-rate",
        type=float,
        default=0.02,
        help="Fraction of records with non-2xx response_code (default 0.02).",
    )
    p.add_argument("--seed", type=int, default=None, help="RNG seed for reproducibility.")
    p.add_argument(
        "--user-id",
        default=SYNTH_USER_ID,
        help=f"Log user_id attribute (default {SYNTH_USER_ID!r}). Match your Loki-visible identity in dev.",
    )
    return p.parse_args()


def main() -> int:
    args = _parse_args()
    if args.count < 1:
        print("count must be >= 1", file=sys.stderr)
        return 2

    rng = random.Random(args.seed)
    now = datetime.now(timezone.utc)
    start = now - timedelta(hours=args.spread_hours)

    resource = Resource.create(
        {
            # Collector upserts service.name anyway; keep logger identity obvious in traces of export issues.
            "service.name": "maas-synthetic-log-injector",
        }
    )
    exporter = OTLPLogExporter(endpoint=args.endpoint, insecure=bool(args.insecure))
    logger_provider = LoggerProvider(resource=resource)
    logger_provider.add_log_record_processor(BatchLogRecordProcessor(exporter))
    set_logger_provider(logger_provider)
    logger = get_logger("inject_synthetic_maas_logs", "1.0.0")

    span_s = max((args.spread_hours * 3600.0) / max(args.count, 1), 0.001)
    for i in range(args.count):
        if args.spread_hours > 0 and args.count > 1:
            # Even spacing with small jitter so every ~5m bucket gets points when spread_hours is large.
            frac = i / (args.count - 1)
            jitter = rng.uniform(-0.35 * span_s, 0.35 * span_s) if args.count > 2 else 0.0
            ts = start + timedelta(seconds=frac * args.spread_hours * 3600.0 + jitter)
        else:
            ts = now
        ts_ns = int(ts.timestamp() * 1e9)

        model = rng.choice(MODELS)
        subscription = rng.choice(SUBSCRIPTIONS)
        path = "/v1/chat/completions" if rng.random() > 0.08 else "/v1/models"
        if rng.random() < args.error_rate:
            response_code = "429" if rng.random() < 0.5 else "503"
        else:
            response_code = "200"

        attrs = _build_attributes(
            user_id=args.user_id,
            model=model,
            subscription=subscription,
            response_code=response_code,
            path=path,
        )
        body = f"{response_code} {attrs['method']} {path}"
        record = LogRecord(
            timestamp=ts_ns,
            body=body,
            attributes=attrs,
            severity_number=SeverityNumber.INFO,
            severity_text="INFO",
        )
        logger.emit(record)

    logger_provider.shutdown()
    print(
        f"Emitted {args.count} synthetic records to {args.endpoint} "
        f"(user_id={args.user_id!r}, spread_hours={args.spread_hours}). "
        "If totals in Loki do not move, inspect otel-collector logs for otlphttp/loki drops."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
