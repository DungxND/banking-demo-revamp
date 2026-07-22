# Tổng Quan Observability với Instana — banking-demo

> **Mục đích:** Giải thích cách Instana thu thập logs, metrics và traces từ mọi tầng của
> stack banking-demo — tập trung vào ba tầng tiếp xúc người dùng: **frontend (Nginx)**,
> **api-producer** (Go Chi HTTP gateway), và **các Go microservices**
> (auth / account / transfer / notification).
>
> Để biết hướng dẫn cài đặt đầy đủ, xem [`13-k8s-agent-install.md`](./13-k8s-agent-install.md).
> Phiên bản tiếng Anh gốc: [`00-overview.md`](./00-overview.md).

---

## Hiểu nhanh: Ba loại tín hiệu Observability

Trước khi đọc chi tiết, cần hiểu ba khái niệm cốt lõi mà Instana thu thập:

| Tín hiệu | Là gì | Dùng để làm gì |
|----------|-------|----------------|
| **Logs** | Chuỗi văn bản ghi lại sự kiện đã xảy ra | Debug lỗi, audit, xem chi tiết request |
| **Metrics** | Số đo định kỳ (CPU, số request/s, latency) | Cảnh báo, dashboard, phát hiện xu hướng |
| **Traces** | "Hành trình" của một request xuyên qua nhiều service | Tìm chính xác service nào gây ra request chậm |

Ví dụ thực tế: user đăng nhập mất 3 giây.
- **Logs** cho biết: `auth-service` có log lỗi gì không?
- **Metrics** cho biết: database đang có bao nhiêu connection?
- **Traces** cho biết: request mất 2.8 giây ở bước `SELECT * FROM users` trong PostgreSQL,
  không phải ở auth-service hay Kong.

---

## Kiến Trúc Tổng Thể

```
Trình duyệt (Browser)
  │
  ▼ HTTPS / WSS  (port 443 → hostPort 80 qua Kong trên EC2)
┌──────────────────────────────────────────────────────────────────┐
│  Nginx (frontend)                    OTLP/gRPC → agent :4317    │
│    /          → SPA tĩnh (React)                                 │
│    /api/*     → kong:8000  (proxy REST, truyền W3C context)     │
│    /ws        → kong:8000  (proxy WebSocket, truyền context)    │
└──────────────────────────────────────────────────────────────────┘
  │
  ▼ Header W3C traceparent được truyền xuống bởi otel_trace_context propagate
┌──────────────────────────────────────────────────────────────────┐
│  Kong 3.9 (API gateway, DB-less, hostPort 80)                   │
│    OTLP/HTTP → agent :4318    (Kong OTel plugin)                │
│    Tất cả /api/* routes → api-producer:8080                     │
│    /ws          → notification-service:8004                     │
└──────────────────────────────────────────────────────────────────┘
  │
  ▼
┌──────────────────────────────────────────────────────────────────┐
│  api-producer (Go Chi)               OTLP/gRPC → agent :4317    │
│    otelhttp.NewHandler bọc toàn bộ router                       │
│    go-sensor → agent :42699  (process metrics + AutoProfile)    │
│    NATS request/reply → các consumer service                    │
└──────────────────────────────────────────────────────────────────┘
  │  NATS RPC (W3C traceparent được inject vào msg.Headers)
  ├─ banking.auth.*         → auth-service
  ├─ banking.accounts.*     → account-service
  ├─ banking.transfers.*    → transfer-service
  └─ banking.notifications.* → notification-service
       │  Mỗi consumer: OTLP/gRPC → agent :4317 + go-sensor → :42699
       ├─ PostgreSQL 18  (instapgx native spans + pg_stat sensor)
       └─ Redis 8        (instaredis native spans + INFO sensor)
```

> **Traefik KHÔNG chạy.** k3s được cài với `--disable traefik`. Kong xử lý toàn bộ
> ingress trực tiếp qua `hostPort: 80`. Không có tầng Traefik trong deployment này.

---

## Ma Trận Tín Hiệu — Instana Thu Thập Gì và Như Thế Nào

| Component | Logs | Metrics | Traces |
|-----------|------|---------|--------|
| **frontend (Nginx)** | stdout → k8s | `stub_status` scrape (agent tự phát hiện) | `ngx_otel_module` → OTLP/gRPC `:4317` |
| **Kong** | stdout → k8s | Poll Admin API (`/metrics` qua sensor) | Kong OTel plugin → OTLP/HTTP `:4318` |
| **api-producer** | JSON có cấu trúc (slog) → stdout | `/metrics` Prometheus (có annotation) | `otelhttp` → OTLP/gRPC `:4317` + go-sensor `:42699` |
| **auth-service** | JSON có cấu trúc (slog) → stdout | `/metrics` NATS consumer metrics | NATS consumer span (con của api-producer) + go-sensor `:42699` |
| **account-service** | JSON có cấu trúc (slog) → stdout | `/metrics` NATS consumer metrics | NATS consumer span + go-sensor `:42699` |
| **transfer-service** | JSON có cấu trúc (slog) → stdout | `/metrics` NATS consumer metrics | NATS consumer span + go-sensor `:42699` |
| **notification-service** | JSON có cấu trúc (slog) → stdout | `/metrics` NATS consumer metrics | `otelhttp` WS span + NATS consumer span + go-sensor `:42699` |
| **PostgreSQL** | — | `pg_stat_*` views (agent sensor, poll 10 giây) | `instapgx` native DB spans |
| **Redis** | — | `INFO`/`SLOWLOG` (agent tự phát hiện, poll 10 giây) | `instaredis` native Redis spans |
| **NATS** | — | `nats-exporter` → Prometheus `:7777` (annotated) | Không có native sensor — trace liên tục qua W3C headers |
| **k3s cluster** | — | k8sensor (DaemonSet Deployment) | — |

---

## Triển Khai Agent

### DaemonSet agent là gì?

Instana **DaemonSet agent** (Helm, namespace `instana-agent`) chạy trực tiếp bên trong
cluster k3s trên EC2 node.

> **DaemonSet** là loại Kubernetes workload đảm bảo mỗi node luôn có **đúng một pod**.
> Khi cluster thêm node mới, Kubernetes tự tạo thêm agent pod trên node đó. Khi node bị
> xóa, pod cũng bị xóa. DaemonSet là lựa chọn chuẩn cho các công cụ monitoring vì nó
> đảm bảo coverage toàn diện — không node nào bị bỏ sót.

Bên cạnh DaemonSet agent, còn có **k8sensor** Deployment xử lý toàn bộ giám sát Kubernetes API
(pods, services, namespaces, workloads).

```
namespace instana-agent
  ├── DaemonSet: instana-agent    ← một pod trên mỗi node
  │     ├── lắng nghe: :4317 (OTLP/gRPC), :4318 (OTLP/HTTP), :42699 (Instana native)
  │     ├── tự phát hiện: Nginx (stub_status), Redis (scan containerd), PostgreSQL
  │     ├── poll: PostgreSQL pg_stat_*, Kong Admin API :8001, nats-exporter :7777
  │     └── kéo: instana/configuration.yaml từ git (nhánh main, hot-reload)
  └── Deployment: k8sensor        ← theo dõi Kubernetes API
        └── báo cáo pods/services/namespaces/workloads lên Instana backend
```

**Cấu hình được quản lý qua git:** push lên `main` → Click update config trên Instana UI →
agent kéo và hot-reload [`instana/configuration.yaml`](../configuration.yaml) trong ~30 giây.
Không cần `helm upgrade` để thay đổi cấu hình sensor.

### NODE_IP vs Cluster-DNS — Hai cách gửi OTLP

Đây là điểm hay gây nhầm lẫn nhất trong toàn bộ thiết lập. Cần hiểu rõ vì sao có hai endpoint:

**NODE_IP là gì?**

Mỗi pod trong Kubernetes chạy trên một **node** (máy chủ / EC2 instance). `NODE_IP` là
địa chỉ IP của chính cái máy đó — không phải IP của pod.

Nó được inject vào mỗi pod qua Kubernetes **Downward API** — cơ chế cho phép pod đọc
thông tin về chính nó và môi trường nơi nó đang chạy:

```yaml
# helm/values.yaml — env block trong mọi service pod
env:
  - name: NODE_IP
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP   # ← IP của node (máy chủ), không phải IP của pod
```

Vì DaemonSet agent mở port `:4317` trên **host network interface** của node (via `hostPort`),
gửi tới `NODE_IP:4317` nghĩa là gửi thẳng tới agent pod trên **cùng node vật lý**:

```
EC2 node  (ví dụ IP: 10.0.1.47)
  ├── instana-agent pod    ← lắng nghe trên 10.0.1.47:4317
  ├── api-producer pod     ← gửi tới http://10.0.1.47:4317  ✔ cùng node
  └── auth-service pod     ← gửi tới http://10.0.1.47:4317  ✔ cùng node
```

Không có DNS lookup, không có cross-node traffic — nhanh nhất.

**Tại sao Nginx (frontend) KHÔNG thể dùng NODE_IP?**

`nginx.conf` là file cấu hình **tĩnh**. Nginx không có cơ chế expand biến môi trường
trong directive (như `otel_exporter { endpoint ... }`). Nếu viết:

```nginx
# KHÔNG hoạt động — nginx xử lý đây là chuỗi ký tự "(NODE_IP):4317"
otel_exporter {
    endpoint $(NODE_IP):4317;
}
```

Nginx sẽ cố gắng kết nối tới hostname có tên là chuỗi `$(NODE_IP)` — tất nhiên không
tồn tại, và module OTel sẽ không hoạt động.

**Giải pháp: Cluster-DNS FQDN**

Helm chart Instana tạo một **Kubernetes ClusterIP Service** tên là
`instana-agent.instana-agent.svc.cluster.local`. Tên này:
- Là chuỗi tĩnh, hardcode được vào `nginx.conf`
- Luôn resolve tới đúng agent pod qua kube-dns
- Không phụ thuộc vào IP của node

```nginx
# Hoạt động — tên FQDN tĩnh, kube-dns resolve tới agent
otel_exporter {
    endpoint instana-agent.instana-agent.svc.cluster.local:4317;
}
```

**Bảng tóm tắt:**

| Component | Pattern | Lý do |
|-----------|---------|-------|
| Go services (api-producer, auth, ...) | `NODE_IP:4317` qua Downward API | Go đọc env var lúc runtime được; ở cùng node với DaemonSet |
| Frontend (Nginx) | `instana-agent.instana-agent.svc.cluster.local:4317` | `nginx.conf` không expand env vars — cluster-DNS là lựa chọn duy nhất |
| Kong OTel plugin | `NODE_IP:4318` qua Downward API | Kong đọc env var được; dùng HTTP port 4318 thay vì gRPC 4317 |

Cả hai đều đến cùng một agent pod — chỉ khác cách resolve địa chỉ.

---

## Frontend (Nginx) — Logs, Metrics, Traces

Frontend là một SPA React tĩnh được phục vụ bởi Nginx. Nó proxy các request API và
WebSocket tới Kong. Cả ba loại tín hiệu đều được thu thập bởi Instana DaemonSet agent.

### Image được build như thế nào

[`frontend/Dockerfile`](../../frontend/Dockerfile) cài package `nginx-module-otel` từ Alpine,
package này chứa module OpenTelemetry động của NGINX (`ngx_otel_module.so`).
Module này được load ở đầu [`frontend/nginx.conf`](../../frontend/nginx.conf):

```nginx
load_module modules/ngx_otel_module.so;
```

> Module này được IBM/NGINX phát triển riêng cho Nginx. Không phải tất cả Nginx image đều
> có nó — phải dùng đúng Alpine package `nginx-module-otel`. Khi module không được load,
> toàn bộ lệnh `otel_*` trong nginx.conf sẽ bị lỗi ngay khi khởi động.

### Traces — ngx_otel_module

Cấu hình trong [`frontend/nginx.conf`](../../frontend/nginx.conf):

```nginx
otel_exporter {
    endpoint instana-agent.instana-agent.svc.cluster.local:4317;
}
otel_service_name frontend;
otel_trace on;
```

- Mỗi HTTP request qua Nginx tạo ra một OTLP/gRPC span gửi tới cluster-DNS Service của agent.
- `otel_trace_context extract` trên `/` — đọc W3C `traceparent` từ request đến (nếu có).
- `otel_trace_context propagate` trên `/api/` và `/ws` — đọc **và** ghi lại `traceparent`
  vào request upstream gửi tới Kong, tiếp tục chuỗi trace.

```nginx
location /api/ {
    otel_trace_context propagate;          # đọc traceparent từ client + inject vào upstream
    proxy_pass http://kong:8000/api/;
    ...
}

location /ws {
    otel_trace_context propagate;          # tương tự cho WebSocket upgrade
    proxy_pass http://kong:8000/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    ...
}
```

> **Tại sao cần `propagate` thay vì chỉ `extract`?** Nếu chỉ dùng `extract`, Nginx đọc
> `traceparent` từ request đến nhưng **không** ghi nó vào request gửi đi. Khi đó Kong và
> api-producer nhận request không có `traceparent` → tạo trace mới → không liên kết với
> span của Nginx → waterfall view bị đứt ở giữa.

### Metrics — stub_status

```nginx
location /nginx_status {
    stub_status;
    otel_trace off;           # không trace health-check scrapes
    allow 127.0.0.1;
    allow 10.42.0.0/16;       # k3s flannel CIDR
    allow 172.30.0.0/24;      # Docker Compose bridge
    deny all;
}
```

Instana DaemonSet agent tự phát hiện Nginx bằng cách scan process image tìm binary `nginx`
và đọc file config chính để tìm location `stub_status`.

Metrics thu thập được: active connections, accepts, handled, requests, reading, writing, waiting.

> **Tại sao `stub_status` phải ở `nginx.conf` chính (không phải `conf.d/`)?**
> Agent chỉ đọc file config chính khi tự phát hiện qua process scan. Nếu đặt trong
> `conf.d/default.conf` hay file con bất kỳ, agent sẽ không tìm thấy và không kích
> hoạt Nginx sensor.

### Logs

Nginx ghi access log và error log ra stdout/stderr. k3s container runtime bắt lấy và lưu:

```bash
kubectl logs -n banking deploy/frontend
```

Instana Infrastructure tự động liên kết log stream với frontend pod khi spans bắt đầu đến.

**Xem thêm:** [`09-pod-service-detection.md`](./09-pod-service-detection.md) §Frontend

---

## api-producer — Logs, Metrics, Traces

`api-producer` là Go Chi HTTP server nhận tất cả REST request từ Kong và dispatch chúng
dưới dạng NATS RPC calls tới các consumer microservice. Nó là **điểm vào chính** của
distributed traces trong backend.

### Traces — Dual Instrumentation (Hai lớp instrumentation)

`api-producer` dùng **hai** path tracing bổ sung cho nhau, cả hai được khởi tạo từ
[`internal/tracing/tracing.go`](../../internal/tracing/tracing.go).

**Tại sao cần hai lớp?** Vì chúng đo hai thứ hoàn toàn khác nhau:

| | OTel OTLP | Instana go-sensor |
|-|-----------|------------------|
| **Đo gì** | Request qua lại giữa các service | Trạng thái nội tại của tiến trình Go |
| **Đơn vị** | Span (thời gian bắt đầu/kết thúc của một operation) | Time-series metrics + profiling liên tục |
| **Xem ở đâu** | Analytics → Traces (waterfall view) | Infrastructure → Processes |
| **Protocol** | OTLP gRPC → port :4317 | Instana native protocol → port :42699 |
| **Có thể thay thế nhau không?** | Không | Không |

**1. OTel OTLP — distributed traces**

Chi router được bọc bằng `otelhttp.NewHandler` trong [`producer/main.go`](../../producer/main.go):

```go
server := &http.Server{
    Handler: otelhttp.NewHandler(router, serviceName),
}
```

Wrapper này tự động tạo một OTLP/gRPC span cho **mỗi HTTP request**, export tới
`http://$(NODE_IP):4317` (NODE_IP được inject qua Kubernetes Downward API: `status.hostIP`).

Mỗi span mang:
- Attribute: `http.method`, `http.route`, `http.status_code`
- Extract W3C `traceparent` từ Kong (Kong nhận từ Nginx) → span là **con** của Kong span,
  nối vào chuỗi trace đang có
- Inject context vào NATS RPC calls:

```go
// producer/rpc.go — NATS RPC dispatch
otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(hdr))
// rồi nats.PublishMsg(msg) với hdr đính kèm vào message
```

**2. Instana Go Collector — process-level metrics**

[`internal/tracing/tracing.go`](../../internal/tracing/tracing.go) gọi
`instana.InitCollector()` khi khởi động. Collector kết nối tới agent tại
`$(NODE_IP):42699` bằng Instana native protocol, bổ sung:

- **Go process dashboard**: heap đang dùng, GC pause duration, số goroutine, open file descriptors
- **Health signatures**: calls/s, mean response time, latency P95
- **AutoProfile™**: profiling CPU/memory/goroutine liên tục, không cần restart — bật qua
  `INSTANA_AUTO_PROFILE=true`

Collector instance được expose qua `tracing.Collector()` để `internal/db` và `internal/redis`
có thể attach Instana-native tracers vào driver của chúng.

> **Nếu chỉ dùng OTel mà không có go-sensor:** Bạn sẽ thấy trace waterfall hoạt động bình
> thường, nhưng toàn bộ phần "Processes" trên Instana Infrastructure sẽ trống — không có
> heap metrics, không có goroutine count, không có AutoProfile flame graphs. Quan trọng hơn,
> `instapgx` và `instaredis` sẽ không hoạt động vì chúng cần `tracing.Collector()` không nil.

### Metrics — Prometheus

`api-producer` expose Prometheus endpoint tại `http://api-producer:8080/metrics`.
Helm chart annotate pod với `prometheus.io/scrape: "true"` để Instana agent tự scrape
qua `prometheusAnnotations: strict`.

Key metrics:

| Metric | Labels |
|--------|--------|
| `http_requests_total` | `method`, `route`, `status` |
| `http_request_duration_seconds` | `method`, `route` |
| `rpc_requests_total` | `subject`, `status` |
| `rpc_roundtrip_duration_seconds` | `subject` |
| `nats_connected` | — |

### Logs

JSON có cấu trúc qua `log/slog`, ghi ra stdout. Mỗi dòng log đều có `"service": "api-producer"`:

```bash
kubectl logs -n banking deploy/api-producer
```

**Xem thêm:** [`03-opentelemetry.md`](./03-opentelemetry.md), [`14-go-sensor.md`](./14-go-sensor.md)

---

## Consumer Microservices — auth / account / transfer / notification

### Kiến Trúc

Cả bốn consumer service chia sẻ cùng một bộ instrumentation từ `internal/`:

| Package | Cung cấp gì |
|---------|------------|
| [`internal/tracing`](../../internal/tracing/) | OTel provider (OTLP/gRPC `:4317`) + Instana Go Collector (`:42699`) |
| [`internal/metrics`](../../internal/metrics/) | Prometheus metrics server (`/metrics`) |
| [`internal/nats`](../../internal/nats/) | NATS consumer với W3C traceparent extraction |
| [`internal/db`](../../internal/db/) | pgxpool với `instapgx` native DB spans |
| [`internal/redis`](../../internal/redis/) | go-redis client với `instaredis` native Redis spans |

Không có boilerplate tracing trong từng service — toàn bộ instrumentation nằm trong `internal/`.

`notification-service` bổ sung thêm `otelhttp.NewHandler` bọc HTTP Chi router để request
WebSocket upgrade (`GET /ws`) tạo ra span riêng. Span này:
- Nhận W3C `traceparent` được propagate từ Kong
- Tồn tại lâu — bao gồm từ lúc HTTP 101 upgrade đến khi client disconnect
- Là parent của các NATS consumer span tiếp theo được trigger bởi notifications

### Traces — NATS Span Continuation

#### NATS propagation hoạt động như thế nào?

NATS là message broker — khi `api-producer` gửi message tới NATS, không có HTTP request nào
xảy ra, nên không có header HTTP nào được tự động truyền. Phải **thủ công** inject context
vào NATS message header.

**Bước 1 — api-producer inject traceparent vào NATS message:**

```go
// producer/rpc.go
hdr := make(nats.Header)
otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(hdr))
// hdr["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
msg := &nats.Msg{Subject: subject, Header: hdr, Data: payload}
nc.PublishMsg(msg)
```

**Bước 2 — Consumer extract traceparent và tạo child span:**

```go
// internal/nats/consumer.go
ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(req.Headers()))
// Span mới tạo bằng ctx này sẽ là con của span api-producer
```

Kết quả: một distributed trace duy nhất kéo dài qua:
**Nginx → Kong → api-producer → NATS → consumer service**

**PostgreSQL spans** — [`internal/db/db.go`](../../internal/db/db.go) attach
`instapgx.InstanaTracer` vào cấu hình pgxpool connection:

```go
cfg.ConnConfig.Tracer = instapgx.InstanaTracer(cfg.ConnConfig, tracing.Collector())
```

Mỗi SQL query trở thành Instana-native DB span, là con của NATS consumer span.

**Redis spans** — [`internal/redis/redis.go`](../../internal/redis/redis.go) bọc
go-redis client bằng `instaredis.WrapClient`:

```go
instaredis.WrapClient(client, tracing.Collector())
```

Mỗi Redis command (GET, SET, PUBLISH) trở thành Instana-native Redis span, là con của
NATS consumer span.

> **Tại sao instapgx và instaredis dùng Instana native protocol thay vì OTel?**
> Vì chúng được IBM phát triển riêng cho Instana và tích hợp sâu hơn OTel: chúng biết
> cách map SQL statement thành Instana "database call" entity trong dependency graph,
> và chúng link với Infrastructure entities (PostgreSQL/Redis server) một cách chính xác.
> Nếu dùng OTel thuần (`otelpgx`, `redisotel`), các span sẽ xuất hiện nhưng có thể không
> link đúng với infrastructure entity trong Instana UI.

### Metrics — Prometheus

Mỗi consumer expose `/metrics` (port khác nhau, tất cả được annotate để auto-scrape):

| Service | Port | Annotation |
|---------|------|-----------|
| auth-service | :8001 | `prometheus.io/scrape: "true"` |
| account-service | :8002 | `prometheus.io/scrape: "true"` |
| transfer-service | :8003 | `prometheus.io/scrape: "true"` |
| notification-service | :8004 | `prometheus.io/scrape: "true"` |

Key metrics mỗi consumer:

| Metric | Labels |
|--------|--------|
| `nats_messages_total` | `service`, `action`, `status` |
| `nats_handler_duration_seconds` | `service`, `action` |
| `nats_reconnects_total` | `service` |

### Logs

JSON có cấu trúc qua `log/slog`, stdout. Mỗi dòng có `"service": "<tên>"`.
Các field bổ sung theo service:

| Service | Field bổ sung |
|---------|--------------|
| auth-service | `user_id`, `action` |
| transfer-service | `transfer_id`, `sender_id`, `receiver_id`, `amount` |
| account-service | `account_id` |
| notification-service | `user_id`, `channel` (`ws` hoặc `nats`) |

```bash
kubectl logs -n banking deploy/auth-service
kubectl logs -n banking deploy/transfer-service
```

**Xem thêm:** [`03-opentelemetry.md`](./03-opentelemetry.md), [`14-go-sensor.md`](./14-go-sensor.md),
[`11-nats-monitoring.md`](./11-nats-monitoring.md)

---

## PostgreSQL — Metrics và Traces

### Agent sensor (infrastructure metrics)

Instana PostgreSQL sensor tự phát hiện tiến trình PostgreSQL qua `/proc` scanning và kết nối
tới `postgres.banking-demo.svc.cluster.local:5432` bằng credentials từ
[`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.postgresql:
  user: 'banking'
  password: 'bankingpass'
  database: 'banking'
  poll_rate: 10
```

Sensor query `pg_stat_*` views mỗi 10 giây. Cần bật các PostgreSQL settings này (được set
qua init ConfigMap trong
[`helm/templates/postgres-init-configmap.yaml`](../../helm/templates/postgres-init-configmap.yaml)):

```
track_activities = on    # cho phép pg_stat_activity — ai đang làm gì
track_counts = on        # cho phép pg_stat_user_tables — rows inserted/updated/deleted
track_io_timing = on     # đo thời gian I/O — quan trọng cho phát hiện disk bottleneck
```

Key metrics: connections (active/idle/waiting), TPS, cache hit ratio, lock waits,
slow queries, bgwriter stats, tuple read/write rates.

> **Tại sao cần `password` trong config nhưng KHÔNG liệt kê `password` trong secrets list?**
> File `configuration.yaml` có block `com.instana.secrets` để agent redact (ẩn) các
> sensitive value trước khi log. Ban đầu có `password` trong list đó, nhưng agent redact
> cả password trong cấu hình sensor PostgreSQL trước khi kết nối → SCRAM authentication
> thất bại (error 08004). Giải pháp: bỏ `password` khỏi secrets list, PostgreSQL sensor
> mới hoạt động được.

### Native DB spans (instapgx)

go-sensor instrument mọi pgxpool query qua `instapgx.InstanaTracer`:

```go
// internal/db/db.go
cfg.ConnConfig.Tracer = instapgx.InstanaTracer(cfg.ConnConfig, tracing.Collector())
```

Kết quả trong Instana UI:

```
auth-service  NATS consumer span
  └─ SELECT * FROM users WHERE username = $1      instapgx span  ~1 ms
  └─ INSERT INTO sessions ...                     instapgx span  ~0.5 ms
```

**Xem thêm:** [`07-postgresql-sensor.md`](./07-postgresql-sensor.md)

---

## Redis — Metrics và Traces

### Agent sensor (infrastructure metrics)

Instana DaemonSet agent **tự phát hiện** Redis pod qua containerd process scanning —
nó thấy process `redis-server` trên node và kích hoạt sensor trực tiếp cho IP/port của pod đó.
Không cần block `hosts:` nào (và nếu thêm vào sẽ tạo ra node Redis trùng lặp trong UI).

Config hiện tại trong [`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.redis:
  use_ssl: false
  # hosts: []  ← có chủ đích bỏ trống; auto-discovery xử lý Redis
```

Flag `use_ssl: false` chặn một SSL warning thoáng qua khi sensor khởi động.
Metrics thu thập mỗi 10 giây: memory usage, connected clients, ops/sec,
keyspace hit ratio, slow command log.

Cách banking-demo dùng Redis:
- `auth-service`: session storage (`SET session:{sid}`, `GET session:{sid}`)
- `auth-service`: user cache (cache user records được đọc nhiều)
- `account-service`: balance read model (cache + invalidation)
- `notification-service`: pub/sub channel cho real-time notifications

> **Tại sao không dùng explicit `hosts:` block?** Khi DaemonSet agent tự phát hiện
> Redis qua `/proc`/containerd scan, nó tạo entity được key bởi **pod IP**
> (ví dụ `10.42.0.5:6379`). Nếu đồng thời thêm `hosts: [{host: redis, port: 6379}]`,
> agent tạo thêm entity thứ hai key bởi DNS name. Instana không merge hai entity này →
> dependency graph hiển thị hai Redis node riêng biệt, gây confusion.

### Native Redis spans (instaredis)

go-sensor instrument mọi go-redis command qua `instaredis.WrapClient`:

```go
// internal/redis/redis.go
instaredis.WrapClient(client, tracing.Collector())
```

Kết quả trong Instana UI:

```
auth-service  NATS consumer span
  └─ SET session:{sid}    instaredis span  ~0.3 ms
  └─ GET session:{sid}    instaredis span  ~0.2 ms
```

**Xem thêm:** [`06-redis-sensor.md`](./06-redis-sensor.md)

---

## Kong — Metrics và Traces

### Agent sensor (infrastructure metrics)

Instana Kong sensor poll Kong Admin API tại `kong.banking-demo.svc.cluster.local:8001`
mỗi 30 giây. `KONG_ADMIN_LISTEN` được set thành `0.0.0.0:8001` để agent DaemonSet pod
có thể reach qua cluster-DNS (port 8001 nằm trên Kong ClusterIP Service nhưng **không**
expose qua hostPort — chỉ internal trong cluster).

```yaml
com.instana.plugin.kong:
  enabled: true
  remote:
    - host: 'kong.banking-demo.svc.cluster.local'
      port: '8001'
      protocol: 'http'
      poll_rate: 30
```

Metrics: requests/s, latency (P50/P99), upstream connect time, HTTP status distributions,
error rate theo service/route.

### Traces — OTel plugin

Kong's built-in OpenTelemetry plugin push spans tới agent qua OTLP/HTTP:

```
Kong OTel plugin → http://$(NODE_IP):4318/v1/traces
```

Kong extract W3C `traceparent` đến từ Nginx và inject vào upstream request gửi tới
`api-producer` — Kong spans xuất hiện là con của Nginx spans.

**Xem thêm:** [`04-kong-sensor.md`](./04-kong-sensor.md)

---

## NATS — Chỉ có Metrics

NATS không có native Instana sensor. Metrics được thu thập qua `prometheus-nats-exporter`
sidecar pod expose `gnatsd_*` metrics trên `:7777`. Pod này có Prometheus annotations
để agent auto-scrape:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "7777"
```

Trace continuity qua NATS được cung cấp bởi W3C `traceparent` header propagation —
`api-producer` inject vào mỗi NATS message trước khi publish, mỗi consumer extract và tạo
child span.

> NATS messages không tạo span riêng, nhưng chuỗi trace không bị đứt.

**Xem thêm:** [`11-nats-monitoring.md`](./11-nats-monitoring.md)

---

## Chuỗi Trace End-to-End (Ví Dụ: User Đăng Nhập)

Một request hoàn chỉnh — user đăng nhập — tạo ra distributed trace này trong Instana UI:

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

Tất cả các tầng được liên kết bởi cùng một W3C `traceparent` header:

| Bước chuyển | Cơ chế |
|-------------|--------|
| Nginx → Kong | `otel_trace_context propagate` ghi header `traceparent` vào proxy request |
| Kong → api-producer | Kong OTel plugin extract + re-inject `traceparent` |
| api-producer → NATS msg | `otel.GetTextMapPropagator().Inject(ctx, HeaderCarrier(hdr))` |
| NATS msg → consumer span | `otel.GetTextMapPropagator().Extract(ctx, HeaderCarrier(req.Headers()))` |

### Điều gì xảy ra nếu một bước trong chuỗi bị đứt?

```
Nếu Nginx không propagate traceparent:
  → Kong tạo trace mới (traceID khác)
  → api-producer tạo trace mới
  → Instana thấy 3 trace riêng biệt thay vì 1 waterfall duy nhất

Nếu api-producer không inject vào NATS message:
  → auth-service tạo trace mới
  → Không thể thấy SQL query là do request nào gây ra
```

Config `com.instana.tracing.extra-http-headers` đảm bảo Instana cũng track giá trị
header thô cho mixed OTel + Instana-native span correlation:

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

## Tìm Tín Hiệu Trên Instana UI

| Muốn xem gì | Đường dẫn trên Instana UI |
|-------------|--------------------------|
| Tất cả pods & workloads | Infrastructure → Kubernetes → namespace `banking` |
| EC2 host metrics | Infrastructure → Hosts → `banking-dung-ec2` |
| Go process dashboards | Infrastructure → Processes → `<tên-service>` |
| Nginx metrics | Infrastructure → Processes → `frontend` (Nginx sensor) |
| PostgreSQL metrics | Infrastructure → `postgresql` |
| Redis metrics | Infrastructure → `redis` |
| Kong metrics | Infrastructure → `kong` |
| Service call rates & errors | Applications → Services |
| Distributed traces (waterfall) | Analytics → Calls → filter theo `service.name` |
| NATS broker metrics | Custom metrics (filter `entity.type:prometheus`) |
| Synthetic test results | Synthetic Monitoring → tests `banking-demo` |
| AutoProfile flame graphs | Analytics → Profiles |

---

## Những Điểm Còn Thiếu (Known Gaps)

| Vấn đề | Trạng thái | Cách khắc phục |
|--------|-----------|----------------|
| **NATS broker spans** | Không có native sensor — chỉ có metrics qua nats-exporter | Chấp nhận được: W3C propagation cho trace continuity, không cần NATS-internal spans |
| **Redis client spans via OTel** | Hiện chỉ có qua `instaredis` (Instana native) | Thêm `redisotel.InstrumentTracing` từ `go-redis/extra/redisotel` |
| **PostgreSQL client spans via OTel** | Hiện chỉ có qua `instapgx` (Instana native) | Thêm `otelpgx.NewTracer()` từ `exaring/otelpgx` |
| **Frontend structured logs** | Log forwarding chưa cấu hình | Deploy Fluentd/Fluent Bit hoặc dùng Instana log connector |
| **Kong trong Concert** | Không auto-import dù có traffic | Root cause chưa xác nhận — workaround: upload SBOM thủ công ([`15-concert-sbom-upload.md`](./15-concert-sbom-upload.md)) |
| **api-producer trong Concert** | Tương tự Kong | Workaround tương tự |
| **WebSocket session spans** | WS upgrade span tồn tại lâu; không emit span theo từng message | Mỗi WS message cần tạo span thủ công bên trong `notification-service` |

---

## Configuration Reference (instana/configuration.yaml)

Agent kéo [`instana/configuration.yaml`](../configuration.yaml) từ nhánh `main`
khi khởi động và sau mỗi GitOps hot-reload. Tóm tắt có chú thích:

```yaml
# --- OTel receiver (bật cả gRPC và HTTP đồng thời)
com.instana.plugin.opentelemetry:
  grpc: { enabled: true, port: 4317 }   # nhận OTLP từ Go pods + Nginx
  http: { enabled: true, port: 4318 }   # nhận OTLP từ Kong OTel plugin

# --- Prometheus auto-scrape (dựa trên annotation, không cần khai báo endpoint thủ công)
com.instana.plugin.prometheus:
  prometheusAnnotations: strict         # scrape pod có prometheus.io/scrape: "true"

# --- Nginx sensor (DaemonSet tự phát hiện; service_name đặt tên trên UI)
com.instana.plugin.nginx:
  service_name: frontend

# --- PostgreSQL sensor (credentials để query pg_stat_*)
com.instana.plugin.postgresql:
  user: 'banking'
  password: 'bankingpass'
  database: 'banking'
  poll_rate: 10

# --- Redis sensor (auto-discovery; không dùng hosts: để tránh node trùng lặp)
com.instana.plugin.redis:
  use_ssl: false

# --- Kong sensor (poll Admin API qua cluster-DNS)
com.instana.plugin.kong:
  enabled: true
  remote:
    - host: 'kong.banking-demo.svc.cluster.local'
      port: '8001'
      protocol: 'http'
      poll_rate: 30

# --- Traefik sensor (TẮT — k3s cài với --disable traefik)
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
  # LƯU Ý: "password" và "key" có chủ đích bị bỏ ra để tránh redact
  # credentials PostgreSQL sensor và các internal agent fields.

---

## Checklist Xác Minh Nhanh

```bash
# 1. Agent pods đang chạy
kubectl -n instana-agent get pods
# Kỳ vọng: instana-agent-<hash> 1/1 Running   k8sensor-<hash> 1/1 Running

# 2. OTLP ports đang lắng nghe trong agent pod
kubectl -n instana-agent exec ds/instana-agent -- \
  sh -c 'ss -tlnp | grep -E "4317|4318|42699"'
# Kỳ vọng: LISTEN trên :4317 :4318 :42699

# 3. Go services có đúng env vars
kubectl -n banking exec deploy/api-producer -- env | grep -E "NODE_IP|OTEL|INSTANA"
# Kỳ vọng: NODE_IP=10.x.x.x  OTEL_EXPORTER_OTLP_ENDPOINT=http://10.x.x.x:4317
#           INSTANA_AGENT_HOST=10.x.x.x  INSTANA_AGENT_PORT=42699

# 4. Frontend OTel module được load và config hợp lệ
kubectl -n banking exec deploy/frontend -- nginx -t
# Kỳ vọng: nginx: the configuration file /etc/nginx/nginx.conf syntax is ok

# 5. Kong có thể reach Admin API của nó
kubectl -n banking exec deploy/kong -- \
  curl -s http://localhost:8001/status | grep -o '"database":{"reachable":[^}]*}'
# Kỳ vọng: "database":{"reachable":true}

# 6. Tạo traffic để populate Application Services
for i in $(seq 1 10); do
  curl -sf "http://<EC2_IP>/api/health" > /dev/null
  sleep 1
done

# 7. Xác nhận spans đang đến agent
kubectl -n instana-agent logs ds/instana-agent --tail=30 \
  | grep -iE "otlp|span|frontend|api-producer|auth|account"

# 8. Xác nhận Redis auto-discovery
kubectl -n instana-agent logs ds/instana-agent --tail=100 \
  | grep -i "redis\|Activated Sensor"
# Kỳ vọng: "Activated Sensor for 10.42.x.x:6379"

# 9. Instana UI (đợi 30–60 giây sau khi có traffic)
#    Infrastructure → Kubernetes → banking namespace ✔
#    Applications → Services → api-producer, auth-service, ... ✔
#    Applications → Traces → waterfall qua tất cả các tầng ✔
#    Infrastructure → redis (auto-discovered, một entry duy nhất) ✔
#    Infrastructure → postgresql ✔
#    Infrastructure → kong ✔
```

---

## Chỉ Mục Tài Liệu Tham Khảo

| Chủ đề | Tài liệu |
|--------|---------|
| Cài đặt Agent (DaemonSet, Helm) — **bắt đầu từ đây** | [`13-k8s-agent-install.md`](./13-k8s-agent-install.md) |
| Cài đặt Agent (EC2 host-agent, legacy) | [`01-agent-install.md`](./01-agent-install.md) |
| Kubernetes sensor và pod discovery | [`02-kubernetes-monitoring.md`](./02-kubernetes-monitoring.md) |
| OTel SDK, NATS trace propagation, NODE_IP vs cluster-DNS | [`03-opentelemetry.md`](./03-opentelemetry.md) |
| Kong sensor và OTel plugin | [`04-kong-sensor.md`](./04-kong-sensor.md) |
| Traefik sensor (Traefik BỊ TẮT trong cluster này) | [`05-traefik-sensor.md`](./05-traefik-sensor.md) |
| Redis sensor + instaredis native spans | [`06-redis-sensor.md`](./06-redis-sensor.md) |
| PostgreSQL sensor + instapgx native spans | [`07-postgresql-sensor.md`](./07-postgresql-sensor.md) |
| Synthetic monitoring (API Script tests) | [`08-synthetic-monitoring.md`](./08-synthetic-monitoring.md) |
| Tại sao services hiện 0 calls / pod service detection | [`09-pod-service-detection.md`](./09-pod-service-detection.md) |
| K8s pods không được phát hiện (host-agent troubleshooting, legacy) | [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) |
| NATS metrics qua nats-exporter + W3C propagation | [`11-nats-monitoring.md`](./11-nats-monitoring.md) |
| Ansible Automation Action sensor | [`12-ansible-automation-action.md`](./12-ansible-automation-action.md) |
| Instana Go Collector (go-sensor) — AutoProfile, process metrics | [`14-go-sensor.md`](./14-go-sensor.md) |
| Concert SBOM upload workflow | [`15-concert-sbom-upload.md`](./15-concert-sbom-upload.md) |
