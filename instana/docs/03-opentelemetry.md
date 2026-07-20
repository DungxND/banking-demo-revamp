# OpenTelemetry — Instana Agent OTLP Ingestion

> **Source:** https://www.ibm.com/docs/en/instana-observability/current?topic=opentelemetry
> https://www.ibm.com/docs/en/instana-observability/current?topic=instana-agent (Sending OTel to agent)
> Condensed for: banking-demo Go microservices sending OTLP → Instana DaemonSet agent on k3s

---

## How OTel Fits into banking-demo

```
Go service (api-producer / auth / account / transfer / notification)
  └─ go.opentelemetry.io/otel SDK
       └─ OTLP/gRPC exporter → http://$(NODE_IP):4317
                                        │              (or cluster-DNS Service — see below)
                              Instana DaemonSet agent pod (instana-agent namespace)
                                        │
                              Instana backend (SaaS) :443
```

Every service calls [`internal/tracing.Init()`](../../internal/tracing/tracing.go) at startup:

- Configures an `otlptracegrpc` exporter pointing at `OTEL_EXPORTER_OTLP_ENDPOINT`
- Registers a `TracerProvider` with a batch exporter and W3C `TraceContext` propagator
- Returns a shutdown function that flushes buffered spans within 5 s on `SIGTERM`
- When `OTEL_EXPORTER_OTLP_ENDPOINT` is empty, installs a no-op propagator and skips export silently

`api-producer` and `notification-service` additionally wrap their Chi routers with
`otelhttp.NewHandler` so every HTTP request (and WebSocket upgrade for `/ws`) automatically
generates an OTel span — no per-handler instrumentation needed.

---

## Instana Agent OTLP Config

The agent accepts OTLP by default (agent ≥ 1.1.726). Explicit config in [`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.opentelemetry:
  grpc:
    enabled: true
    port: 4317
  http:
    enabled: true
    port: 4318
```

### Ports

| Protocol | Port | Used by |
|----------|------|---------|
| OTLP/gRPC | 4317 | Go services, Traefik, Nginx (`ngx_otel_module`) |
| OTLP/HTTP | 4318 | Kong OTel plugin |

---

## Pod Environment Variables

Set in each Deployment template (e.g. [`helm/templates/auth-service.yaml`](../../helm/templates/auth-service.yaml)):

```yaml
env:
  - name: NODE_IP
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "http://$(NODE_IP):4317"
  - name: INSTANA_AGENT_HOST       # go-sensor native protocol
    value: "$(NODE_IP)"
  - name: OTEL_SERVICE_NAME
    value: auth-service
  - name: OTEL_RESOURCE_ATTRIBUTES
    value: "service.namespace=banking-demo"
```

`internal/tracing.Init()` reads `OTEL_EXPORTER_OTLP_ENDPOINT` directly. The service name is
hardcoded as the `serviceName` constant in each service's `main.go` and embedded in the OTel
`resource.Resource`; `OTEL_SERVICE_NAME` is set for tooling that reads it directly (e.g.
Instana auto-correlation) but the Go SDK uses the value passed to `tracing.Init()`.

### Two valid OTLP destinations in DaemonSet mode

| Pattern | Endpoint | Best for |
|---------|----------|----------|
| **NODE_IP** (used by banking-demo) | `http://$(NODE_IP):4317` | Keeps traffic on-node; no cross-node hops |
| **Cluster-DNS Service** | `http://instana-agent.instana-agent.svc.cluster.local:4317` | Works from any namespace; used by Nginx (can't expand env vars in `nginx.conf`) |

`$(NODE_IP)` resolves to `status.hostIP` — the IP of the Kubernetes node the pod is
scheduled on. The DaemonSet agent pod runs on the host network of that same node, so the
connection stays local. The Helm chart creates the `instana-agent` cluster-wide Service
automatically (`service.create=true` in the agent Helm values).

---

## Go OTel Instrumentation

### Tracing library

All services use [`internal/tracing`](../../internal/tracing/tracing.go) — a thin wrapper
around `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`:

```go
// In each service's main.go (e.g. auth-service)
shutdownTracing := tracing.Init(serviceName, cfg.OTLPEndpoint, logger)
defer shutdownTracing(context.Background())
```

### HTTP instrumentation (api-producer, notification-service)

`notification-service` wraps its Chi router with `otelhttp.NewHandler` only:

```go
server := &http.Server{
    Handler: otelhttp.NewHandler(router, serviceName),
    ...
}
```

`api-producer` uses a two-layer wrapper — the go-sensor's `TracingHandlerFunc` on the outside,
`otelhttp` on the inside:

```go
// producer/main.go
instanaHandler := instana.TracingHandlerFunc(
    tracing.Collector(),
    "/",
    otelhttp.NewHandler(router, serviceName).ServeHTTP,
)
server := &http.Server{
    Handler: instanaHandler,
    ...
}
```

Handler order: **`instana.TracingHandlerFunc` → `otelhttp.NewHandler` → chi router**

- The instana wrapper fires first: starts a `g.http` span via the native Instana OpenTracing
  tracer → sent to agent `:42699`. This span extracts `traceparent` from the inbound request
  (from Kong), encodes it into Instana's native trace/span ID format, and re-injects a
  matching `traceparent` into the response headers — so the native span and downstream OTel
  spans share the same W3C trace ID. This `g.http` span is what Instana uses to classify
  the service as technology **Go**.
- `otelhttp` fires second: emits an OTel `SpanKindServer` HTTP span → agent `:4317`,
  continues the same trace context downstream into NATS RPC calls.

Both handlers generate a span per HTTP request with:
- `http.method`, `http.route`, `http.status_code` attributes
- W3C `traceparent`/`tracestate` header extraction from inbound requests (from Kong / Traefik)
- Span propagation to downstream NATS RPC calls via context (api-producer)
- Long-lived WebSocket session span for the `/ws` upgrade (notification-service)

> **Why api-producer needs both wrappers but the NATS services don't**
>
> The four NATS consumer services (auth / account / transfer / notification) are detected as
> **Go** because their primary activity is NATS message consumption — the go-sensor's process
> registration on port `42699` tags them as Go, and Instana infers "messaging" from the OTel
> `messaging.system=nats` spans. Their small internal Chi router (`/health`, `/metrics`) is
> never the entry point for external traffic.
>
> `api-producer` is an HTTP-first service. Without `instana.TracingHandlerFunc`, `InitCollector`
> still connects on `:42699` for process metrics, but no native `g.http` entry span is ever
> emitted — so Instana classifies the service as generic **HTTP** instead of **Go**.
> Adding the instana wrapper at the outermost layer fixes the technology tag without affecting
> the OTel trace propagation chain.

### NATS span propagation

W3C `traceparent` is propagated end-to-end across the NATS boundary:

- **Producer** ([`producer/rpc.go`](../../producer/rpc.go)) — injects `traceparent` into NATS
  message headers via `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(hdr))`
  after creating the `rpc.request` span.
- **Consumer** ([`internal/nats/consumer.go`](../../internal/nats/consumer.go)) — extracts the
  propagated span context in `dispatch` via `otel.GetTextMapPropagator().Extract(ctx, ...)` before
  calling the handler, continuing the trace as a child span.

The `propagation.TraceContext{}` propagator is registered globally by
[`internal/tracing.Init()`](../../internal/tracing/tracing.go) at every service startup.
Instana maps `messaging.system=nats` / `messaging.destination.name=<subject>` semantic conventions
to its service dependency graph — same arrows and latency charts as a native sensor.

### What appears in traces

| Span source | Span attributes | Destination |
|-------------|----------------|-------------|
| Traefik (ingress) | `http.method`, `http.route`, `http.url`, `http.status_code` | OTLP/gRPC → agent `:4317` |
| Nginx / frontend | `http.method`, `http.route`, `http.status_code` | OTLP/gRPC → agent `:4317` (cluster-DNS) |
| Kong (API gateway) | `http.method`, `http.route`, `http.status_code` | OTLP/HTTP → agent `:4318` |
| api-producer HTTP handler | `http.method`, `http.route`, `http.status_code` | OTLP/gRPC → agent `:4317` |
| notification-service WS upgrade | `http.method=/ws`, `http.status_code=101` | OTLP/gRPC → agent `:4317` |
| NATS RPC request span | `messaging.system=nats`, `messaging.destination.name`, `rpc.duration_ms` | OTLP/gRPC → agent `:4317` |
| NATS RPC roundtrip duration | via `rpc_roundtrip_seconds` Prometheus histogram | Prometheus only |
| PostgreSQL queries | `db.type=postgresql`, `db.statement` via `instapgx` | go-sensor → agent `:42699` |
| Redis commands | `db.type=redis`, `db.statement` via `instaredis` | go-sensor → agent `:42699` |

---

## Trace Headers Propagated

Config in [`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.tracing:
  extra-http-headers:
    - traceparent     # W3C Trace Context
    - tracestate
    - x-instana-t    # Instana native
    - x-instana-s
    - x-instana-l
```

Traefik injects `traceparent`/`tracestate` on inbound requests (via OTLP tracing configured in
the `HelmChartConfig` — `--tracing.instana` was removed in Traefik v3). Kong propagates the
same headers downstream via its OTel plugin (`propagation.default_format: w3c`). The
`api-producer` and `notification-service` both extract these headers via `otelhttp.NewHandler`
and propagate the trace context further downstream.

---

## OTel Signals Supported

| Signal | Status |
|--------|--------|
| Traces (OTLP/gRPC, OTLP/HTTP) | GA |
| Metrics (OTLP) | GA |
| Logs (OTLP) | GA |

Instana correlates OTel spans with its own AutoTrace spans. Mixed tracing (some hops instrumented
with OTel, others with Instana tracer) is supported.

---

## Go Collector vs OTel SDK — Two Approaches

IBM Instana provides two distinct Go instrumentation paths. banking-demo uses **both**:

| | Instana Go Collector (`go-sensor`) | OTel SDK (`go.opentelemetry.io/otel`) |
|---|---|---|
| Package | `github.com/instana/go-sensor` | `go.opentelemetry.io/otel` + exporter |
| Protocol | Instana native, port **42699** | OTLP/gRPC port **4317** |
| Transport | Direct to agent (no OTLP) | OTLP → agent or OTel Collector |
| Go runtime metrics | ✔ Free: GC pause, goroutines, heap, CPU | ✘ (use Prometheus `process_*` metrics) |
| AutoProfile™ | ✔ Continuous CPU/memory profiling | ✘ |
| W3C TraceContext | ✔ Propagates | ✔ Propagates |
| Native PostgreSQL DB spans | ✔ via `instapgx` | ✘ |
| Native Redis DB spans | ✔ via `instaredis` | ✘ |
| NATS tracing | ✘ No native module | ✔ W3C header propagation (producer inject + consumer extract) |
| Used by banking-demo | ✔ (`internal/tracing.Init()`) | ✔ (`internal/tracing.Init()`) |

Both are initialised together inside the same `tracing.Init()` call. OTel handles distributed
trace export (OTLP → agent `:4317`); the go-sensor connects in parallel to the agent on port
`:42699` for Go process metrics, health signatures, AutoProfile™, and native DB spans.

See [`14-go-sensor.md`](./14-go-sensor.md) for the full go-sensor integration reference.

---

## OTel Collector (Optional — not used by default)

The `monitoring/` directory ships an OTel Collector that can be deployed as an alternative or
addition to sending spans directly to the Instana agent:

```bash
# Deploy self-hosted monitoring stack (Prometheus + Grafana + Jaeger + OTel Collector)
kubectl apply -f monitoring/

# Point services at the OTel Collector instead of the Instana agent
helm upgrade banking-demo ./helm -n banking --reuse-values \
  --set 'global.env.OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.monitoring.svc.cluster.local:4317'
```

For Instana-only deployments, services export directly to the agent — no collector needed.

To switch back to the Instana agent:

```bash
helm upgrade banking-demo ./helm -n banking --reuse-values \
  --set 'global.env.OTEL_EXPORTER_OTLP_ENDPOINT=http://instana-agent.instana-agent.svc.cluster.local:4317'
```

---

## Verifying Traces in Instana UI

1. **Instana UI → Services** — each banking service appears after first traces
2. **Instana UI → Analytics → Calls** — filter by `service.name = api-producer` to see spans
3. **Instana UI → Infrastructure → Kubernetes** — k3s cluster with all pods

### Troubleshooting

```bash
# Check agent pod is running
kubectl -n instana-agent get pods
# Expected: instana-agent-<hash> 1/1 Running   k8sensor-<hash> 1/1 Running

# Check agent is listening on OTLP ports inside the DaemonSet pod
kubectl -n instana-agent exec ds/instana-agent -- sh -c 'ss -tlnp | grep -E "4317|4318"'

# Check agent logs for OTLP span ingestion
kubectl -n instana-agent logs ds/instana-agent --tail=50 | grep -i "opentelemetry\|otlp"

# Verify a Go pod has the correct endpoint
kubectl -n banking exec deploy/auth-service -- env | grep -E 'NODE_IP|OTEL|INSTANA'
# Expected:
# NODE_IP=10.0.x.x
# OTEL_EXPORTER_OTLP_ENDPOINT=http://10.0.x.x:4317
# OTEL_SERVICE_NAME=auth-service
# OTEL_RESOURCE_ATTRIBUTES=service.namespace=banking-demo
# INSTANA_AGENT_HOST=10.0.x.x

# Verify the agent Service is reachable from a pod (cluster-DNS path)
kubectl -n banking exec deploy/auth-service -- \
  sh -c 'nc -zv instana-agent.instana-agent.svc.cluster.local 4317 2>&1'
# Expected: open / succeeded
```
