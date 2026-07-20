# Instana Observability Overview — banking-demo

> **Purpose:** How Instana collects logs, metrics, and traces from every layer of the
> banking-demo stack — with a focus on the three user-facing tiers: **frontend (Nginx)**,
> **api-producer** (Go Chi HTTP gateway), and the **Go microservices**
> (auth / account / transfer / notification).
>
> For full install instructions see [`13-k8s-agent-install.md`](./13-k8s-agent-install.md).
> For per-technology detail follow the cross-references in each section.

---

## Stack at a Glance

```
Browser
  │
  ▼ HTTPS / WSS  (port 443 → hostPort 80 via Kong on EC2)
┌──────────────────────────────────────────────────────────────────┐
│  Nginx (frontend)                    OTLP/gRPC → agent :4317    │
│    /          → static SPA (React)                               │
│    /api/*     → kong:8000  (proxies REST, propagates W3C ctx)   │
│    /ws        → kong:8000  (proxies WebSocket, propagates ctx)  │
└──────────────────────────────────────────────────────────────────┘
  │
  ▼ W3C traceparent propagated downstream by otel_trace_context propagate
┌──────────────────────────────────────────────────────────────────┐
│  Kong 3.9 (API gateway, DB-less, hostPort 80)                   │
│    OTLP/HTTP → agent :4318    (Kong OTel plugin)                │
│    All /api/* routes → api-producer:8080                        │
│    /ws          → notification-service:8004                     │
└──────────────────────────────────────────────────────────────────┘
  │
  ▼
┌──────────────────────────────────────────────────────────────────┐
│  api-producer (Go Chi)               OTLP/gRPC → agent :4317    │
│    otelhttp.NewHandler wraps all routes                         │
│    go-sensor → agent :42699  (process metrics + AutoProfile)    │
│    NATS request/reply → consumer services                       │
└──────────────────────────────────────────────────────────────────┘
  │  NATS RPC (W3C traceparent injected into msg.Headers)
  ├─ banking.auth.*         → auth-service
  ├─ banking.accounts.*     → account-service
  ├─ banking.transfers.*    → transfer-service
  └─ banking.notifications.* → notification-service
       │  Each consumer: OTLP/gRPC → agent :4317 + go-sensor → :42699
       ├─ PostgreSQL 18  (instapgx native spans + pg_stat sensor)
       └─ Redis 8        (instaredis native spans + INFO sensor)
```

> **Traefik is NOT running.** k3s is installed with `--disable traefik`. Kong handles
> all ingress directly via `hostPort: 80`. There is no Traefik tier in this deployment.

---

## Signal Matrix — What Instana Collects and How

| Component | Logs | Metrics | Traces |
|-----------|------|---------|--------|
| **frontend (Nginx)** | stdout → k8s | `stub_status` scrape (agent auto-detect) | `ngx_otel_module` → OTLP/gRPC `:4317` |
| **Kong** | stdout → k8s | Admin API poll (`/metrics` via sensor) | Kong OTel plugin → OTLP/HTTP `:4318` |
| **api-producer** | structured JSON (slog) → stdout | `/metrics` Prometheus (annotated) | `otelhttp` → OTLP/gRPC `:4317` + go-sensor `:42699` |
| **auth-service** | structured JSON (slog) → stdout | `/metrics` NATS consumer metrics | NATS consumer span (child of api-producer) + go-sensor `:42699` |
| **account-service** | structured JSON (slog) → stdout | `/metrics` NATS consumer metrics | NATS consumer span + go-sensor `:42699` |
| **transfer-service** | structured JSON (slog) → stdout | `/metrics` NATS consumer metrics | NATS consumer span + go-sensor `:42699` |
| **notification-service** | structured JSON (slog) → stdout | `/metrics` NATS consumer metrics | `otelhttp` WS upgrade span + NATS consumer span + go-sensor `:42699` |
| **PostgreSQL** | — | `pg_stat_*` views (agent sensor, poll every 10 s) | `instapgx` native DB spans |
| **Redis** | — | `INFO`/`SLOWLOG` (agent auto-discovery, poll 10 s) | `instaredis` native Redis spans |
| **NATS** | — | `nats-exporter` → Prometheus `:7777` (annotated) | No native sensor — trace continuity via W3C headers |
| **k3s cluster** | — | k8sensor (DaemonSet Deployment) | — |

---

## Agent Deployment

The Instana **DaemonSet agent** (Helm, namespace `instana-agent`) runs directly inside the k3s
cluster on the EC2 node. A companion **k8sensor** Deployment handles all Kubernetes API
monitoring (pods, services, namespaces, workloads).

```
instana-agent namespace
  ├── DaemonSet: instana-agent    ← one pod per node
  │     ├── listens: :4317 (OTLP/gRPC), :4318 (OTLP/HTTP), :42699 (Instana native)
  │     ├── auto-discovers: Nginx (stub_status), Redis (containerd scan), PostgreSQL
  │     ├── polls: PostgreSQL pg_stat_*, Kong Admin API :8001, nats-exporter :7777
  │     └── pulls: instana/configuration.yaml from git (main branch, hot-reload)
  └── Deployment: k8sensor        ← Kubernetes API watcher
        └── reports pods/services/namespaces/workloads to Instana backend
```

**Configuration is git-managed:** push to `main` → Click update config on Instana Website → agent pulls and hot-reloads [`instana/configuration.yaml`](../configuration.yaml)
within ~30 s. No `helm upgrade` needed for sensor config changes. =

**Two valid OTLP export endpoints (both enabled):**

| Pattern | Endpoint | Used by |
|---------|----------|---------|
| Cluster-DNS Service | `instana-agent.instana-agent.svc.cluster.local:4317` | frontend (nginx.conf cannot expand env vars) |
| NODE_IP (DaemonSet host) | `http://$(NODE_IP):4317` via `status.hostIP` downward API | all Go services |
| OTLP/HTTP (Kong plugin) | `http://$(NODE_IP):4318` | Kong OTel plugin |

> **Legacy mode:** The EC2 host-agent (systemd install, [`01-agent-install.md`](./01-agent-install.md))
> is kept for reference. The DaemonSet mode ([`13-k8s-agent-install.md`](./13-k8s-agent-install.md))
> is the recommended install for this project.

---

## Frontend (Nginx) — Logs, Metrics, Traces

The frontend is a static React SPA served by Nginx. It proxies API and WebSocket traffic
to Kong. All three signal types are collected by the Instana DaemonSet agent.

### How the image is built

[`frontend/Dockerfile`](../../frontend/Dockerfile) installs the official `nginx-module-otel`
Alpine package which ships the NGINX OpenTelemetry dynamic module (`ngx_otel_module.so`).
This module is loaded at the top of [`frontend/nginx.conf`](../../frontend/nginx.conf):

```nginx
load_module modules/ngx_otel_module.so;
```

### Traces — ngx_otel_module

Configured in [`frontend/nginx.conf`](../../frontend/nginx.conf):

```nginx
otel_exporter {
    endpoint instana-agent.instana-agent.svc.cluster.local:4317;
}
otel_service_name frontend;
otel_trace on;
```

- Every HTTP request through Nginx generates an OTLP/gRPC span sent to the agent
  cluster-DNS Service. The cluster-DNS FQDN is used because `nginx.conf` cannot expand
  environment variables in directives without Lua/Perl modules.
- `otel_trace_context extract` on `/` — extracts incoming W3C `traceparent` (if present)
- `otel_trace_context propagate` on `/api/` and `/ws` — extracts **and** re-injects
  `traceparent` into the upstream Kong request, continuing the trace chain

```nginx
location /api/ {
    otel_trace_context propagate;          # extract + inject into upstream
    proxy_pass http://kong:8000/api/;
    ...
}

location /ws {
    otel_trace_context propagate;          # same for WebSocket upgrade
    proxy_pass http://kong:8000/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    ...
}
```

### Metrics — stub_status

```nginx
location /nginx_status {
    stub_status;
    otel_trace off;           # don't trace health-check scrapes
    allow 127.0.0.1;
    allow 10.42.0.0/16;       # k3s flannel CIDR
    allow 172.30.0.0/24;      # Docker Compose bridge
    deny all;
}
```

The Instana DaemonSet agent auto-discovers Nginx by scanning process images for
`nginx` binaries and reading the main config file for a `stub_status` location.

Metrics collected: active connections, accepts, handled, requests, reading, writing, waiting.

> The `stub_status` location **must be in `nginx.conf`** (not in `conf.d/`). The agent
> reads only the main config file when auto-discovering via process scan.

### Logs

Nginx writes access and error logs to stdout/stderr. The k3s container runtime captures
these and they are available via:

```bash
kubectl logs -n banking deploy/frontend
```

Instana Infrastructure correlates the log stream to the frontend pod once spans arrive
and the `frontend` service name is resolved.

**Further reading:** [`09-pod-service-detection.md`](./09-pod-service-detection.md) §Frontend

---

## api-producer — Logs, Metrics, Traces

`api-producer` is the Go Chi HTTP server that receives all REST requests from Kong and
dispatches them as NATS RPC calls to the consumer microservices. It is the **primary
entry point** for distributed traces in the backend.

### Traces — dual instrumentation

`api-producer` uses two complementary tracing paths, both initialised from
[`internal/tracing/tracing.go`](../../internal/tracing/tracing.go):

**1. OTel OTLP — distributed traces**

The Chi router is wrapped with `otelhttp.NewHandler` in [`producer/main.go`](../../producer/main.go):

```go
server := &http.Server{
    Handler: otelhttp.NewHandler(router, serviceName),
}
```

This generates an OTLP/gRPC span per HTTP request, exported to `http://$(NODE_IP):4317`
(where `NODE_IP` is injected via the Kubernetes downward API: `status.hostIP`).

Each span:
- carries `http.method`, `http.route`, `http.status_code` attributes
- extracts the incoming W3C `traceparent` from Kong (which received it from Nginx),
  so the span is stitched into the single distributed trace chain
- propagates context forward into NATS RPC calls:

```go
// producer/rpc.go — NATS RPC dispatch
otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(hdr))
// then nats.PublishMsg(msg) with hdr attached to the message
```

**2. Instana Go Collector — process-level metrics**

[`internal/tracing/tracing.go`](../../internal/tracing/tracing.go) calls
`instana.InitCollector()` at startup. The collector connects to the agent at
`$(NODE_IP):42699` using Instana's native protocol, adding:

- Go process dashboard: heap, GC pauses, goroutines, open file descriptors
- Health signatures: calls/s, mean response time, 95th percentile latency
- AutoProfile™: continuous CPU/memory/goroutine profiling (opt-in via `INSTANA_AUTO_PROFILE=true`)

The collector instance is exposed via `tracing.Collector()` so that `internal/db` and
`internal/redis` can attach Instana-native tracers to their drivers.

### Metrics — Prometheus

`api-producer` exposes its own Prometheus endpoint at `http://api-producer:8080/metrics`.
The Helm chart annotates the pod with `prometheus.io/scrape: "true"` so the Instana
agent picks it up automatically via `prometheusAnnotations: strict`.

Key metrics:

| Metric | Labels |
|--------|--------|
| `http_requests_total` | `method`, `route`, `status` |
| `http_request_duration_seconds` | `method`, `route` |
| `rpc_requests_total` | `subject`, `status` |
| `rpc_roundtrip_duration_seconds` | `subject` |
| `nats_connected` | — |

### Logs

Structured JSON via `log/slog`, written to stdout. Every log line includes
`"service": "api-producer"`. Available via:

```bash
kubectl logs -n banking deploy/api-producer
```

**Further reading:** [`03-opentelemetry.md`](./03-opentelemetry.md), [`14-go-sensor.md`](./14-go-sensor.md)

---

## Consumer Microservices — auth / account / transfer / notification

### Architecture

All four consumer services share the same observability wiring from `internal/`:

| Package | What it provides |
|---------|-----------------|
| [`internal/tracing`](../../internal/tracing/) | OTel provider (OTLP/gRPC `:4317`) + Instana Go Collector (`:42699`) |
| [`internal/metrics`](../../internal/metrics/) | Prometheus metrics server (`/metrics`) |
| [`internal/nats`](../../internal/nats/) | NATS consumer with W3C traceparent extraction |
| [`internal/db`](../../internal/db/) | pgxpool with `instapgx` native DB spans |
| [`internal/redis`](../../internal/redis/) | go-redis client with `instaredis` native Redis spans |

No per-service tracing boilerplate — all instrumentation lives in `internal/`.

`notification-service` additionally wraps its HTTP Chi router with `otelhttp.NewHandler`
so the WebSocket upgrade request (`GET /ws`) produces its own span. This span:
- receives the W3C `traceparent` propagated from Kong
- is long-lived — it covers the HTTP 101 upgrade through to client disconnect
- is a parent to any subsequent NATS consumer spans triggered by notifications

### Traces — NATS span continuation

The NATS consumer in [`internal/nats/consumer.go`](../../internal/nats/consumer.go) extracts
the W3C `traceparent` header injected by `api-producer`:

```go
// internal/nats/consumer.go — message dispatch
ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(req.Headers()))
// child span is created with this ctx — parent = api-producer's rpc.request span
```

This means a single distributed trace spans:
**Nginx → Kong → api-producer → NATS → consumer service**

Each consumer exports spans to `http://$(NODE_IP):4317` (OTLP/gRPC) and registers
the go-sensor at `$(NODE_IP):42699` for process-level metrics.

**PostgreSQL spans** — [`internal/db/db.go`](../../internal/db/db.go) attaches
`instapgx.InstanaTracer` to the pgxpool connection config:

```go
cfg.ConnConfig.Tracer = instapgx.InstanaTracer(cfg.ConnConfig, tracing.Collector())
```

Every SQL query becomes an Instana-native DB span, child of the NATS consumer span.

**Redis spans** — [`internal/redis/redis.go`](../../internal/redis/redis.go) wraps
the go-redis client with `instaredis.WrapClient`:

```go
instaredis.WrapClient(client, tracing.Collector())
```

Every Redis command (GET, SET, PUBLISH) becomes an Instana-native Redis span, child
of the NATS consumer span.

### Metrics — Prometheus

Each consumer exposes `/metrics` (port varies per service, all annotated for auto-scrape):

| Service | Port | Annotation |
|---------|------|-----------|
| auth-service | :8001 | `prometheus.io/scrape: "true"` |
| account-service | :8002 | `prometheus.io/scrape: "true"` |
| transfer-service | :8003 | `prometheus.io/scrape: "true"` |
| notification-service | :8004 | `prometheus.io/scrape: "true"` |

Key metrics per consumer:

| Metric | Labels |
|--------|--------|
| `nats_messages_total` | `service`, `action`, `status` |
| `nats_handler_duration_seconds` | `service`, `action` |
| `nats_reconnects_total` | `service` |

### Logs

Structured JSON via `log/slog`, stdout. Every log line includes `"service": "<name>"`.
Additional context fields per service:

| Service | Extra log fields |
|---------|-----------------|
| auth-service | `user_id`, `action` |
| transfer-service | `transfer_id`, `sender_id`, `receiver_id`, `amount` |
| account-service | `account_id` |
| notification-service | `user_id`, `channel` (`ws` or `nats`) |

```bash
kubectl logs -n banking deploy/auth-service
kubectl logs -n banking deploy/transfer-service
```

**Further reading:** [`03-opentelemetry.md`](./03-opentelemetry.md), [`14-go-sensor.md`](./14-go-sensor.md),
[`11-nats-monitoring.md`](./11-nats-monitoring.md)

---

## PostgreSQL — Metrics and Traces

### Agent sensor (infrastructure metrics)

The Instana PostgreSQL sensor auto-discovers the PostgreSQL process via `/proc` scanning
and connects to `postgres.banking.svc.cluster.local:5432` using credentials from
[`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.postgresql:
  user: 'banking'
  password: 'bankingpass'
  database: 'banking'
  poll_rate: 10
```

The sensor queries `pg_stat_*` views every 10 s. Required PostgreSQL settings (set via
the init ConfigMap in
[`helm/templates/postgres-init-configmap.yaml`](../../helm/templates/postgres-init-configmap.yaml)):

```
track_activities = on
track_counts = on
track_io_timing = on
```

Key metrics: connections (active/idle/waiting), TPS, cache hit ratio, lock waits,
slow queries, bgwriter stats, tuple read/write rates.

### Native DB spans (instapgx)

The go-sensor instruments every pgxpool query via `instapgx.InstanaTracer`:

```go
// internal/db/db.go
cfg.ConnConfig.Tracer = instapgx.InstanaTracer(cfg.ConnConfig, tracing.Collector())
```

Result in Instana UI:

```
auth-service  NATS consumer span
  └─ SELECT * FROM users WHERE username = $1      instapgx span  ~1 ms
  └─ INSERT INTO sessions ...                     instapgx span  ~0.5 ms
```

**Further reading:** [`07-postgresql-sensor.md`](./07-postgresql-sensor.md)

---

## Redis — Metrics and Traces

### Agent sensor (infrastructure metrics)

The Instana DaemonSet agent **auto-discovers** the Redis pod via containerd process
scanning — it sees the `redis-server` process image on the node and activates a sensor
directly for that pod's IP/port. No explicit `hosts:` block is needed (and adding one
creates a duplicate Redis node in the UI).

Current config in [`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.redis:
  use_ssl: false
  # hosts: []  ← intentionally omitted; auto-discovery handles Redis
```

The `use_ssl: false` flag suppresses a transient SSL warning at sensor startup.
Metrics collected every 10 s: memory usage, connected clients, ops/sec,
keyspace hit ratio, slow command log.

How banking-demo uses Redis:
- `auth-service`: session storage (`SET session:{sid}`, `GET session:{sid}`)
- `auth-service`: user cache (frequently-read user records)
- `account-service`: balance read model (cache + invalidation)
- `notification-service`: pub/sub channel for real-time notifications

### Native Redis spans (instaredis)

The go-sensor instruments every go-redis command via `instaredis.WrapClient`:

```go
// internal/redis/redis.go
instaredis.WrapClient(client, tracing.Collector())
```

Result in Instana UI:

```
auth-service  NATS consumer span
  └─ SET session:{sid}    instaredis span  ~0.3 ms
  └─ GET session:{sid}    instaredis span  ~0.2 ms
```

**Further reading:** [`06-redis-sensor.md`](./06-redis-sensor.md)

---

## Kong — Metrics and Traces

### Agent sensor (infrastructure metrics)

The Instana Kong sensor polls the Kong Admin API at `kong.banking.svc.cluster.local:8001`
every 30 s. `KONG_ADMIN_LISTEN` is set to `0.0.0.0:8001` so the agent DaemonSet pod
can reach it via cluster-DNS (port 8001 is on the Kong ClusterIP Service but **not**
exposed via hostPort).

```yaml
com.instana.plugin.kong:
  enabled: true
  remote:
    - host: 'kong.banking.svc.cluster.local'
      port: '8001'
      protocol: 'http'
      poll_rate: 30
```

Metrics: requests/s, latency (P50/P99), upstream connect time, HTTP status distributions,
error rate per service/route.

### Traces — OTel plugin

Kong's built-in OpenTelemetry plugin pushes spans to the agent via OTLP/HTTP:

```
Kong OTel plugin → http://$(NODE_IP):4318/v1/traces
```

Kong extracts the incoming W3C `traceparent` from Nginx and injects it into the upstream
request to `api-producer`, so Kong spans appear as children of the Nginx spans.

**Further reading:** [`04-kong-sensor.md`](./04-kong-sensor.md)

---

## NATS — Metrics only

NATS has no native Instana sensor. Metrics are collected via the `prometheus-nats-exporter`
sidecar pod which exposes `gnatsd_*` metrics on `:7777`. The pod has Prometheus annotations
so the agent auto-scrapes it:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "7777"
```

Trace continuity through NATS is provided by W3C `traceparent` header propagation:
- `api-producer` injects the header into every NATS message before publish
- Each consumer extracts it and creates a child span

This means NATS messages do not produce their own spans, but the trace chain is unbroken.

**Further reading:** [`11-nats-monitoring.md`](./11-nats-monitoring.md)

---

## End-to-End Trace Chain

A complete request — user logs in — produces this distributed trace in the Instana UI:

```
[Nginx/frontend]  POST /api/sessions
  ngx_otel span → agent :4317                               ~1 ms
  └─ [Kong]  route: auth/login
       Kong OTel span → agent :4318                        ~2 ms
       └─ [api-producer]  POST /api/sessions
            otelhttp span → agent :4317                    ~5 ms
            └─ NATS publish banking.auth.login
                 └─ [auth-service]  handle banking.auth.login
                      NATS consumer span → agent :4317     ~3 ms
                        └─ [PostgreSQL]  SELECT users      instapgx  ~1 ms
                        └─ [Redis]  SET session:{sid}      instaredis ~0.3 ms
```

All layers are connected by the same W3C `traceparent` header, propagated:

| Hop | How |
|-----|-----|
| Nginx → Kong | `otel_trace_context propagate` writes `traceparent` header on proxy |
| Kong → api-producer | Kong OTel plugin extracts + re-injects `traceparent` |
| api-producer → NATS msg | `otel.GetTextMapPropagator().Inject(ctx, HeaderCarrier(hdr))` |
| NATS msg → consumer span | `otel.GetTextMapPropagator().Extract(ctx, HeaderCarrier(req.Headers()))` |

The `com.instana.tracing.extra-http-headers` config in `configuration.yaml` ensures
Instana also tracks the raw header values for mixed OTel + Instana-native span correlation:

```yaml
com.instana.tracing:
  extra-http-headers:
    - traceparent
    - tracestate
    - x-instana-t
    - x-instana-s
    - x-instana-l
```

---

## Instana UI — Where to Find Each Signal

| What you want to see | Instana UI path |
|----------------------|-----------------|
| All pods & workloads | Infrastructure → Kubernetes → `banking` namespace |
| EC2 host metrics | Infrastructure → Hosts → `banking-dung-ec2` |
| Go process dashboards | Infrastructure → Processes → `<service-name>` |
| Nginx metrics | Infrastructure → Processes → `frontend` (Nginx sensor) |
| PostgreSQL metrics | Infrastructure → `postgresql` |
| Redis metrics | Infrastructure → `redis` |
| Kong metrics | Infrastructure → `kong` |
| Service call rates & errors | Applications → Services |
| Distributed traces (waterfall) | Analytics → Calls → filter `service.name` |
| NATS broker metrics | Custom metrics (`entity.type:prometheus` filter) |
| Synthetic test results | Synthetic Monitoring → `banking-demo` tests |
| AutoProfile flame graphs | Analytics → Profiles |

---

## Observability Gaps (Known)

| Gap | Status | Path to close |
|-----|--------|---------------|
| **NATS broker spans** | No native sensor — metrics only via nats-exporter | Acceptable: W3C propagation gives end-to-end trace continuity without NATS-internal spans |
| **Redis client spans via OTel** | Currently only via `instaredis` (Instana native) | Add `redisotel.InstrumentTracing` from `go-redis/extra/redisotel` for OTel-native spans |
| **PostgreSQL client spans via OTel** | Currently only via `instapgx` (Instana native) | Add `otelpgx.NewTracer()` from `exaring/otelpgx` for OTel-native spans |
| **Frontend structured logs** | Log forwarding not configured | Deploy Fluentd/Fluent Bit log shipper or use Instana log connector |
| **Kong in Concert** | Not auto-imported despite live traffic | Root cause unconfirmed — manual SBOM upload workaround ([`15-concert-sbom-upload.md`](./15-concert-sbom-upload.md)) |
| **api-producer in Concert** | Same as Kong | Same workaround |
| **WebSocket session spans** | WS upgrade span is long-lived; message-level spans not emitted | Each WS message would need manual span creation inside `notification-service` |

---

## Configuration Reference (instana/configuration.yaml)

The agent pulls [`instana/configuration.yaml`](../configuration.yaml) from the `main` branch
on startup and after each GitOps hot-reload. Annotated summary of active blocks:

```yaml
# --- OTel receiver (both gRPC and HTTP enabled simultaneously)
com.instana.plugin.opentelemetry:
  grpc: { enabled: true, port: 4317 }   # receives OTLP from Go pods + Nginx
  http: { enabled: true, port: 4318 }   # receives OTLP from Kong OTel plugin

# --- Prometheus auto-scrape (annotation-driven, no explicit endpoints)
com.instana.plugin.prometheus:
  prometheusAnnotations: strict         # scrape pods with prometheus.io/scrape: "true"

# --- Nginx sensor (DaemonSet auto-discovers; service_name sets UI label)
com.instana.plugin.nginx:
  service_name: frontend

# --- PostgreSQL sensor (credentials for pg_stat_* queries)
com.instana.plugin.postgresql:
  user: 'banking'
  password: 'bankingpass'
  database: 'banking'
  poll_rate: 10

# --- Redis sensor (auto-discovery; no hosts: block to avoid duplicate nodes)
com.instana.plugin.redis:
  use_ssl: false

# --- Kong sensor (polls Admin API via cluster-DNS)
com.instana.plugin.kong:
  enabled: true
  remote:
    - host: 'kong.banking.svc.cluster.local'
      port: '8001'
      protocol: 'http'
      poll_rate: 30

# --- Traefik sensor (DISABLED — k3s installed with --disable traefik)
# com.instana.plugin.traefik:
#   enabled: true

# --- W3C + Instana native trace header propagation
com.instana.tracing:
  extra-http-headers:
    - traceparent
    - tracestate
    - x-instana-t
    - x-instana-s
    - x-instana-l

# --- Secrets redaction
com.instana.secrets:
  matcher: contains-ignore-case
  list: [ secret, token, DATABASE_URL, REDIS_URL, NATS_URL ]
  # NOTE: "password" and "key" intentionally omitted to avoid redacting
  # PostgreSQL sensor credentials and internal agent fields.

---

## Quick Verification Checklist

```bash
# 1. Agent pods running
kubectl -n instana-agent get pods
# Expected: instana-agent-<hash> 1/1 Running   k8sensor-<hash> 1/1 Running

# 2. OTLP ports listening inside the agent pod
kubectl -n instana-agent exec ds/instana-agent -- \
  sh -c 'ss -tlnp | grep -E "4317|4318|42699"'
# Expected: LISTEN on :4317 :4318 :42699

# 3. Go services have correct env vars
kubectl -n banking exec deploy/api-producer -- env | grep -E "NODE_IP|OTEL|INSTANA"
# Expected: NODE_IP=10.x.x.x  OTEL_EXPORTER_OTLP_ENDPOINT=http://10.x.x.x:4317
#           INSTANA_AGENT_HOST=10.x.x.x  INSTANA_AGENT_PORT=42699

# 4. Frontend OTel module is loaded and config is valid
kubectl -n banking exec deploy/frontend -- nginx -t
# Expected: nginx: the configuration file /etc/nginx/nginx.conf syntax is ok

# 5. Kong can reach its Admin API
kubectl -n banking exec deploy/kong -- \
  curl -s http://localhost:8001/status | grep -o '"database":{"reachable":[^}]*}'
# Expected: "database":{"reachable":true}

# 6. Generate traffic to populate Application Services
for i in $(seq 1 10); do
  curl -sf "http://<EC2_IP>/api/health" > /dev/null
  sleep 1
done

# 7. Confirm spans arriving at agent
kubectl -n instana-agent logs ds/instana-agent --tail=30 \
  | grep -iE "otlp|span|frontend|api-producer|auth|account"

# 8. Redis auto-discovery confirmed
kubectl -n instana-agent logs ds/instana-agent --tail=100 \
  | grep -i "redis\|Activated Sensor"
# Expected: "Activated Sensor for 10.42.x.x:6379"

# 9. Instana UI (wait 30–60 s after traffic)
#    Infrastructure → Kubernetes → banking namespace ✔
#    Applications → Services → api-producer, auth-service, ... ✔
#    Applications → Traces → waterfall spanning all layers ✔
#    Infrastructure → redis (auto-discovered, single entry) ✔
#    Infrastructure → postgresql ✔
#    Infrastructure → kong ✔
```

---

## Cross-Reference Index

| Topic | Document |
|-------|----------|
| Agent install (DaemonSet, Helm) — **start here** | [`13-k8s-agent-install.md`](./13-k8s-agent-install.md) |
| Agent install (EC2 host-agent, legacy) | [`01-agent-install.md`](./01-agent-install.md) |
| Kubernetes sensor and pod discovery | [`02-kubernetes-monitoring.md`](./02-kubernetes-monitoring.md) |
| OTel SDK, NATS trace propagation, NODE_IP vs cluster-DNS | [`03-opentelemetry.md`](./03-opentelemetry.md) |
| Kong sensor and OTel plugin | [`04-kong-sensor.md`](./04-kong-sensor.md) |
| Traefik sensor (Traefik DISABLED in this cluster) | [`05-traefik-sensor.md`](./05-traefik-sensor.md) |
| Redis sensor + instaredis native spans | [`06-redis-sensor.md`](./06-redis-sensor.md) |
| PostgreSQL sensor + instapgx native spans | [`07-postgresql-sensor.md`](./07-postgresql-sensor.md) |
| Synthetic monitoring (API Script tests) | [`08-synthetic-monitoring.md`](./08-synthetic-monitoring.md) |
| Why services show 0 calls / pod service detection | [`09-pod-service-detection.md`](./09-pod-service-detection.md) |
| K8s pods not detected (host-agent troubleshooting, legacy) | [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) |
| NATS metrics via nats-exporter + W3C propagation | [`11-nats-monitoring.md`](./11-nats-monitoring.md) |
| Ansible Automation Action sensor | [`12-ansible-automation-action.md`](./12-ansible-automation-action.md) |
| Instana Go Collector (go-sensor) — AutoProfile, process metrics | [`14-go-sensor.md`](./14-go-sensor.md) |
| Concert SBOM upload workflow | [`15-concert-sbom-upload.md`](./15-concert-sbom-upload.md) |
