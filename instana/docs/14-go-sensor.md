# Instana Go Collector (go-sensor) — banking-demo

> **Source:** https://www.ibm.com/docs/en/instana-observability?topic=technologies-monitoring-go
> **GitHub:** https://github.com/instana/go-sensor
> Condensed for: Go 1.26 workspace, NATS RPC, k3s on EC2, Instana Helm DaemonSet agent.

---

## What it adds over OTel alone

The banking-demo Go services already export distributed traces via **OTel OTLP → Instana agent :4317**.
The Instana Go Collector (`github.com/instana/go-sensor`) runs **alongside** OTel and provides
capabilities that OTel metrics/traces cannot replicate:

| Capability | OTel | go-sensor |
|-----------|------|-----------|
| Distributed traces (HTTP, NATS) | ✅ | ✅ (native protocol) |
| Go process dashboard (memory, heap, GC, goroutines) | ❌ | ✅ auto |
| Health signatures (Calls / Response time / Scaling) | ❌ | ✅ auto |
| AutoProfile™ (continuous profiling) | ❌ | ✅ opt-in |
| Native PostgreSQL DB spans (via instapgx) | ❌ | ✅ |
| Native Redis DB spans (via instaredis) | ❌ | ✅ |
| Instana native trace correlation (no OTLP round-trip) | ❌ | ✅ |

The two coexist: OTel handles span propagation over NATS and OTLP export; the go-sensor
connects in parallel to the agent on port **42699** using Instana's native protocol.

---

## Implementation

### Where the code lives

| File | What it does |
|------|-------------|
| [`internal/tracing/tracing.go`](../../internal/tracing/tracing.go) | Collector init, OTel provider, shutdown flush |
| [`internal/db/db.go`](../../internal/db/db.go) | `instapgx.WrapClient` — PostgreSQL native spans |
| [`internal/redis/redis.go`](../../internal/redis/redis.go) | `instaredis.WrapClient` — Redis native spans |

Every service calls `tracing.Init(serviceName, otlpEndpoint, logger)` from its `main.go`. All
shared instrumentation lives in the `internal` module. `api-producer` additionally wraps its
HTTP handler with `instana.TracingHandlerFunc` — see below.

```go
// internal/tracing/tracing.go
import instana "github.com/instana/go-sensor"

var collector instana.TracerLogger

func Init(serviceName, otlpEndpoint string, logger *slog.Logger) func(context.Context) {
    // Instana Collector — connects to INSTANA_AGENT_HOST:42699 in the background.
    // If the agent is unreachable, it retries silently (no startup penalty).
    collector = instana.InitCollector(&instana.Options{
        Service:           serviceName,
        EnableAutoProfile: autoProfileEnabled(), // reads INSTANA_AUTO_PROFILE env var
        Tracer:            instana.DefaultTracerOptions(),
    })
    // ...
    // On shutdown: instana.Flush(ctx) is called to drain buffered spans.
}

// Collector is used by db.go and redis.go to attach Instana tracers.
func Collector() instana.TracerLogger { return collector }
```

```go
// internal/db/db.go — PostgreSQL native spans via instapgx/v2
import instapgx "github.com/instana/go-sensor/instrumentation/instapgx/v2"

if c := tracing.Collector(); c != nil {
    cfg.ConnConfig.Tracer = instapgx.InstanaTracer(cfg.ConnConfig, c)
}
```

```go
// internal/redis/redis.go — Redis native spans via instaredis/v2
import instaredis "github.com/instana/go-sensor/instrumentation/instaredis/v2"

if c := tracing.Collector(); c != nil {
    instaredis.WrapClient(client, c) // registers hook on *goredis.Client in-place
}
```

### api-producer — HTTP handler wrapping for Go technology detection

The go-sensor classifies a process as technology **Go** in the Instana UI by emitting native
`g.http` entry spans. Without wrapping the HTTP handler, `InitCollector` still connects on
port `42699` for process metrics, but Instana sees only OTel `SpanKindServer` HTTP spans and
classifies the service as generic **HTTP** instead of **Go**.

`api-producer` wraps its handler stack with `instana.TracingHandlerFunc` at the outermost layer:

```go
// producer/main.go
import instana "github.com/instana/go-sensor"
import "banking-demo/internal/tracing"

// tracing.Collector() returns instana.TracerLogger from InitCollector() — the type
// TracingHandlerFunc expects. The IBM docs show NewSensor() (legacy API); InitCollector
// is the current equivalent and satisfies the same TracerLogger interface.
instanaHandler := instana.TracingHandlerFunc(
    tracing.Collector(),   // instana.TracerLogger, initialised by tracing.Init() above
    "/",
    otelhttp.NewHandler(router, serviceName).ServeHTTP,
)
server := &http.Server{Handler: instanaHandler, ...}
```

Execution order per request: **instana wrapper → otelhttp → chi router**

1. `instana.TracingHandlerFunc` fires: starts a `g.http` span via the native Instana
   OpenTracing tracer → sent to agent `:42699`. This span extracts `traceparent` from the
   inbound request (from Kong), encodes it into Instana's native trace/span ID format, and
   re-injects a matching `traceparent` into the response headers — so the Instana native span
   and the downstream OTel spans share the same W3C trace ID.
2. `otelhttp.NewHandler` fires: emits an OTel `SpanKindServer` HTTP span → agent `:4317`,
   continues the same trace context downstream into NATS RPC calls.
3. The `g.http` span on port `:42699` is what Instana uses to classify the service as
   technology **Go**. OTel HTTP spans alone (`SpanKindServer`) do not trigger Go detection.

The four NATS consumer services do **not** need this wrapper because their primary entry signals
are `messaging.system=nats` spans + the go-sensor process registration, which is sufficient for
Go technology detection even without native HTTP entry spans.

---

### Module dependencies

```
internal/go.mod
  └─ github.com/instana/go-sensor v1.73.4          (direct)
       ├─ github.com/looplab/fsm v1.0.3
       └─ github.com/opentracing/opentracing-go v1.2.0
  └─ github.com/instana/go-sensor/instrumentation/instapgx/v2 v2.31.0
  └─ github.com/instana/go-sensor/instrumentation/instaredis/v2 v2.51.0

producer/go.mod
  └─ github.com/instana/go-sensor v1.73.4          (direct — needed for TracingHandlerFunc)
```

### Kubernetes env vars — Helm chart

The go-sensor must reach the Instana agent, which runs as a DaemonSet pod **on the host network**
of the same node. Inside a Kubernetes pod, `localhost:42699` does **not** work — the agent
is not in the pod network. The correct address is the node IP (`status.hostIP`).

`INSTANA_AGENT_HOST=$(NODE_IP)` was added to all six Deployment templates:

```yaml
# helm/templates/<service>.yaml — added alongside the existing OTel env block
- name: NODE_IP
  valueFrom:
    fieldRef:
      fieldPath: status.hostIP
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: "http://$(NODE_IP):4317"
- name: INSTANA_AGENT_HOST        # ← new: go-sensor connects here
  value: "$(NODE_IP)"
- name: OTEL_SERVICE_NAME
  value: <service-name>
```

> `INSTANA_AGENT_PORT` defaults to `42699` — leave unset unless the agent is configured
> to use a non-standard port.

---

## What appears in the Instana UI

After the first request reaches a service, the UI shows:

| View | Path | What you see |
|------|------|-------------|
| Go process dashboard | Infrastructure → Processes → `<service-name>` | Memory, heap, GC pauses, goroutine count, open FDs |
| Service dashboard | Applications → Services → `<service-name>` | Calls/min, error rate, mean latency, percentiles |
| Health issues | Events | Auto-raised when health signatures breach thresholds |
| Dependency graph | Applications → <AP> → Dependencies | NATS arcs (OTel), PostgreSQL node (instapgx), Redis node (instaredis) |
| Profiles | Analytics → Profiles | CPU, heap, goroutine flame graphs (requires `INSTANA_AUTO_PROFILE=true`) |

---

## Configuration

### Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `INSTANA_AGENT_HOST` | `localhost` | Node IP of the Instana DaemonSet — **must set in k8s** |
| `INSTANA_AGENT_PORT` | `42699` | Agent native port — leave unset |
| `INSTANA_SERVICE_NAME` | `<Options.Service>` | Overrides the service name set in code |
| `INSTANA_DEBUG` | unset | Set any non-empty value to enable debug logs (overrides in-code log level) |
| `INSTANA_AUTO_PROFILE` | unset | Set to `true` to enable AutoProfile™ per pod (overrides in-code setting) |
| `INSTANA_ALLOW_ROOT_EXIT_SPAN` | `0` | Set to `1` to trace cron/background tasks without an entry span |

### Enabling AutoProfile™ selectively

AutoProfile™ adds ~1% CPU overhead. Enable it per-service via `extraEnv` in `helm/values.yaml`
rather than globally:

```yaml
# helm/values.yaml
auth-service:
  extraEnv:
    INSTANA_AUTO_PROFILE: "true"
```

---

## Verification

```bash
# 1. Confirm the go-sensor is connecting to the agent
kubectl -n banking logs deployment/auth-service --tail=50 | grep -i instana
# Expected: "INSTANA: collector initialized" (INFO level)

# 2. Check agent sees the Go process
kubectl -n instana-agent logs ds/instana-agent --tail=100 | grep -i "go\|golang"
# Expected: "Go process discovered: auth-service"

# 3. Instana UI
#   Infrastructure → Processes → search "auth-service" → Go dashboard should appear
#   Applications → Services → auth-service → should show calls after traffic

# 4. Test agent reachability from a pod
kubectl -n banking exec deploy/auth-service -- \
  sh -c 'nc -zv $INSTANA_AGENT_HOST 42699 2>&1'
# Expected: "open" or "succeeded"
```

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| No Go process dashboard | `INSTANA_AGENT_HOST` not set → sensor connects to `localhost` inside pod (wrong) | Verify env var is `$(NODE_IP)` in the Deployment template |
| "connection refused :42699" | Agent not running or pod on different node | `kubectl -n instana-agent get pods -o wide` — confirm DaemonSet pod on same node |
| Go dashboard shows 0 goroutines | Agent sees the process but collector handshake is slow | Wait 30–60 s after pod start; enable `INSTANA_DEBUG=true` in `extraEnv` |
| No PostgreSQL/Redis spans | `tracing.Init()` not called before `db.NewPool()` / `redis.NewClient()` | Ensure `tracing.Init()` is the first call in `main()` |
| `api-producer` shows as technology **HTTP** not **Go** | `instana.TracingHandlerFunc` missing — no native `g.http` spans emitted | Fixed in commit `0e81f0f`; `api-producer` now shows technology **Go** (service type remains **HTTP** — that is correct and expected) |
| `fsm` compile error | Old `looplab/fsm` < v1 in workspace | `go get github.com/looplab/fsm@v1` in the affected module |

---

## NATS — no native Instana instrumentation module

There is **no `instanats` instrumentation module** in the official [supported libraries list](https://www.ibm.com/docs/en/instana-observability?topic=go-collector-supported-libraries). NATS tracing is handled exclusively by OTel:

- `producer/rpc.go` — injects W3C `traceparent` into NATS headers before publish
- `internal/nats/consumer.go` — extracts `traceparent` from NATS headers and propagates context to handlers

The Instana UI shows NATS as a messaging arc in the dependency graph via the OTel `messaging.system=nats` span attribute. This is the correct approach — no code changes needed.

---

## Related docs

| File | What it covers |
|------|----------------|
| [`03-opentelemetry.md`](./03-opentelemetry.md) | OTel OTLP tracing, NATS trace propagation |
| [`06-redis-sensor.md`](./06-redis-sensor.md) | Redis agent sensor config, instaredis native spans |
| [`07-postgresql-sensor.md`](./07-postgresql-sensor.md) | PostgreSQL agent sensor config, instapgx native spans |
| [`11-nats-monitoring.md`](./11-nats-monitoring.md) | NATS metrics via Prometheus exporter |
| [`13-k8s-agent-install.md`](./13-k8s-agent-install.md) | Installing the Instana DaemonSet agent via Helm |

> **Official docs:** https://www.ibm.com/docs/en/instana-observability?topic=technologies-monitoring-go
> **Supported libraries:** https://www.ibm.com/docs/en/instana-observability?topic=go-collector-supported-libraries
> **Common operations FAQ:** https://www.ibm.com/docs/en/instana-observability?topic=go-collector-common-operations
> **GitHub (examples, changelog):** https://github.com/instana/go-sensor
