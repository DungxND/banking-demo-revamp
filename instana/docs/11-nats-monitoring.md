# NATS Monitoring — banking-demo golang branch

> **Sources:**
> - https://docs.nats.io/running-a-nats-service/nats_admin/monitoring
> - https://github.com/nats-io/prometheus-nats-exporter
> - https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-messaging
>
> Condensed for: NATS 2.x (`nats:2-alpine`) running in k3s, banking namespace.
> Instana has **no native NATS sensor** — observability flows via Prometheus + OTLP.

---

## NATS in banking-demo

The golang branch replaces RabbitMQ with NATS for all inter-service messaging:

```
api-producer (Go Chi HTTP)
  └─ NATS request/reply (nats/micro subjects)
       ├─ banking.auth.*        → auth-service
       ├─ banking.accounts.*    → account-service
       ├─ banking.transfers.*   → transfer-service
       └─ banking.notifications.* → notification-service
```

JetStream is enabled for durable notification events (subject `banking.notifications.>`).

See [`ARCH-NATS-RPC.md`](../../ARCH-NATS-RPC.md) for the full subject hierarchy.

---

## NATS HTTP Monitoring Port

NATS exposes a built-in HTTP monitoring endpoint on **port 8222** when enabled. It provides
JSON stats for connections, routes, subscriptions, JetStream, and individual accounts.

### Key endpoints

| Path | Description |
|------|-------------|
| `GET /varz` | Server stats: connections, memory, uptime, CPU |
| `GET /connz` | Per-connection details (subscriptions, messages, bytes) |
| `GET /subsz` | Subscription stats per subject |
| `GET /jsz` | JetStream server summary |
| `GET /jsz?accounts=true` | Per-account JetStream stats |
| `GET /jsz?consumers=true&streams=true` | Streams + consumers with lag metrics |
| `GET /healthz` | Liveness: `{"status":"ok"}` |

### Enable in docker-compose

```yaml
# docker-compose.yml — nats service
services:
  nats:
    image: nats:2-alpine
    command: ["-js", "-m", "8222"]  # -m enables HTTP monitoring on 8222
    ports:
      - "4222:4222"   # client
      - "8222:8222"   # monitoring (localhost only in prod)
```

### Enable in Helm (k3s)

```yaml
# helm/values.yaml — nats section
nats:
  args: ["-js", "-m", "8222"]
```

The monitoring port is internal to the pod. To query it from outside the cluster:

```bash
kubectl -n banking port-forward svc/nats 8222:8222 &
curl -s http://localhost:8222/varz | jq '{connections, total_connections, mem, uptime}'
curl -s http://localhost:8222/jsz?consumers=true | jq '.account_details[].streams[].consumer_detail[].num_pending'
```

---

## Prometheus Integration via nats-exporter

The [prometheus-nats-exporter](https://github.com/nats-io/prometheus-nats-exporter) converts
the NATS HTTP monitoring endpoints into Prometheus metrics. It is the recommended path for
surfacing NATS metrics in Instana (which scrapes Prometheus endpoints).

### Quick start (docker-compose)

```yaml
# Add to docker-compose.yml
  nats-exporter:
    image: natsio/prometheus-nats-exporter:latest
    command:
      - "-varz"
      - "-jsz=all"
      - "-connz"
      - "http://nats:8222"
    ports:
      - "7777:7777"   # Prometheus scrape target
```

### Helm deployment (k3s)

```yaml
# helm/templates/nats-exporter.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nats-exporter
  namespace: banking
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: exporter
          image: natsio/prometheus-nats-exporter:latest
          args: ["-varz", "-jsz=all", "-connz", "http://nats:8222"]
          ports:
            - containerPort: 7777
```

---

## Key Prometheus Metrics (from nats-exporter)

| Metric | Description |
|--------|-------------|
| `gnatsd_varz_connections` | Current client connections |
| `gnatsd_varz_total_connections` | Total connections since start |
| `gnatsd_varz_in_msgs` | Messages received/sec |
| `gnatsd_varz_out_msgs` | Messages sent/sec |
| `gnatsd_varz_mem` | Server memory usage (bytes) |
| `gnatsd_varz_uptime` | Server uptime (seconds) |
| `gnatsd_jsz_streams` | Number of JetStream streams |
| `gnatsd_jsz_consumers` | Number of JetStream consumers |
| `gnatsd_jsz_messages` | Total JetStream messages stored |
| `gnatsd_jsz_bytes` | Total JetStream bytes stored |

### JetStream consumer lag (pending messages)

Consumer lag is not directly exposed as a single metric by nats-exporter. Query it via the
HTTP API and expose as a custom metric, or use:

```bash
# Check pending messages for all JetStream consumers
curl -s 'http://localhost:8222/jsz?consumers=true&streams=true' \
  | jq '.account_details[].streams[].consumer_detail[] | {name, num_pending, num_ack_pending}'
```

---

## nats CLI — Real-Time Stats

The `nats` CLI (`github.com/nats-io/natscli`) provides live monitoring:

```bash
# Server overview
nats server info

# Live throughput dashboard
nats server watch

# JetStream stream stats
nats stream ls
nats stream info banking_notifications

# Consumer lag per subject
nats consumer ls banking_notifications
nats consumer info banking_notifications push-consumer

# nats/micro service endpoints (banking-demo services)
nats micro ls
nats micro stats
nats micro stats banking-auth
```

`nats micro stats` shows per-endpoint request counts, error rates, and average latency for
the `nats/micro` services registered by banking-demo consumers.

---

## Instana Integration Path

Instana has no native NATS sensor. The recommended integration is:

```
NATS :8222 (HTTP monitoring)
  └─ nats-exporter :7777 (Prometheus)
        └─ Instana agent (Prometheus scrape, auto-discovered)
              └─ Instana UI → Infrastructure → Custom Metrics
```

The DaemonSet agent auto-discovers Prometheus endpoints via pod annotations. The nats-exporter
pod carries the standard annotations (see `helm/templates/nats.yaml`):

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "7777"
```

This is handled by the `prometheusAnnotations: strict` setting in `configuration.yaml` —
no explicit `endpoints:` block is needed. To add one manually as a fallback:

```yaml
# instana/configuration.yaml — explicit Prometheus scrape (fallback only)
com.instana.plugin.prometheus:
  prometheusAnnotations: strict
  # endpoints:
  #   - url: http://nats-exporter.banking.svc.cluster.local:7777/metrics
  #     poll_rate: 10
```

> **Instana NATS tracing:** IBM Instana has no native Go NATS sensor. Distributed trace
> continuity across the NATS boundary is achieved via **W3C `traceparent` header propagation**:
>
> - **Producer** ([`producer/rpc.go`](../../producer/rpc.go)) — after creating the `rpc.request`
>   OTel span, calls `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(hdr))`
>   to write `traceparent` into the NATS message headers before publish.
> - **Consumer** ([`internal/nats/consumer.go`](../../internal/nats/consumer.go)) — in `dispatch`,
>   calls `otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(req.Headers()))`
>   to resume the producer's trace as a child span, before calling the handler.
>
> The `propagation.TraceContext{}` propagator is registered globally by
> [`internal/tracing.Init()`](../../internal/tracing/tracing.go) at every service startup.
> Instana maps the OTel messaging semantic conventions (`messaging.system=nats`,
> `messaging.destination.name=<subject>`) to its service dependency graph —
> the same arrows and latency charts as a native sensor would produce.
> See [`03-opentelemetry.md`](./03-opentelemetry.md) for the full OTel picture.

---

## Related Docs

| File | What it covers |
|------|----------------|
| [`03-opentelemetry.md`](./03-opentelemetry.md) | Go OTel SDK, NATS W3C trace propagation |
| [`09-pod-service-detection.md`](./09-pod-service-detection.md) | Why NATS consumer services may not appear in Applications → Services |
| [`ARCH-NATS-RPC.md`](../../ARCH-NATS-RPC.md) | Full NATS subject hierarchy and nats/micro patterns |
