# Pod & Service Detection

> For pods completely missing from Infrastructure → Kubernetes, start with
> [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) first.

---

## Two Things Called "Services"

| Instana view | Populated by | When |
|---|---|---|
| **Infrastructure → Kubernetes → Pods** | Containerd sensor — auto-discovery | Immediately on agent start |
| **Applications → Services** | OTLP span data from pods | Only after the first trace arrives |

Infrastructure is always detected. Application Services require trace data.

---

## What Each Service Looks Like in Instana

| Service | Tracing source | AP entry type |
|---|---|---|
| `api-producer` | `instana.TracingHandlerFunc` (native `g.http`) + `otelhttp` → `SpanKindServer` | Go HTTP Application Service |
| `auth-service` | NATS consumer → `SpanKindConsumer` | Messaging Application Service |
| `account-service` | NATS consumer → `SpanKindConsumer` | Messaging Application Service |
| `transfer-service` | NATS consumer → `SpanKindConsumer` | Messaging Application Service |
| `notification-service` | `otelhttp` WS upgrade → `SpanKindServer` + NATS consumer → `SpanKindConsumer` | HTTP + Messaging Application Service |
| `frontend` | `ngx_otel_module` → `SpanKindServer` | HTTP Application Service (after rebuild) |
| `kong` | Kong OTel plugin → `SpanKindServer` | HTTP Application Service |
| `postgres` | PostgreSQL sensor | Infrastructure metrics only |
| `redis` | Redis sensor | Infrastructure metrics only |
| `nats` | nats-exporter + Prometheus | Prometheus metrics only |

---

## End-to-End Trace Flow

```
Browser
  └─ frontend (Nginx, ngx_otel_module → agent :4317, W3C traceparent propagated)
       └─ Kong (OTel plugin → agent :4318, W3C traceparent propagated)
            ├─ /api/* → api-producer (Go, instana g.http → agent :42699 + otelhttp → agent :4317)
            │                └─ NATS RPC → auth / account / transfer / notification-service
            │                               (W3C traceparent extracted in dispatch → child span)
            │                                    └─ postgres / redis
            └─ /ws   → notification-service (Go, otelhttp WS upgrade span → agent :4317)
                                                 └─ Redis SUB (instaredis span, per-message)
```

`api-producer` injects `traceparent` into every NATS message header; each consumer extracts
it in `dispatch` and continues the trace as a child span.
The `/ws` WebSocket path bypasses `api-producer` entirely — Kong propagates `traceparent`
directly to `notification-service`, whose `otelhttp.NewHandler` wraps the upgrade request
and produces a single long-lived span covering the entire WebSocket session.
See [`11-nats-monitoring.md`](./11-nats-monitoring.md) for implementation details.

---

## Application Perspective Setup

> **Required for IBM Concert import.** Concert only ingests data from APs that filter by
> **both** `kubernetes.cluster.name` and `kubernetes.namespace.name`. OTel-only filters
> (e.g. `service.namespace`) are ignored by Concert.

1. **Instana UI → Applications → + New Application Perspective**
2. Add both filters:
   ```
   kubernetes.cluster.name   =  banking-dung
   kubernetes.namespace.name =  banking
   ```
   (`banking-dung` = `cluster.name` in [`helm-agent-values.yaml`](../helm-agent-values.yaml);
   `banking` = `global.namespace` in [`helm/values.yaml`](../../helm/values.yaml))
3. Name it `banking-demo`

---

## IBM Concert — Why Only 4 Container Images Are Imported

### What IBM documents as the only two limiting factors

Per IBM docs ([Integrating with IBM Concert](https://www.ibm.com/docs/en/instana-observability?topic=hosted-integrating-concert)):

1. **AP filter** — Concert only retrieves data for applications that define **both** `kubernetes.cluster.name` and `kubernetes.namespace.name` in their AP. OTel-only filters are ignored.
2. **7-day live traffic window** — only applications with live traffic in the past 7 days are included.

No other filtering by span kind, service type, or topology position is documented. Concert's stated behavior is to import **all** container images from pods in the matched cluster/namespace.

### Observed result vs. expected

| Service | In AP | Has live traffic | Concert imports image |
|---|---|---|---|
| `auth-service` | ✅ | ✅ | ✅ |
| `account-service` | ✅ | ✅ | ✅ |
| `transfer-service` | ✅ | ✅ | ✅ |
| `notification-service` | ✅ | ✅ | ✅ |
| `api-producer` | ✅ | ✅ | ✅ — fixed in commit `0e81f0f` |
| `frontend` | ✅ (after rebuild) | ✅ | ➖ — not applicable (static asset server, no business logic) |
| `kong` | ✅ | ✅ | ❌ — unexpected |

`kong` is still absent from Concert's `runtime-components`. `api-producer` is now detected
correctly (see below).

`frontend` is a **static Nginx asset server** — no application runtime, no deployable business
logic, no CVE-relevant language dependencies in the running container (Node.js exists only in
the discarded build stage). Concert's primary value for a container is CVE correlation via image
SBOMs and runtime topology mapping. For a pure static server both are marginal; the image SBOM
is still generated and uploaded for completeness (OS-level CVEs in the Nginx base image), but
Concert not detecting it in `runtime-components` is **expected and acceptable** — not a bug to
fix.

### Root cause — confirmed

The four original services are all **NATS consumers** whose entry spans carry
`messaging.system=nats`. The three initially absent services are HTTP-first workloads whose
entry spans are `SpanKindServer` HTTP.

`api-producer` previously lacked `instana.TracingHandlerFunc` — it had no native go-sensor
`g.http` entry spans, only OTel `SpanKindServer` HTTP spans. Instana was classifying it as a
generic **HTTP** service rather than a **Go** service. Concert's ingestion correlates container
images to AP service records by technology type, and generic HTTP services (without a go-sensor
process record) fall outside the correlation path.

**Fixed in commit [`0e81f0f`](https://github.com/dungxnd/banking-demo/commit/0e81f0f686e89d58e907ab01953d224bbc597615)** —
`instana.TracingHandlerFunc` added to `api-producer` in
[`producer/main.go`](../../producer/main.go). `api-producer` now emits native `g.http` entry
spans → Instana classifies it as technology **Go** (service type still displayed as **HTTP**,
which is correct — the HTTP protocol type is shown alongside the Go runtime) → Concert ingestion
now imports the image.

> **Note:** `api-producer` shows as service type **HTTP** in the Instana UI, not **Go**. This
> is expected: the technology field (Go) and the service type field (HTTP) are separate. What
> matters for Concert correlation is the go-sensor process record, which is now present.

### Fix regardless of root cause: manual SBOM upload

Image and code SBOMs are generated by `docker-build.yml` (Trivy → CycloneDX artifact).
See [`15-concert-sbom-upload.md`](./15-concert-sbom-upload.md).

> **frontend**: no manual workaround needed or planned — static Nginx server, Concert detection
> gap is expected. The image SBOM is still uploaded to cover OS-level CVEs in the Nginx base
> image.

---

## Checklist: Why OTLP Traces May Not Flow

### 1. Go services — OTLP endpoint not resolving

Each Go service Deployment sets:
```yaml
- name: NODE_IP
  valueFrom:
    fieldRef:
      fieldPath: status.hostIP
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: "http://$(NODE_IP):4317"
```

`$(NODE_IP)` expands to the host IP at pod start. If the `fieldRef` is missing, the literal
string `$(NODE_IP)` is passed to the exporter, which silently falls back to no-op.

```bash
kubectl -n banking exec deploy/auth-service -- env | grep -E 'NODE_IP|OTEL'
# Expected: NODE_IP=10.0.x.x   OTEL_EXPORTER_OTLP_ENDPOINT=http://10.0.x.x:4317

# Must show LISTEN inside the DaemonSet agent pod (not on the EC2 host)
kubectl -n instana-agent exec ds/instana-agent -- sh -c 'ss -tlnp | grep 4317'
```

### 2. Frontend — ngx_otel_module uses cluster DNS, not NODE_IP

`nginx.conf` cannot expand environment variables in directives. The OTLP endpoint is hardcoded
to the agent cluster-DNS Service FQDN:
```
instana-agent.instana-agent.svc.cluster.local:4317
```
This is always reachable from any namespace when the Helm agent is installed.

```bash
# Verify module loads and config is valid
kubectl -n banking exec deploy/frontend -- nginx -t

# Check spans arriving at the agent
kubectl -n instana-agent logs ds/instana-agent --tail=30 | grep -i "frontend\|span"
```

### 3. OTel plugin not enabled

[`instana/configuration.yaml`](../configuration.yaml) must have:
```yaml
com.instana.plugin.opentelemetry:
  grpc:
    enabled: true
    port: 4317
  http:
    enabled: true
    port: 4318
```
Already configured. If removed, all spans are silently dropped.

### 4. No traffic → no traces

Services appear in the AP only after a request creates a span:
```bash
curl -s http://<EC2-IP>/api/auth/health
curl -s http://<EC2-IP>/api/account/health
# Allow 30–60 s, then check Applications → Services
```

---

## Full Verification Sequence

```bash
# 1 — pods running
kubectl -n banking get pods

# 2 — OTLP env vars correct in Go services
kubectl -n banking exec deploy/auth-service -- env | grep OTEL

# 3 — frontend module loads
kubectl -n banking exec deploy/frontend -- nginx -t

# 4 — send traffic
for i in $(seq 1 10); do
  curl -s http://<EC2-IP>/api/health > /dev/null
  curl -s http://<EC2-IP>/api/health/account > /dev/null
done

# 5 — confirm spans arriving
kubectl -n instana-agent logs ds/instana-agent --tail=30 | grep -i "span\|otlp"

# 6 — Instana UI → Applications → Services (allow 30–60 s)
```

---

## Troubleshooting Quick Reference

| Symptom | Cause | Fix |
|---|---|---|
| Zero pods in Infrastructure → Kubernetes | k8s plugin disabled or kubeconfig unreadable | See [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) |
| Services show 0 calls | No OTLP traces received | Send traffic; verify `OTEL_EXPORTER_OTLP_ENDPOINT` in pods |
| `NODE_IP` not expanding | `fieldRef.fieldPath: status.hostIP` missing | All Go Deployment templates include the `NODE_IP` downward-API var |
| Go service missing from Applications | `OTEL_EXPORTER_OTLP_ENDPOINT` empty → no-op tracer | Check env vars in the pod |
| Agent not listening on 4317 | OTel gRPC plugin disabled | Add `grpc.enabled: true` to `com.instana.plugin.opentelemetry` |
| Pods in Infrastructure but not Applications | OTLP not flowing | Follow Full Verification Sequence above |
| `api-producer` / `kong` disappeared from AP after commit `a96de6bf` | `com.instana.plugin.kubernetes` was accidentally commented out — K8s metadata lost, OTel spans could not be correlated to pods | Fixed: block is restored in `configuration.yaml` (k8sensor handles discovery in DaemonSet mode) |
| PostgreSQL disappeared from Instana after commit `a96de6bf` | `com.instana.plugin.postgresql` was regressed to a non-existent `hosts:` array format — the official IBM schema uses flat keys (`user`, `password`, `database`) | Fixed: reverted to official flat format in `configuration.yaml` |
| Kong missing from Applications (Infrastructure sensor) | Admin API on loopback `127.0.0.1:8001` unreachable from agent pod | Fixed: `KONG_ADMIN_LISTEN` changed to `0.0.0.0:8001`; admin port added to Kong ClusterIP Service; `com.instana.plugin.kong` enabled in `configuration.yaml` |
| Frontend missing from Applications after rebuild | `ngx_otel_module` not loaded, or cluster-DNS unreachable | Run `nginx -t` inside the container; check agent log for `frontend` spans |
| Frontend absent from Concert `runtime-components` | Expected — static Nginx asset server with no application runtime; Concert detection gap is not a bug | Image SBOM is still uploaded for OS-level CVE coverage; no further action needed |
| `nginx_status_not_found` | `stub_status` location absent from `nginx.conf` | Already present in `frontend/nginx.conf` |
| NATS missing from Applications | NATS has no OTLP — expected | Metrics via nats-exporter + Prometheus |
| Postgres `password is empty string` error (every 10 min) | Agent auto-discovers the postgres pod and starts a second credential-less sensor | Benign — real metrics flow from the `configuration.yaml` sensor |
| Redis `SSL is disabled… Read timed out` | Transient SSL check at sensor init | Benign — sensor retries automatically |
| CVE sensor `PKIX path building failed` | Concert self-signed cert rejected by JVM | Set `ignore_cert: true` in `com.instana.plugin.cve.concert` |
| CVE sensor silent (no error, no data) | Wrong port in `base_url` | Use `12433` (VM/self-hosted) or `443` (SaaS) |
| ~~Concert imports only 4 of 6 images~~ ✅ resolved | `api-producer` lacked native go-sensor HTTP spans → Instana classified it as generic HTTP, not Go; Concert correlates by go-sensor technology | Fixed in commit `0e81f0f`: `instana.TracingHandlerFunc` added to `producer/main.go`; api-producer now detected as Go technology and imported by Concert |
| Concert imports 0 images | AP filter missing `kubernetes.cluster.name` or `kubernetes.namespace.name` | Reconfigure AP with both K8s filters: `banking-dung` / `banking` |
