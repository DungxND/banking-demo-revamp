# internal

Shared Go library used by all four consumer services and `api-producer`. Each subdirectory is a package within the `banking-demo/internal` module.

## Packages

### `nats` — RPC consumer framework + JetStream helpers

**`consumer.go`** — the core infrastructure every service is built on. Provides a `Consumer` struct that wraps the `nats/micro` service framework and dispatches messages to registered handlers.

```go
// Connect once — share connection with JetStream (transfer-service, account-service)
nc, _ := nats.Connect(cfg.NATSURL, serviceName, logger, metrics.ReconnectsTotal.Inc)
defer nc.Drain()

consumer := nats.NewConsumer(cfg.NATSURL, serviceName, "banking.auth", logger,
    nats.WithConn(nc),    // optional: share pre-connected conn
    nats.WithMetrics(m),
    nats.WithHandler("register", handleRegister(db, logger)),
    nats.WithHandler("login",    handleLogin(db, rc, logger)),
    nats.WithHandler("health",   health.NATSHandler("auth-service", pool, rc)),
)
consumer.Run(ctx) // blocks; each action gets its own nats/micro endpoint
```

**Key exported symbols — `consumer.go`:**

| Symbol | Description |
|--------|-------------|
| `Handler` | `func(ctx, action, payload, headers) (any, error)` — every handler signature |
| `Connect(url, name, logger, reconnectMetric)` | Opens a NATS connection with standard options (retry, jitter, ping keepalive). Use when sharing the conn with JetStream. |
| `NewConsumer(url, name, subjectPrefix, logger, ...Option)` | Creates a consumer; endpoints become `subjectPrefix + "." + action` |
| `WithHandler(action, h)` | Registers a handler for an action name |
| `WithConn(nc)` | Injects a pre-connected `*nats.Conn` (caller manages drain) |
| `WithMetrics(m)` | Enables Prometheus instrumentation via `ConsumerMetrics` |
| `WithPendingMsgs(n)` | Sets per-endpoint in-memory message limit (default 64) |
| `RequireSession(rc, next)` | Middleware: validates `x-session` header, injects user ID into context |
| `RequireAdmin(secret, rc, next)` | Middleware: checks `x-admin-secret` then validates session |
| `SessionMiddleware(rc)` | Returns a `func(Handler) Handler` closure for cleaner `main.go` code |
| `AdminMiddleware(secret, rc)` | Returns a `func(Handler) Handler` closure |
| `UserIDFromContext(ctx)` | Retrieves the user ID injected by `RequireSession`/`RequireAdmin` |
| `Reply(status, body)` | Convenience constructor for a typed RPC response |

**`jetstream.go`** — JetStream event bus helpers for the `BANKING_EVENTS` durable stream:

| Symbol | Description |
|--------|-------------|
| `StreamName = "BANKING_EVENTS"` | Canonical stream name constant |
| `SubjectTransferCompleted` | `banking.events.transfer.completed` |
| `ConsumerBalanceProjection` | `"account-service-balance"` (durable consumer name) |
| `InitStream(ctx, nc)` | Idempotent `CreateOrUpdateStream`; 30-day retention, 5-min dedup window |
| `PublishTransferEvent(ctx, js, evt)` | Publishes with `Nats-Msg-Id: {transferID}` dedup header |
| `NewBalanceConsumer(ctx, js)` | Creates/resumes durable pull consumer with `DeliverAllPolicy`, `MaxAckPending=100` |

**Middleware pattern:**

```go
requireSession := nats.SessionMiddleware(rc)
requireAdmin   := nats.AdminMiddleware(cfg.AdminSecret, rc)

nats.WithHandler("balance", requireSession(handleBalance(bdb, rc, logger)))
nats.WithHandler("stats",   requireAdmin(handleAdminStats(bdb, logger)))
```

`nats/micro` provides `$SRV.PING`, `$SRV.STATS`, `$SRV.INFO` for every registered service — visible
via `nats micro ls`, `nats micro stats <name>`, etc.

---

### `db` — Database access

pgx v5 connection pool, a `bob.DB` adapter, hand-written row structs, and query helpers.

```go
pool, _ := db.NewPool(ctx)           // reads DATABASE_URL; applies SCRAM-SHA-256 auth
bdb     := db.NewBobDB(pool)         // bob.DB for parameterized queries

u, err  := db.QueryUser(ctx, bdb, psql.Quote("phone"), phone)
db.IsNotFound(err)                   // true when sql.ErrNoRows
db.IsUniqueViolation(err)            // true when SQLSTATE 23505 — stable, no string matching

err = db.SerializableTx(ctx, bdb, func(ctx context.Context, tx bob.Transaction) error {
    // runs at SERIALIZABLE isolation
    return nil
})
```

**Exported types:** `User`, `Transfer`, `Notification` — all mirror the SQL schema exactly.

`UserIdentifierCol(accountNumber, phone, username)` returns the `(column, value)` pair for the
first non-empty identifier — used by both auth-service and transfer-service for consistent
multi-identifier lookup.

**Env vars:** `DATABASE_URL` (required), `DB_POOL_SIZE` (default 15).

---

### `redis` — Session, cache, balance read model, presence, pub/sub

```go
client, _ := redis.NewClient(redisURL)  // type Client = goredis.Client (alias, no go-redis import in callers)

// Session
sid, _   := redis.CreateSession(ctx, client, userID)
uid, err := redis.GetUserIDFromSession(ctx, client, sid)
// → redis.ErrUnauthorized when session missing or expired

// User cache — phone + username keys; both written on login
redis.SetUserCache(ctx, client, redis.CachedUser{...})
u, _ := redis.GetUserCacheByPhone(ctx, client, "0912345678")
u, _ := redis.GetUserCacheByUsername(ctx, client, "alice")

// Balance read model (Redis Hash "balance", field = userID string)
redis.SetBalance(ctx, client, userID, balance)
balance, ok, err := redis.GetBalance(ctx, client, userID)
// ok=false on cache miss (goredis.Nil absorbed) — caller falls back to DB

// Post-commit pipeline: DEL user_cache × 2 + HSET balance × 2 + PUBLISH (single round-trip)
redis.PublishTransferCompleted(ctx, client, evt, senderPhone, receiverPhone)

// Presence (TTL from PRESENCE_TTL_SECONDS, default 60 s)
redis.SetPresence(ctx, client, userID, true)
redis.PresenceTTL()  // derived heartbeat interval for ws.go

// Pub/sub
msgCh, unsub := redis.SubscribeNotify(ctx, client, userID)
defer unsub()
```

`type Client = goredis.Client` — services import `iredis "banking-demo/internal/redis"` and type
parameters as `*iredis.Client` without importing `go-redis` directly.

**Env vars:** `REDIS_URL`, `SESSION_TTL_SECONDS` (default 86400), `USER_CACHE_TTL_SECONDS`
(default 300), `PRESENCE_TTL_SECONDS` (default 60). All TTL values read once via `sync.OnceValue`.

---

### `auth` — Password hashing

```go
hash, _ := auth.HashPassword(password)   // bcrypt, rounds from BCRYPT_ROUNDS (default 10)
ok      := auth.VerifyPassword(password, hash)
```

**Env vars:** `BCRYPT_ROUNDS` (default 10).

---

### `logging` — Structured logger and masking helpers

```go
logger := logging.NewLogger("auth-service")  // JSON slog to stdout, service field pre-set
                                              // level from LOG_LEVEL env (default INFO)

logging.MaskPhone("0912345678")              // "09****78"
logging.MaskAccount("123456789012")          // "1234****12"
logging.MaskAmount(5000)                     // 12-char HMAC hex — hides exact amount in logs
```

`MaskAmount` reads `LOG_AMOUNT_SECRET` once via `sync.OnceValue`. `LOG_LEVEL` is read once at
first logger construction — set to `DEBUG` to see all request-level log lines.

**Env vars:** `LOG_AMOUNT_SECRET` (default built-in key), `LOG_LEVEL` (default `INFO`).

---

### `metrics` — Prometheus helpers

```go
m := metrics.NewConsumerMetrics("transfer-service")

// Pass to consumer — dispatch records action/status counters and duration histograms
nats.NewConsumer(..., nats.WithMetrics(m))

// Serve the scrape endpoint
router.Handle("/metrics", promhttp.Handler())
```

**Registered metrics per service:**

| Metric | Type | Labels |
|--------|------|--------|
| `nats_messages_total` | Counter | `action`, `status`, `service` |
| `nats_handler_duration_seconds` | Histogram | `action`, `service` |
| `nats_reconnects_total` | Counter | `service` |

---

### `tracing` — OpenTelemetry init

```go
shutdown := tracing.Init("auth-service", otlpEndpoint, logger)
defer shutdown(context.Background())
```

No-op when `otlpEndpoint` is empty. Configures a global OTLP/gRPC trace provider with a 5-second
shutdown timeout to prevent hung OTLP exporters on SIGTERM.

---

### `health` — Readiness handlers

Shared HTTP and NATS health-check handlers that ping both PostgreSQL and Redis:

```go
// HTTP — for Docker Compose healthcheck and Kubernetes readiness probe
router.Get("/health", health.HTTPHandler("auth-service", pool, redisClient))

// NATS — responds to "health" action on the service subject
nats.WithHandler("health", health.NATSHandler("auth-service", pool, redisClient))
```

Response on healthy: `200 {"status":"healthy","service":"...","database":"ok","redis":"ok"}`
Response on unhealthy: `503 {"status":"unhealthy","database":"error","redis":"ok"}`

---

### `service` — Shared lifecycle scaffolding

```go
// InitDeps opens DB pool + Redis; returns cleanup func
d, cleanup, err := service.InitDeps(ctx, cfg.RedisURL, logger)
defer cleanup()

// Runner manages 3 goroutines: NATS consumer, HTTP server, OS signal handler
service.NewRunner(consumer, server, logger).Run(ctx)
```

`Runner.Run` blocks until SIGINT/SIGTERM or ctx cancellation. The HTTP server gets a 10-second
graceful drain window; NATS `nc.Drain()` flushes pending outbound after the context is cancelled.

---

## Module

```
module: banking-demo/internal
go:     1.26.1
```

All services reference this module via a `replace` directive in their `go.mod`:

```
replace banking-demo/internal => ../../internal
```

This is wired automatically by `go.work` at the workspace root — no manual `replace` management needed during development.
