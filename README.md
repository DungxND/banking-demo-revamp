# Banking Demo

> **Forked from** [kevinram164/banking-demo](https://github.com/kevinram164/banking-demo) — extended with Go microservices, OpenTelemetry tracing, Helm chart, Ansible/Terraform infra, and performance benchmarks.

A microservices banking application demonstrating an event-driven architecture: Kong API gateway → NATS RPC request/reply → Go consumer services, with a JetStream durable event bus for CQRS read-model projection. Includes a React 19 + Vite frontend, a full Helm chart for Kubernetes, and a Docker Compose stack for local development.

## Architecture

```mermaid
flowchart TD
    Browser["🌐 Browser"]

    subgraph Frontend["Frontend  (nginx :80)"]
        FE["React 19 + Vite SPA"]
    end

    subgraph Gateway["API Gateway  (Kong 3.9 :8000)"]
        Kong["Kong\nDB-less declarative config\nCORS plugin on all routes"]
    end

    subgraph RPC["HTTP → NATS Bridge"]
        Producer["api-producer  :8080\nsubjectFromPath() → nc.RequestMsgWithContext()"]
    end

    subgraph Messaging["Message Bus  (NATS 2 + JetStream :4222)"]
        NATS["Core NATS — request/reply\neper-action inbox subjects\nnats/micro service framework"]
        JS["JetStream — BANKING_EVENTS\ndurable event log · 30-day retention"]
    end

    subgraph Consumers["Consumer Services  (Go 1.26)"]
        Auth["auth-service  :8001\nbanking.auth.*"]
        Account["account-service  :8002\nbanking.account.*\n+ balance projection consumer"]
        Transfer["transfer-service  :8003\nbanking.transfer.*\n+ JS publisher"]
        Notify["notification-service  :8004\nbanking.notification.*\n+ HTTP/WebSocket server"]
    end

    subgraph Data["Data Layer"]
        PG[("PostgreSQL 18\nusers · transfers · notifications")]
        Redis[("Redis 8\nsession · user_cache · balance hash\npresence · notify pub/sub")]
    end

    Browser -->|"GET /"| Frontend
    Browser -->|"POST /api/*"| Kong
    Browser -->|"GET /ws"| Kong
    Frontend -->|"proxies /api/* + /ws"| Kong
    Kong -->|"/api/* → strip_path:false"| Producer
    Kong -->|"/ws direct route"| Notify
    Producer -->|"NATS RPC {payload}"| NATS
    NATS --> Auth & Account & Transfer & Notify
    Transfer -->|"banking.events.transfer.completed\nNats-Msg-Id dedup"| JS
    JS -->|"durable pull consumer\nDeliverAllPolicy"| Account
    Transfer -->|"DEL user_cache\nHSET balance\nPUBLISH notify:{id}"| Redis
    Notify -->|"SUBSCRIBE notify:{uid} → WebSocket"| Redis
    Auth & Account & Transfer & Notify --> PG
    Auth & Account & Transfer & Notify --> Redis
```

**REST request flow:**
1. Browser → nginx → Kong → `api-producer`
2. `api-producer` maps the exact URL path to a full NATS action subject via `subjectFromPath()` (e.g. `/api/auth/login` → `banking.auth.login`), then calls `nc.RequestMsgWithContext` — NATS creates a temporary inbox reply subject automatically
3. The target consumer's `nats/micro` endpoint processes the message and calls `req.Respond(data)`
4. The reply is delivered directly to the waiting goroutine in `api-producer` and returned as the HTTP response

**Transfer post-commit pipeline (after PostgreSQL commit):**
1. Redis pipeline (single round-trip): `DEL user_cache:phone:{sender}`, `DEL user_cache:phone:{receiver}`, `HSET balance {senderID} {newBalance}`, `HSET balance {receiverID} {newBalance}`, `PUBLISH notify:{receiverID}`
2. JetStream publish: `banking.events.transfer.completed` with `Nats-Msg-Id: {transferID}` — server deduplicates retries within 5-minute window

**Balance read model:**
- `account-service` serves balance from the Redis `balance` hash (written by the post-commit pipeline above)
- On cache miss (cold start, Redis restart): falls back to PostgreSQL and warms the hash
- JetStream pull consumer (`account-service-balance`, `DeliverAllPolicy`): replays the full event log after a Redis wipe — no DB round-trip needed

**WebSocket flow:** Browser → Kong → `notification-service` directly (bypasses NATS entirely). Real-time transfer events arrive via Redis pub/sub subscription.

---

## Granular architecture — request lifecycle & internal wiring

```mermaid
flowchart LR
    Browser(["🌐 Browser"])

    subgraph FE["nginx :80"]
        SPA["React SPA"]
    end

    subgraph GW["Kong 3.9  :8000"]
        route_api["/api/* → api-producer"]
        route_ws["/ws   → notification-service"]
        cors["CORS plugin"]
    end

    subgraph PROD["api-producer  :8080"]
        chi["chi router"]
        pTS["subjectFromPath()\nexact-match switch"]
        rpc["rpcClient.call()\nnc.RequestMsgWithContext()\nNats-Trace-Dest header (1% sampled)"]
        inbox["← _INBOX reply\n(NATS ephemeral)"]
    end

    subgraph NATS_BUS["NATS 2 + JetStream  :4222"]
        s_auth["banking.auth.login\nbanking.auth.register"]
        s_acct["banking.account.balance\nbanking.account.me\nbanking.account.lookup …"]
        s_xfer["banking.transfer.transfer"]
        s_ntfy["banking.notification.notifications"]
        s_evt["banking.events.transfer.completed\n(JetStream BANKING_EVENTS stream)"]
    end

    subgraph INT["internal/ shared library"]
        mw["nats.RequireSession\nnats.RequireAdmin\n(middleware chain)"]
        micro["nats/micro service framework\n$SRV.PING · $SRV.STATS · $SRV.INFO"]
        metrics_pkg["metrics.ConsumerMetrics\nnats_messages_total\nnats_handler_duration_seconds"]
        tracing_pkg["tracing.NewProvider()\nOTel OTLP/gRPC"]
        redis_pkg["redis.Session\nredis.UserCache (phone+username)\nredis.GetBalance / SetBalance\nredis.PublishTransferCompleted\nredis.SubscribeNotify"]
        db_pkg["db.SerializableTx\nbob.DB query builder\npgx v5 pool"]
        log_pkg["logging.MaskPhone\nlogging.MaskAccount\nlogging.MaskAmount (HMAC)"]
        js_pkg["nats.InitStream\nnats.PublishTransferEvent\nnats.NewBalanceConsumer"]
    end

    subgraph AUTH["auth-service  :8001"]
        h_reg["register\nbcrypt hash → INSERT users"]
        h_login["login\nVerifyPassword\nSET session:{sid}\ncache → user_cache:phone + username"]
    end

    subgraph ACCT["account-service  :8002"]
        h_me["me / lookup\n(RequireSession)"]
        h_bal["balance\nRedis HGet → DB fallback\n+ SetBalance warm-up"]
        h_admin["stats / users / transfers\n(RequireAdmin)"]
        js_cons["balance projection goroutine\nJetStream pull consumer\nDeliverAllPolicy"]
    end

    subgraph XFER["transfer-service  :8003"]
        h_tx["transfer\nSERIALIZABLE TX\nSELECT FOR UPDATE (ordered)\ndebit + credit + rows + notify"]
        pipe["Tier 2: Redis pipeline\nDEL user_cache + HSET balance + PUBLISH"]
        js_pub["Tier 3: JetStream publish\nNats-Msg-Id = transferID"]
    end

    subgraph NTFY["notification-service  :8004"]
        h_list["notifications action\n(RequireSession)"]
        ws_srv["WebSocket /ws\nsession via query param\nheartbeat → presence:{uid}"]
        sub["SubscribeNotify(userID)\n→ push to WS client"]
    end

    PG[("PostgreSQL 18")]
    Redis[("Redis 8")]

    Browser --> SPA
    Browser -->|"/api/*"| GW
    Browser -->|"/ws"| GW
    GW --> route_api & route_ws
    route_api --> chi
    route_ws --> ws_srv

    chi --> pTS --> rpc
    rpc -->|"NATS msg + headers\n{x-session, x-admin-secret}"| NATS_BUS
    NATS_BUS -->|"msg.Respond → _INBOX"| inbox
    inbox -->|"rpcResponse{status,body}"| rpc

    s_auth --> mw --> AUTH
    s_acct --> mw --> ACCT
    s_xfer --> mw --> XFER
    s_ntfy --> mw --> NTFY

    NATS_BUS --- s_auth & s_acct & s_xfer & s_ntfy
    s_evt --- js_cons

    h_tx --> pipe --> redis_pkg
    h_tx --> js_pub --> s_evt
    js_cons --> redis_pkg

    AUTH --> db_pkg --> PG
    ACCT --> db_pkg
    XFER --> db_pkg
    NTFY --> db_pkg

    h_login --> redis_pkg
    h_me & h_bal & h_admin --> redis_pkg
    ws_srv --> sub --> redis_pkg
    redis_pkg --> Redis

    AUTH & ACCT & XFER & NTFY --> metrics_pkg & micro
    AUTH & ACCT & XFER & NTFY --> tracing_pkg
    AUTH & ACCT & XFER & NTFY --> log_pkg
    XFER & ACCT --> js_pkg
```

**Redis key-space at a glance:**

| Key | Written by | Read by | Purpose |
|-----|-----------|---------|---------|
| `session:{sid}` | auth-service (login) | all services via `RequireSession` | Session token → user ID |
| `user_cache:phone:{phone}` | auth-service | auth-service | Login fast-path; DEL'd on transfer commit |
| `user_cache:username:{username}` | auth-service | auth-service | Login fast-path; DEL'd on transfer commit |
| `balance` (Hash) | transfer-service (post-commit) · account-service (warm-up) | account-service | Balance read model — HGet O(1) per request |
| `presence:{userID}` | notification-service (WS heartbeat) | — | Online status; TTL = `PRESENCE_TTL_SECONDS` |
| `notify:{userID}` | transfer-service (post-commit pub) | notification-service (sub) | Real-time transfer push to open WebSocket |

---

## Repository layout

```
.
├── go.work                  # Go workspace — links all modules
├── internal/                # Shared Go library (all services import this)
│   ├── nats/                # Consumer framework (nats/micro), JetStream helpers, middleware
│   ├── auth/                # bcrypt hash + verify
│   ├── db/                  # pgx pool, bob.DB adapter, typed row structs, query helpers
│   ├── health/              # HTTP + NATS readiness handlers
│   ├── logging/             # slog JSON logger, MaskPhone/Account/Amount helpers
│   ├── metrics/             # Prometheus helpers + ConsumerMetrics
│   ├── redis/               # Session, user cache, balance read model, presence, pub/sub
│   └── tracing/             # OpenTelemetry provider init
│
├── producer/                # api-producer: stateless HTTP → NATS RPC proxy         (:8080)
│
├── services/
│   ├── auth-service/        # register, login                                        (:8001)
│   ├── account-service/     # me, balance, lookup, admin/*, balance projection       (:8002)
│   ├── transfer-service/    # initiate transfer + JS event publish                  (:8003)
│   └── notification-service/# GET /notifications, WebSocket /ws                     (:8004)
│
├── migrations/              # golang-migrate SQL files (source of truth for schema)
│
├── frontend/                # React 19 + Vite + Tailwind CSS v4 SPA
│   ├── src/
│   ├── vite.config.js
│   ├── Dockerfile           # multi-stage: node build → nginx:alpine serve
│   └── nginx.conf           # SPA fallback + /api/* and /ws proxy to Kong
│
├── helm/                    # Umbrella Helm chart — deploys the full stack
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── charts/
│   └── templates/
│
├── monitoring/              # Kubernetes manifests: Prometheus, Grafana, Jaeger, OTel collector
├── kong-ha/                 # Kong DB-mode config import job (HA deployments)
├── instana/                 # Instana agent configs, synthetic tests, runbooks
├── fork-docs/               # Migration plans and architecture notes
│
├── docker-compose.yml       # Local dev: full stack in one command (NATS with JetStream)
└── kong-compose.yml         # Kong DB-less declarative config (mounted by Compose)
```

---

## Tech stack

| Layer | Technology |
|-------|-----------|
| Frontend | React 19, Vite, Tailwind CSS v4 |
| API Gateway | Kong 3.9 (DB-less in Compose; DB-mode in k8s HA) |
| HTTP entry | Go + chi (`api-producer`) |
| Message bus | NATS 2 — `nats/micro` service framework; request/reply via inbox subjects |
| Durable event bus | NATS JetStream — `BANKING_EVENTS` stream; pull consumer for balance projection |
| Session / cache / WS notify | Redis 8 |
| Consumer services | Go 1.26 |
| Database | PostgreSQL 18 |
| DB layer | `stephenafamo/bob` — composable query builder + codegen |
| Schema migrations | `golang-migrate` SQL files |
| Auth | bcrypt (`golang.org/x/crypto`) |
| Observability | OpenTelemetry OTLP/gRPC, Prometheus `/metrics`, `nats/micro` `$SRV.STATS`, Instana |
| Packaging | Helm (Helm 4 compatible) |
| CI | GitHub Actions — GHCR image build + k8s deploy |

---

## Quick start — Docker Compose

```bash
git clone https://github.com/dungxnd/banking-demo
cd banking-demo
docker compose up --build
```

| Service | URL |
|---------|-----|
| App (frontend) | http://localhost:3000 |
| Kong proxy | http://localhost:8000 |
| NATS monitoring | http://localhost:8222 — `/healthz` `/varz` `/connz` `/jsz` |
| NATS metrics (exporter) | http://localhost:7777/metrics |
| PostgreSQL | localhost:5432 |

Demo credentials are seeded automatically on first boot.

JetStream is enabled in the Compose stack (`-js` flag). To run without it (Core NATS only), remove `-js` from the NATS `command` in `docker-compose.yml` — all services degrade gracefully.

> The frontend container proxies `/api/*` and `/ws` to Kong at `http://kong:8000`, so all API traffic flows through Kong even in Compose.

---

## Frontend dev server (without Docker)

```bash
cd frontend
npm install
npm run dev   # Vite dev server on http://localhost:5173
```

Point `vite.config.js` proxy target at your Kong instance, or run `docker compose up` for the backend and connect from there.

---

## Building and pushing images

Each service has its own Dockerfile. Build from the repo root:

```bash
REGISTRY=ghcr.io/your-org/banking-demo

docker build -f producer/Dockerfile                      -t $REGISTRY/api-producer:latest .
docker build -f services/auth-service/Dockerfile         -t $REGISTRY/auth-service:latest .
docker build -f services/account-service/Dockerfile      -t $REGISTRY/account-service:latest .
docker build -f services/transfer-service/Dockerfile     -t $REGISTRY/transfer-service:latest .
docker build -f services/notification-service/Dockerfile -t $REGISTRY/notification-service:latest .
docker build -f frontend/Dockerfile frontend/            -t $REGISTRY/frontend:latest
```

CI builds and pushes automatically on every push via `.github/workflows/docker-build.yml`.

---

## Kubernetes deployment (Helm)

### Prerequisites

- Kubernetes cluster (k3d, minikube, EKS, EC2 k3s)
- `kubectl` configured for the target cluster
- `helm` ≥ 3.12
- Images pushed to a registry accessible from the cluster

### Create the NATS secret

```bash
kubectl create secret generic nats-connection-secret \
  --namespace banking \
  --from-literal=NATS_URL=nats://nats:4222
```

### Deploy

```bash
helm upgrade --install banking-demo ./helm \
  --namespace banking --create-namespace \
  --set global.imageRegistry=ghcr.io/your-org/banking-demo \
  --set ingress.host=banking.example.com
```

JetStream is enabled by default in `helm/charts/nats/values.yaml` (`nats.jetstream.enabled: true`). A 1 Gi `PersistentVolumeClaim` is created for stream storage. To disable JetStream:

```bash
helm upgrade banking-demo ./helm -n banking --reuse-values \
  --set nats.jetstream.enabled=false
```

Override image tags for a rolling deploy:

```bash
helm upgrade banking-demo ./helm -n banking --reuse-values \
  --set auth-service.image.tag=sha-abc1234 \
  --set account-service.image.tag=sha-abc1234 \
  --set transfer-service.image.tag=sha-abc1234 \
  --set notification-service.image.tag=sha-abc1234 \
  --set api-producer.image.tag=sha-abc1234 \
  --set frontend.image.tag=sha-abc1234
```

### Verify

```bash
kubectl get pods -n banking
# All pods reach Running; consumers log `nats_micro_service_started` on success

kubectl get ingress -n banking
```

### Ingress class

| Cluster | `ingress.className` |
|---------|---------------------|
| k3d / k3s | `traefik` (default) |
| minikube | `nginx` |
| Production (HAProxy) | `haproxy` |

---

## Environment variables

All services share these variables. Each service also accepts its own (documented in its `README.md`).

| Variable | Default | Used by |
|----------|---------|---------|
| `DATABASE_URL` | `postgresql://banking:bankingpass@postgres:5432/banking` | auth, account, transfer, notification |
| `REDIS_URL` | `redis://redis:6379/0` | auth, account, transfer, notification |
| `NATS_URL` | `nats://nats:4222` | api-producer + all consumers |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | _(unset — tracing disabled)_ | all — e.g. `http://otel-collector:4317` |
| `DB_POOL_SIZE` | `15` | all consumers |
| `SESSION_TTL_SECONDS` | `86400` (24 h) | auth, account, transfer, notification |
| `USER_CACHE_TTL_SECONDS` | `300` (5 min) | auth |
| `PRESENCE_TTL_SECONDS` | `60` | notification |
| `LOG_AMOUNT_SECRET` | _(default key)_ | transfer, account |
| `NATS_TRACE_SAMPLE_RATE` | `0.01` (1%) | api-producer — `Nats-Trace-Dest` header rate |

---

## NATS subject routing

`api-producer` maps each known URL path to a full NATS action subject (Phase 6b per-action routing):

| HTTP path | NATS subject | Consumer |
|-----------|-------------|---------|
| `POST /api/auth/register` | `banking.auth.register` | auth-service |
| `POST /api/auth/login` | `banking.auth.login` | auth-service |
| `GET /api/account/me` | `banking.account.me` | account-service |
| `GET /api/account/balance` | `banking.account.balance` | account-service |
| `GET /api/account/lookup` | `banking.account.lookup` | account-service |
| `GET /api/account/stats` | `banking.account.stats` | account-service (admin) |
| `GET /api/account/users` | `banking.account.users` | account-service (admin) |
| `GET /api/account/transfers` | `banking.account.transfers` | account-service (admin) |
| `GET /api/account/notifications` | `banking.account.notifications` | account-service (admin) |
| `GET /api/account/user-detail` | `banking.account.user-detail` | account-service (admin) |
| `POST /api/transfer/transfer` | `banking.transfer.transfer` | transfer-service |
| `GET /api/notifications/notifications` | `banking.notification.notifications` | notification-service |
| `/ws` | — | notification-service (direct via Kong, no NATS) |

Each consumer registers one `nats/micro` endpoint per action. Unknown paths return HTTP `404` immediately at the producer — no NATS round-trip.

**JetStream event subject** (not RPC — no reply):

| Subject | Publisher | Consumers |
|---------|-----------|-----------|
| `banking.events.transfer.completed` | transfer-service | account-service (balance projection) |

---

## Observability

### Structured JSON logs

All services emit newline-delimited JSON via `log/slog`. Every line carries `"service"` and `"level"`.

```bash
# Watch for NATS reconnect events across all consumers
kubectl logs -n banking --all-containers=true -f | grep '"msg":"nats_reconnected"'

# Tail transfer completions
kubectl logs -n banking -l app=transfer-service -f | grep '"msg":"transfer_success"'

# Follow balance projection updates
kubectl logs -n banking -l app=account-service -f | grep '"msg":"balance_projection_updated"'

# Follow all ERROR-level events
kubectl logs -n banking --all-containers=true -f | grep '"level":"ERROR"'
```

### Prometheus metrics

Every service exposes `GET /metrics`. Consumer services automatically emit:

| Metric | Type | Description |
|--------|------|-------------|
| `nats_messages_total` | Counter | Messages processed `{action, status, service}` |
| `nats_handler_duration_seconds` | Histogram | Handler execution time `{action, service}` |
| `nats_reconnects_total` | Counter | NATS reconnect events `{service}` |

`nats-exporter` (`:7777/metrics`) additionally exposes NATS server stats. The `api-producer` exposes HTTP and RPC metrics — see [`producer/README.md`](producer/README.md).

### nats/micro built-in observability

Every consumer service registers with the `nats/micro` framework, which provides:

```bash
# List all running service instances
nats micro ls

# Per-action request counts, errors, average latency
nats micro stats auth-service
nats micro stats account-service
nats micro stats transfer-service
nats micro stats notification-service

# Service descriptor + all endpoint subjects
nats micro info account-service

# Health ping (replaces the explicit health action)
nats micro ping
```

### OpenTelemetry tracing

Disabled by default. Enable by setting `OTEL_EXPORTER_OTLP_ENDPOINT`:

```bash
helm upgrade ... \
  --set 'global.env.OTEL_EXPORTER_OTLP_ENDPOINT=http://instana-agent.instana-agent:4317'
```

---

## Common operations

```bash
# Restart a service after a config change
kubectl rollout restart deployment/auth-service -n banking

# Scale a consumer horizontally (nats/micro queue groups handle load balancing)
kubectl scale deployment/transfer-service --replicas=3 -n banking

# Inspect NATS server stats (including JetStream)
kubectl port-forward -n banking svc/nats 8222:8222
curl http://localhost:8222/varz    # server info
curl http://localhost:8222/connz   # connected clients
curl http://localhost:8222/jsz     # JetStream streams and consumers

# Inspect JetStream stream directly
nats stream info BANKING_EVENTS
nats consumer info BANKING_EVENTS account-service-balance

# Open psql on the in-cluster Postgres
kubectl exec -it -n banking deploy/postgres -- psql -U banking banking

# Run schema migrations
docker run --rm \
  -v $(pwd)/migrations:/migrations \
  migrate/migrate \
  -path=/migrations -database "$DATABASE_URL" up

# Uninstall (keeps PVCs and namespace)
helm uninstall banking-demo -n banking
```

---

## Architecture deep-dive

See [`ARCH-NATS-RPC.md`](ARCH-NATS-RPC.md) for the NATS request/reply architecture, `nats/micro` service framework, per-action subject hierarchy, RPC envelope format, JetStream event bus, and the rationale for choosing NATS over AMQP.

See [`MICROSERVICES.md`](MICROSERVICES.md) for per-service action tables, the shared `internal/` library, Kong routing, database schema, and Redis key space.

See [`OBSERVABILITY.md`](OBSERVABILITY.md) for the full metrics reference, Prometheus queries, `nats/micro` stats, and the self-hosted monitoring stack setup.

See [`fork-docs/golang-migration-plan.md`](fork-docs/golang-migration-plan.md) for the Go rewrite plan and library decisions.

See [`fork-docs/cqrs-plan.md`](fork-docs/cqrs-plan.md) for the CQRS implementation (Tier 1b cache fix, Tier 2 balance read model, Tier 3 JetStream event bus) — all tiers implemented.

See [`fork-docs/amqp-to-nats-migration.md`](fork-docs/amqp-to-nats-migration.md) for the full AMQP → NATS migration plan including Phase 6a/6b (`nats/micro` + per-action subjects) — all phases implemented.
