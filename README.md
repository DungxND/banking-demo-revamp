# Banking Demo

A full-stack banking application where users can **create an account, log in, check their balance, send money to other users, and receive real-time notifications** when a transfer arrives.

The app has a web interface you open in a browser. Everything you do — logging in, viewing your balance, sending a transfer — happens instantly, and the recipient sees a live notification without refreshing the page.

> **Forked from** [kevinram164/banking-demo](https://github.com/kevinram164/banking-demo) — extended with Go microservices, OpenTelemetry tracing, Helm/Kubernetes deployment, and Instana observability.

---

## What this project demonstrates

- **API gateway pattern** — Kong routes all browser traffic. The frontend never talks directly to any service.
- **Event-driven microservices** — services communicate via NATS message passing, not direct HTTP calls between services.
- **Real-time notifications** — WebSocket push when a transfer lands, powered by Redis pub/sub.
- **CQRS read model** — account balances are served from a Redis cache, kept in sync by a durable event stream (NATS JetStream), with PostgreSQL as the fallback.
- **Production-ready observability** — structured JSON logs, Prometheus metrics on every service, OpenTelemetry distributed tracing, and Instana integration.

---

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

**How a request flows:** every browser call goes through Kong. Kong forwards `/api/*` to `api-producer`, which translates the URL into a NATS message — the right service picks it up, does the work, and replies. The browser gets a normal HTTP response and never knows NATS is involved.

The WebSocket path (`/ws`) is the exception: Kong routes it directly to `notification-service`, which subscribes to Redis for live transfer events and pushes them to the open socket.

---

## Services at a glance

| Service | What it does | Port |
|---|---|---|
| `frontend` | React 19 + Vite SPA, served by nginx | 80 |
| `kong` | API gateway — routes, CORS, rate limiting, tracing | 8000 |
| `api-producer` | Translates HTTP requests into NATS messages | 8080 |
| `auth-service` | Register, login, session management | 8001 |
| `account-service` | Balance, profile, admin queries | 8002 |
| `transfer-service` | Send money between accounts | 8003 |
| `notification-service` | Notification history + live WebSocket push | 8004 |
| `postgres` | Primary database (users, transfers, notifications) | 5432 |
| `redis` | Sessions, balance cache, real-time pub/sub | 6379 |
| `nats` | Message bus (request/reply + durable event stream) | 4222 |

---

## Run locally with Docker Compose

**Requirements:** Docker Desktop (or Docker Engine + Compose plugin)

```bash
git clone https://github.com/dungxnd/banking-demo-revamp
cd banking-demo-revamp
docker compose up --build
```

That's it. All services start together. Demo accounts are seeded automatically on first boot.

| What | URL |
|---|---|
| App | http://localhost:3000 |
| Kong proxy (direct API access) | http://localhost:8000 |
| NATS monitoring dashboard | http://localhost:8222 |
| NATS Prometheus metrics | http://localhost:7777/metrics |

To stop everything: `docker compose down`
To wipe the database and start fresh: `docker compose down -v && docker compose up --build`

### Frontend only (without Docker)

If you want to work on just the UI and already have the backend running elsewhere:

```bash
cd frontend
npm install
npm run dev   # opens at http://localhost:5173
```

Update the proxy target in `vite.config.js` to point at your Kong instance.

---

## Deploy to Kubernetes with Helm

### What you need before starting

- **A Kubernetes cluster** — the chart is designed for k3s (e.g. a single EC2 instance). It also works on k3d, minikube, EKS, and GKE.
- **`local-path` StorageClass** — used for Postgres and Redis volumes. k3s includes this by default. For other clusters, install [local-path-provisioner](https://github.com/rancher/local-path-provisioner).
- **`kubectl`** configured to talk to your cluster (`kubectl get nodes` should work)
- **`helm` ≥ 3.12** — check with `helm version`

The container images are public on `ghcr.io/dungxnd/banking-demo-revamp` — no registry credentials needed.

### Deploy

The chart sets up everything: namespaced secrets, NATS, Postgres, Redis, Kong, and all services.

```bash
# From the repo root
helm upgrade --install banking ./helm \
  --namespace banking --create-namespace \
  --wait --timeout 300s
```

Kong uses **NodePort 30080** — every node forwards port 30080 to Kong. No cloud load balancer required. The app is available at `http://<any-node-ip>:30080/`.

> **Helm 4 users:** `--atomic` is now `--rollback-on-failure`. Fresh installs default to Server-Side Apply. If you installed with Helm 3 and are upgrading, add `--server-side=false`.

### Check that everything started

```bash
# All pods should show Running or Completed within about 60 seconds
kubectl get pods -n banking

# Send a test request
curl -s http://<node-ip>:30080/api/health

# Consumers print this when they've connected to NATS and are ready
kubectl logs -n banking -l app=auth-service --tail=5 | grep nats_micro_service_started
```

### Tear down

```bash
# Remove the release (keeps data volumes)
helm uninstall banking -n banking

# Remove everything including data volumes and stuck pods
./helm/nuke.sh

# Remove everything AND reinstall from scratch
./helm/nuke.sh --reinstall
```

> **Warning:** `nuke.sh` deletes all PersistentVolumeClaims — database and Redis data is gone permanently.

### Deploy a new image version

Service names with hyphens must be quoted when passed to `--set`:

```bash
helm upgrade banking ./helm -n banking --reuse-values \
  --set 'auth-service.image.tag=sha-abc1234' \
  --set 'account-service.image.tag=sha-abc1234' \
  --set 'transfer-service.image.tag=sha-abc1234' \
  --set 'notification-service.image.tag=sha-abc1234' \
  --set 'api-producer.image.tag=sha-abc1234' \
  --set frontend.image.tag=sha-abc1234
```

### NodePort access (vanilla k8s / bare-metal)

Kong exposes port 30080 on every node via NodePort. Access the app at `http://<any-node-ip>:30080/`.

### Optional: Ingress for cloud clusters (EKS / GKE)

On bare-metal/k3s, Kong uses NodePort — no Ingress needed.
For cloud clusters with a domain name, enable the Ingress resource:

```bash
helm upgrade banking ./helm -n banking --reuse-values \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.host=banking.example.com \
  --set kong.service.type=LoadBalancer \
  --set kong.service.nodePort=null
```

| Cluster type | `ingress.className` |
|---|---|
| k3s (default target) | `traefik` |
| minikube | `nginx` |
| HAProxy Ingress | `haproxy` |

### Optional: Disable JetStream

JetStream (NATS durable event stream) is on by default and creates a 1 Gi volume for the event log. All services degrade gracefully without it. To disable:

```bash
helm upgrade banking ./helm -n banking --reuse-values \
  --set nats.jetstream.enabled=false
```

### Optional: Run database migrations via Helm

The chart includes a migration job that runs at upgrade time (disabled by default):

```bash
helm upgrade banking ./helm -n banking --reuse-values \
  --set dbMigration.enabled=true
```

To run migrations manually instead:

```bash
kubectl exec -it -n banking deploy/postgres -- \
  psql -U banking banking -f /migrations/<file>.sql
```

---

## Observability

### Logs

All services emit structured JSON logs (one JSON object per line). Useful commands:

```bash
# Watch all ERROR-level events in real time
kubectl logs -n banking --all-containers=true -f | grep '"level":"ERROR"'

# Follow completed transfers
kubectl logs -n banking -l app=transfer-service -f | grep '"msg":"transfer_success"'

# Follow balance cache updates
kubectl logs -n banking -l app=account-service -f | grep '"msg":"balance_projection_updated"'
```

### Prometheus metrics

Every service exposes a `/metrics` endpoint. Scrape them with Prometheus or check manually:

```bash
kubectl port-forward -n banking svc/auth-service 8001:8001
curl http://localhost:8001/metrics
```

Key metrics emitted by all consumer services:

| Metric | What it measures |
|---|---|
| `nats_messages_total` | Requests processed, labeled by action and outcome |
| `nats_handler_duration_seconds` | Time spent handling each message type |
| `nats_reconnects_total` | How often a service had to reconnect to NATS |

### OpenTelemetry tracing

Distributed traces are enabled automatically when an Instana agent (or any OTel-compatible collector) is running on the node. Each service Deployment already contains:

```yaml
- name: NODE_IP
  valueFrom:
    fieldRef:
      fieldPath: status.hostIP
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: "http://$(NODE_IP):4317"
```

Traces start flowing as soon as the agent is present — no extra configuration needed.
To point at a different collector (e.g. a standalone OpenTelemetry Collector):

```bash
helm upgrade banking ./helm -n banking --reuse-values \
  --set 'api-producer.extraEnv.OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability:4317' \
  --set 'auth-service.extraEnv.OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability:4317'
```

### NATS service health

The `nats/micro` framework provides built-in observability for all consumer services:

```bash
# Are all services up?
nats micro ping

# How many requests has each service handled?
nats micro stats auth-service
nats micro stats account-service
nats micro stats transfer-service
nats micro stats notification-service
```

---

## Common operations

```bash
# Restart a service after a config change
kubectl rollout restart deployment/auth-service -n banking

# Scale a service (NATS queue groups handle load balancing automatically)
kubectl scale deployment/transfer-service --replicas=3 -n banking

# Open a database shell
kubectl exec -it -n banking deploy/postgres -- psql -U banking banking

# Inspect NATS internals
kubectl port-forward -n banking svc/nats 8222:8222
curl http://localhost:8222/varz    # server info
curl http://localhost:8222/jsz     # JetStream streams and consumers

# Check the JetStream event stream
nats stream info BANKING_EVENTS
nats consumer info BANKING_EVENTS account-service-balance
```

---

## API endpoints

`api-producer` maps URL paths to NATS subjects. Unknown paths return `404` immediately — no NATS round-trip.

| HTTP method + path | Service | What it does |
|---|---|---|
| `POST /api/auth/register` | auth-service | Create a new account |
| `POST /api/auth/login` | auth-service | Log in, receive a session token |
| `GET /api/account/me` | account-service | Current user's profile |
| `GET /api/account/balance` | account-service | Current balance |
| `GET /api/account/lookup` | account-service | Look up another user |
| `GET /api/account/stats` | account-service | Admin: system stats |
| `GET /api/account/users` | account-service | Admin: all users |
| `GET /api/account/transfers` | account-service | Admin: all transfers |
| `GET /api/account/notifications` | account-service | Admin: all notifications |
| `GET /api/account/user-detail` | account-service | Admin: user detail |
| `POST /api/transfer/transfer` | transfer-service | Send money |
| `GET /api/notifications/notifications` | notification-service | Notification history |
| `GET /ws` | notification-service | WebSocket — live transfer events |

---

## Environment variables

These variables are injected from the Helm-managed secret. Override them in `helm/values.yaml` or via `--set`.

| Variable | Default | Used by |
|---|---|---|
| `DATABASE_URL` | `postgres://banking:bankingpass@postgres:5432/banking` | auth, account, transfer, notification |
| `REDIS_URL` | `redis://redis:6379` | auth, account, transfer, notification |
| `NATS_URL` | `nats://nats:4222` | api-producer + all consumers |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | set from node IP at pod start | all services |
| `DB_POOL_SIZE` | `15` | all consumers |
| `SESSION_TTL_SECONDS` | `86400` (24 h) | auth, account, transfer, notification |
| `USER_CACHE_TTL_SECONDS` | `300` (5 min) | auth |
| `PRESENCE_TTL_SECONDS` | `60` | notification |
| `LOG_AMOUNT_SECRET` | _(built-in default key)_ | transfer, account |
| `NATS_TRACE_SAMPLE_RATE` | `0.01` (1%) | api-producer |

---

## Repository layout

```
.
├── go.work                  # Go workspace — links all service modules
├── internal/                # Shared Go library imported by all services
│   ├── nats/                #   NATS consumer framework, JetStream helpers, middleware
│   ├── auth/                #   bcrypt helpers
│   ├── db/                  #   PostgreSQL pool, query builder, typed row structs
│   ├── health/              #   HTTP + NATS readiness handlers
│   ├── logging/             #   JSON logger, data masking (phone, account, amount)
│   ├── metrics/             #   Prometheus helpers
│   ├── redis/               #   Session, balance cache, presence, pub/sub
│   └── tracing/             #   OpenTelemetry provider initialisation
│
├── producer/                # api-producer — HTTP → NATS bridge              (:8080)
│
├── services/
│   ├── auth-service/        # register, login                                (:8001)
│   ├── account-service/     # balance, profile, admin, balance projection    (:8002)
│   ├── transfer-service/    # send money + JetStream event publish           (:8003)
│   └── notification-service/# notification history + WebSocket               (:8004)
│
├── migrations/              # SQL migration files (golang-migrate)
│
├── frontend/                # React 19 + Vite + Tailwind CSS v4 SPA
│   ├── src/
│   ├── Dockerfile           #   multi-stage build: Node → nginx:alpine
│   └── nginx.conf           #   SPA fallback + /api/* and /ws proxy to Kong
│
├── helm/                    # Helm chart — deploys the complete stack to Kubernetes
│   ├── Chart.yaml
│   ├── values.yaml          #   edit this to change images, credentials, resources
│   └── templates/
│
├── monitoring/              # Kubernetes manifests for Prometheus, Grafana, Jaeger
├── instana/                 # Instana agent config, synthetic tests, runbooks
├── docker-compose.yml       # Local dev stack
└── kong-compose.yml         # Kong declarative config for Compose
```

---

## Building images yourself

The CI pipeline builds and pushes images automatically on every commit. To build manually:

```bash
REGISTRY=ghcr.io/your-org/banking-demo

docker build -f producer/Dockerfile                      -t $REGISTRY/api-producer:latest .
docker build -f services/auth-service/Dockerfile         -t $REGISTRY/auth-service:latest .
docker build -f services/account-service/Dockerfile      -t $REGISTRY/account-service:latest .
docker build -f services/transfer-service/Dockerfile     -t $REGISTRY/transfer-service:latest .
docker build -f services/notification-service/Dockerfile -t $REGISTRY/notification-service:latest .
docker build -f frontend/Dockerfile frontend/            -t $REGISTRY/frontend:latest
```

Then update `image.repository` and `image.tag` in `helm/values.yaml` before deploying.

---

## Tech stack

| Layer | Technology |
|---|---|
| Frontend | React 19, Vite, Tailwind CSS v4 |
| API gateway | Kong 3.9 (DB-less, declarative config) |
| HTTP entry | Go + chi router (`api-producer`) |
| Message bus | NATS 2 with `nats/micro` service framework |
| Durable event bus | NATS JetStream (`BANKING_EVENTS` stream) |
| Session / cache / WS push | Redis 8 |
| Backend services | Go 1.26 |
| Database | PostgreSQL 18 |
| Query builder | `stephenafamo/bob` |
| Schema migrations | `golang-migrate` SQL files |
| Auth | bcrypt (`golang.org/x/crypto`) |
| Observability | OpenTelemetry OTLP/gRPC, Prometheus, Instana |
| Packaging | Helm (Helm 3 / Helm 4 compatible) |
| CI | GitHub Actions — GHCR image build + Kubernetes deploy |

---

## Architecture deep-dive

For a deeper look at how everything fits together, see the supplementary docs:

- [`ARCH-NATS-RPC.md`](ARCH-NATS-RPC.md) — NATS request/reply design, `nats/micro` service framework, per-action subject routing, JetStream event bus
- [`MICROSERVICES.md`](MICROSERVICES.md) — per-service action tables, shared `internal/` library, Kong routing config, database schema, Redis key space
- [`OBSERVABILITY.md`](OBSERVABILITY.md) — Prometheus query reference, `nats/micro` stats, self-hosted monitoring stack setup
- [`fork-docs/cqrs-plan.md`](fork-docs/cqrs-plan.md) — CQRS implementation: Redis balance cache, JetStream event projection, PostgreSQL fallback
- [`fork-docs/amqp-to-nats-migration.md`](fork-docs/amqp-to-nats-migration.md) — how the project migrated from AMQP to NATS, including `nats/micro` and per-action subjects
