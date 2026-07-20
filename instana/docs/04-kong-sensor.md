# Kong API Gateway Sensor

> **Source:** https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-kong-api-gateway
> Condensed for: Kong 3.9 DB-less, monitored by Instana DaemonSet agent from within the cluster

---

## How It Works

The Instana Kong sensor is **automatically installed** after the agent starts. The DaemonSet
agent pod reaches Kong's Admin API directly via cluster-internal DNS — no NodePort needed.

```
DaemonSet agent pod (instana-agent namespace)
  └─ HTTP poll every 30 s → kong.banking.svc.cluster.local:8001 (Admin API)
```

Kong also emits distributed traces via its OTel plugin (push to agent `:4318`):

```
Kong (banking namespace)
  └─ OTLP/HTTP span → instana-agent.instana-agent.svc.cluster.local:4318/v1/traces
```

---

## Prerequisites

### 1. Kong Admin API accessible within the cluster

`KONG_ADMIN_LISTEN=0.0.0.0:8001` is set in [`helm/values.yaml`](../../helm/values.yaml) so
Kong binds the admin interface on all interfaces (not just loopback). Port 8001 is exposed
on the Kong ClusterIP Service in [`helm/templates/kong.yaml`](../../helm/templates/kong.yaml),
making it reachable from the `instana-agent` namespace via `kong.banking.svc.cluster.local:8001`.

### 2. Prometheus plugin enabled

The sensor depends on the Kong Prometheus plugin for latency, bandwidth, and request metrics.
Enabled as a global plugin in [`helm/values.yaml`](../../helm/values.yaml):

```yaml
globalPlugins:
  - name: prometheus
```

### 3. OTel plugin enabled

Distributed tracing for Kong requests is handled by the Kong OTel plugin, also a global plugin:

```yaml
  - name: opentelemetry
    config:
      traces_endpoint: "http://instana-agent.instana-agent.svc.cluster.local:4318/v1/traces"
      resource_attributes:
        service.name: kong
        service.namespace: banking-demo
      propagation:
        default_format: w3c
        extract: [w3c]
        inject:  [w3c]
```

---

## Supported Versions

| Technology | Support policy | Latest supported |
|------------|---------------|-----------------|
| Kong Gateway (OSS/Enterprise) | On demand | 3.10.0.0 |

Banking-demo uses Kong 3.9.

---

## Agent Configuration

From [`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.kong:
  enabled: true
  dataset_size: 10
  status_code_group: '2xx,3xx,4xx,5xx'
  remote:
    - host: 'kong.banking.svc.cluster.local'
      port: '8001'
      availabilityZone: 'banking-dung-ec2'
      poll_rate: 30        # minimum: 30 s per IBM docs
      protocol: 'http'
```

### Key Notes

- `poll_rate` minimum is **30 seconds** — do not set lower
- No auth configured — banking-demo Kong runs DB-less without RBAC
- Multiple `remote` entries can be listed for multiple Kong instances

---

## Metrics Collected

| Metric | Description |
|--------|-------------|
| Total HTTP requests | By service, route, status code |
| Kong latency | Time Kong spends processing requests |
| Upstream latency | Time upstream service takes to respond |
| Bandwidth | Ingress/egress bytes per service |
| Upstream health | Status of upstream targets |

---

## Kong Routes (banking-demo)

Kong proxies all API traffic to `api-producer` (port 8080) and WebSocket traffic to
`notification-service` (port 8004). All routes are path-prefix matched.

| Route | Protocols | Upstream | Notes |
|-------|-----------|----------|-------|
| `/api` | http, https | api-producer:8080 | All REST API — strip_path: false |
| `/ws` | http, https | notification-service:8004 | WebSocket upgrade — strip_path: false |
| `/` | http, https | frontend:80 | SPA catch-all |

> All REST API routes go through `api-producer` (Go Chi HTTP server), which forwards them as
> NATS RPC calls to the consumer services. Only WebSocket connections (`/ws`) bypass
> `api-producer` and reach `notification-service` directly.

---

## Verifying in Instana UI

1. **Infrastructure → Kubernetes → `banking` → kong** — Kong dashboard with request rates, latency
2. **Applications → Services → kong** — service health and call graph
3. **Analytics → Calls** — filter `service.name=kong` to see OTel spans

```bash
# Check sensor is active in agent logs
kubectl -n instana-agent logs ds/instana-agent --tail=50 | grep -i "kong"
# Expected: "Activated Sensor ... kong" or "Kong sensor activated"

# Verify Admin API is reachable from within the cluster
kubectl -n instana-agent exec ds/instana-agent -- \
  sh -c 'curl -sf http://kong.banking.svc.cluster.local:8001/status | head -c 200'
# Expected: {"database":{"reachability":true ...}

# Verify OTel traces are reaching the agent
kubectl -n instana-agent logs ds/instana-agent --tail=50 | grep -i "otlp\|kong.*span"
```

### Common Issue: `kong_admin_api_not_accessible`

Cause: Agent cannot reach the Admin API.

Checklist:
1. `KONG_ADMIN_LISTEN=0.0.0.0:8001` is set in `helm/values.yaml` kong env block
2. Port 8001 is listed in the Kong ClusterIP Service (`helm/templates/kong.yaml`)
3. `host: 'kong.banking.svc.cluster.local'` matches the Service name and namespace

```bash
# Confirm Kong Service exposes port 8001
kubectl -n banking get svc kong -o jsonpath='{.spec.ports}'
```
